package update

// The llama-ring engine is not part of a fleet release (a CUDA build is hundreds of megabytes): its variants are
// published as a generic package of the git server, <host>/api/packages/<owner>/generic/fleet-engine/<version>/,
// each an xz-compressed fleet binary with the engine linked in. This file chooses the variant that suits a machine,
// downloads it, checks it and puts it in place of the running binary.

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"runtime"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/tevfik/fleet-router/internal/config"
)

// EngineVersion is the version of the engine package this fleet is built against (the submodule third_party/fleet-engine
// at that point). A fleet asks for exactly this one: a newer engine may speak another protocol.
// FLEET_ENGINE_VERSION overrides it.
const EngineVersion = "0.5.7"

func engineVersion() string {
	if v := strings.TrimSpace(os.Getenv("FLEET_ENGINE_VERSION")); v != "" {
		return v
	}
	return EngineVersion
}

// PinnedEngineVersion is the package version in use (FLEET_ENGINE_VERSION, else the one this binary is built against).
func PinnedEngineVersion() string { return engineVersion() }

var engineVersionRe = regexp.MustCompile(`^[0-9]+\.[0-9]+\.[0-9]+([-+][0-9A-Za-z.]+)?$`)

// PinEngineVersion makes this process ask for package version v (`fleet engine install --version`). The value goes
// into an URL and, for --all, into a remote shell line: only a version number passes.
func PinEngineVersion(v string) error {
	v = strings.TrimSpace(v)
	if !engineVersionRe.MatchString(v) {
		return fmt.Errorf("--version %q: expected a version number such as 0.4.3", v)
	}
	return os.Setenv("FLEET_ENGINE_VERSION", v)
}

// EngineFile is one variant in the package (Gitea reports the SHA-256 of the file as uploaded).
type EngineFile struct {
	Name   string `json:"name"`
	Size   int64  `json:"size"`
	SHA256 string `json:"sha256"`
}

// EngineRepo is the repository whose package registry holds the fleet-engine packages. It is not built in: a build that
// may install the engine gets it at link time (-ldflags "-X …/internal/update.EngineRepo=<url>", see private.mk), any
// other fleet only through FLEET_ENGINE_REPO. Without one this fleet has no engine source.
var EngineRepo = ""

// EngineSource is where fleet-engine packages come from: FLEET_ENGINE_REPO, else EngineRepo ("": nowhere).
func EngineSource() string {
	if r := strings.TrimSpace(os.Getenv("FLEET_ENGINE_REPO")); r != "" {
		return strings.TrimRight(r, "/")
	}
	return EngineRepo
}

// ErrNoEngineSource: neither the build nor FLEET_ENGINE_REPO says where the engine packages are.
var ErrNoEngineSource = errors.New("no fleet-engine source: set FLEET_ENGINE_REPO=<repository URL> (fleet-engine is not distributed with fleet)")

// engineBase gives the API root and the owner of the engine repository: https://host/owner/fleet → (https://host, owner).
// FLEET_ENGINE_API replaces the root (tests, mirrors).
func engineBase() (root, owner string, err error) {
	src := EngineSource()
	if src == "" {
		return "", "", ErrNoEngineSource
	}
	u, err := url.Parse(src)
	if err != nil || u.Host == "" {
		return "", "", fmt.Errorf("bad engine repository URL %q", src)
	}
	parts := strings.Split(strings.Trim(u.Path, "/"), "/")
	if len(parts) < 2 {
		return "", "", fmt.Errorf("bad engine repository URL %q", src)
	}
	root = u.Scheme + "://" + u.Host
	if r := strings.TrimRight(os.Getenv("FLEET_ENGINE_API"), "/"); r != "" {
		root = r
	}
	return root, parts[len(parts)-2], nil
}

// EngineFiles lists the variants of the pinned package version.
func EngineFiles(ctx context.Context) ([]EngineFile, error) {
	root, owner, err := engineBase()
	if err != nil {
		return nil, err
	}
	resp, err := get(ctx, fmt.Sprintf("%s/api/v1/packages/%s/generic/fleet-engine/%s/files", root, owner, engineVersion()))
	if err != nil {
		return nil, fmt.Errorf("engine package %s: %w (the token needs the read:package scope)", engineVersion(), err)
	}
	defer resp.Body.Close()
	var fs []EngineFile
	if err := json.NewDecoder(io.LimitReader(resp.Body, 1<<20)).Decode(&fs); err != nil {
		return nil, err
	}
	var out []EngineFile
	for _, f := range fs {
		if strings.HasPrefix(f.Name, "fleet-engine-") && strings.HasSuffix(f.Name, ".xz") {
			out = append(out, f)
		}
	}
	return out, nil
}

// Machine is what decides which variant runs here.
type Machine struct {
	OS, Arch string
	GPU      string // NVIDIA: the card, "" when there is none
	Cap      string // its compute capability without the dot ("89")
	CUDA     int    // the CUDA major version its driver supports (0: unknown)
}

var cudaVer = regexp.MustCompile(`CUDA Version:\s*(\d+)`)

// DetectMachine reads the operating system, the architecture and, with nvidia-smi, the card and its driver.
func DetectMachine() Machine {
	m := Machine{OS: runtime.GOOS, Arch: runtime.GOARCH}
	if out, err := exec.Command("nvidia-smi", "--query-gpu=name,compute_cap", "--format=csv,noheader").Output(); err == nil {
		if l := strings.TrimSpace(strings.SplitN(string(out), "\n", 2)[0]); l != "" {
			name, cc, _ := strings.Cut(l, ",")
			m.GPU, m.Cap = strings.TrimSpace(name), strings.ReplaceAll(strings.TrimSpace(cc), ".", "")
		}
	}
	if m.GPU != "" {
		if out, err := exec.Command("nvidia-smi").Output(); err == nil {
			if g := cudaVer.FindStringSubmatch(string(out)); g != nil {
				m.CUDA, _ = strconv.Atoi(g[1])
			}
		}
	}
	return m
}

// variant is a file name taken apart: fleet-engine-linux-amd64-cuda12-sm89.xz → linux, amd64, cuda, 12, 89.
type variant struct {
	File                  EngineFile
	OS, Arch, Backend, Sm string
	CUDA                  int
}

var variantName = regexp.MustCompile(`^fleet-engine-([a-z]+)-([a-z0-9]+)-([a-z]+?)(\d*)(?:-sm(\d+))?\.xz$`)

func parseVariant(f EngineFile) (variant, bool) {
	g := variantName.FindStringSubmatch(f.Name)
	if g == nil {
		return variant{}, false
	}
	v := variant{File: f, OS: g[1], Arch: g[2], Backend: g[3], Sm: g[5]}
	v.CUDA, _ = strconv.Atoi(g[4])
	return v, true
}

// generalArchs are the architectures the variants without an -sm suffix are built for (.gitea/workflows of the
// engine); the identity of the downloaded binary is checked again before it replaces anything.
var generalArchs = map[string][]string{"amd64": {"80", "86", "89", "120"}, "arm64": {"120", "121"}}

// ChooseVariant picks the file for a machine and says why. An exact -sm match comes first, then a build for several
// cards, then CPU. The driver must support the CUDA major version of a build.
func ChooseVariant(m Machine, files []EngineFile) (EngineFile, string, error) {
	var mine []variant
	for _, f := range files {
		if v, ok := parseVariant(f); ok && v.OS == m.OS && v.Arch == m.Arch {
			mine = append(mine, v)
		}
	}
	if len(mine) == 0 {
		return EngineFile{}, "", fmt.Errorf("no engine build for %s/%s in package %s", m.OS, m.Arch, engineVersion())
	}
	sort.Slice(mine, func(i, j int) bool { return mine[i].CUDA > mine[j].CUDA }) // newest CUDA first
	usable := func(v variant) bool { return v.CUDA == 0 || m.CUDA == 0 || v.CUDA <= m.CUDA }
	if m.Cap != "" {
		for _, v := range mine {
			if v.Backend == "cuda" && v.Sm == m.Cap && usable(v) {
				return v.File, fmt.Sprintf("built for exactly this card (%s, sm_%s)", m.GPU, m.Cap), nil
			}
		}
		for _, v := range mine {
			if v.Backend != "cuda" || v.Sm != "" || !usable(v) {
				continue
			}
			for _, a := range generalArchs[m.Arch] {
				if a == m.Cap {
					return v.File, fmt.Sprintf("built for several cards, %s (sm_%s) is one of them", m.GPU, m.Cap), nil
				}
			}
		}
	}
	for _, v := range mine {
		if v.Backend == "cpu" {
			why := "no NVIDIA card"
			if m.Cap != "" {
				why = fmt.Sprintf("no CUDA build for %s (sm_%s) with this driver", m.GPU, m.Cap)
			}
			return v.File, why + ": the CPU build", nil
		}
	}
	return EngineFile{}, "", fmt.Errorf("no build of package %s suits this machine (%s, CUDA %d, %s/%s) and there is no CPU build", engineVersion(), orNone(m.GPU), m.CUDA, m.OS, m.Arch)
}

func orNone(s string) string {
	if s == "" {
		return "no GPU"
	}
	return s
}

// EngineInfo is what a binary says about its engine (`fleet ring info`).
type EngineInfo struct {
	Engine    string `json:"engine"`
	Protocol  int    `json:"protocol"`
	OS        string `json:"os"`
	Arch      string `json:"arch"`
	Backend   string `json:"backend"`
	CudaArchs string `json:"cuda_archs"`
}

func infoOf(bin string) (EngineInfo, error) {
	// a build that hangs at start must not hang the update with it
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	out, err := exec.CommandContext(ctx, bin, "ring", "info").Output()
	if err != nil {
		return EngineInfo{}, fmt.Errorf("the downloaded binary does not run here: %w", err)
	}
	var i EngineInfo
	if err := json.Unmarshal(out, &i); err != nil || i.Engine == "" {
		return EngineInfo{}, fmt.Errorf("the downloaded binary has no engine (ring info: %q)", strings.TrimSpace(string(out)))
	}
	return i, nil
}

// compatible: a CUDA build must contain a kernel for this card (never "native", which is rejected on release packages).
func compatible(i EngineInfo, m Machine) error {
	if i.OS != m.OS || i.Arch != m.Arch {
		return fmt.Errorf("the build is for %s/%s, this is %s/%s", i.OS, i.Arch, m.OS, m.Arch)
	}
	if i.Backend == "cuda" {
		if strings.EqualFold(strings.TrimSpace(i.CudaArchs), "native") {
			return fmt.Errorf("the CUDA build holds kernels for native (release packages must declare explicit CUDA architectures)")
		}
		if m.Cap != "" && i.CudaArchs != "" {
			for _, a := range strings.FieldsFunc(i.CudaArchs, func(r rune) bool { return r == ';' || r == ',' || r == ' ' }) {
				if strings.TrimSuffix(strings.TrimSuffix(a, "-real"), "-virtual") == m.Cap {
					return nil
				}
			}
			return fmt.Errorf("the CUDA build holds kernels for %s, this card is sm_%s", i.CudaArchs, m.Cap)
		}
	}
	return nil
}

func engineStampPath() string { return filepath.Join(filepath.Dir(config.Path()), "engine_installed") }

// EngineInstalled is "<version> <file>" of the last engine install here ("" when none).
func EngineInstalled() string {
	b, _ := os.ReadFile(engineStampPath())
	return strings.TrimSpace(string(b))
}

// InstallEngine downloads file, checks its SHA-256 against what the server reported, unpacks it with xz, runs it once
// to see its identity, and only then replaces exe (the previous binary is kept for `fleet update --rollback`).
func InstallEngine(ctx context.Context, f EngineFile, m Machine, exe string, progress func(string)) error {
	say := func(s string) {
		if progress != nil {
			progress(s)
		}
	}
	if _, err := exec.LookPath("xz"); err != nil {
		return fmt.Errorf("xz is needed to unpack the engine build (apt install xz-utils)")
	}
	root, owner, err := engineBase()
	if err != nil {
		return err
	}
	dir := filepath.Dir(exe)
	xzPath := filepath.Join(dir, ".fleet-engine.xz")
	binPath := filepath.Join(dir, fmt.Sprintf(".fleet-engine.new-%d", os.Getpid()))
	xzPath = filepath.Join(dir, fmt.Sprintf(".fleet-engine.xz-%d", os.Getpid()))
	defer os.Remove(xzPath)
	defer os.Remove(binPath)
	say(fmt.Sprintf("downloading %s (%.0f MB)", f.Name, float64(f.Size)/1e6))
	resp, err := getLong(ctx, fmt.Sprintf("%s/api/packages/%s/generic/fleet-engine/%s/%s", root, owner, engineVersion(), f.Name))
	if err != nil {
		return err
	}
	out, err := os.OpenFile(xzPath, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0o600)
	if err != nil {
		resp.Body.Close()
		return err
	}
	h := sha256.New()
	_, err = io.Copy(io.MultiWriter(out, h), resp.Body)
	resp.Body.Close()
	if cerr := out.Close(); err == nil {
		err = cerr
	}
	if err != nil {
		return err
	}
	if got := hex.EncodeToString(h.Sum(nil)); f.SHA256 == "" || got != f.SHA256 {
		return fmt.Errorf("checksum mismatch for %s (the server says %q, got %s)", f.Name, f.SHA256, got)
	}
	say("unpacking")
	bin, err := os.OpenFile(binPath, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0o755)
	if err != nil {
		return err
	}
	cmd := exec.CommandContext(ctx, "xz", "-dc", xzPath)
	cmd.Stdout = bin
	xerr := cmd.Run()
	if cerr := bin.Close(); xerr == nil {
		xerr = cerr
	}
	if xerr != nil {
		return fmt.Errorf("unpacking %s: %w", f.Name, xerr)
	}
	info, err := infoOf(binPath)
	if err != nil {
		return err
	}
	if err := compatible(info, m); err != nil {
		return err
	}
	say(fmt.Sprintf("engine %s, protocol %d, %s/%s %s %s", info.Engine, info.Protocol, info.OS, info.Arch, info.Backend, info.CudaArchs))
	if err := keepPrevious(exe); err != nil {
		return fmt.Errorf("cannot keep the current binary for a rollback: %w", err)
	}
	if err := os.Rename(binPath, filepath.Clean(exe)); err != nil {
		return err
	}
	if msg := ensureStageLink(exe); msg != "" {
		say(msg)
	}
	_ = os.WriteFile(engineStampPath(), []byte(engineVersion()+" "+f.Name+"\n"), 0o644)
	// like a release update: a build that crash-loops at start is rolled back by OnStart (it was not tracked at all)
	MarkPending(engineVersion())
	return nil
}

// ensureStageLink makes fleet-engine-stage (the name a chain deploy starts its stages by) a link to this binary, which
// carries the stage mode. A real file of that name (a standalone build) is left alone; the answer says so.
func ensureStageLink(exe string) string {
	link := filepath.Join(filepath.Dir(exe), "fleet-engine-stage")
	if fi, err := os.Lstat(link); err == nil {
		if fi.Mode()&os.ModeSymlink == 0 {
			return "a standalone " + link + " exists and stays: remove it so that chain deploys use this build"
		}
		_ = os.Remove(link)
	}
	if err := os.Symlink(filepath.Base(exe), link); err != nil {
		return "cannot link " + link + ": " + err.Error()
	}
	return ""
}

// getLong is get without the short client timeout: an engine build is hundreds of megabytes.
func getLong(ctx context.Context, u string) (*http.Response, error) {
	req, err := http.NewRequestWithContext(ctx, "GET", u, nil)
	if err != nil {
		return nil, err
	}
	if t := token(); t != "" && tokenMaySendTo(req.URL) { // https or this machine only, as in get
		req.Header.Set("Authorization", "token "+t)
	}
	resp, err := (&http.Client{Transport: &http.Transport{ResponseHeaderTimeout: 2 * time.Minute}}).Do(req)
	if err != nil {
		return nil, err
	}
	if resp.StatusCode != 200 {
		resp.Body.Close()
		return nil, fmt.Errorf("%s: %s", u, resp.Status)
	}
	return resp, nil
}

// EnsureEngine puts the engine build that suits this machine in place of exe. variant ("" = chosen by the machine) is a
// file name or any part of one; without force, a binary that already is that build is left alone.
// It returns what happened in one line.
func EnsureEngine(ctx context.Context, exe, variant string, force bool, progress func(string)) (string, error) {
	files, err := EngineFiles(ctx)
	if err != nil {
		return "", err
	}
	m := DetectMachine()
	var f EngineFile
	why := "chosen by hand"
	if variant != "" {
		found := 0
		for _, c := range files {
			if c.Name == variant || c.Name == variant+".xz" || strings.Contains(c.Name, variant) {
				f, found = c, found+1
			}
		}
		if found != 1 {
			return "", fmt.Errorf("%d builds match %q (have: %s)", found, variant, names(files))
		}
	} else if f, why, err = ChooseVariant(m, files); err != nil {
		return "", err
	}
	if !force && strings.HasPrefix(EngineInstalled(), engineVersion()+" "+f.Name) {
		return fmt.Sprintf("already the engine build %s %s (--force installs it again)", engineVersion(), f.Name), nil
	}
	if err := InstallEngine(ctx, f, m, exe, progress); err != nil {
		return "", err
	}
	return fmt.Sprintf("installed the engine build %s %s — %s (go back with `fleet update --rollback`)", engineVersion(), f.Name, why), nil
}

func names(fs []EngineFile) string {
	var n []string
	for _, f := range fs {
		n = append(n, strings.TrimSuffix(strings.TrimPrefix(f.Name, "fleet-engine-"), ".xz"))
	}
	return strings.Join(n, ", ")
}
