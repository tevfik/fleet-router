// Package update replaces the running binary with the newest release published in the git repository (Gitea or
// GitHub releases).
package update

import (
	"bufio"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"time"

	"github.com/tevfik/fleet-router/internal/config"
)

// Repo is where this build looks for new releases (a Gitea or GitHub repository URL). It is set at link time
// (-X github.com/tevfik/fleet-router/internal/update.Repo=…): the public build points at the public repository, a private
// build at its own (private.env). FLEET_REPO overrides it; FLEET_TOKEN authenticates for a private Gitea repository.
// Empty: the build does not update itself (a plain `go build`).
var Repo = ""

// ErrNoSource: neither the build nor FLEET_REPO says where releases are published.
var ErrNoSource = errors.New("this build has no update source: set FLEET_REPO=<repository URL> (Gitea or GitHub)")

// Source is the repository releases come from, "" when the build has none.
func Source() string {
	if r := os.Getenv("FLEET_REPO"); r != "" {
		return strings.TrimRight(r, "/")
	}
	return strings.TrimRight(Repo, "/")
}

func repo() string { return Source() }

// githubHost/githubAPI: a repository on githubHost is read through githubAPI (variables for the tests).
var (
	githubHost = "github.com"
	githubAPI  = "https://api.github.com"
)

func isGitHub(u *url.URL) bool {
	h := u.Hostname()
	if u.Port() != "" {
		h = u.Host
	}
	return h == githubHost || h == "api.github.com" || strings.HasSuffix(h, ".githubusercontent.com")
}

// Release is the part of a Gitea release that matters here.
type Release struct {
	Tag  string `json:"tag_name"`
	Body string `json:"body"` // the release notes (the tag message when CI published it)
}

// Notes is the release text trimmed for a terminal: at most 12 lines.
func (r Release) Notes() string {
	var out []string
	for _, l := range strings.Split(strings.TrimSpace(r.Body), "\n") {
		if strings.TrimSpace(l) != "" {
			out = append(out, "  "+strings.TrimRight(l, " \r"))
		}
	}
	if len(out) > 12 {
		out = append(out[:12], "  …")
	}
	return strings.Join(out, "\n")
}

// token is FLEET_TOKEN, else the first line of <config dir>/update_token (what a systemd service uses:
// it has no shell environment). Only needed when the repository is private.
func token() string {
	if t := strings.TrimSpace(os.Getenv("FLEET_TOKEN")); t != "" {
		return t
	}
	b, err := os.ReadFile(filepath.Join(filepath.Dir(config.Path()), "update_token"))
	if err != nil {
		return ""
	}
	return strings.TrimSpace(strings.SplitN(string(b), "\n", 2)[0])
}

func get(ctx context.Context, url string) (*http.Response, error) {
	req, err := http.NewRequestWithContext(ctx, "GET", url, nil)
	if err != nil {
		return nil, err
	}
	// the token is for a private Gitea: never to GitHub (public releases there need none)
	if t := token(); t != "" && tokenMaySendTo(req.URL) && !isGitHub(req.URL) {
		req.Header.Set("Authorization", "token "+t)
	}
	resp, err := (&http.Client{Timeout: 5 * time.Minute}).Do(req)
	if err != nil {
		return nil, err
	}
	if resp.StatusCode != 200 {
		resp.Body.Close()
		return nil, fmt.Errorf("%s: %s", url, resp.Status)
	}
	return resp, nil
}

// apiURL turns https://host/owner/name into https://host/api/v1/repos/owner/name/<path> (Gitea), or
// https://api.github.com/repos/owner/name/<path> for a GitHub repository. The release JSON (tag_name, body) and the
// download URL (<repo>/releases/download/<tag>/<asset>) are the same on both.
func apiURL(repoURL, path string) (string, error) {
	if u, err := url.Parse(repoURL); err == nil && u.Host != "" && isGitHub(u) {
		p := strings.Trim(u.Path, "/")
		if strings.Count(p, "/") != 1 {
			return "", fmt.Errorf("bad GitHub repository URL %q (want https://github.com/owner/name)", repoURL)
		}
		return githubAPI + "/repos/" + p + path, nil
	}
	i := strings.Index(repoURL, "://")
	if i < 0 {
		return "", fmt.Errorf("bad repository URL %q", repoURL)
	}
	rest := repoURL[i+3:]
	j := strings.Index(rest, "/")
	if j < 0 {
		return "", fmt.Errorf("bad repository URL %q", repoURL)
	}
	return repoURL[:i+3] + rest[:j] + "/api/v1/repos/" + rest[j+1:] + path, nil
}

// Latest returns the newest published release.
func Latest(ctx context.Context) (Release, error) {
	var r Release
	if repo() == "" {
		return r, ErrNoSource
	}
	u, err := apiURL(repo(), "/releases/latest")
	if err != nil {
		return r, err
	}
	resp, err := get(ctx, u)
	if err != nil {
		return r, err
	}
	defer resp.Body.Close()
	if err := json.NewDecoder(resp.Body).Decode(&r); err != nil {
		return r, err
	}
	if r.Tag == "" {
		return r, fmt.Errorf("no release published yet")
	}
	return r, nil
}

func parse(v string) (n [3]int, ok bool) {
	parts := strings.SplitN(strings.TrimPrefix(v, "v"), ".", 3)
	if len(parts) != 3 {
		return n, false
	}
	for i, p := range parts {
		if k := strings.IndexAny(p, "-+"); k >= 0 { // v1.2.3-4-gabc, v1.2.3-dirty
			p = p[:k]
		}
		x, err := strconv.Atoi(p)
		if err != nil {
			return n, false
		}
		n[i] = x
	}
	return n, true
}

// Newer reports whether release tag is newer than the running version. A running version that is not a
// release (dev, a bare commit hash) is "unknown": it is updated only when force is set.
func Newer(tag, current string, force bool) bool {
	t, ok := parse(tag)
	if !ok {
		return false
	}
	c, ok := parse(current)
	if !ok {
		return force
	}
	for i := range t {
		if t[i] != c[i] {
			return t[i] > c[i]
		}
	}
	return force && strings.ContainsAny(current, "-+") && current != tag // dirty/ahead build of the same number
}

func stampPath() string { return filepath.Join(filepath.Dir(config.Path()), "last_update") }

// Installed remembers the tag the daemon last installed by itself.
func Installed() string {
	b, _ := os.ReadFile(stampPath())
	return strings.TrimSpace(string(b))
}

// MarkInstalled records tag as auto-installed. A binary whose built-in version does not match its release
// tag would otherwise look outdated forever and be re-downloaded (and the daemon restarted) in a loop.
func MarkInstalled(tag string) { _ = os.WriteFile(stampPath(), []byte(tag+"\n"), 0o644) }

// Apply downloads the release binary for this machine, checks it against SHA256SUMS and atomically
// replaces the file at exe (a running binary can be renamed over on Linux).
func Apply(ctx context.Context, tag, exe string) error {
	name := "fleet-" + runtime.GOOS + "-" + runtime.GOARCH
	if repo() == "" {
		return ErrNoSource
	}
	base := fmt.Sprintf("%s/releases/download/%s/", repo(), tag)
	// A temp name of its own (two updaters in one directory must not write into one file), next to exe so the final
	// rename is atomic.
	out, err := os.CreateTemp(filepath.Dir(filepath.Clean(exe)), ".fleet-update-*")
	if err != nil {
		return err
	}
	tmp := out.Name()
	if err := out.Chmod(0o755); err != nil {
		out.Close()
		os.Remove(tmp)
		return err
	}
	defer os.Remove(tmp) // no-op after a successful rename
	resp, err := get(ctx, base+name)
	if err != nil {
		out.Close()
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
	sums, err := get(ctx, base+"SHA256SUMS")
	if err != nil {
		return fmt.Errorf("cannot verify the download (no SHA256SUMS): %w", err)
	}
	defer sums.Body.Close()
	want := ""
	sc := bufio.NewScanner(sums.Body)
	for sc.Scan() {
		if f := strings.Fields(sc.Text()); len(f) == 2 && strings.TrimPrefix(f[1], "*") == name {
			want = f[0]
		}
	}
	if got := hex.EncodeToString(h.Sum(nil)); want == "" || got != want {
		return fmt.Errorf("checksum mismatch for %s (expected %q, got %s)", name, want, got)
	}
	if err := keepPrevious(exe); err != nil {
		return fmt.Errorf("cannot keep the current binary for a rollback: %w", err)
	}
	return os.Rename(tmp, filepath.Clean(exe))
}

// keepPrevious preserves the binary about to be replaced as exe+".prev" (a hard link: the old inode stays
// alive after the rename, copy as a fallback across file systems).
func keepPrevious(exe string) error {
	prev := exe + ".prev"
	os.Remove(prev)
	if err := os.Link(exe, prev); err == nil {
		return nil
	}
	in, err := os.Open(exe)
	if err != nil {
		return err
	}
	defer in.Close()
	out, err := os.OpenFile(prev, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0o755)
	if err != nil {
		return err
	}
	if _, err := io.Copy(out, in); err != nil {
		out.Close()
		return err
	}
	return out.Close()
}

// Rollback puts the previous binary back. The tag being rolled back from is remembered so that the
// auto-update does not install it again.
func Rollback(exe, fromTag string) error {
	prev := exe + ".prev"
	if _, err := os.Stat(prev); err != nil {
		return fmt.Errorf("no previous version kept (%s)", prev)
	}
	tmp := exe + ".rollback"
	if err := copyFile(prev, tmp); err != nil {
		return err
	}
	if err := os.Rename(tmp, exe); err != nil {
		os.Remove(tmp)
		return err
	}
	if fromTag != "" {
		MarkInstalled(fromTag)
	}
	ClearPending()
	return nil
}

func copyFile(src, dst string) error {
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()
	out, err := os.OpenFile(dst, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0o755)
	if err != nil {
		return err
	}
	if _, err := io.Copy(out, in); err != nil {
		out.Close()
		return err
	}
	return out.Close()
}

// A freshly installed version is "pending" until it has run for a while. If the daemon restarts again and
// again without getting there (it crashes on start), the previous binary is put back automatically.
type pending struct {
	Tag      string `json:"tag"`
	Attempts int    `json:"attempts"`
}

func pendingPath() string { return filepath.Join(filepath.Dir(config.Path()), "update_pending") }

// MarkPending records that tag was just installed and has not proven itself yet.
func MarkPending(tag string) {
	b, _ := json.Marshal(pending{Tag: tag})
	_ = os.WriteFile(pendingPath(), b, 0o644)
}

// ClearPending is called once the new version has run healthily.
func ClearPending() { _ = os.Remove(pendingPath()) }

// MaxStarts is how many starts of an unproven version are tolerated before rolling back.
const MaxStarts = 3

// OnStart is called when the daemon starts. It counts the start of an unproven version and, when the
// limit is exceeded, restores the previous binary; it returns the rolled-back tag then.
func OnStart(exe string) (rolledBack string, err error) {
	b, rerr := os.ReadFile(pendingPath())
	if rerr != nil {
		return "", nil
	}
	var p pending
	if json.Unmarshal(b, &p) != nil {
		ClearPending()
		return "", nil
	}
	p.Attempts++
	if p.Attempts > MaxStarts {
		if err := Rollback(exe, p.Tag); err != nil {
			ClearPending()
			return "", err
		}
		return p.Tag, nil
	}
	nb, _ := json.Marshal(p)
	_ = os.WriteFile(pendingPath(), nb, 0o644)
	return "", nil
}

// tokenMaySendTo: the update token goes only over https (or to this machine): with FLEET_REPO on plain http it would
// cross the network in clear text and any on-path party could also replace the binary and its checksums.
func tokenMaySendTo(u *url.URL) bool {
	if u.Scheme == "https" {
		return true
	}
	h := u.Hostname()
	if ip := net.ParseIP(h); ip != nil {
		return ip.IsLoopback()
	}
	return h == "localhost"
}
