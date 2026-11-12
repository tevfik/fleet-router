// Package profile keeps deploy profiles: a named, saved `fleet deploy` configuration (one YAML file per profile in
// the fleet config directory) and what its runs measured. A profile is data the user manages (save, edit, export,
// import, rm); fleet has no built-in knowledge of any model or machine.
package profile

import (
	"bytes"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"

	"gopkg.in/yaml.v3"

	"github.com/tevfik/fleet-router/internal/config"
	"github.com/tevfik/fleet-router/internal/plan"
)

// Layout modes.
const (
	LayoutAuto  = "auto"  // the planner picks the split (and uses the measurements of earlier runs)
	LayoutFixed = "fixed" // the split is what the profile says (ts / worker_cpu_experts, or the generic fixed split)
)

// Profile is everything `fleet deploy` takes, plus notes and the recorded runs.
type Profile struct {
	Name  string `yaml:"name" json:"name"`
	Notes string `yaml:"notes,omitempty" json:"notes,omitempty"`

	Model        string            `yaml:"model" json:"model"`                                     // the GGUF on the master
	WorkerModels map[string]string `yaml:"worker_models,omitempty" json:"worker_models,omitempty"` // node -> path of the model on that worker (local_weights)
	Master       string            `yaml:"master,omitempty" json:"master,omitempty"`               // "": the machine fleet deploy runs on
	Workers      []string          `yaml:"workers,omitempty" json:"workers,omitempty"`             // fixed: exactly these; auto: the machines the planner may use (empty: every machine of the fleet)
	Role         string            `yaml:"role,omitempty" json:"role,omitempty"`
	Strategy     string            `yaml:"strategy,omitempty" json:"strategy,omitempty"`
	Engine       string            `yaml:"engine,omitempty" json:"engine,omitempty"`

	Port    int  `yaml:"port,omitempty" json:"port,omitempty"`
	Ctx     int  `yaml:"ctx,omitempty" json:"ctx,omitempty"`
	Np      int  `yaml:"np,omitempty" json:"np,omitempty"`
	NGL     int  `yaml:"ngl,omitempty" json:"ngl,omitempty"`
	CPUOnly bool `yaml:"cpu_only,omitempty" json:"cpu_only,omitempty"`

	Layout Layout `yaml:"layout" json:"layout"`
	Spec   Spec   `yaml:"spec,omitempty" json:"spec,omitempty"`
	Cache  Cache  `yaml:"cache,omitempty" json:"cache,omitempty"`
	MoE    MoE    `yaml:"moe,omitempty" json:"moe,omitempty"`
	MMProj MMProj `yaml:"mmproj,omitempty" json:"mmproj,omitempty"`

	Wire               string `yaml:"wire,omitempty" json:"wire,omitempty"`
	MMap               *bool  `yaml:"mmap,omitempty" json:"mmap,omitempty"`
	IOTimeout          int    `yaml:"io_timeout,omitempty" json:"io_timeout,omitempty"`
	WorkerReadyTimeout int    `yaml:"worker_ready_timeout,omitempty" json:"worker_ready_timeout,omitempty"`
	ExitOnFail         bool   `yaml:"exit_on_fail,omitempty" json:"exit_on_fail,omitempty"`
	LocalWeights       bool   `yaml:"local_weights,omitempty" json:"local_weights,omitempty"`
	ShardsManifest     string `yaml:"shards_manifest,omitempty" json:"shards_manifest,omitempty"`
	Chain              bool   `yaml:"chain,omitempty" json:"chain,omitempty"`
	Force              bool   `yaml:"force,omitempty" json:"force,omitempty"`

	Env             Env      `yaml:"env,omitempty" json:"env,omitempty"`
	ExtraMasterArgs []string `yaml:"extra_master_args,omitempty" json:"extra_master_args,omitempty"`

	// Runs are what deploying this profile did and measured (newest last): data, fed back into the next auto plan.
	Runs []Run `yaml:"runs,omitempty" json:"runs,omitempty"`
}

// Layout is how the blocks are placed and how the workers are reached.
type Layout struct {
	Mode             string            `yaml:"mode,omitempty" json:"mode,omitempty"`                             // auto (default) | fixed
	TS               string            `yaml:"ts,omitempty" json:"ts,omitempty"`                                 // -ts of the GPU devices (fixed)
	WorkerCPUExperts map[string]string `yaml:"worker_cpu_experts,omitempty" json:"worker_cpu_experts,omitempty"` // node -> "FIRST-LAST" (fixed)
	ViaHub           bool              `yaml:"via_hub,omitempty" json:"via_hub,omitempty"`
	ViaHubNodes      []string          `yaml:"via_hub_nodes,omitempty" json:"via_hub_nodes,omitempty"`
	NoHub            bool              `yaml:"no_hub,omitempty" json:"no_hub,omitempty"`
	Hub              string            `yaml:"hub,omitempty" json:"hub,omitempty"`
	HubListenBase    int               `yaml:"hub_listen_base,omitempty" json:"hub_listen_base,omitempty"`
	WorkerPort       int               `yaml:"worker_port,omitempty" json:"worker_port,omitempty"`
	// HubRTTms is the round trip the auto planner assumes for a worker reached through the hub (0: config hub_rtt_ms,
	// else plan.DefaultHubRTTms, an estimate). Measurements of earlier runs correct it through the calibration.
	HubRTTms float64 `yaml:"hub_rtt_ms,omitempty" json:"hub_rtt_ms,omitempty"`
}

// Spec is speculative decoding.
type Spec struct {
	Type       string `yaml:"type,omitempty" json:"type,omitempty"` // auto | none | draft-dflash | draft-mtp | ngram-mod | ...
	DraftModel string `yaml:"draft_model,omitempty" json:"draft_model,omitempty"`
	DraftMax   int    `yaml:"draft_max,omitempty" json:"draft_max,omitempty"`
}

// Cache is the KV cache types.
type Cache struct {
	K string `yaml:"k,omitempty" json:"k,omitempty"`
	V string `yaml:"v,omitempty" json:"v,omitempty"`
}

// MoE is the master's host-RAM expert options.
type MoE struct {
	CPU      bool `yaml:"cpu,omitempty" json:"cpu,omitempty"`
	NCPU     int  `yaml:"n_cpu,omitempty" json:"n_cpu,omitempty"`
	CacheMiB int  `yaml:"cache_mib,omitempty" json:"cache_mib,omitempty"`
}

// MMProj is the multimodal projector.
type MMProj struct {
	Path   string `yaml:"path,omitempty" json:"path,omitempty"`
	CPU    bool   `yaml:"cpu,omitempty" json:"cpu,omitempty"`
	Device string `yaml:"device,omitempty" json:"device,omitempty"`
}

// Env is extra environment: for the master, and per worker node (validated by deploy.ValidateExtras).
type Env struct {
	Master  map[string]string            `yaml:"master,omitempty" json:"master,omitempty"`
	Workers map[string]map[string]string `yaml:"workers,omitempty" json:"workers,omitempty"`
}

// Run is one deploy of the profile: the concrete layout it ran with and what was measured.
type Run struct {
	At            time.Time    `yaml:"at" json:"at"`
	RunID         string       `yaml:"run_id,omitempty" json:"run_id,omitempty"`
	Endpoint      string       `yaml:"endpoint,omitempty" json:"endpoint,omitempty"`
	URL           string       `yaml:"url,omitempty" json:"url,omitempty"`
	StatsPort     int          `yaml:"stats_port,omitempty" json:"stats_port,omitempty"`
	Layout        RunLayout    `yaml:"layout" json:"layout"`
	PlannedTPS    float64      `yaml:"planned_tps,omitempty" json:"planned_tps,omitempty"` // the planner's baseline estimate (no speculation)
	Choice        string       `yaml:"choice,omitempty" json:"choice,omitempty"`           // why this layout (auto: measured / planned / explore; fixed)
	FleetVersion  string       `yaml:"fleet_version,omitempty" json:"fleet_version,omitempty"`
	EngineVersion string       `yaml:"engine_version,omitempty" json:"engine_version,omitempty"`
	Measured      *Measurement `yaml:"measured,omitempty" json:"measured,omitempty"`
}

// RunLayout is the concrete layout of a run: enough to deploy it again exactly.
type RunLayout struct {
	Master      string            `yaml:"master" json:"master"`
	Workers     []string          `yaml:"workers,omitempty" json:"workers,omitempty"` // RING_SERVERS order
	Devices     string            `yaml:"devices,omitempty" json:"devices,omitempty"`
	TS          string            `yaml:"ts,omitempty" json:"ts,omitempty"`
	CPUExperts  map[string]string `yaml:"cpu_experts,omitempty" json:"cpu_experts,omitempty"` // node -> FIRST-LAST
	ViaHubNodes []string          `yaml:"via_hub_nodes,omitempty" json:"via_hub_nodes,omitempty"`
	SpecType    string            `yaml:"spec_type,omitempty" json:"spec_type,omitempty"` // as asked (auto when not given)
	DraftMax    int               `yaml:"draft_max,omitempty" json:"draft_max,omitempty"`
	// FromPlan: the layout was a plan candidate (parts in the planner's order, CPU experts on a part's last blocks);
	// it is redeployed as that candidate. Otherwise as --ts / --worker-cpu-experts.
	FromPlan       bool `yaml:"from_plan,omitempty" json:"from_plan,omitempty"`
	OutputOnMaster bool `yaml:"output_on_master,omitempty" json:"output_on_master,omitempty"`
}

// Candidate rebuilds the plan candidate of a FromPlan layout (master first, then the workers, blocks in that order).
func (l RunLayout) Candidate() (*plan.Candidate, error) {
	ts := strings.Split(l.TS, ",")
	if len(ts) != len(l.Workers)+1 {
		return nil, fmt.Errorf("recorded layout: -ts %q does not match %d machines", l.TS, len(l.Workers)+1)
	}
	c := &plan.Candidate{Kind: "split", Master: l.Master}
	next := 0
	for i, n := range append([]string{l.Master}, l.Workers...) {
		v, err := strconv.Atoi(strings.TrimSpace(ts[i]))
		if err != nil || v < 0 {
			return nil, fmt.Errorf("recorded layout: -ts %q", l.TS)
		}
		pt := plan.Part{Node: n, Layers: v, First: next}
		if r, ok := l.CPUExperts[n]; ok {
			f, e, err := ParseRange(r)
			if err != nil {
				return nil, err
			}
			if e != next+v-1 || f < next {
				return nil, fmt.Errorf("recorded layout: the CPU experts of %s (%s) are not its last blocks", n, r)
			}
			pt.CPUExperts = e - f + 1
		}
		next += v
		c.Parts = append(c.Parts, pt)
	}
	if l.OutputOnMaster {
		c.Parts[0].Output = true
	} else {
		c.Parts[len(c.Parts)-1].Output = true
	}
	return c, nil
}

// Key identifies a layout for comparing runs and plan candidates (the order of the hub nodes does not matter).
func (l RunLayout) Key() string {
	cpx := make([]string, 0, len(l.CPUExperts))
	for n, r := range l.CPUExperts {
		cpx = append(cpx, strings.ToLower(n)+"="+r)
	}
	sort.Strings(cpx)
	hub := append([]string(nil), l.ViaHubNodes...)
	for i := range hub {
		hub[i] = strings.ToLower(hub[i])
	}
	sort.Strings(hub)
	ws := append([]string(nil), l.Workers...)
	for i := range ws {
		ws[i] = strings.ToLower(ws[i])
	}
	return strings.Join([]string{strings.ToLower(l.Master), strings.Join(ws, ","), l.TS, strings.Join(cpx, ","), strings.Join(hub, ","), l.SpecType, strconv.Itoa(l.DraftMax)}, "|")
}

// Measurement is what a run measured.
type Measurement struct {
	At        time.Time `yaml:"at" json:"at"`
	TPS       float64   `yaml:"tps" json:"tps"`                                   // median over the prompts, with the run's speculation setting
	PerPrompt []float64 `yaml:"per_prompt,omitempty" json:"per_prompt,omitempty"` // median t/s of each prompt
	// Trace is the engine's own decode profile (GET /ring/trace on the master's stats port) when it had one: it
	// calibrates the next plan (plan.Calibrate).
	Trace  *plan.Measured `yaml:"trace,omitempty" json:"trace,omitempty"`
	Source string         `yaml:"source,omitempty" json:"source,omitempty"` // bench | manual
	Notes  string         `yaml:"notes,omitempty" json:"notes,omitempty"`
}

// ---- storage ----

var nameRe = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]{0,63}$`)

// ValidName checks a profile name: letters, digits and . _ - (64 at most, not starting with a dot or dash).
func ValidName(name string) error {
	if !nameRe.MatchString(name) || strings.HasSuffix(strings.ToLower(name), ".yaml") || strings.Contains(name, "..") {
		return fmt.Errorf("profile name %q: use letters, digits and . _ - (up to 64, starting with a letter or digit, no .yaml)", name)
	}
	return nil
}

// Dir is where the profiles are kept: $FLEET_PROFILES, else profiles/ next to the configuration
// (~/.config/fleet/profiles, or next to $FLEET_CONFIG).
func Dir() string {
	if d := os.Getenv("FLEET_PROFILES"); d != "" {
		return d
	}
	return filepath.Join(filepath.Dir(config.Path()), "profiles")
}

// Store is a directory of profiles.
type Store struct{ Dir string }

// Default is the store in Dir().
func Default() Store { return Store{Dir: Dir()} }

func (s Store) path(name string) string { return filepath.Join(s.Dir, name+".yaml") }

// Load reads a profile by name.
func (s Store) Load(name string) (*Profile, error) {
	if err := ValidName(name); err != nil {
		return nil, err
	}
	b, err := os.ReadFile(s.path(name))
	if os.IsNotExist(err) {
		return nil, fmt.Errorf("no profile %q (fleet profile list)", name)
	}
	if err != nil {
		return nil, err
	}
	p, err := Parse(b)
	if err != nil {
		return nil, fmt.Errorf("profile %s: %w", name, err)
	}
	p.Name = name
	return p, nil
}

// Parse reads a profile from YAML (unknown keys are refused: a typo must not be silently ignored).
func Parse(b []byte) (*Profile, error) {
	var p Profile
	dec := yaml.NewDecoder(bytes.NewReader(b))
	dec.KnownFields(true)
	if err := dec.Decode(&p); err != nil {
		return nil, err
	}
	return &p, p.Validate()
}

// Marshal is the profile as YAML.
func (p *Profile) Marshal() ([]byte, error) { return yaml.Marshal(p) }

// Validate checks what can be checked without a machine.
func (p *Profile) Validate() error {
	if p.Name != "" {
		if err := ValidName(p.Name); err != nil {
			return err
		}
	}
	if strings.TrimSpace(p.Model) == "" {
		return fmt.Errorf("a profile needs a model path")
	}
	switch p.Layout.Mode {
	case "", LayoutAuto, LayoutFixed:
	default:
		return fmt.Errorf("layout.mode %q: auto or fixed", p.Layout.Mode)
	}
	switch p.Engine {
	case "", "ring", "chain":
	default:
		return fmt.Errorf("engine %q: must be ring or chain", p.Engine)
	}
	for n, r := range p.Layout.WorkerCPUExperts {
		if _, _, err := ParseRange(r); err != nil {
			return fmt.Errorf("layout.worker_cpu_experts.%s: %w", n, err)
		}
	}
	if p.Layout.HubRTTms < 0 {
		return fmt.Errorf("layout.hub_rtt_ms must not be negative")
	}
	return nil
}

// ParseRange reads "FIRST-LAST".
func ParseRange(s string) (int, int, error) {
	a, b, ok := strings.Cut(strings.TrimSpace(s), "-")
	f, e1 := strconv.Atoi(strings.TrimSpace(a))
	l, e2 := strconv.Atoi(strings.TrimSpace(b))
	if !ok || e1 != nil || e2 != nil || f < 0 || l < f {
		return 0, 0, fmt.Errorf("%q is not FIRST-LAST", s)
	}
	return f, l, nil
}

// EffectiveMode is the layout mode: fixed when the profile pins the split (ts / worker_cpu_experts) without saying
// otherwise, else what it says, else auto.
func (p *Profile) EffectiveMode() string {
	if p.Layout.Mode != "" {
		return p.Layout.Mode
	}
	if p.Layout.TS != "" || len(p.Layout.WorkerCPUExperts) > 0 {
		return LayoutFixed
	}
	return LayoutAuto
}

// Save writes the profile atomically (a temporary file in the same directory, synced, renamed over the old one).
func (s Store) Save(p *Profile) error {
	if err := ValidName(p.Name); err != nil {
		return err
	}
	if err := p.Validate(); err != nil {
		return err
	}
	b, err := p.Marshal()
	if err != nil {
		return err
	}
	if err := os.MkdirAll(s.Dir, 0o700); err != nil {
		return err
	}
	return atomicWrite(s.path(p.Name), b)
}

func atomicWrite(path string, b []byte) error {
	f, err := os.CreateTemp(filepath.Dir(path), "."+filepath.Base(path)+".tmp-*")
	if err != nil {
		return err
	}
	tmp := f.Name()
	ok := false
	defer func() {
		if !ok {
			os.Remove(tmp)
		}
	}()
	if _, err := f.Write(b); err != nil {
		f.Close()
		return err
	}
	if err := f.Chmod(0o600); err != nil {
		f.Close()
		return err
	}
	if err := f.Sync(); err != nil {
		f.Close()
		return err
	}
	if err := f.Close(); err != nil {
		return err
	}
	if err := os.Rename(tmp, path); err != nil {
		return err
	}
	ok = true
	return nil
}

// Remove deletes a profile.
func (s Store) Remove(name string) error {
	if err := ValidName(name); err != nil {
		return err
	}
	err := os.Remove(s.path(name))
	if os.IsNotExist(err) {
		return fmt.Errorf("no profile %q", name)
	}
	return err
}

// List returns the profiles, sorted by name (a file that does not parse is reported in bad, not dropped silently).
func (s Store) List() (out []*Profile, bad map[string]error, err error) {
	ents, err := os.ReadDir(s.Dir)
	if os.IsNotExist(err) {
		return nil, nil, nil
	}
	if err != nil {
		return nil, nil, err
	}
	for _, e := range ents {
		name, ok := strings.CutSuffix(e.Name(), ".yaml")
		if !ok || e.IsDir() || ValidName(name) != nil {
			continue
		}
		p, err := s.Load(name)
		if err != nil {
			if bad == nil {
				bad = map[string]error{}
			}
			bad[name] = err
			continue
		}
		out = append(out, p)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out, bad, nil
}

// Import reads a profile file and saves it under name (empty: the name inside the file). An existing profile is
// only replaced with force.
func (s Store) Import(file, name string, force bool) (*Profile, error) {
	b, err := os.ReadFile(file)
	if err != nil {
		return nil, err
	}
	p, err := Parse(b)
	if err != nil {
		return nil, fmt.Errorf("%s: %w", file, err)
	}
	if name != "" {
		p.Name = name
	}
	if p.Name == "" {
		return nil, fmt.Errorf("%s has no name: give --name", file)
	}
	if _, err := os.Stat(s.path(p.Name)); err == nil && !force {
		return nil, fmt.Errorf("profile %q exists (--force replaces it)", p.Name)
	}
	return p, s.Save(p)
}

// LastMeasured is the newest run with a measurement (nil: none).
func (p *Profile) LastMeasured() *Run {
	for i := len(p.Runs) - 1; i >= 0; i-- {
		if p.Runs[i].Measured != nil {
			return &p.Runs[i]
		}
	}
	return nil
}

// MeasuredLine is "last measured on <date>: X t/s (layout ...)" for the deploy output ("" when nothing was measured).
func (p *Profile) MeasuredLine() string {
	r := p.LastMeasured()
	if r == nil {
		return ""
	}
	s := fmt.Sprintf("profile %s: last measured on %s: %.1f t/s (master %s", p.Name, r.Measured.At.Format("2006-01-02"), r.Measured.TPS, r.Layout.Master)
	if len(r.Layout.Workers) > 0 {
		s += " + " + strings.Join(r.Layout.Workers, ",")
	}
	if r.Layout.TS != "" {
		s += ", -ts " + r.Layout.TS
	}
	if r.Layout.SpecType != "" && r.Layout.SpecType != "none" {
		s += ", " + r.Layout.SpecType
	}
	return s + ")"
}

// AddRun appends a run, keeping at most maxRuns (the oldest unmeasured ones go first).
func (p *Profile) AddRun(r Run) {
	p.Runs = append(p.Runs, r)
	for len(p.Runs) > maxRuns {
		drop := 0
		for i, x := range p.Runs {
			if x.Measured == nil {
				drop = i
				break
			}
		}
		p.Runs = append(p.Runs[:drop], p.Runs[drop+1:]...)
	}
}

const maxRuns = 50
