// Package discover finds OpenAI-compatible endpoints (local + SSH). It skips itself and its own tunnels.
package discover

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/tevfik/fleet-router/internal/config"
)

type Found struct {
	Node   string   `json:"node"` // machine name (hostname / ssh alias)
	SSH    string   `json:"ssh"`  // empty = local
	Port   int      `json:"port"`
	Models []string `json:"models"`
	// Caps maps model → capabilities reported by Ollama's /api/show (absent for other servers).
	Caps map[string][]string `json:"caps,omitempty"`
	// Fleet is set when the port belongs to another fleet (it answered GET /fleet/hello): such a port is
	// offered as a peer, not as a model endpoint.
	Fleet *FleetInfo `json:"fleet,omitempty"`
}

// FleetInfo is what a fleet's /fleet/hello says.
type FleetInfo struct {
	ID   string `json:"id"`
	Name string `json:"name"`
}

// FleetPort is the port fleets listen on by default; scans always include it so peers are found
// without any registry.
const FleetPort = 4711

// ScanFleetPort is the fleet port every local scan adds (0: none — tests, so they never reach the real daemon).
var ScanFleetPort = FleetPort

func parseHello(b []byte) *FleetInfo {
	var h struct {
		Fleet bool   `json:"fleet"`
		ID    string `json:"id"`
		Name  string `json:"name"`
	}
	if json.Unmarshal(b, &h) != nil || !h.Fleet {
		return nil
	}
	return &FleetInfo{ID: h.ID, Name: h.Name}
}

func localHello(ctx context.Context, port int) *FleetInfo {
	cx, cancel := context.WithTimeout(ctx, 3*time.Second)
	defer cancel()
	req, _ := http.NewRequestWithContext(cx, "GET", fmt.Sprintf("http://127.0.0.1:%d/fleet/hello", port), nil)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return nil
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<16))
	if resp.StatusCode != 200 {
		return nil
	}
	return parseHello(b)
}

func (f Found) URL() string { return fmt.Sprintf("http://127.0.0.1:%d/v1", f.Port) }

var selfName string // overrides the hostname (set from the fleet's `name:`)

// SetSelf names this machine in scan results (empty = the hostname).
func SetSelf(n string) { selfName = n }

func hostname() string { h, _ := os.Hostname(); return strings.Split(h, ".")[0] }

// Self is the name this machine is registered under: the configured fleet name, else the hostname.
func Self() string {
	if selfName != "" {
		return selfName
	}
	return hostname()
}

func ParsePorts(spec string) []int {
	set := map[int]bool{}
	for _, part := range strings.Split(spec, ",") {
		part = strings.TrimSpace(part)
		if lo, hi, ok := strings.Cut(part, "-"); ok {
			a, e1 := strconv.Atoi(lo)
			b, e2 := strconv.Atoi(hi)
			for p := a; e1 == nil && e2 == nil && p <= b && p-a < 5000; p++ {
				set[p] = true
			}
		} else if p, err := strconv.Atoi(part); err == nil {
			set[p] = true
		}
	}
	var out []int
	for p := range set {
		out = append(out, p)
	}
	sortInts(out)
	return out
}

func sortInts(a []int) {
	for i := 1; i < len(a); i++ {
		for j := i; j > 0 && a[j] < a[j-1]; j-- {
			a[j], a[j-1] = a[j-1], a[j]
		}
	}
}

func parseModels(b []byte) []string {
	var d struct {
		Data []struct {
			ID string `json:"id"`
		} `json:"data"`
		Models []struct {
			ID   string `json:"id"`
			Name string `json:"name"`
		} `json:"models"`
	}
	if json.Unmarshal(b, &d) != nil {
		return nil
	}
	var ms []string
	for _, m := range d.Data {
		ms = append(ms, m.ID)
	}
	for _, m := range d.Models {
		if m.ID != "" {
			ms = append(ms, m.ID)
		} else if m.Name != "" {
			ms = append(ms, m.Name)
		}
	}
	return ms
}

// Scan: local ports + ssh aliases. skipLocal: ports to skip in the local scan (our own tunnels).
// notes: skip/error messages to show the user.
func Scan(ctx context.Context, ports []int, sshHosts []string, skipLocal map[int]string) (found []Found, notes []string) {
	self := Self()
	if ScanFleetPort > 0 {
		ports = withPort(ports, ScanFleetPort)
	}
	var mu sync.Mutex
	var wg sync.WaitGroup

	// local
	for _, p := range ports {
		if owner, ok := skipLocal[p]; ok {
			notes = append(notes, fmt.Sprintf("- %s:%d: tunnel (%s) — registered under its owner, skipped", self, p, owner))
			continue
		}
		wg.Add(1)
		go func(p int) {
			defer wg.Done()
			if fi := localHello(ctx, p); fi != nil { // a fleet is a peer even when it offers no models yet
				mu.Lock()
				found = append(found, Found{Node: self, Port: p, Fleet: fi})
				mu.Unlock()
				return
			}
			ms := localModels(ctx, p)
			if len(ms) == 0 {
				return
			}
			caps := localCaps(ctx, p, ms)
			mu.Lock()
			found = append(found, Found{Node: self, Port: p, Models: ms, Caps: caps})
			mu.Unlock()
		}(p)
	}
	wg.Wait()

	// remote (parallel, one SSH per machine)
	for _, h := range sshHosts {
		h = strings.TrimSpace(h)
		if h == "" {
			continue
		}
		if h == self || h == hostname() {
			notes = append(notes, fmt.Sprintf("- %s: this machine itself — SSH skipped (the local scan covers it)", h))
			continue
		}
		wg.Add(1)
		go func(h string) {
			defer wg.Done()
			fs, note := sshScan(ctx, h, ports, self)
			mu.Lock()
			found = append(found, fs...)
			if note != "" {
				notes = append(notes, note)
			}
			mu.Unlock()
		}(h)
	}
	wg.Wait()
	return
}

func localModels(ctx context.Context, port int) []string {
	c, err := net.DialTimeout("tcp", fmt.Sprintf("127.0.0.1:%d", port), 300*time.Millisecond)
	if err != nil {
		return nil
	}
	c.Close()
	cx, cancel := context.WithTimeout(ctx, 4*time.Second)
	defer cancel()
	for _, path := range []string{"/v1/models", "/models"} {
		req, _ := http.NewRequestWithContext(cx, "GET", fmt.Sprintf("http://127.0.0.1:%d%s", port, path), nil)
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			continue
		}
		b, _ := io.ReadAll(io.LimitReader(resp.Body, 4<<20))
		resp.Body.Close()
		if resp.StatusCode == 200 {
			if ms := parseModels(b); len(ms) > 0 {
				return ms
			}
		}
	}
	return nil
}

func sshScan(ctx context.Context, host string, ports []int, self string) ([]Found, string) {
	opts := []string{"-o", "BatchMode=yes", "-o", "ConnectTimeout=6"}
	cx, cancel := context.WithTimeout(ctx, 20*time.Second)
	out, err := exec.CommandContext(cx, "ssh", append(opts, "--", host, "hostname")...).CombinedOutput()
	cancel()
	if err != nil {
		return nil, fmt.Sprintf("! %s: unreachable (%s)", host, firstLine(string(out)))
	}
	if rh := strings.Split(strings.TrimSpace(string(out)), ".")[0]; rh == self || rh == hostname() {
		return nil, fmt.Sprintf("- %s: remote hostname=%s → this machine itself, skipped", host, self)
	}
	var ps []string
	for _, p := range ports {
		ps = append(ps, strconv.Itoa(p))
	}
	script := fmt.Sprintf(`for p in %s; do (echo > /dev/tcp/127.0.0.1/$p) >/dev/null 2>&1 && `+
		`{ printf '%%s\t' $p; curl -s -m 4 http://127.0.0.1:$p/v1/models | tr -d '\n'; printf '\t'; curl -s -m 3 http://127.0.0.1:$p/fleet/hello | tr -d '\n'; echo; }; done`, strings.Join(ps, " "))
	cx, cancel = context.WithTimeout(ctx, 60*time.Second)
	defer cancel()
	probe := exec.CommandContext(cx, "ssh", append(opts, "--", host, "bash", "-s")...) // /dev/tcp needs bash, whatever the login shell is
	probe.Stdin = strings.NewReader(script)
	out, err = probe.Output()
	if err != nil && len(out) == 0 {
		return nil, fmt.Sprintf("! %s: scan failed (%v)", host, err)
	}
	var fs []Found
	defer func() {
		for i := range fs { // second round trip: Ollama capabilities (best effort)
			if fs[i].Fleet != nil {
				continue
			}
			fs[i].Caps = sshCaps(ctx, host, fs[i].Port, fs[i].Models)
		}
	}()
	for _, line := range strings.Split(string(out), "\n") {
		p, rest, ok := strings.Cut(line, "\t")
		port, e := strconv.Atoi(strings.TrimSpace(p))
		if !ok || e != nil {
			continue
		}
		js, hello, _ := strings.Cut(rest, "\t")
		if fi := parseHello([]byte(hello)); fi != nil {
			fs = append(fs, Found{Node: host, SSH: host, Port: port, Fleet: fi})
			continue
		}
		if ms := parseModels([]byte(js)); len(ms) > 0 {
			fs = append(fs, Found{Node: host, SSH: host, Port: port, Models: ms})
		}
	}
	return fs, ""
}

func firstLine(s string) string {
	s = strings.TrimSpace(s)
	if i := strings.IndexByte(s, '\n'); i >= 0 {
		s = s[:i]
	}
	if len(s) > 90 {
		s = s[:90]
	}
	return s
}

// Plan compares findings with the current configuration and produces the endpoints to add.
// includeEmbed=false → embedding/reranker models are skipped.
type Plan struct {
	Add   []config.Endpoint `json:"add"`
	Peers []config.Peer     `json:"peers,omitempty"` // other fleets found; registering them imports their models
	Known []string          `json:"known"`
	Skip  []string          `json:"skip"`
}

func MakePlan(cfg *config.Config, found []Found, includeEmbed bool) Plan {
	var pl Plan
	for _, f := range found {
		if f.Fleet != nil {
			planPeer(cfg, f, &pl)
			continue
		}
		remote := fmt.Sprintf("127.0.0.1:%d", f.Port)
		for _, m := range f.Models {
			label := fmt.Sprintf("%s %s %s", f.Node, remote, m)
			if !includeEmbed && (config.IsEmbedModel(m) || (len(f.Caps[m]) > 0 && !contains(f.Caps[m], "chat"))) {
				pl.Skip = append(pl.Skip, label+" (embedding/reranker)")
				continue
			}
			dup := false
			for _, e := range cfg.Endpoints {
				sameHost := e.Node == f.Node
				samePort := urlHostPort(e.URL) == remote || e.Remote == remote
				if e.Model == m && sameHost && samePort {
					dup = true
					break
				}
			}
			if dup {
				pl.Known = append(pl.Known, label)
				continue
			}
			ep := config.Endpoint{Name: f.Node + "-" + config.Slug(m), Node: f.Node, Model: m}
			if f.SSH == "" {
				ep.URL = f.URL()
			} else {
				ep.SSH, ep.Remote = f.SSH, remote
			}
			if c := f.Caps[m]; len(c) > 0 {
				ep.Caps = c // authoritative: reported by the server itself
			} else if config.IsEmbedModel(m) {
				ep.Kind = "embed"
			}
			for n := 2; cfg.Endpoint(ep.Name) != nil || planHas(pl.Add, ep.Name); n++ { // name collision
				ep.Name = fmt.Sprintf("%s-%s-%d", f.Node, config.Slug(m), n)
			}
			pl.Add = append(pl.Add, ep)
		}
	}
	return pl
}

func planHas(es []config.Endpoint, name string) bool {
	for _, e := range es {
		if e.Name == name {
			return true
		}
	}
	return false
}

var safeName = regexp.MustCompile(`^[A-Za-z0-9._:/-]+$`)

// mapCaps converts Ollama capability names to fleet's.
func mapCaps(raw []string) []string {
	m := map[string]string{"completion": "chat", "vision": "vision", "tools": "tools", "thinking": "thinking", "embedding": "embed", "audio": "audio"}
	have := map[string]bool{}
	for _, r := range raw {
		if v, ok := m[r]; ok {
			have[v] = true
		}
	}
	var out []string
	for _, c := range []string{"chat", "vision", "audio", "tools", "thinking", "embed"} { // stable order
		if have[c] {
			out = append(out, c)
		}
	}
	return out
}

func parseCaps(b []byte) []string {
	var d struct {
		Capabilities []string `json:"capabilities"`
	}
	if json.Unmarshal(b, &d) != nil {
		return nil
	}
	return mapCaps(d.Capabilities)
}

func localCaps(ctx context.Context, port int, models []string) map[string][]string {
	out := map[string][]string{}
	for _, m := range models {
		if !safeName.MatchString(m) {
			continue
		}
		cx, cancel := context.WithTimeout(ctx, 5*time.Second)
		body := strings.NewReader(fmt.Sprintf(`{"model":%q}`, m))
		req, _ := http.NewRequestWithContext(cx, "POST", fmt.Sprintf("http://127.0.0.1:%d/api/show", port), body)
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			cancel()
			return nil // not Ollama (or unreachable) — give up on the rest
		}
		b, _ := io.ReadAll(io.LimitReader(resp.Body, 4<<20))
		resp.Body.Close()
		cancel()
		if resp.StatusCode != 200 {
			return nil
		}
		if c := parseCaps(b); len(c) > 0 {
			out[m] = c
		}
	}
	return out
}

// sshCaps asks Ollama's /api/show for every model in a single SSH round trip.
func sshCaps(ctx context.Context, host string, port int, models []string) map[string][]string {
	var sb strings.Builder
	for _, m := range models {
		if safeName.MatchString(m) {
			fmt.Fprintf(&sb, "printf '%%s\\t' '%s'; curl -s -m 6 -d '{\"model\":\"%s\"}' http://127.0.0.1:%d/api/show | tr -d '\\n' | grep -o '\"capabilities\":\\[[^]]*\\]'; echo; ", m, m, port)
		}
	}
	cx, cancel := context.WithTimeout(ctx, 90*time.Second)
	defer cancel()
	out, _ := exec.CommandContext(cx, "ssh", "-o", "BatchMode=yes", "-o", "ConnectTimeout=6", "--", host, sb.String()).Output()
	res := map[string][]string{}
	for _, line := range strings.Split(string(out), "\n") {
		name, js, ok := strings.Cut(line, "\t")
		if !ok || js == "" {
			continue
		}
		if c := parseCaps([]byte("{" + js + "}")); len(c) > 0 {
			res[name] = c
		}
	}
	return res
}

func contains(l []string, x string) bool {
	for _, v := range l {
		if v == x {
			return true
		}
	}
	return false
}

// FormatPorts is the inverse of ParsePorts: sorted, de-duplicated, runs of 3+ collapsed to "a-b".
func FormatPorts(ports []int) string {
	set := map[int]bool{}
	for _, p := range ports {
		set[p] = true
	}
	var l []int
	for p := range set {
		l = append(l, p)
	}
	sortInts(l)
	var parts []string
	for i := 0; i < len(l); {
		j := i
		for j+1 < len(l) && l[j+1] == l[j]+1 {
			j++
		}
		if j-i >= 2 {
			parts = append(parts, fmt.Sprintf("%d-%d", l[i], l[j]))
		} else {
			for k := i; k <= j; k++ {
				parts = append(parts, strconv.Itoa(l[k]))
			}
		}
		i = j + 1
	}
	return strings.Join(parts, ",")
}

// PortPreset is a well-known inference-server port (or range) offered as a checkbox in the menu.
type PortPreset struct {
	Spec string // "11434" or "30000-30020"
	Name string
}

var PortPresets = []PortPreset{
	{"11434", "Ollama"},
	{"8080", "llama.cpp server / Strata"},
	{"1234", "LM Studio"},
	{"8000", "vLLM"},
	{"4000", "LiteLLM gateway"},
	{"30000-30020", "SGLang / llama.cpp range"},
}

// urlHostPort returns "host:port" of a URL ("" when it cannot be parsed); exact, unlike a substring match
// ("127.0.0.1:3000" is a substring of "127.0.0.1:30000").
func urlHostPort(u string) string {
	p, err := url.Parse(u)
	if err != nil {
		return ""
	}
	return p.Host
}

// planPeer turns a found fleet into a peer to register (or notes why not).
func planPeer(cfg *config.Config, f Found, pl *Plan) {
	remote := fmt.Sprintf("127.0.0.1:%d", f.Port)
	label := fmt.Sprintf("fleet %q on %s %s", f.Fleet.Name, f.Node, remote)
	if f.Fleet.ID != "" && f.Fleet.ID == cfg.ID {
		pl.Skip = append(pl.Skip, label+" (this fleet itself)")
		return
	}
	p := config.Peer{Name: config.Slug(f.Node)}
	if f.SSH == "" {
		p.URL = f.URL()
	} else {
		p.SSH, p.Remote = f.SSH, remote
	}
	for _, x := range cfg.Peers {
		if (x.SSH != "" && x.SSH == p.SSH && (x.Remote == p.Remote || (x.Remote == "" && f.Port == FleetPort))) || (x.URL != "" && x.URL == p.URL) {
			pl.Known = append(pl.Known, label+" (already a peer)")
			return
		}
	}
	if p.SSH != "" && f.Port == FleetPort {
		p.Remote = "" // the default fleet address
	}
	for n, base := 2, p.Name; planHasPeer(cfg, pl, p.Name); n++ {
		p.Name = fmt.Sprintf("%s-%d", base, n)
	}
	pl.Peers = append(pl.Peers, p)
}

func planHasPeer(cfg *config.Config, pl *Plan, name string) bool {
	for _, x := range cfg.Peers {
		if x.Name == name {
			return true
		}
	}
	for _, x := range pl.Peers {
		if x.Name == name {
			return true
		}
	}
	return false
}

func withPort(ports []int, p int) []int {
	for _, x := range ports {
		if x == p {
			return ports
		}
	}
	return append(append([]int{}, ports...), p)
}
