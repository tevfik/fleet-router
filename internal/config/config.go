// Package config holds the fleet configuration (a single YAML file) and secret resolution.
package config

import (
	"bufio"
	"bytes"
	cryptorand "crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"path"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"sync"
	"time"

	"gopkg.in/yaml.v3"
	"net"
	"strconv"
)

const DefaultListen = "127.0.0.1:4711"

// Endpoint is a model service. Reachable in one of two ways:
//   - URL: a base reachable directly from this machine (…/v1)
//   - SSH+Remote: host:port on a remote machine; the daemon opens an SSH tunnel
type Endpoint struct {
	Name   string `yaml:"name" json:"name"`
	Node   string `yaml:"node" json:"node"` // machine name (hostname / ssh alias)
	URL    string `yaml:"url,omitempty" json:"url,omitempty"`
	SSH    string `yaml:"ssh,omitempty" json:"ssh,omitempty"`
	Remote string `yaml:"remote,omitempty" json:"remote,omitempty"`
	Model  string `yaml:"model" json:"model"`
	// ModelSHA256: the content hash of the model file the endpoint serves, when fleet knew it at deploy time (an
	// Ollama blob's name, or the store's record of the file); never computed for the purpose. `fleet endpoint check`
	// compares it between endpoints that serve the same model name.
	ModelSHA256 string  `yaml:"model_sha256,omitempty" json:"model_sha256,omitempty"`
	KeyEnv      string  `yaml:"key_env,omitempty" json:"key_env,omitempty"`
	Key         string  `yaml:"key,omitempty" json:"-"`
	Kind        string  `yaml:"kind,omitempty" json:"kind,omitempty"`   // chat (default) | embed
	Probe       string  `yaml:"probe,omitempty" json:"probe,omitempty"` // models (default) | chat
	Ctx         int     `yaml:"ctx,omitempty" json:"ctx,omitempty"`
	TPS         float64 `yaml:"tps,omitempty" json:"tps,omitempty"`
	// Caps lists what the model can do: chat, vision, audio, image_gen, embed, rerank, tools, thinking.
	// Empty = inferred (embed by name patterns, vision by obvious names such as "-vl"/"llava"); discovery fills
	// it from Ollama's own capability list when available.
	Caps []string `yaml:"caps,omitempty" json:"caps,omitempty"`
	// Dim is the embedding vector size (learned automatically for embed endpoints). Roles refuse to mix sizes.
	Dim int `yaml:"dim,omitempty" json:"dim,omitempty"`
	// Slots: concurrent requests this endpoint handles (0 = unlimited). Match it to the server's real
	// parallelism (llama.cpp --parallel, Ollama NUM_PARALLEL); excess requests queue.
	Slots int `yaml:"slots,omitempty" json:"slots,omitempty"`
	// Peer names the peer fleet this endpoint was imported from (managed by `fleet peers`, refreshed
	// automatically, never edited by hand). Requests go to that fleet, which routes them to the real
	// model — so the model is named by its "node/model" there (see Wire).
	Peer string `yaml:"peer,omitempty" json:"peer,omitempty"`
	// Path lists the fleet ids between us and the machine that owns the model, owner side first, ending
	// with the peer we talk to (so len(Path) is the number of fleets crossed). Used to prevent loops.
	Path []string `yaml:"path,omitempty" json:"path,omitempty"`
	// Tags are free labels ("fast", "gpu") that roles can select members by (see Role.Match).
	Tags []string       `yaml:"tags,omitempty" json:"tags,omitempty"`
	Body map[string]any `yaml:"body,omitempty" json:"body,omitempty"` // default fields merged into requests (think:false …)
	// FleetPort is the port of the fleet daemon on this endpoint's machine, where it is not 4711 (the machine's
	// readings, model files and plans are asked there). 0: 4711.
	FleetPort int `yaml:"fleet_port,omitempty" json:"fleet_port,omitempty"`
	// Group marks a model spread over several machines (llama-ring, prima.cpp, llama.cpp RPC): the URL is the
	// master's server, the workers are other machines. Fleet does not start anything (attached mode): it
	// watches the group, and the endpoint is down when a worker's machine is unreachable or the group stalls.
	Group *Group `yaml:"group,omitempty" json:"group,omitempty"`
	// Stall: a streaming request whose upstream says nothing for this long is cut and the endpoint is set aside
	// for a while (a stalled worker link blocks the master for ever). Default 90s for a group, off otherwise;
	// the first token gets three times as long (the prompt has to be processed first). "off" disables it.
	Stall string `yaml:"stall,omitempty" json:"stall,omitempty"`
}

// Group describes a model that runs on several machines.
type Group struct {
	Engine  string   `yaml:"engine" json:"engine"`                       // llama-ring | prima | llamacpp-rpc | …
	Workers []string `yaml:"workers,omitempty" json:"workers,omitempty"` // the machines that hold the rest of the blocks
	// StatsPort is the port the engine's live statistics answer on (RING_STATS_PORT of `fleet ring serve`, bound to
	// 127.0.0.1 of the master's machine): fleet reads GET /ring/trace there, so it works for a master on this machine.
	StatsPort int `yaml:"stats_port,omitempty" json:"stats_port,omitempty"`
	// Addrs says which ring address ("host:port", as in RING_SERVERS) belongs to which worker machine; a group with
	// one worker needs none.
	Addrs map[string]string `yaml:"addrs,omitempty" json:"addrs,omitempty"`
	// Streams are the workers the master reaches through the hub (fleet deploy --via-hub, or a worker without a direct
	// address): worker -> the listen address of its `fleet stream connect` on the master ("127.0.0.1:51000"), which is
	// its RING_SERVERS entry. Addrs keeps the worker's own address and port. Ending the run stops these too.
	Streams map[string]string `yaml:"streams,omitempty" json:"streams,omitempty"`
	Hub     string            `yaml:"hub,omitempty" json:"hub,omitempty"` // the hub the streams go through
	Relays  []Relay           `yaml:"relays,omitempty" json:"relays,omitempty"`
	// Stages (chain) lists every stage after the first in order, as Addrs cannot: two stages on one machine share
	// its key there, and the second was never stopped (it kept its GPU and RAM).
	Stages []StageAddr `yaml:"stages,omitempty" json:"stages,omitempty"`
}

// StageAddr is one stage of a chain: its machine and the address it serves on ("host:port").
type StageAddr struct {
	Node string `yaml:"node" json:"node"`
	Addr string `yaml:"addr" json:"addr"`
}

// FleetRemote is the address of the machine's fleet as the SSH host sees it ("127.0.0.1:4711" by default).
func (e *Endpoint) FleetRemote() string {
	if e.FleetPort > 0 {
		return net.JoinHostPort("127.0.0.1", strconv.Itoa(e.FleetPort))
	}
	return DefaultListen
}

// FleetHostPort is the port of the machine's fleet ("4711" by default).
func (e *Endpoint) FleetHostPort() string {
	if e.FleetPort > 0 {
		return strconv.Itoa(e.FleetPort)
	}
	return "4711"
}

// StatsRemote is where the engine's statistics listen as seen from the SSH host of the endpoint (the master's own
// 127.0.0.1), so the fleet can reach them through a tunnel of its own ("" when there is no group statistics port
// or the endpoint is not reached over SSH).
func (e *Endpoint) StatsRemote() string {
	if e.Group == nil || e.Group.StatsPort <= 0 || e.URL != "" || e.SSH == "" {
		return ""
	}
	host, _, err := net.SplitHostPort(e.Remote)
	if err != nil || host == "" {
		host = "127.0.0.1"
	}
	return net.JoinHostPort(host, strconv.Itoa(e.Group.StatsPort))
}

// DefaultGroupStall is how long a group's upstream may stay silent while a request streams.
const DefaultGroupStall = 90 * time.Second

// StallAfter is how long this endpoint's upstream may be silent during a streaming request (0: no limit).
func (e *Endpoint) StallAfter() time.Duration {
	switch v := strings.ToLower(strings.TrimSpace(e.Stall)); v {
	case "off", "never", "0":
		return 0
	case "":
		if e.Group != nil {
			return DefaultGroupStall
		}
		return 0
	default:
		d, _ := time.ParseDuration(v)
		return d
	}
}

// Paired is a fleet that paired with this one; only the hash of its key is kept.
type Paired struct {
	ID      string    `yaml:"id" json:"id"`
	Name    string    `yaml:"name" json:"name"`
	KeyHash string    `yaml:"key_sha256" json:"-"`
	At      time.Time `yaml:"at" json:"at"`
}

// Peer is another fleet this one can reach (directly by URL or through an SSH tunnel). Every model the
// peer can reach shows up here as an endpoint, named by its node/model; requests are forwarded and the
// speed and usage are measured locally, so network latency counts.
type Peer struct {
	Name   string `yaml:"name" json:"name"`
	URL    string `yaml:"url,omitempty" json:"url,omitempty"` // the peer's .../v1 base
	SSH    string `yaml:"ssh,omitempty" json:"ssh,omitempty"`
	Remote string `yaml:"remote,omitempty" json:"remote,omitempty"` // the peer's fleet address as seen from the SSH host (default 127.0.0.1:4711)
	KeyEnv string `yaml:"key_env,omitempty" json:"key_env,omitempty"`
	Key    string `yaml:"key,omitempty" json:"-"` // the key pairing gave (fleet peers add … pair=CODE)
	// Only / Skip limit what is imported, with the role selectors (node:, tag:, cap:, model:glob).
	Only []string `yaml:"only,omitempty" json:"only,omitempty"`
	Skip []string `yaml:"skip,omitempty" json:"skip,omitempty"`
	// MaxHops is how many fleets a model may be behind this peer (1 = only the peer's own models; default 2).
	MaxHops int `yaml:"max_hops,omitempty" json:"max_hops,omitempty"`
}

// Hops returns MaxHops with its default applied.
func (p Peer) Hops() int {
	if p.MaxHops <= 0 {
		return 2
	}
	return p.MaxHops
}

// Transport turns the peer into an endpoint-shaped value (URL / SSH+Remote) so tunnels and keys are reused.
func (p Peer) Transport() Endpoint {
	e := Endpoint{Name: "peer:" + p.Name, URL: p.URL, SSH: p.SSH, Remote: p.Remote, KeyEnv: p.KeyEnv, Key: p.Key}
	if e.URL == "" && e.SSH != "" && e.Remote == "" {
		e.Remote = DefaultListen
	}
	return e
}

type Role struct {
	Preferred string   `yaml:"preferred" json:"preferred"`
	Fallback  []string `yaml:"fallback,omitempty" json:"fallback,omitempty"`
	// Match adds every endpoint that fits a selector to the pool, after preferred + fallback:
	// "tag:fast", "node:dino", "cap:vision", "model:gemma4*" (glob). A role may have only Match.
	Match []string `yaml:"match,omitempty" json:"match,omitempty"`
	Use   string   `yaml:"use,omitempty" json:"use,omitempty"` // "what it is for" — goes into the skill/agent guidance
	// Priority: among roles queued for the same endpoint the higher one goes first (default 0).
	Priority int `yaml:"priority,omitempty" json:"priority,omitempty"`
	// MaxParallel: concurrent requests this role may have in flight (0 = unlimited).
	MaxParallel int `yaml:"max_parallel,omitempty" json:"max_parallel,omitempty"`
	// Strategy picks the endpoint within the role's pool (preferred + fallbacks):
	//   failover    (default) first healthy one in order; later ones only when it is down
	//   least-busy  the pool member with the lowest load (in use + waiting, relative to its slots)
	//   round-robin rotate through the healthy pool members
	//   fastest     the shortest typical reply from live speed, stretched by load
	//   cost        the shortest expected time for THIS request: queue + model load (cold) + prompt + reply, on the
	//               machine's current strain and busyness (see server/cost.go; `fleet route <role>` shows the terms)
	Strategy string `yaml:"strategy,omitempty" json:"strategy,omitempty"`
	// Overflow: when the preferred endpoint is full (no slot), spill to the next fallback instead of waiting.
	Overflow bool `yaml:"overflow,omitempty" json:"overflow,omitempty"`
	// PreferWarm puts models that are loaded right now ahead of ones the runtime would have to load first
	// (Ollama unloads idle models). Default: on for least-busy / round-robin / fastest, off for failover,
	// whose order is the one you chose.
	PreferWarm *bool `yaml:"prefer_warm,omitempty" json:"prefer_warm,omitempty"`
}

type Discover struct {
	Ports string   `yaml:"ports,omitempty" json:"ports"`
	SSH   []string `yaml:"ssh,omitempty" json:"ssh"`
	// Ignore: "machine:port" entries to skip while scanning (e.g. "papa:1919" — a tunnel to another machine)
	Ignore []string `yaml:"ignore,omitempty" json:"ignore,omitempty"`
}

type Config struct {
	Listen string `yaml:"listen,omitempty" json:"listen"`
	// RingCache is where llama-ring workers keep the weights they were sent (LLAMA_CACHE/rpc): the model store
	// lists what is in it. Default: $LLAMA_CACHE/rpc of the daemon, when set.
	RingCache string `yaml:"ring_cache,omitempty" json:"ring_cache,omitempty"`
	// PullMbps caps the speed of model-file transfers between fleets, in megabits per second (0: no cap), so a
	// transfer does not starve inference on the same link.
	PullMbps int `yaml:"pull_mbps,omitempty" json:"pull_mbps,omitempty"`
	// Drain: this machine is leaving the line (fleet leave): its models are used last, plans skip it, and the
	// groups it belongs to are told it is going. Only the machine itself can say so.
	Drain bool `yaml:"drain,omitempty" json:"drain,omitempty"`

	VisionURL  string `yaml:"vision_url,omitempty" json:"vision_url,omitempty"` // URL of decoupled vision microservice (e.g. http://127.0.0.1:18095)
	AdminToken string `yaml:"admin_token,omitempty" json:"-"`                   // required when listening on a non-loopback address
	// APIKeyEnv names the variable holding the key clients outside this machine must send as
	// "Authorization: Bearer <key>" on /v1/* (loopback clients need none).
	APIKeyEnv string `yaml:"api_key_env,omitempty" json:"api_key_env,omitempty"`
	// TunnelIdle: an SSH connection nobody used for this long is closed and opens again on the next request
	// ("10m" by default, "off" keeps connections open for good). Health checks do not count as use.
	TunnelIdle string `yaml:"tunnel_idle,omitempty" json:"tunnel_idle,omitempty"`
	// VRAMIdle: a model unused for this long has its GPU VRAM evicted to host RAM ("10m" by default, "off" to disable).
	VRAMIdle string `yaml:"vram_idle,omitempty" json:"vram_idle,omitempty"`
	// AutoUpdate (default true): the daemon checks the repository for a new release every few hours and
	// installs it (restarting itself when it runs under systemd). Set false to turn it off.
	AutoUpdate *bool `yaml:"auto_update,omitempty" json:"auto_update,omitempty"`
	// MDNS (default true): the daemon announces itself on the local network (mDNS / DNS-SD, _fleet._tcp)
	// and lists the fleets it hears there, so they can be added as peers. Set false to stay silent.
	MDNS *bool `yaml:"mdns,omitempty" json:"mdns,omitempty"`
	// AvoidStrain (default true): models on a machine that is struggling right now (memory stalls, a GPU
	// slowing down for heat) go behind the healthy alternatives of their role until it recovers.
	AvoidStrain *bool `yaml:"avoid_strain,omitempty" json:"avoid_strain,omitempty"`
	// GPUBandwidth gives the memory bandwidth (GB/s) of cards the machine benchmark does not know, by name
	// ("rtx 9090": 2000, or "*" for every card of this machine).
	GPUBandwidth map[string]float64 `yaml:"gpu_bandwidth,omitempty" json:"gpu_bandwidth,omitempty"`
	// Paired are the fleets that paired with this one (fleet peers pair): each holds a key of its own,
	// accepted wherever the API key is.
	Paired []Paired `yaml:"paired,omitempty" json:"paired,omitempty"`
	// ID identifies this fleet to its peers (generated on first start). Name is how it is shown (default: hostname).
	ID        string          `yaml:"id,omitempty" json:"id,omitempty"`
	Name      string          `yaml:"name,omitempty" json:"name,omitempty"`
	Peers     []Peer          `yaml:"peers,omitempty" json:"peers,omitempty"`
	Roles     map[string]Role `yaml:"roles,omitempty" json:"roles"`
	Endpoints []Endpoint      `yaml:"endpoints" json:"endpoints"`
	Discover  Discover        `yaml:"discover,omitempty" json:"discover"`
	// Economy settings: peer-to-peer compute token accounting, fair queuing, and compute leasing.
	Economy *EconomyConfig `yaml:"economy,omitempty" json:"economy,omitempty"`
	// Hub configuration: custom cloud peering hub target and optional bearer token
	HubURL    string `yaml:"hub_url,omitempty" json:"hub_url,omitempty"`
	HubToken  string `yaml:"hub_token,omitempty" json:"hub_token,omitempty"`
	HubAPIKey string `yaml:"hub_api_key,omitempty" json:"hub_api_key,omitempty"`
	// HubInsecure: do not verify the hub's TLS certificate (a self-signed hub). Off by default; set it with
	// fleet join --insecure.
	HubInsecure bool `yaml:"hub_insecure,omitempty" json:"hub_insecure,omitempty"`
	// HubRequireKey (on a machine that runs the hub): nodes must present an API key (streamAuth's rules) to open the
	// tunnel. Off by default so that nodes that predate keys keep joining; turn it on once every node has its key.
	HubRequireKey bool `yaml:"hub_require_key,omitempty" json:"hub_require_key,omitempty"`
	// SuperviseChains: the daemon starts a dead stage of a chain it deployed again (default on; false turns it off)
	SuperviseChains *bool `yaml:"supervise_chains,omitempty" json:"supervise_chains,omitempty"`
	// HubStreamPorts are the local TCP ports this node lets the hub open raw streams to when it joins
	// (fleet join --stream-port overrides them), e.g. a llama-ring worker's port. Nothing else is reachable.
	HubStreamPorts []int `yaml:"hub_stream_ports,omitempty" json:"hub_stream_ports,omitempty"`
	// HubAutoJoin: the daemon joins HubURL (with HubToken and HubStreamPorts) at start and keeps retrying until it
	// is connected. fleet join sets it, fleet join --leave clears it.
	HubAutoJoin bool `yaml:"hub_auto_join,omitempty" json:"hub_auto_join,omitempty"`
	// HealthEverySec: how often the daemon checks an endpoint that is healthy (seconds; 0 = 60). A failing one is retried
	// sooner (10 s, doubling to 5 min); one that answered a real request meanwhile is not asked at all.
	HealthEverySec int `yaml:"health_every_s,omitempty" json:"health_every_s,omitempty"`
	// Nodes is what the user declares about the machines a deployment uses (fleet config node set): the address the
	// cluster dials a node on and, optionally, its memory and weight read rate for the fixed splits and estimates of
	// fleet deploy. Nothing about a machine is built into fleet.
	Nodes map[string]NodeSpec `yaml:"nodes,omitempty" json:"nodes,omitempty"`
	// HubRTTms is the round trip the auto deploy plan assumes for a worker reached through the hub (0: the planner's
	// estimate, plan.DefaultHubRTTms; a profile's layout.hub_rtt_ms wins).
	HubRTTms float64 `yaml:"hub_rtt_ms,omitempty" json:"hub_rtt_ms,omitempty"`
}

// NodeSpec is one entry of the nodes: section.
type NodeSpec struct {
	Addr string `yaml:"addr,omitempty" json:"addr,omitempty"` // IP the other machines of a ring dial it on
	// MemGiB: the memory a model can use on it (VRAM, or unified memory). When every machine of a split declares it,
	// fleet deploy gives each one blocks in proportion to it (without a plan candidate and without --ts).
	MemGiB float64 `yaml:"mem_gib,omitempty" json:"mem_gib,omitempty"`
	// WeightGBps: the effective rate it reads weights at while decoding (GB/s). With it on every machine of the
	// ring, fleet deploy prints the planner's cost-model estimate for a fixed split.
	WeightGBps float64 `yaml:"weight_gbps,omitempty" json:"weight_gbps,omitempty"`
	// ExpertGBps: the rate its CPU reads MoE expert tensors kept in host RAM (measured or declared, GB/s); the
	// planner's CPU-expert tier uses it instead of the benchmark's RAM bandwidth estimate.
	ExpertGBps float64 `yaml:"expert_gbps,omitempty" json:"expert_gbps,omitempty"`
	// ReserveGiB: memory a deploy leaves free on it on top of fleet's own headroom (the desktop, other programs that
	// come and go): a stage that would eat into it is refused or given a smaller MoE cache.
	ReserveGiB float64 `yaml:"reserve_gib,omitempty" json:"reserve_gib,omitempty"`
	Notes      string  `yaml:"notes,omitempty" json:"notes,omitempty"`
}

// Node returns the declared spec of a machine (case-insensitive name), and whether there is one.
func (c *Config) Node(name string) (NodeSpec, bool) {
	if c == nil || name == "" {
		return NodeSpec{}, false
	}
	if n, ok := c.Nodes[name]; ok {
		return n, true
	}
	for k, n := range c.Nodes {
		if strings.EqualFold(k, name) {
			return n, true
		}
	}
	return NodeSpec{}, false
}

// DefaultHubURL is the hub used when neither the command nor hub_url names one. It is empty in the public build:
// there is no default hub (run one yourself — every `fleet serve` is one — or join someone's). A private build sets
// it at link time (-X github.com/tevfik/fleet-router/internal/config.DefaultHubURL=…, private.env).
var DefaultHubURL = ""

// ExampleHubURL is what help texts show as a hub address.
const ExampleHubURL = "https://hub.example.com"

// ErrNoHub: no hub was named and the build has no default.
var ErrNoHub = errors.New("no hub: give its URL (e.g. " + ExampleHubURL + ") or set hub_url in the configuration")

// HubOrDefault is u, else DefaultHubURL; ErrNoHub when both are empty.
func HubOrDefault(u string) (string, error) {
	if u == "" {
		u = DefaultHubURL
	}
	if u == "" {
		return "", ErrNoHub
	}
	return u, nil
}

// HubDefaultHelp is how help texts name the fallback.
func HubDefaultHelp() string {
	if DefaultHubURL != "" {
		return DefaultHubURL
	}
	return "none: give one, e.g. " + ExampleHubURL
}

// EconomyConfig controls peer-to-peer token accounting, fair queuing, and compute leasing.
type EconomyConfig struct {
	Enabled     *bool `yaml:"enabled,omitempty" json:"enabled,omitempty"`           // master switch (default: true)
	FairQueuing *bool `yaml:"fair_queuing,omitempty" json:"fair_queuing,omitempty"` // credit-weighted QoS priority boost (default: true)
	ShareIdle   *bool `yaml:"share_idle,omitempty" json:"share_idle,omitempty"`     // idle GPU compute sharing (default: false)
}

func (e *EconomyConfig) IsEnabled() bool {
	if e == nil || e.Enabled == nil {
		return true // enabled by default
	}
	return *e.Enabled
}

func (e *EconomyConfig) IsFairQueuing() bool {
	if e == nil || e.FairQueuing == nil {
		return true // enabled by default
	}
	return *e.FairQueuing
}

func (e *EconomyConfig) IsShareIdle() bool {
	if e == nil || e.ShareIdle == nil {
		return false // off by default until enabled
	}
	return *e.ShareIdle
}

var (
	mu      sync.Mutex
	embedRe = regexp.MustCompile(`(?i)(embed|rerank|minilm|bge-|arctic-embed)`)
	slugRe  = regexp.MustCompile(`[^a-z0-9]+`)
)

func Dir() string {
	if d := os.Getenv("XDG_CONFIG_HOME"); d != "" {
		return filepath.Join(d, "fleet")
	}
	h, _ := os.UserHomeDir()
	return filepath.Join(h, ".config", "fleet")
}

// StatsPath is where measured speeds and totals are kept between runs: next to a custom config
// (FLEET_CONFIG), otherwise in the user's state directory. FLEET_STATS overrides both.
func StatsPath() string {
	if p := os.Getenv("FLEET_STATS"); p != "" {
		return p
	}
	if p := os.Getenv("FLEET_CONFIG"); p != "" {
		return p + ".stats.json"
	}
	if d := os.Getenv("XDG_STATE_HOME"); d != "" {
		return filepath.Join(d, "fleet", "stats.json")
	}
	h, _ := os.UserHomeDir()
	return filepath.Join(h, ".local", "state", "fleet", "stats.json")
}

// RunsPath is where the records of distributed runs are kept.
func RunsPath() string { return filepath.Join(filepath.Dir(StatsPath()), "runs.json") }

// RTTPath is where measured master-to-worker round trips are cached (fleet nodes rtt, fleet plan).
func RTTPath() string { return filepath.Join(filepath.Dir(StatsPath()), "rtt.json") }

// EvalPath is where `fleet eval` keeps its scores (next to the speed statistics).
func EvalPath() string { return filepath.Join(filepath.Dir(StatsPath()), "eval.json") }

// HWBenchPath is where the machine benchmark (AI score) of this machine is kept.
func HWBenchPath() string { return filepath.Join(filepath.Dir(StatsPath()), "machine.json") }

// ProbePath is where `fleet probe` keeps what it found out about each model.
func ProbePath() string { return filepath.Join(filepath.Dir(StatsPath()), "probe.json") }

func Path() string {
	if p := os.Getenv("FLEET_CONFIG"); p != "" {
		return p
	}
	return filepath.Join(Dir(), "config.yaml")
}

func Default() *Config {
	return &Config{
		Listen:   DefaultListen,
		Roles:    map[string]Role{},
		Discover: Discover{Ports: "8080,1919,4000,11434,30000-30020"},
	}
}

// Load returns the default (empty) configuration when the file does not exist.
func Load(path string) (*Config, error) {
	b, err := os.ReadFile(path)
	if os.IsNotExist(err) {
		return Default(), nil
	}
	if err != nil {
		return nil, err
	}
	c := Default()
	if err := yaml.Unmarshal(b, c); err != nil {
		return nil, fmt.Errorf("%s: %w", path, err)
	}
	if c.Roles == nil {
		c.Roles = map[string]Role{}
	}
	return c, c.Validate()
}

func (c *Config) Save(path string) error {
	mu.Lock()
	defer mu.Unlock()
	if err := c.Validate(); err != nil {
		return err
	}
	b, err := yaml.Marshal(c)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	snapshot(path, b)
	return writeFile(path, b)
}

// writeFile replaces path atomically: a temp file of its own (two writers sharing one fixed ".tmp" name could rename
// each other's half-written file into place), synced before the rename so a crash leaves the old or the new file.
func writeFile(path string, b []byte) error {
	f, err := os.CreateTemp(filepath.Dir(path), filepath.Base(path)+".*.tmp")
	if err != nil {
		return err
	}
	tmp := f.Name()
	ok := false
	defer func() {
		if !ok {
			_ = os.Remove(tmp)
		}
	}()
	if err := f.Chmod(0o600); err != nil {
		f.Close()
		return err
	}
	if _, err := f.Write(b); err != nil {
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
	if d, err := os.Open(filepath.Dir(path)); err == nil {
		_ = d.Sync()
		d.Close()
	}
	return nil
}

// ---- history: every change keeps the configuration it replaced, so a wrong key press can be undone ----

const keepSnapshots = 30

func historyDir(path string) string { return filepath.Join(filepath.Dir(path), "history") }

// snapshot stores the current file (if it differs from the new content) before it is overwritten.
func snapshot(path string, next []byte) {
	old, err := os.ReadFile(path)
	if err != nil || bytes.Equal(old, next) {
		return
	}
	dir := historyDir(path)
	if os.MkdirAll(dir, 0o700) != nil {
		return
	}
	name := filepath.Join(dir, time.Now().Format("20060102-150405.000000")+".yaml")
	for i := 1; ; i++ { // two saves within the same microsecond must not overwrite each other
		if _, err := os.Stat(name); err != nil {
			break
		}
		name = filepath.Join(dir, fmt.Sprintf("%s-%d.yaml", strings.TrimSuffix(filepath.Base(name), ".yaml"), i))
	}
	_ = os.WriteFile(name, old, 0o600)
	if all := Snapshots(path); len(all) > keepSnapshots {
		for _, old := range all[:len(all)-keepSnapshots] {
			os.Remove(filepath.Join(dir, old))
		}
	}
}

// Snapshots lists the saved configurations, oldest first (file names are timestamps).
func Snapshots(path string) []string {
	ents, _ := os.ReadDir(historyDir(path))
	var out []string
	for _, e := range ents {
		if strings.HasSuffix(e.Name(), ".yaml") {
			out = append(out, e.Name())
		}
	}
	sort.Slice(out, func(i, j int) bool { // compare without ".yaml": "…5-1" comes after "…5"
		return strings.TrimSuffix(out[i], ".yaml") < strings.TrimSuffix(out[j], ".yaml")
	})
	return out
}

// Undo restores the most recent snapshot (and drops it, so the next Undo goes one step further back).
// It returns the snapshot's name. The configuration being replaced is not snapshotted again.
func Undo(path string) (string, error) {
	mu.Lock()
	defer mu.Unlock()
	all := Snapshots(path)
	if len(all) == 0 {
		return "", fmt.Errorf("nothing to undo")
	}
	last := all[len(all)-1]
	file := filepath.Join(historyDir(path), last)
	b, err := os.ReadFile(file)
	if err != nil {
		return "", err
	}
	var c Config
	if err := yaml.Unmarshal(b, &c); err != nil {
		return "", fmt.Errorf("snapshot %s is unreadable: %w", last, err)
	}
	if err := c.Validate(); err != nil {
		return "", fmt.Errorf("snapshot %s is not valid any more: %w", last, err)
	}
	if err := writeFile(path, b); err != nil {
		return "", err
	}
	os.Remove(file)
	return last, nil
}

// CheckSSHTarget rejects an ssh host (alias, host, user@host, IP) that ssh would read as an option or that carries
// shell metacharacters: a value like "-oProxyCommand=..." runs a command on this machine when handed to ssh.
func CheckSSHTarget(host string) error {
	if host == "" {
		return fmt.Errorf("empty ssh target")
	}
	if strings.HasPrefix(host, "-") || strings.ContainsAny(host, " \t\r\n'\"$;&|<>`\\") {
		return fmt.Errorf("ssh target %q starts with - or contains a space or shell character", host)
	}
	return nil
}

func (c *Config) Validate() error {
	seen := map[string]bool{}
	for _, e := range c.Endpoints {
		if e.Name == "" {
			return fmt.Errorf("endpoint without a name (model %q)", e.Model)
		}
		if seen[e.Name] {
			return fmt.Errorf("duplicate endpoint name: %s", e.Name)
		}
		seen[e.Name] = true
		if e.URL == "" && (e.SSH == "" || e.Remote == "") {
			return fmt.Errorf("%s: url or (ssh + remote) is required", e.Name)
		}
		if e.SSH != "" {
			if err := CheckSSHTarget(e.SSH); err != nil {
				return fmt.Errorf("endpoint %s: %w", e.Name, err)
			}
		}
	}
	for name, n := range c.Nodes {
		if name == "" || strings.HasPrefix(name, "-") || strings.ContainsAny(name, " \t/:=,'\"$;&|<>`\\") {
			return fmt.Errorf("node name %q is empty or contains a space or one of / : = , quotes or shell characters", name)
		}
		if n.Addr != "" && net.ParseIP(n.Addr) == nil {
			return fmt.Errorf("node %s: addr %q is not an IP address", name, n.Addr)
		}
		if n.MemGiB < 0 || n.WeightGBps < 0 || n.ExpertGBps < 0 || n.ReserveGiB < 0 {
			return fmt.Errorf("node %s: mem_gib, weight_gbps and expert_gbps must not be negative", name)
		}
	}
	for _, h := range c.Discover.SSH {
		if err := CheckSSHTarget(h); err != nil {
			return fmt.Errorf("discover.ssh: %w", err)
		}
	}
	pseen := map[string]bool{}
	for _, p := range c.Peers {
		if p.Name == "" || strings.ContainsAny(p.Name, " /:") {
			return fmt.Errorf("peer name %q is empty or contains space, / or :", p.Name)
		}
		if pseen[p.Name] {
			return fmt.Errorf("duplicate peer name: %s", p.Name)
		}
		pseen[p.Name] = true
		if p.URL == "" && p.SSH == "" {
			return fmt.Errorf("peer %s: url or ssh is required", p.Name)
		}
		if p.SSH != "" {
			if err := CheckSSHTarget(p.SSH); err != nil {
				return fmt.Errorf("peer %s: %w", p.Name, err)
			}
		}
		for _, sel := range append(append([]string{}, p.Only...), p.Skip...) {
			if _, _, err := ParseSelector(sel); err != nil {
				return fmt.Errorf("peer %s: %w", p.Name, err)
			}
		}
	}
	for _, e := range c.Endpoints {
		if e.Peer != "" && !pseen[e.Peer] {
			return fmt.Errorf("endpoint %s belongs to unknown peer %q", e.Name, e.Peer)
		}
	}
	if v := strings.ToLower(strings.TrimSpace(c.TunnelIdle)); v != "" && v != "off" && v != "never" && v != "0" {
		if d, err := time.ParseDuration(v); err != nil || d <= 0 {
			return fmt.Errorf("tunnel_idle: %q is not a duration like 10m (or off)", c.TunnelIdle)
		}
	}
	if v := strings.ToLower(strings.TrimSpace(c.VRAMIdle)); v != "" && v != "off" && v != "never" && v != "0" {
		if d, err := time.ParseDuration(v); err != nil || d <= 0 {
			return fmt.Errorf("vram_idle: %q is not a duration like 10m (or off)", c.VRAMIdle)
		}
	}
	for _, e := range c.Endpoints {
		if e.FleetPort < 0 || e.FleetPort > 65535 {
			return fmt.Errorf("endpoint %s: fleet_port %d is not a port", e.Name, e.FleetPort)
		}
		if v := strings.ToLower(strings.TrimSpace(e.Stall)); v != "" && v != "off" && v != "never" && v != "0" {
			if d, err := time.ParseDuration(v); err != nil || d < time.Second {
				return fmt.Errorf("endpoint %s: stall %q is not a duration of at least 1s (like 90s) or off", e.Name, e.Stall)
			}
		}
		if g := e.Group; g != nil {
			if strings.TrimSpace(g.Engine) == "" {
				return fmt.Errorf("endpoint %s: group needs an engine (llama-ring, prima, llamacpp-rpc)", e.Name)
			}
			for _, w := range g.Workers {
				if strings.TrimSpace(w) == "" {
					return fmt.Errorf("endpoint %s: a group worker has no name", e.Name)
				}
			}
			if g.StatsPort < 0 || g.StatsPort > 65535 {
				return fmt.Errorf("endpoint %s: stats_port %d is not a port", e.Name, g.StatsPort)
			}
		}
	}
	for r, role := range c.Roles {
		switch role.Strategy {
		case "", "failover", "least-busy", "round-robin", "fastest", "cost":
		default:
			return fmt.Errorf("role %s: unknown strategy %q (failover, least-busy, round-robin, fastest, cost)", r, role.Strategy)
		}
		if seen[r] {
			return fmt.Errorf("role name collides with an endpoint name: %s", r)
		}
		for _, n := range append([]string{role.Preferred}, role.Fallback...) {
			if n != "" && !seen[n] {
				return fmt.Errorf("role %s: unknown endpoint %q", r, n)
			}
		}
		for _, m := range role.Match {
			if _, _, err := ParseSelector(m); err != nil {
				return fmt.Errorf("role %s: %w", r, err)
			}
		}
		if role.Preferred == "" && len(role.Match) == 0 {
			return fmt.Errorf("role %s: needs a preferred endpoint or a match selector", r)
		}
	}
	// Embedding roles must not mix vector sizes: switching models silently would corrupt vector indexes.
	for r, role := range c.Roles {
		var first *Endpoint
		for _, e := range c.Pool(role) {
			e := e
			if !e.IsEmbed() || e.Dim == 0 {
				continue
			}
			if first == nil {
				first = &e
			} else if e.Dim != first.Dim {
				return fmt.Errorf("role %s: %s has %d dimensions but %s has %d — mixing sizes would corrupt vector indexes",
					r, e.Name, e.Dim, first.Name, first.Dim)
			}
		}
	}
	return nil
}

func (c *Config) Endpoint(name string) *Endpoint {
	for i := range c.Endpoints {
		if c.Endpoints[i].Name == name {
			return &c.Endpoints[i]
		}
	}
	return nil
}

// Selectors pick endpoints by property instead of by name.
var selectorKinds = map[string]bool{"tag": true, "node": true, "cap": true, "model": true}

func ParseSelector(s string) (kind, val string, err error) {
	kind, val, ok := strings.Cut(s, ":")
	if !ok || !selectorKinds[kind] || val == "" {
		return "", "", fmt.Errorf("bad selector %q (use tag:x, node:x, cap:x or model:glob)", s)
	}
	return kind, val, nil
}

// Matches reports whether the endpoint fits a selector.
func (e *Endpoint) Matches(sel string) bool {
	kind, val, err := ParseSelector(sel)
	if err != nil {
		return false
	}
	switch kind {
	case "tag":
		for _, t := range e.Tags {
			if t == val {
				return true
			}
		}
	case "node":
		return e.Node == val
	case "cap":
		return e.Has(val)
	case "model":
		ok, _ := path.Match(val, e.Model)
		return ok
	}
	return false
}

// Pool lists the role's endpoints in order: preferred, fallbacks, then selector matches (config order, no duplicates).
func (c *Config) Pool(r Role) []Endpoint {
	var out []Endpoint
	seen := map[string]bool{}
	add := func(e *Endpoint) {
		if e != nil && !seen[e.Name] {
			seen[e.Name] = true
			out = append(out, *e)
		}
	}
	if r.Preferred != "" {
		add(c.Endpoint(r.Preferred))
	}
	for _, n := range r.Fallback {
		add(c.Endpoint(n))
	}
	byHops := append([]Endpoint{}, c.Endpoints...) // direct models before the ones behind a peer
	sort.SliceStable(byHops, func(i, j int) bool { return len(byHops[i].Path) < len(byHops[j].Path) })
	for i := range byHops {
		for _, m := range r.Match {
			if byHops[i].Matches(m) {
				add(&byHops[i])
				break
			}
		}
	}
	return out
}

// Lookup resolves a model reference to endpoints: an endpoint name, "node/model", or a bare model id
// (":latest" is optional). A bare id can match several nodes — they then form a pool.
func (c *Config) Lookup(ref string) []Endpoint {
	out := c.lookup(ref)
	sort.SliceStable(out, func(i, j int) bool { return len(out[i].Path) < len(out[j].Path) }) // direct before behind-a-peer
	return out
}

func (c *Config) lookup(ref string) []Endpoint {
	if e := c.Endpoint(ref); e != nil {
		return []Endpoint{*e}
	}
	norm := func(m string) string { return strings.TrimSuffix(m, ":latest") }
	var out []Endpoint
	if node, m, ok := strings.Cut(ref, "/"); ok {
		for _, e := range c.Endpoints {
			if e.Node == node && norm(e.Model) == norm(m) {
				out = append(out, e)
			}
		}
		if len(out) > 0 {
			return out
		}
	}
	for _, e := range c.Endpoints {
		if norm(e.Model) == norm(ref) {
			out = append(out, e)
		}
	}
	return out
}

// Wire is the model value sent to the endpoint: the plain model id, except for peer endpoints, where it is
// "node/model" so the peer fleet picks the right model on the right machine.
func (e *Endpoint) Wire() string {
	if e.Peer != "" {
		return e.Ref()
	}
	return e.Model
}

// Ref is the public name of an endpoint's model: "node/model".
func (e *Endpoint) Ref() string { return e.Node + "/" + e.Model }

func (c *Config) RoleNames() []string {
	var n []string
	for r := range c.Roles {
		n = append(n, r)
	}
	sort.Strings(n)
	return n
}

func (e *Endpoint) IsEmbed() bool {
	if len(e.Caps) > 0 {
		return e.Has("embed") && !e.Has("chat")
	}
	return e.Kind == "embed" || (e.Kind == "" && embedRe.MatchString(e.Model))
}

var visionRe = regexp.MustCompile(`(?i)(-vl\b|vl:|vision|llava|pixtral|omni|minicpm-v|moondream|qwen.*-vl)`)
var audioRe = regexp.MustCompile(`(?i)(omni|whisper|audio|voxtral)`)

// Has reports whether the endpoint supports a capability. Explicit Caps win; otherwise it is inferred
// from the name (see Caps). "chat" is true for everything that is not embedding-only.
func (e *Endpoint) Has(c string) bool {
	if len(e.Caps) > 0 {
		for _, x := range e.Caps {
			if x == c {
				return true
			}
		}
		return false
	}
	switch c {
	case "embed":
		return e.IsEmbed()
	case "chat":
		return !e.IsEmbed()
	case "vision":
		return !e.IsEmbed() && visionRe.MatchString(e.Model)
	case "audio":
		return !e.IsEmbed() && audioRe.MatchString(e.Model)
	}
	return false
}

func IsEmbedModel(m string) bool { return embedRe.MatchString(m) }

func Slug(s string) string {
	return strings.Trim(slugRe.ReplaceAllString(strings.ToLower(s), "-"), "-")
}

// SecretSource says where Secret(name) finds its value: "env", "<path of the .env file>" or "" (not found).
func SecretSource(name string) string {
	if os.Getenv(name) != "" {
		return "env"
	}
	for _, f := range envFiles() {
		if v, ok := readEnvFile(f, name); ok && v != "" {
			return f
		}
	}
	return ""
}

func envFiles() []string {
	h, _ := os.UserHomeDir()
	return []string{filepath.Join(Dir(), ".env"), filepath.Join(h, ".config", "opencode", ".env")}
}

// readEnvFile returns name's value from a .env file: "NAME=value", an optional "export " prefix, surrounding
// quotes removed; blank lines and # comments are skipped.
func readEnvFile(path, name string) (string, bool) {
	fh, err := os.Open(path)
	if err != nil {
		return "", false
	}
	defer fh.Close()
	sc := bufio.NewScanner(fh)
	for sc.Scan() {
		l := strings.TrimSpace(sc.Text())
		l = strings.TrimSpace(strings.TrimPrefix(l, "export "))
		if strings.HasPrefix(l, name+"=") {
			return strings.Trim(strings.TrimPrefix(l, name+"="), `"'`), true
		}
	}
	return "", false
}

// HubKeyEnv is the variable / .env name of the hub API key (fleet key set).
const HubKeyEnv = "FLEET_API_KEY"

// ResolveHubAPIKey finds the hub API key and where it came from. Order, the same everywhere
// (fleet stream connect, deploy's hub streams, the daemon's tunnel, fleet key show, fleet doctor):
//  1. $FLEET_API_KEY
//  2. FLEET_API_KEY in <config dir>/.env (then ~/.config/opencode/.env)
//  3. hub_api_key of the configuration
//  4. the key of api_key_env (a master that is its own hub)
//
// Environment beats files beats the config, as for every Secret: a value you export for one run overrides what is saved.
func ResolveHubAPIKey(cfg *Config) (key, source string) {
	if src := SecretSource(HubKeyEnv); src != "" {
		return Secret(HubKeyEnv), src
	}
	if cfg != nil {
		if cfg.HubAPIKey != "" {
			return cfg.HubAPIKey, "config hub_api_key"
		}
		if cfg.APIKeyEnv != "" {
			if v := Secret(cfg.APIKeyEnv); v != "" {
				return v, "api_key_env " + cfg.APIKeyEnv
			}
		}
	}
	return "", ""
}

// MaskSecret shows that a secret is set without giving it away: its length and, for long ones, the last 2 characters.
func MaskSecret(v string) string {
	switch {
	case v == "":
		return "(not set)"
	case len(v) < 16:
		return fmt.Sprintf("set (%d characters)", len(v))
	default:
		return fmt.Sprintf("set (%d characters, ends …%s)", len(v), v[len(v)-2:])
	}
}

// Secret looks in the environment first, then <config dir>/.env and ~/.config/opencode/.env.
func Secret(name string) string {
	if v := os.Getenv(name); v != "" {
		return v
	}
	for _, f := range envFiles() {
		if v, ok := readEnvFile(f, name); ok && v != "" {
			return v
		}
	}
	return ""
}

// envLineIs reports whether an .env line assigns name ("NAME=…" or "export NAME=…").
func envLineIs(line, name string) bool {
	l := strings.TrimSpace(line)
	l = strings.TrimSpace(strings.TrimPrefix(l, "export "))
	return strings.HasPrefix(l, name+"=")
}

// writeEnv writes the .env atomically with mode 0600 (also when the file already existed with a looser mode).
// With no lines left it removes the file instead of leaving an empty one.
func writeEnv(envPath string, lines []string) error {
	empty := true
	for _, l := range lines {
		if strings.TrimSpace(l) != "" {
			empty = false
		}
	}
	if empty {
		if err := os.Remove(envPath); err != nil && !os.IsNotExist(err) {
			return err
		}
		return nil
	}
	tmp := envPath + ".tmp"
	_ = os.Remove(tmp) // a stale temp file may carry another mode
	if err := os.WriteFile(tmp, []byte(strings.Join(lines, "\n")+"\n"), 0o600); err != nil {
		return err
	}
	if err := os.Chmod(tmp, 0o600); err != nil {
		_ = os.Remove(tmp)
		return err
	}
	return os.Rename(tmp, envPath)
}

func readEnvLines(envPath string) ([]string, error) {
	b, err := os.ReadFile(envPath)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, err
	}
	var lines []string
	sc := bufio.NewScanner(strings.NewReader(string(b)))
	for sc.Scan() {
		lines = append(lines, sc.Text())
	}
	return lines, sc.Err()
}

// SetSecret sets or updates name=value in <config dir>/.env, with mode 0600. An empty value or one with a
// line break is refused (it would write an empty or broken file).
func SetSecret(name, value string) error {
	if strings.TrimSpace(name) == "" || strings.ContainsAny(name, "=\r\n ") {
		return fmt.Errorf("bad secret name %q", name)
	}
	if strings.TrimSpace(value) == "" {
		return fmt.Errorf("refusing to save an empty value for %s", name)
	}
	if strings.ContainsAny(value, "\r\n") {
		return fmt.Errorf("the value of %s cannot contain a line break", name)
	}
	dir := Dir()
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	envPath := filepath.Join(dir, ".env")
	old, err := readEnvLines(envPath)
	if err != nil {
		return err
	}
	var lines []string
	found := false
	for _, l := range old {
		if envLineIs(l, name) {
			if !found {
				lines = append(lines, fmt.Sprintf("%s=%s", name, value))
			}
			found = true // a duplicate assignment is dropped
			continue
		}
		lines = append(lines, l)
	}
	if !found {
		lines = append(lines, fmt.Sprintf("%s=%s", name, value))
	}
	return writeEnv(envPath, lines)
}

// RemoveSecret removes name=... from <config dir>/.env; the file goes with its last line.
func RemoveSecret(name string) error {
	envPath := filepath.Join(Dir(), ".env")
	old, err := readEnvLines(envPath)
	if err != nil || old == nil {
		return err
	}
	var lines []string
	for _, l := range old {
		if !envLineIs(l, name) {
			lines = append(lines, l)
		}
	}
	return writeEnv(envPath, lines)
}

func (e *Endpoint) APIKey() string {
	if e.KeyEnv != "" {
		return Secret(e.KeyEnv)
	}
	return e.Key
}

// AutoUpdateOn reports whether the daemon should update itself (on unless auto_update: false).
func (c *Config) AutoUpdateOn() bool { return c.AutoUpdate == nil || *c.AutoUpdate }

// SSHHosts lists the SSH hosts the models and peers are reached through (sorted, once each).
func (c *Config) SSHHosts() []string {
	seen := map[string]bool{}
	var out []string
	add := func(h string) {
		if h != "" && !seen[h] {
			seen[h] = true
			out = append(out, h)
		}
	}
	for _, e := range c.Endpoints {
		if e.Peer == "" {
			add(e.SSH)
		}
	}
	for _, p := range c.Peers {
		add(p.SSH)
	}
	sort.Strings(out)
	return out
}

// WarmFirst reports whether the role puts loaded models first (see Role.PreferWarm).
func (r Role) WarmFirst() bool {
	if r.PreferWarm != nil {
		return *r.PreferWarm
	}
	return r.Strategy != "" && r.Strategy != "failover"
}

// AvoidStrainOn reports whether routing steers around struggling machines (on unless avoid_strain: false).
func (c *Config) AvoidStrainOn() bool { return c.AvoidStrain == nil || *c.AvoidStrain }

// MDNSOn reports whether the daemon announces itself on the local network (on unless mdns: false).
func (c *Config) MDNSOn() bool { return c.MDNS == nil || *c.MDNS }

// PoolWarnings names the roles that cannot survive a failure: a single model, or every model on one machine.
func (c *Config) PoolWarnings() []string {
	var names []string
	for r := range c.Roles {
		names = append(names, r)
	}
	sort.Strings(names)
	var out []string
	for _, r := range names {
		pool := c.Pool(c.Roles[r])
		nodes := map[string]bool{}
		for _, e := range pool {
			nodes[e.Node] = true
		}
		switch {
		case len(pool) == 0:
			out = append(out, fmt.Sprintf("role %s has no model at all", r))
		case len(pool) == 1:
			out = append(out, fmt.Sprintf("role %s has a single model (%s): if it goes down, requests fail — add a fallback", r, pool[0].Name))
		case len(nodes) == 1:
			out = append(out, fmt.Sprintf("role %s: all %d models are on %s — one machine going down takes the role with it", r, len(pool), pool[0].Node))
		}
	}
	return out
}

// IdleDuration is TunnelIdle as a duration: 10 minutes when unset, 0 for "off".
func (c *Config) IdleDuration() time.Duration {
	switch v := strings.TrimSpace(strings.ToLower(c.TunnelIdle)); v {
	case "":
		return 10 * time.Minute
	case "off", "never", "0":
		return 0
	default:
		if d, err := time.ParseDuration(v); err == nil && d > 0 {
			return d
		}
		return 10 * time.Minute
	}
}

// VRAMIdleDuration is VRAMIdle as a duration: 10 minutes when unset, 0 for "off".
func (c *Config) VRAMIdleDuration() time.Duration {
	switch v := strings.TrimSpace(strings.ToLower(c.VRAMIdle)); v {
	case "":
		return 10 * time.Minute
	case "off", "never", "0":
		return 0
	default:
		if d, err := time.ParseDuration(v); err == nil && d > 0 {
			return d
		}
		return 10 * time.Minute
	}
}

// DropEndpoints removes endpoints from the configuration and mends the roles that named them, so the result still
// validates: a removed preferred endpoint is replaced by the role's first remaining fallback, a removed fallback
// just leaves the list, and a role left with nothing to route to (no preferred, no match selector) is deleted.
// It returns one line per change; `fleet config undo` brings everything back.
func (c *Config) DropEndpoints(names ...string) []string {
	gone := map[string]bool{}
	for _, n := range names {
		gone[n] = true
	}
	var eps []Endpoint
	for _, e := range c.Endpoints {
		if !gone[e.Name] {
			eps = append(eps, e)
		}
	}
	c.Endpoints = eps
	var notes []string
	for _, r := range c.RoleNames() {
		role := c.Roles[r]
		changed := false
		var fb []string
		for _, f := range role.Fallback {
			if gone[f] {
				changed = true
				notes = append(notes, fmt.Sprintf("role %s: fallback %s dropped", r, f))
				continue
			}
			fb = append(fb, f)
		}
		role.Fallback = fb
		if gone[role.Preferred] {
			changed = true
			old := role.Preferred
			role.Preferred = ""
			if len(role.Fallback) > 0 {
				role.Preferred, role.Fallback = role.Fallback[0], role.Fallback[1:]
				notes = append(notes, fmt.Sprintf("role %s: preferred %s replaced by %s", r, old, role.Preferred))
			}
		}
		if !changed {
			continue
		}
		if role.Preferred == "" && len(role.Match) == 0 {
			delete(c.Roles, r)
			notes = append(notes, fmt.Sprintf("role %s: nothing left to route to, role deleted", r))
			continue
		}
		if role.Preferred == "" {
			notes = append(notes, fmt.Sprintf("role %s: preferred %s removed; the role's selectors still apply", r, "endpoint"))
		}
		c.Roles[r] = role
	}
	return notes
}

// GenerateHubKey returns a new random hub API key: "flt_hub_" and 192 bits of crypto/rand in hex.
func GenerateHubKey() string {
	b := make([]byte, 24)
	if _, err := cryptorand.Read(b); err != nil {
		panic("no randomness: " + err.Error()) // an unusable system, not a condition to go on with
	}
	return "flt_hub_" + hex.EncodeToString(b)
}

// EnsureHubKey makes sure this machine has a hub API key: when none resolves (environment, .env, config, api_key_env)
// a new one is generated and saved like `fleet key set` does. It returns the key and whether it was just generated.
// The key is what this machine's own hub role accepts from nodes; to join another hub, `fleet key set <its key>`.
func EnsureHubKey(cfg *Config) (key string, generated bool, err error) {
	if k, _ := ResolveHubAPIKey(cfg); k != "" {
		return k, false, nil
	}
	k := GenerateHubKey()
	if err := SaveHubKey(k); err != nil {
		return "", false, err
	}
	return k, true, nil
}

// SaveHubKey stores key in .env (mode 600) and in the configuration's hub_api_key: what `fleet key set` does.
func SaveHubKey(key string) error {
	if err := SetSecret(HubKeyEnv, key); err != nil {
		return fmt.Errorf("save key to .env: %w", err)
	}
	if unlock, err := Lock(Path()); err == nil { // the daemon saves under the same lock
		defer unlock()
	}
	if cfg, err := Load(Path()); err == nil && cfg != nil {
		cfg.HubAPIKey = key
		if err := cfg.Save(Path()); err != nil {
			return fmt.Errorf("saved to .env, but not to the config: %w", err)
		}
	}
	return nil
}

// Relay is one stage-to-stage link of a chain carried over ssh tunnels through the machine that deployed it (Via,
// fleet deploy --engine chain --relay): From dials 127.0.0.1:Port, which reaches To's stage port. Port is listened
// on at Via (an ssh -L to To, unless To is Via) and at From (an ssh -R to Via, unless From is Via). Ending the run
// closes both.
type Relay struct {
	Via  string `yaml:"via" json:"via"`
	From string `yaml:"from" json:"from"`
	To   string `yaml:"to" json:"to"`
	Port int    `yaml:"port" json:"port"`
	// Hub, when set, says the link goes through the hub instead of ssh tunnels: a `fleet stream connect` on From listens on
	// 127.0.0.1:Port (Via is then unused).
	Hub string `yaml:"hub,omitempty" json:"hub,omitempty"`
}
