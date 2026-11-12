package deploy

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/tevfik/fleet-router/internal/config"
	"github.com/tevfik/fleet-router/internal/ggufhead"
	"github.com/tevfik/fleet-router/internal/ggufmeta"
	"github.com/tevfik/fleet-router/internal/plan"
)

// The stage-pipe engine: a model is cut into contiguous ranges of layers, one per machine; every stage
// forwards the hidden state to the next, the last answers with a token. The stages must come up from the last
// to the first, because each one connects to the stage behind it when it starts.
//
// Nothing in this file starts a process: it computes commands and a start order, and runs them through a
// StageRunner. The only runner here records (dry run). A runner that really starts stages over SSH is left
// out until it can be tried on boards.

// StagePipeAdapter holds EVERY runtime-specific string of the engine, so a runtime that changes its names is
// one edit here. Other runtimes come as stage modules (pkg/stagemod) with an adapter of their own.
type StagePipeAdapter struct {
	Provisional         bool     // the names below are not verified on hardware: plans carry a warning
	LayersEnv           string   // env var: "begin:end" (end exclusive)
	NextEnv             string   // env var: endpoint of the next stage
	TimeoutEnv          string   // env var: seconds a stage waits for the next one
	EmbeddingOffloadEnv string   // env var set to "on" on the first stage: embedding tables out of the NPU pool
	StageBin            string   // binary of the middle and last stages
	StageArgs           []string // its arguments; {model} and {listen} are filled in
	IngressBin          string   // binary of the first stage
	IngressArgs         []string // {model}, {port}
	ListenFmt           string   // endpoint a stage listens on ({port})
	NextFmt             string   // endpoint of the next stage ({host}, {port})
	StagePort           int      // first stage-to-stage port; a second stage on one machine takes the next
	IngressPort         int      // the port the first stage serves requests on
	TimeoutS            int      // default of the timeout env var (the runtime's own default)
	ModelRoot           string   // where model directories sit on a board
	StageModelSub       string   // subdirectory of the model directory the middle/last stages load ({stage_model}); "" = the directory itself
	IngressPortFixed    bool     // the first stage cannot choose its port (IngressPort is the only one): a conflict or another --port is an error
	HealthPath          string   // HTTP path of the first stage's health answer [UNKNOWN: unverified]
}

// ChainStageV1 (also aliased as FleetStageV1) is the vendor-neutral adapter for Chain Stage Protocol v1 (CSP1).
// The first stage is the chain driver with its HTTP front (fleet-engine 17a8d9f): OpenAI/llama-server API for the
// router; a bare `serve` would only speak CSP1, which no HTTP client can use.
var ChainStageV1 = StagePipeAdapter{
	Provisional:         false,
	LayersEnv:           "STAGE_LAYERS",
	NextEnv:             "STAGE_NEXT",
	TimeoutEnv:          "STAGE_TIMEOUT_S",
	EmbeddingOffloadEnv: "STAGE_EMBEDDING_OFFLOAD",
	StageBin:            "fleet-engine-stage",
	StageArgs:           []string{"serve", "-m", "{model}", "{listen}"},
	IngressBin:          "fleet-engine-stage",
	IngressArgs:         []string{"chain", "-m", "{model}", "--http", "0.0.0.0:{port}"},
	ListenFmt:           "tcp://*:{port}",
	NextFmt:             "tcp://{host}:{port}",
	StagePort:           5600,
	IngressPort:         52310,
	TimeoutS:            600,
	ModelRoot:           "",
	HealthPath:          "/health",
}

// FleetStageV1 is kept as an alias for ChainStageV1.
var FleetStageV1 = ChainStageV1

// StagePipeOptions are the choices of one stage-pipe deployment.
type StagePipeOptions struct {
	ModelDir         string // the model directory on the boards ("": the adapter's root + the model's name)
	TimeoutS         int    // 0: the adapter's
	EmbeddingOffload bool   // the first stage keeps the embedding tables outside the NPU pool
	Host             func(node string) string
	CPUMoE           bool // keep all MoE expert weights in host CPU RAM
	NCPUMoE          int  // keep MoE expert weights of first N layers in host CPU RAM
	MoECacheMiB      int  // GPU cache size in MiB for host-resident MoE experts
	// StageMoECacheMiB: the MoE cache of one stage (key: its first layer), set by the memory check from the VRAM that
	// stage's machine has free; it wins over MoECacheMiB for that stage
	StageMoECacheMiB map[int]int
	Force            bool          // deploy even when the memory check says a stage does not fit
	Auto             bool          // E13: set the deploy up from what the machines have (common model path, free VRAM, ready wait; chainauto.go)
	HeadTarget       string        // the target model file read here to make the head ("": the model path of the stages); --auto: the file as it is on this machine
	HeadDir          string        // where the head-embedded draft is kept ("": ~/.cache/fleet/dflash); --auto: a path every machine has
	ReadyTimeout     time.Duration // how long each stage may take to answer (0: TimeoutS, else 120s; --auto: from the model size)
	EndpointName     string        // endpoint name (default: "distributed-" + base(model))
	DraftModel       string        // path to DFlash draft model file
	DraftMax         int           // max draft tokens (0: auto: 2 if CPU-MoE, 4 if GPU)
	Ctx              int           // context length (0: engine default 2048)
	Np               int           // concurrent requests: --n-seq on every stage (a sequence each, KV cache per sequence); 0 or 1: one at a time
	Lanes            int           // parallel connections to next stage: --lanes on ingress (needs Np >= 2, > 1 passed to engine)
	planName         string        // endpoint name the deploy will register (default derived), for the foreign-port lookup
	MMProj           string        // vision projector GGUF on the ingress machine (--mmproj of the engine): images in chat requests
	Wire             string        // hidden states between stages: "" (engine default auto) | "f32" | "bf16" | "auto"
	Port             int           // ingress port (0: adapter's IngressPort, 52310 for csp1)
	StagePort        int           // stage port (0: adapter's StagePort, 5600 for csp1)
	Spec             string        // speculation spec: "" | "auto" | "draft-dflash" | "none"
	ConfigPath       string        // config path for endpoint registration ("": ~/.config/fleet/fleet.yaml)
	Role             string        // target role for registered endpoint
	Relay            bool          // stages on different machines are joined by ssh tunnels through this machine (sites that cannot reach each other)
	ViaHub           bool          // stages on different machines are joined through the hub's raw TCP stream mode (relayhub.go)
	HubURL           string        // the hub for ViaHub (DeployChain fills it: hub_url of the config, else config.DefaultHubURL)
	DryRun           bool          // print only: no endpoint registration, no run record
}

// Readiness says how to tell that a stage is up.
type Readiness struct {
	Kind string `json:"kind"` // "tcp" | "http"
	Addr string `json:"addr"` // host:port
	Path string `json:"path,omitempty"`
}

// StageLaunch is one stage: what it holds, how to start it and how to tell it is up.
type StageLaunch struct {
	Index   int       `json:"index"` // position in the pipeline: 0 is the first stage (it takes the requests)
	Node    string    `json:"node"`
	Host    string    `json:"host"`
	Role    string    `json:"role"` // ingress | middle | last
	Begin   int       `json:"begin"`
	End     int       `json:"end"` // exclusive
	Port    int       `json:"port"`
	Next    string    `json:"next,omitempty"`
	Env     []string  `json:"env"`
	Command string    `json:"command"` // env assignments + binary + arguments, as one shell line
	Ready   Readiness `json:"ready"`
	Dir     string    `json:"dir,omitempty"` // the model directory this stage loads (for runners that do not run Command)
}

// StagePipePlan is the stages in pipeline order and the order to start them in.
type StagePipePlan struct {
	Model      string        `json:"model"`
	ModelDir   string        `json:"model_dir"`
	Layers     int           `json:"layers"`
	Stages     []StageLaunch `json:"stages"`
	StartOrder []int         `json:"start_order"` // indexes into Stages: last stage first
	Warnings   []string      `json:"warnings,omitempty"`
	// Draft is the DFlash draft the first stage loads (a head-embedded copy made on THIS machine for >= 2 stages), "" without one
	Draft string `json:"draft,omitempty"`
	// Relays are the tunnelled links (--relay), opened before the stage that dials them starts
	Relays []StageRelay `json:"relays,omitempty"`
}

// StageRelay is the link from stage FromStage to the next one over ssh tunnels through Via (this machine).
type StageRelay struct {
	config.Relay
	FromStage int `json:"from_stage"`
	ToPort    int `json:"to_port"` // the next stage's own port on its machine
}

// relayBasePort is the first tunnel port (on Via and on the stage that dials it): clear of the stage and ingress ports.
const relayBasePort = 53100

var (
	dflashCacheDir string
	embedHeadFn    = ggufhead.EmbedHead
)

func getDFlashCacheDir() (string, error) {
	if dflashCacheDir != "" {
		return dflashCacheDir, nil
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(home, ".cache", "fleet", "dflash"), nil
}

// ensureCachedHead embeds the target's LM head into the draft once and keeps the result in cacheDir ("": the user's
// ~/.cache/fleet/dflash). --auto passes a directory with the same path on every machine, so the master can load it.
func ensureCachedHead(draftPath, targetPath, cacheDir string) (string, error) {
	draftFi, err := os.Stat(draftPath)
	if err != nil {
		return "", fmt.Errorf("stat draft: %w", err)
	}
	targetFi, err := os.Stat(targetPath)
	if err != nil {
		return "", fmt.Errorf("stat target model: %w", err)
	}
	if cacheDir == "" {
		cacheDir, err = getDFlashCacheDir()
		if err != nil {
			return "", fmt.Errorf("cache dir: %w", err)
		}
	}
	if err := os.MkdirAll(cacheDir, 0o755); err != nil {
		return "", fmt.Errorf("mkdir cache dir: %w", err)
	}

	draftID := fmt.Sprintf("%s:%d:%d", draftPath, draftFi.Size(), draftFi.ModTime().UnixNano())
	targetID := fmt.Sprintf("%s:%d:%d", targetPath, targetFi.Size(), targetFi.ModTime().UnixNano())
	h := sha256.Sum256([]byte(draftID + "|" + targetID))
	cachePath := filepath.Join(cacheDir, fmt.Sprintf("%x-head.gguf", h[:16]))

	if fi, err := os.Stat(cachePath); err == nil && fi.Size() > 0 {
		return cachePath, nil
	}

	if _, err := embedHeadFn(draftPath, targetPath, cachePath); err != nil {
		return "", fmt.Errorf("embed LM head: %w", err)
	}
	return cachePath, nil
}

// trimNextNTail drops the NextN / MTP blocks from the end of the last part. A GGUF such as Qwen3.5 MoE counts them in
// block_count (49 = 48 + 1 head), the planners and the even split cut that whole count, but the stage engine loads
// only the trunk and rejects a range past it ("invalid layer range [25, 49) for a model with 48 layers"). Parts that
// do not add up to the full count yield the zero Candidate (nothing trimmed) and meet the check that follows.
func trimNextNTail(c plan.Candidate, m *plan.Model) plan.Candidate {
	if m == nil || m.MTPLayers <= 0 || len(c.Parts) == 0 || partLayers(c) != m.Layers {
		return plan.Candidate{}
	}
	parts := append([]plan.Part(nil), c.Parts...)
	drop := m.MTPLayers
	for drop > 0 && len(parts) > 1 {
		last := &parts[len(parts)-1]
		if last.Layers > drop {
			last.Layers -= drop
			drop = 0
			break
		}
		drop -= last.Layers
		parts = parts[:len(parts)-1]
		parts[len(parts)-1].Output = true
	}
	if drop > 0 && parts[0].Layers > drop {
		parts[0].Layers -= drop
	}
	c.Parts = parts
	return c
}

func partLayers(c plan.Candidate) int {
	n := 0
	for _, p := range c.Parts {
		n += p.Layers
	}
	return n
}

// foreignRunPorts: the ports (per machine, canonical node) that the active deploys of OTHER endpoints use, from the run
// records. A port is not probed on the machine: what a deploy replaces in place (the same endpoint name; the start script
// stops its own engine on the port) must not count as taken, and a foreign process on the port is the start script's
// FLEET_BUSY. Replaceable by tests.
var foreignRunPorts = recordedForeignPorts

func canonicalNode(node string) string {
	if defaultIsThisMachine(node) {
		return ":local:"
	}
	return strings.TrimSpace(node)
}

// PlanStagePipe turns a split candidate into stage launches. The candidate's parts, in order, are the stages
// (the planner lists the machine that takes requests first); they must cut the model's layers into
// contiguous ranges from 0.
func PlanStagePipe(c plan.Candidate, m *plan.Model, ad StagePipeAdapter, o StagePipeOptions) (*StagePipePlan, error) {
	if m == nil || m.Layers <= 0 {
		return nil, fmt.Errorf("stage-pipe: the model has no layers")
	}
	if len(c.Parts) == 0 {
		return nil, fmt.Errorf("stage-pipe: the candidate has no parts")
	}
	wire := strings.ToLower(strings.TrimSpace(o.Wire))
	// fleet-engine-stage --wire knows f32, bf16 and auto (f32 for hyper-connection models); anything else it would treat as bf16
	if wire != "" && wire != "f32" && wire != "bf16" && wire != "auto" {
		return nil, fmt.Errorf("stage-pipe: invalid wire format %q (expected f32, bf16 or auto)", o.Wire)
	}
	if o.Ctx < 0 {
		return nil, fmt.Errorf("stage-pipe: --ctx must be >= 0")
	}
	if o.Np < 0 {
		return nil, fmt.Errorf("stage-pipe: --np must be >= 0")
	}
	if o.Lanes < 0 {
		return nil, fmt.Errorf("stage-pipe: --lanes must be >= 0")
	}
	if o.Lanes > 8 {
		return nil, fmt.Errorf("stage-pipe: --lanes %d: maximum is 8", o.Lanes)
	}
	if o.Lanes > 1 && o.Np < 2 {
		return nil, fmt.Errorf("stage-pipe: --lanes needs --np >= 2")
	}

	spec := strings.ToLower(strings.TrimSpace(o.Spec))
	isDFlash := spec == "draft-dflash" || strings.HasPrefix(spec, "draft-dflash") || (spec != "none" && o.DraftModel != "")
	if o.Lanes > 1 && isDFlash {
		return nil, fmt.Errorf("stage-pipe: --lanes %d with speculative decoding: its rounds do not batch (use --lanes 1; --np N still serves N requests, one draft per sequence)", o.Lanes)
	}
	if o.MMProj != "" {
		if isDFlash {
			return nil, fmt.Errorf("stage-pipe: --mmproj cannot be combined with speculative decoding (the draft has no image context)")
		}
		if ad.StageBin != "fleet-engine-stage" {
			return nil, fmt.Errorf("stage-pipe: --mmproj is only supported by the fleet-engine chain (CSP1)")
		}
	}
	if (spec == "draft-dflash" || strings.HasPrefix(spec, "draft-dflash")) && o.DraftModel == "" {
		return nil, fmt.Errorf("stage-pipe: --spec draft-dflash requires a draft model (--draft-model)")
	}
	if o.DraftMax < 0 {
		return nil, fmt.Errorf("stage-pipe: --draft-max must be >= 1")
	}
	if isDFlash && (ad.StageBin != "fleet-engine-stage") {
		return nil, fmt.Errorf("stage-pipe: draft-dflash is not supported with this protocol")
	}
	if o.Lanes > 1 && (ad.StageBin != "fleet-engine-stage") {
		return nil, fmt.Errorf("stage-pipe: --lanes is not supported with this protocol")
	}

	hasMoE := o.CPUMoE || o.NCPUMoE > 0 || o.MoECacheMiB > 0
	if o.CPUMoE && o.NCPUMoE > 0 {
		return nil, fmt.Errorf("stage-pipe: --cpu-moe and --n-cpu-moe cannot be used together")
	}
	if o.NCPUMoE < 0 {
		return nil, fmt.Errorf("stage-pipe: --n-cpu-moe cannot be negative")
	}
	if o.MoECacheMiB < 0 {
		return nil, fmt.Errorf("stage-pipe: --moe-cache-mib cannot be negative")
	}
	if o.MoECacheMiB > 0 && !o.CPUMoE && o.NCPUMoE == 0 {
		return nil, fmt.Errorf("stage-pipe: --moe-cache-mib requires --cpu-moe or --n-cpu-moe")
	}
	if hasMoE && (ad.StageBin != "fleet-engine-stage") {
		return nil, fmt.Errorf("stage-pipe: MoE options are not supported in this protocol")
	}
	moeArgsFor := func(firstLayer int) []string {
		var a []string
		if o.CPUMoE {
			a = append(a, "--cpu-moe")
		} else if o.NCPUMoE > 0 {
			a = append(a, "--n-cpu-moe", fmt.Sprint(o.NCPUMoE))
		}
		cache := o.MoECacheMiB
		if v, ok := o.StageMoECacheMiB[firstLayer]; ok && (o.CPUMoE || o.NCPUMoE > 0) {
			cache = v
		}
		if cache > 0 {
			a = append(a, "--moe-cache-mib", fmt.Sprint(cache))
		}
		return a
	}
	// The chain has no machine that only takes requests: stage 0 runs layer 0 and is where requests enter. A master the
	// plan gave no layers (the ring engine allows it) is left out; the first machine that holds layers takes its place.
	var skippedMaster string
	if len(c.Parts) > 1 && c.Parts[0].Layers <= 0 {
		i := 0
		for i < len(c.Parts)-1 && c.Parts[i].Layers <= 0 {
			i++
		}
		skippedMaster = c.Parts[0].Node
		c.Parts = append([]plan.Part(nil), c.Parts[i:]...)
		c.Master = c.Parts[0].Node
	}
	wantLayers := m.Layers
	if t := trimNextNTail(c, m); t.Parts != nil {
		wantLayers -= partLayers(c) - partLayers(t)
		c = t
	}
	next := 0
	for i, p := range c.Parts {
		if p.Layers <= 0 {
			return nil, fmt.Errorf("stage-pipe: part %d (%s) holds no layers", i, p.Node)
		}
		if p.First != next {
			return nil, fmt.Errorf("stage-pipe: part %d (%s) starts at layer %d, expected %d: the parts must follow each other", i, p.Node, p.First, next)
		}
		next += p.Layers
	}
	if next != wantLayers {
		return nil, fmt.Errorf("stage-pipe: the parts hold %d layers, the model has %d", next, wantLayers)
	}
	last := len(c.Parts) - 1
	if len(c.Parts) > 1 && c.Parts[0].Output {
		return nil, fmt.Errorf("stage-pipe: the output head is on the first stage; the runtime runs it on the last")
	}
	if len(c.Parts) > 1 && !c.Parts[last].Output {
		return nil, fmt.Errorf("stage-pipe: the last part (%s) does not hold the output head", c.Parts[last].Node)
	}
	host := o.Host
	if host == nil {
		host = func(n string) string { return n }
	}
	dir := o.ModelDir
	if dir == "" {
		dir = strings.TrimRight(ad.ModelRoot, "/") + "/" + m.Name
	}

	stageDir := dir
	if ad.StageModelSub != "" {
		stageDir = strings.TrimRight(dir, "/") + "/" + ad.StageModelSub
	}

	var targetLayersCSV string
	var effectiveDraft string
	draftMax := o.DraftMax
	if isDFlash {
		if draftMax <= 0 {
			if o.CPUMoE || o.NCPUMoE > 0 {
				draftMax = 2
			} else {
				draftMax = 4
			}
		}
		if _, err := os.Stat(o.DraftModel); err != nil {
			return nil, fmt.Errorf("stage-pipe: draft model %s: %w", o.DraftModel, err)
		}
		dmeta, err := ggufmeta.Read(o.DraftModel)
		if err != nil {
			return nil, fmt.Errorf("stage-pipe: read draft metadata %s: %w", o.DraftModel, err)
		}
		if dmeta.Arch != "dflash" {
			return nil, fmt.Errorf("stage-pipe: draft model %s: architecture %q, expected dflash", o.DraftModel, dmeta.Arch)
		}
		if len(dmeta.TargetLayers) == 0 {
			return nil, fmt.Errorf("stage-pipe: draft model %s has no dflash.target_layers metadata", o.DraftModel)
		}
		targetLayersCSV = dmeta.TargetLayersCSV()

		if len(c.Parts) > 1 {
			hasHead, err := ggufhead.HasHead(o.DraftModel)
			if err != nil {
				return nil, fmt.Errorf("stage-pipe: check draft head: %w", err)
			}
			if hasHead {
				effectiveDraft = o.DraftModel
			} else {
				headTarget := dir
				if o.HeadTarget != "" {
					headTarget = o.HeadTarget
				}
				headPath, err := ensureCachedHead(o.DraftModel, headTarget, o.HeadDir)
				if err != nil {
					return nil, fmt.Errorf("stage-pipe: ensure cached head: %w", err)
				}
				effectiveDraft = headPath
			}
		} else {
			effectiveDraft = o.DraftModel
		}
	}

	timeout := o.TimeoutS
	if timeout <= 0 {
		timeout = ad.TimeoutS
	}
	p := &StagePipePlan{Model: m.Name, ModelDir: dir, Layers: m.Layers, Draft: effectiveDraft}
	// the port each stage is reached on: the first serves requests, the others take stage ports, a second
	// stage on the same machine the next port
	ports := make([]int, len(c.Parts))
	reserved := make(map[string]map[int]bool)
	markReserved := func(node string, port int) {
		key := canonicalNode(node)
		if reserved[key] == nil {
			reserved[key] = make(map[int]bool)
		}
		reserved[key][port] = true
	}
	who := o.EndpointName
	if who == "" {
		who = o.planName
	}
	foreign := foreignRunPorts(who)
	foreignTaken := func(node string, port int) bool { return foreign[canonicalNode(node)][port] }
	isTaken := func(node string, port int) bool {
		key := canonicalNode(node)
		return (reserved[key] != nil && reserved[key][port]) || foreignTaken(node, port)
	}

	// Ingress port (stage 0):
	ingressNode := c.Parts[0].Node
	manualIngress := o.Port > 0
	ingressPort := ad.IngressPort
	if ad.IngressPortFixed {
		if manualIngress && o.Port != ad.IngressPort {
			return nil, fmt.Errorf("stage-pipe: this runtime's first stage serves on port %d only (--port %d is not possible)", ad.IngressPort, o.Port)
		}
		if isTaken(ingressNode, ingressPort) {
			return nil, fmt.Errorf("stage-pipe: port %d (fixed for this runtime) is already in use on %s", ingressPort, ingressNode)
		}
	} else if manualIngress {
		ingressPort = o.Port
		if isTaken(ingressNode, ingressPort) {
			return nil, fmt.Errorf("stage-pipe: port %d is already in use on %s", ingressPort, ingressNode)
		}
	} else {
		for isTaken(ingressNode, ingressPort) {
			ingressPort++
			if ingressPort > 65535 {
				return nil, fmt.Errorf("stage-pipe: no free ingress port available on %s", ingressNode)
			}
		}
	}
	ports[0] = ingressPort
	markReserved(ingressNode, ingressPort)

	// Stage ports (stages 1..last):
	manualStage := o.StagePort > 0 || (ad.StagePort > 0 && ad.StagePort != 5600)
	baseStagePort := ad.StagePort
	if o.StagePort > 0 {
		baseStagePort = o.StagePort
	}
	for i := 1; i < len(c.Parts); i++ {
		part := c.Parts[i]
		node := part.Node
		nodeKey := canonicalNode(node)
		candPort := baseStagePort
		for {
			if reserved[nodeKey] != nil && reserved[nodeKey][candPort] {
				candPort++
				continue
			}
			if foreignTaken(node, candPort) {
				if manualStage && candPort == baseStagePort {
					return nil, fmt.Errorf("stage-pipe: stage port %d is already in use on %s", candPort, node)
				}
				candPort++
				if candPort > 65535 {
					return nil, fmt.Errorf("stage-pipe: no free stage port available on %s", node)
				}
				continue
			}
			break
		}
		ports[i] = candPort
		markReserved(node, candPort)
	}
	for i, part := range c.Parts {
		st := StageLaunch{Index: i, Node: part.Node, Host: host(part.Node), Begin: part.First, End: part.First + part.Layers, Port: ports[i]}
		switch {
		case i == 0:
			st.Role = "ingress"
		case i == last:
			st.Role = "last"
		default:
			st.Role = "middle"
		}
		if ad.LayersEnv != "" {
			st.Env = append(st.Env, ad.LayersEnv+"="+fmt.Sprintf("%d:%d", st.Begin, st.End))
		}
		if i < last {
			nextHost, nextPort := host(c.Parts[i+1].Node), ports[i+1]
			if (o.Relay || o.ViaHub) && !sameMachine(part.Node, c.Parts[i+1].Node) {
				// the next stage is dialled on a tunnel port of this stage's own machine (opened by RunStagePipe)
				via := LocalHostname()
				viaKey := canonicalNode(via)
				fromKey := canonicalNode(part.Node)
				relayPort := relayBasePort
				for (reserved[viaKey] != nil && reserved[viaKey][relayPort]) ||
					(reserved[fromKey] != nil && reserved[fromKey][relayPort]) ||
					foreignTaken(via, relayPort) || foreignTaken(part.Node, relayPort) {
					relayPort++
					if relayPort > 65535 {
						return nil, fmt.Errorf("stage-pipe: no free relay port available between %s and %s", via, part.Node)
					}
				}
				markReserved(via, relayPort)
				markReserved(part.Node, relayPort)

				nextHost, nextPort = "127.0.0.1", relayPort
				p.Relays = append(p.Relays, StageRelay{
					Relay:     config.Relay{Via: via, From: relayNode(via, part.Node), To: relayNode(via, c.Parts[i+1].Node), Port: nextPort},
					FromStage: i, ToPort: ports[i+1],
				})
				if o.ViaHub {
					p.Relays[len(p.Relays)-1].Hub = o.HubURL
				}
			}
			st.Next = fill(ad.NextFmt, "{host}", nextHost, "{port}", fmt.Sprint(nextPort))
			if ad.NextEnv != "" {
				st.Env = append(st.Env, ad.NextEnv+"="+st.Next)
			}
			if ad.TimeoutEnv != "" {
				st.Env = append(st.Env, ad.TimeoutEnv+"="+fmt.Sprint(timeout))
			}
		}
		kv := []string{
			"{model}", dir,
			"{stage_model}", stageDir,
			"{port}", fmt.Sprint(st.Port),
			"{begin}", fmt.Sprint(st.Begin),
			"{end}", fmt.Sprint(st.End),
			"{next}", st.Next,
		}
		if i == 0 {
			if o.EmbeddingOffload && ad.EmbeddingOffloadEnv != "" {
				st.Env = append(st.Env, ad.EmbeddingOffloadEnv+"=on")
			}
			ingArgs := append([]string{}, ad.IngressArgs...)
			ingArgs = append(ingArgs, moeArgsFor(st.Begin)...)
			if isDFlash {
				ingArgs = append(ingArgs, "--dflash-draft", effectiveDraft, "--draft-max", fmt.Sprint(draftMax))
			}
			if o.MMProj != "" {
				ingArgs = append(ingArgs, "--mmproj", o.MMProj)
			}
			if o.Ctx > 0 {
				ingArgs = append(ingArgs, "-c", fmt.Sprint(o.Ctx))
			}
			if o.Np > 1 {
				ingArgs = append(ingArgs, "--n-seq", fmt.Sprint(o.Np))
			}
			if o.Lanes > 1 {
				ingArgs = append(ingArgs, "--lanes", fmt.Sprint(o.Lanes))
			}
			if wire != "" {
				ingArgs = append(ingArgs, "--wire", wire)
			}
			st.Dir = dir
			st.Command = commandLine(st.Env, ad.IngressBin, ingArgs, kv...)
			if ad.HealthPath != "" {
				st.Ready = Readiness{Kind: "http", Addr: fmt.Sprintf("%s:%d", st.Host, st.Port), Path: ad.HealthPath}
			} else {
				st.Ready = Readiness{Kind: "tcp", Addr: fmt.Sprintf("%s:%d", st.Host, st.Port)}
			}
		} else {
			listen := fill(ad.ListenFmt, "{port}", fmt.Sprint(st.Port))
			stgArgs := append([]string{}, ad.StageArgs...)
			stgArgs = append(stgArgs, moeArgsFor(st.Begin)...)
			if isDFlash {
				stgArgs = append(stgArgs, "--dflash-layers", targetLayersCSV)
				if draftMax > 4 {
					stgArgs = append(stgArgs, "--n-rs-seq", fmt.Sprint(draftMax))
				}
			}
			if o.Ctx > 0 {
				stgArgs = append(stgArgs, "-c", fmt.Sprint(o.Ctx))
			}
			if o.Np > 1 {
				stgArgs = append(stgArgs, "--n-seq", fmt.Sprint(o.Np))
			}
			if wire != "" {
				stgArgs = append(stgArgs, "--wire", wire)
			}
			st.Dir = stageDir
			st.Command = commandLine(st.Env, ad.StageBin, stgArgs, append(kv, "{listen}", listen)...)
			st.Ready = Readiness{Kind: "tcp", Addr: fmt.Sprintf("%s:%d", st.Host, st.Port)}
		}
		p.Stages = append(p.Stages, st)
	}
	for i := last; i >= 0; i-- {
		p.StartOrder = append(p.StartOrder, i)
	}
	if skippedMaster != "" {
		p.Warnings = append(p.Warnings, fmt.Sprintf("the chain engine has no master without layers: %s holds none, so requests enter at %s, the first stage (the ring engine can keep %s as a layerless master)", skippedMaster, c.Parts[0].Node, skippedMaster))
	}
	if ad.Provisional {
		p.Warnings = append(p.Warnings, "PROVISIONAL: stage env names, the stage-server arguments and the fixed web port were seen on a board (2026-11-10); the first stage with STAGE_NEXT, readiness by TCP and embedding offload are not verified")
	}
	if m.Experts > 0 && !hasMoE {
		p.Warnings = append(p.Warnings, "MoE model without MoE flags: if a stage's layer share exceeds node VRAM, --cpu-moe --moe-cache-mib N is recommended")
	}
	return p, nil
}

func fill(s string, kv ...string) string { return strings.NewReplacer(kv...).Replace(s) }

func commandLine(env []string, bin string, args []string, kv ...string) string {
	parts := append([]string{}, env...)
	for i, e := range parts {
		k, v, _ := strings.Cut(e, "=")
		parts[i] = k + "=" + shellQuote(v)
	}
	parts = append(parts, bin)
	for _, a := range args {
		parts = append(parts, shellQuote(fill(a, kv...)))
	}
	return strings.Join(parts, " ")
}

// shellQuote leaves plain words alone and single-quotes the rest.
func shellQuote(s string) string {
	if s != "" && strings.IndexFunc(s, func(r rune) bool {
		return !(r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r >= '0' && r <= '9' || strings.ContainsRune("_-.,:/=@%+", r))
	}) < 0 {
		return s
	}
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}

// StageRunner does what a start plan asks of one machine. The planner and this file never touch a machine;
// whoever implements this does.
type StageRunner interface {
	Cleanup(node string, port int) error // free a stale process on the port
	Launch(s StageLaunch) error
	WaitReady(s StageLaunch, timeout time.Duration) error
	Exists(node, path string) (bool, error) // check whether the model file exists on the stage machine
}

// RunStagePipe starts the stages from the last to the first, each one only after the one behind it answers.
// On the first error it stops and says how far it got; the caller decides what to clean up.
func RunStagePipe(p *StagePipePlan, r StageRunner, ready time.Duration) error {
	ro, _ := r.(relayOpener)
	if len(p.Relays) > 0 && ro == nil {
		return fmt.Errorf("--relay: this runner cannot open ssh tunnels")
	}
	var opened []StageRelay
	closeRelays := func() {
		for _, rel := range opened {
			ro.CloseRelay(rel)
		}
	}
	for n, i := range p.StartOrder {
		s := p.Stages[i]
		if err := r.Cleanup(s.Node, s.Port); err != nil {
			return fmt.Errorf("stage %d (%s): cleaning port %d: %w (%d of %d started)", i, s.Node, s.Port, err, n, len(p.StartOrder))
		}
		// the link this stage dials goes over tunnels: opened now, the stage behind it answers already
		for _, rel := range p.Relays {
			if rel.FromStage != i {
				continue
			}
			opened = append(opened, rel) // closed again on failure even when only half open
			if err := ro.OpenRelay(rel); err != nil {
				stopStarted(p, r, n)
				closeRelays()
				return fmt.Errorf("stage %d (%s): %w (%d of %d started, stopped again)", i, s.Node, err, n, len(p.StartOrder))
			}
		}
		if err := r.Launch(s); err != nil {
			stopStarted(p, r, n)
			closeRelays()
			return fmt.Errorf("stage %d (%s): start: %w (%d of %d started, stopped again)", i, s.Node, err, n, len(p.StartOrder))
		}
		if err := r.WaitReady(s, ready); err != nil {
			stopStarted(p, r, n+1)
			closeRelays()
			return fmt.Errorf("stage %d (%s): not ready: %w (%d of %d started, stopped again)", i, s.Node, err, n+1, len(p.StartOrder))
		}
	}
	return nil
}

// stopStarted stops the first n stages of the start order after a failed deploy: a half pipe holds GPU memory and
// its ports, and no run record points at it (it was left running on dino after a not-ready stage, E10).
func stopStarted(p *StagePipePlan, r StageRunner, n int) {
	for _, i := range p.StartOrder[:n] {
		_ = r.Cleanup(p.Stages[i].Node, p.Stages[i].Port)
	}
}

// DryRunner records what would be done and touches nothing.
type DryRunner struct {
	Lines      []string
	FakeExists func(node, path string) bool
}

func (d *DryRunner) Cleanup(node string, port int) error {
	d.Lines = append(d.Lines, fmt.Sprintf("[%s] free port %d", node, port))
	return nil
}

func (d *DryRunner) Launch(s StageLaunch) error {
	d.Lines = append(d.Lines, fmt.Sprintf("[%s] start stage %d (%s, layers %d:%d): %s", s.Node, s.Index, s.Role, s.Begin, s.End, s.Command))
	return nil
}

func (d *DryRunner) WaitReady(s StageLaunch, timeout time.Duration) error {
	what := "tcp connect " + s.Ready.Addr
	if s.Ready.Kind == "http" {
		what = "GET http://" + s.Ready.Addr + s.Ready.Path
	}
	d.Lines = append(d.Lines, fmt.Sprintf("[%s] wait up to %s for: %s", s.Node, timeout, what))
	return nil
}

func (d *DryRunner) Exists(node, path string) (bool, error) {
	d.Lines = append(d.Lines, fmt.Sprintf("[%s] check model exists: test -f %s", node, path))
	if d.FakeExists != nil {
		return d.FakeExists(node, path), nil
	}
	return true, nil
}

// LiveStageRunner starts pipeline stages on local or remote (SSH) machines.
type LiveStageRunner struct {
	Started  []StageLaunch
	Warnings []string // things a step wants the caller to show (a hub stream port that could not be declared)
	// pids: the process of each started stage ("node:port"), for the memory guard of its load (the port only opens
	// once the weights are in, so it cannot be found by its port while it loads)
	pids map[string]int
}

func stageKey(node string, port int) string { return node + ":" + strconv.Itoa(port) }

func (r *LiveStageRunner) isLocal(node string) bool {
	return defaultIsThisMachine(node)
}

// stageProcNames are the processes a stage port may belong to: the built-in ones and those of every stage module.
func stageProcNames() []string {
	names := []string{"fleet-engine-stage", "fleet-engine", "llama-server", "fleet"}
	seen := map[string]bool{}
	for _, n := range names {
		seen[n] = true
	}
	for _, mn := range StageModuleNames() {
		m, _ := LookupStageModule(mn)
		for _, b := range []string{m.Adapter.StageBin, m.Adapter.IngressBin} {
			if b != "" && !seen[b] {
				seen[b] = true
				names = append(names, b)
			}
		}
	}
	return names
}

func (r *LiveStageRunner) Cleanup(node string, port int) error {
	cmdStr := stopOnPortScript(port, stageProcNames()) + "fleet_stop_port\n"
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	var out []byte
	if r.isLocal(node) {
		out, _ = localScript(ctx, cmdStr)
	} else {
		out, _ = sshScript(ctx, node, cmdStr)
	}
	// a port still held (another user's process, something that is not a stage) would make the new stage die on bind
	return busyError(node, port, string(out))
}

func (r *LiveStageRunner) Launch(s StageLaunch) error {
	logFile := fmt.Sprintf("/tmp/fleet-stage-%d.log", s.Port)
	// env: the command starts with VAR=value assignments, which nohup would take for the program name; a stage
	// that exits at once (missing binary, bad flag, model not found) is reported now with its log, not after the
	// ready timeout
	lf := shellQuote(logFile)
	// ~/.local/bin: where `fleet engine install` puts fleet and its fleet-engine-stage link (a non-interactive ssh shell lacks it)
	// $FLEET_DETACH (detachShell): the stage leaves the cgroup of the fleet service that may have started it, else
	// a restart of that daemon (fleet update, systemctl restart) killed a loaded 122B stage with it
	bgCmd := fmt.Sprintf("export PATH=$HOME/.local/bin:$PATH\n%snohup $FLEET_DETACH env %s > %s 2>&1 < /dev/null &\nPID=$!\nsleep 2\nif kill -0 $PID 2>/dev/null; then echo FLEET_STARTED; echo FLEET_PID $PID; else echo FLEET_DIED; tail -n 20 %s; fi\n", detachShell, s.Command, lf, lf)
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	var out []byte
	var err error
	if r.isLocal(s.Node) {
		if out, err = localScript(ctx, bgCmd); err != nil {
			return fmt.Errorf("local start stage %d: %w", s.Index, err)
		}
	} else {
		if out, err = sshScript(ctx, s.Node, bgCmd); err != nil {
			return fmt.Errorf("ssh %s start stage %d: %w", s.Node, s.Index, err)
		}
	}
	if !strings.Contains(string(out), "FLEET_STARTED") {
		return fmt.Errorf("stage %d on %s exited at start (log %s on that machine):\n%s", s.Index, s.Node, logFile, strings.TrimSpace(string(out)))
	}
	r.Started = append(r.Started, s)
	if pid := parsePID(string(out)); pid > 0 {
		if r.pids == nil {
			r.pids = map[string]int{}
		}
		r.pids[stageKey(s.Node, s.Port)] = pid
	}
	return nil
}

// parsePID reads "FLEET_PID n" from a launch script's output (0: none).
func parsePID(out string) int {
	for _, ln := range strings.Split(out, "\n") {
		if v, ok := strings.CutPrefix(strings.TrimSpace(ln), "FLEET_PID "); ok {
			n, _ := strconv.Atoi(strings.TrimSpace(v))
			return n
		}
	}
	return 0
}

func (r *LiveStageRunner) WaitReady(s StageLaunch, timeout time.Duration) error {
	// a stage on another machine may listen on an address this one cannot reach (another site: its LAN address is
	// not routed here, E10): after one direct try the check runs on that machine over ssh
	pid := r.pids[stageKey(s.Node, s.Port)]
	if !r.isLocal(s.Node) && !directReady(s) {
		return remoteWaitReadyGuarded(s, timeout, pid)
	}
	probe := s
	if r.isLocal(s.Node) {
		// a stage of this machine is asked on loopback: the configured address may be stale (a DHCP lease that
		// changed made a ready stage look dead for 20 minutes, and the deploy then stopped it)
		probe = loopbackReady(s)
	}
	deadline := time.Now().Add(timeout)
	guard := newMemGuard()
	lastGuard := time.Time{}
	for time.Now().Before(deadline) {
		if directReady(probe) {
			return nil
		}
		if r.isLocal(s.Node) && pid > 0 && time.Since(lastGuard) >= time.Second {
			lastGuard = time.Now()
			if syscall.Kill(pid, 0) != nil {
				return fmt.Errorf("stage %d on %s exited while loading (log /tmp/fleet-stage-%d.log):\n%s", s.Index, s.Node, s.Port, stageLogTail(s))
			}
			if why := guard.check(readLocalMem()); why != "" {
				killPID(pid)
				return fmt.Errorf("stage %d on %s was stopped while loading: %s (memory guard; a model that does not fit locks a machine up instead of failing)", s.Index, s.Node, why)
			}
		}
		time.Sleep(readyPoll)
	}
	return fmt.Errorf("timeout (%s) waiting for %s to become ready on %s", timeout, s.Ready.Kind, s.Ready.Addr)
}

// ingressProber is a StageRunner that can tell whether this machine reaches a stage at its registered address (the
// live runner; the dry and test runners do not probe, so their endpoints keep the direct URL).
type ingressProber interface {
	DirectlyReachable(s StageLaunch) bool
}

// DirectlyReachable: a stage of this machine always; another machine's when its ready check answers from here.
func (r *LiveStageRunner) DirectlyReachable(s StageLaunch) bool {
	return r.isLocal(s.Node) || directReady(s)
}

// directReady is one readiness probe of s from this machine.
func directReady(s StageLaunch) bool {
	if s.Ready.Kind == "http" {
		resp, err := (&http.Client{Timeout: 3 * time.Second}).Get(fmt.Sprintf("http://%s%s", s.Ready.Addr, s.Ready.Path))
		if err != nil {
			return false
		}
		resp.Body.Close()
		return resp.StatusCode == http.StatusOK
	}
	conn, err := net.DialTimeout("tcp", s.Ready.Addr, 2*time.Second)
	if err != nil {
		return false
	}
	conn.Close()
	return true
}

// remoteWaitReady waits for s on its own machine (one ssh session, a check a second).
func remoteWaitReady(s StageLaunch, timeout time.Duration) error {
	return remoteWaitReadyGuarded(s, timeout, 0)
}

// remoteWaitReadyGuarded is remoteWaitReady with the memory guard of a loading stage (pid > 0): the check loop on the
// stage's machine stops it when that machine runs out of memory (memGuardScript).
func remoteWaitReadyGuarded(s StageLaunch, timeout time.Duration, pid int) error {
	secs := int(timeout / time.Second)
	if secs < 1 {
		secs = 1
	}
	ctx, cancel := context.WithTimeout(context.Background(), timeout+20*time.Second)
	defer cancel()
	out, err := sshScript(ctx, s.Node, stageReadyScriptGuarded(s, secs, pid))
	if err != nil {
		return fmt.Errorf("ssh %s readiness check: %w", s.Node, err)
	}
	if strings.Contains(string(out), "FLEET_READY") {
		return nil
	}
	if strings.Contains(string(out), "FLEET_EXITED") {
		return fmt.Errorf("stage %d on %s exited while loading (log /tmp/fleet-stage-%d.log there):\n%s", s.Index, s.Node, s.Port, stageLogTail(s))
	}
	if i := strings.Index(string(out), "FLEET_OOM "); i >= 0 {
		why := strings.TrimSpace(strings.SplitN(string(out)[i+len("FLEET_OOM "):], "\n", 2)[0])
		return fmt.Errorf("stage %d on %s was stopped while loading: %s (memory guard; a model that does not fit locks a machine up instead of failing)", s.Index, s.Node, why)
	}
	return fmt.Errorf("timeout (%s) waiting for %s to become ready on %s (checked on %s)", timeout, s.Ready.Kind, s.Ready.Addr, s.Node)
}

// stageReadyScript checks s on its machine through 127.0.0.1 (stages listen on all addresses): http by its health
// path (curl, else wget; with neither, a listening port counts), tcp by a listening port.
func stageReadyScript(s StageLaunch, secs int) string { return stageReadyScriptGuarded(s, secs, 0) }

func stageReadyScriptGuarded(s StageLaunch, secs int, pid int) string {
	port := s.Port
	if _, ps, err := net.SplitHostPort(s.Ready.Addr); err == nil {
		if n, err := strconv.Atoi(ps); err == nil {
			port = n
		}
	}
	listening := fmt.Sprintf(`{ ss -ltn 2>/dev/null || netstat -ltn 2>/dev/null; } | awk '{print $4}' | grep -Eq '[:.]%d$'`, port)
	check := listening
	if s.Ready.Kind == "http" {
		url := shellQuote(fmt.Sprintf("http://127.0.0.1:%d%s", port, s.Ready.Path))
		check = fmt.Sprintf(`if command -v curl >/dev/null 2>&1; then curl -fsS -o /dev/null --max-time 3 %[1]s 2>/dev/null; `+
			`elif command -v wget >/dev/null 2>&1; then wget -q -O /dev/null -T 3 %[1]s 2>/dev/null; else %[2]s; fi`, url, listening)
	}
	guard := ""
	if pid > 0 {
		guard = memGuardScript(pid)
	}
	return fmt.Sprintf(`i=0
while :; do
  if %s; then echo FLEET_READY; exit 0; fi
  %s
  i=$((i+1))
  if [ $i -ge %d ]; then echo FLEET_TIMEOUT; exit 0; fi
  sleep 1
done
`, check, guard, secs)
}

func (r *LiveStageRunner) Exists(node, path string) (bool, error) {
	if r.isLocal(node) {
		_, err := os.Stat(path)
		if err == nil {
			return true, nil
		}
		if os.IsNotExist(err) {
			return false, nil
		}
		return false, err
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	cmd := fmt.Sprintf("if test -f %s; then echo FLEET_EXISTS; fi\n", shellQuote(path))
	out, err := sshScript(ctx, node, cmd)
	if err != nil {
		return false, err
	}
	return strings.Contains(string(out), "FLEET_EXISTS"), nil
}

// DeployChain deploys a model across pipeline stages using Chain Stage Protocol v1 (CSP1).
// It converts a planner candidate and model into stage commands, launches them in reverse order
// (last stage to ingress), waits for readiness, records the run, and returns the DeployResult.
func DeployChain(ctx context.Context, cand plan.Candidate, modelPath string, opts StagePipeOptions, runner StageRunner, rec RunRecorder) (out *DeployResult, retErr error) {
	if rec == nil {
		rec = DefaultRunRecorder{}
	}
	if opts.Relay && opts.ViaHub {
		return nil, fmt.Errorf("deploy-chain: --relay (ssh tunnels) and --via-hub (the hub's stream mode) do the same job: pick one")
	}
	if modelPath == "" {
		return nil, fmt.Errorf("deploy-chain: model path is required")
	}
	m, err := plan.LoadModel(modelPath)
	if err != nil {
		return nil, fmt.Errorf("deploy-chain: load model: %w", err)
	}
	if opts.Host == nil {
		opts.Host = ResolveStageHost
	}
	if opts.ModelDir == "" {
		opts.ModelDir = modelPath
	}
	if opts.ViaHub {
		if opts.HubURL == "" {
			cfg, _ := config.Load(configPathOr(opts.ConfigPath))
			if cfg != nil {
				opts.HubURL = cfg.HubURL
			}
		}
		var err error
		if opts.HubURL, err = config.HubOrDefault(opts.HubURL); err != nil {
			return nil, err
		}
		if !hubShellSafe(opts.HubURL) {
			return nil, fmt.Errorf("hub URL %q has characters fleet does not put into a shell line", opts.HubURL)
		}
	}
	var autoNotes []string
	// --auto works on a GGUF file; a descriptor directory is left as it is
	autoOn := false
	if fi, serr := os.Stat(modelPath); opts.Auto && serr == nil && fi.Mode().IsRegular() {
		autoOn = true
		// before the plan: it builds every stage line from the model path and the options
		common, notes, err := ResolveCommonModelPath(ctx, modelPath, chainNodes(cand), opts.DryRun)
		if err != nil {
			return nil, fmt.Errorf("deploy-chain: %w", err)
		}
		autoNotes = append(autoNotes, notes...)
		if opts.ModelDir == modelPath {
			opts.ModelDir = common
		}
		opts.HeadTarget = modelPath // the stages use the common path; the head is made from the file as it is here
		modelPath = common
		tuned, err := AutoTuneChain(ctx, cand, m, &opts)
		autoNotes = append(autoNotes, tuned...)
		if err != nil {
			return nil, fmt.Errorf("deploy-chain: %w", err)
		}
	}
	// the memory check: every live chain deploy (not only --auto), before any stage starts
	if _, live := runner.(*LiveStageRunner); (runner == nil || live) && m != nil && !opts.DryRun {
		cfg, _ := config.Load(configPathOr(opts.ConfigPath))
		if cfg == nil {
			cfg = &config.Config{}
		}
		notes, _, err := CheckChainMemory(ctx, cand, m, cfg, &opts)
		autoNotes = append(autoNotes, notes...)
		if err != nil {
			return nil, fmt.Errorf("deploy-chain: %w", err)
		}
	}
	{
		mn := ""
		if m != nil {
			mn = m.Name
		}
		opts.planName = "distributed-" + endpointSlug(modelPath, mn)
	}
	// Two deploys that plan at the same moment would both read "no foreign record" and take the same default port. The
	// lock covers the plan and the "starting" record below (which is what the other deploy's plan reads).
	unlockPorts := func() {}
	if !opts.DryRun {
		if un, lerr := config.Lock(filepath.Join(runDir(), "ports")); lerr == nil {
			unlockPorts = un
		}
	}
	defer func() { unlockPorts() }()
	p, err := PlanStagePipe(cand, m, ChainStageV1, opts)
	if err != nil {
		return nil, err
	}
	runID := newRunID()
	masterNode := p.Stages[0].Node
	workerNodes := make([]string, 0, len(p.Stages)-1)
	workerPorts := make(map[string]int)
	workerCmds := make(map[string]string)
	for i := 1; i < len(p.Stages); i++ {
		st := p.Stages[i]
		workerNodes = append(workerNodes, st.Node)
		workerPorts[st.Node] = st.Port
		workerCmds[st.Node] = st.Command
	}
	ingressAddr := fmt.Sprintf("http://%s:%d/v1", p.Stages[0].Host, p.Stages[0].Port)
	if p.Stages[0].Host == "127.0.0.1" || p.Stages[0].Host == "localhost" || p.Stages[0].Host == "0.0.0.0" {
		ingressAddr = fmt.Sprintf("http://127.0.0.1:%d/v1", p.Stages[0].Port)
	}

	modelName := ""
	if m != nil {
		modelName = m.Name
	}
	epName := opts.EndpointName
	if epName == "" {
		epName = "distributed-" + endpointSlug(modelPath, modelName)
	}

	res := &DeployResult{
		RunID:        runID,
		Engine:       "chain",
		EndpointName: epName,
		ModelSHA256:  knownModelSHA(modelPath),
		URL:          ingressAddr,
		MasterNode:   masterNode,
		WorkerNodes:  workerNodes,
		WorkerPorts:  workerPorts,
		MasterCmd:    p.Stages[0].Command,
		WorkerCmds:   workerCmds,
		Status:       StatusStarting,
		StartedAt:    time.Now(),
		Warnings:     append(append([]string(nil), p.Warnings...), autoNotes...),
		Relays:       relayRecords(p.Relays),
		Ports:        planPorts(p),
		Stages:       append([]StageLaunch(nil), p.Stages...),
		Ctx:          opts.Ctx,
		Options: &DeployOptions{
			ModelPath: modelPath,
			Candidate: &cand,
		},
	}
	for _, n := range uniqueStageNodes(p) {
		if w := staleOwnAddr(n); w != "" {
			res.Warnings = append(res.Warnings, w)
		}
	}

	if runner == nil {
		runner = &LiveStageRunner{}
	}
	if !opts.DryRun {
		_ = rec.RecordRun(*res) // "starting": the ports are held from here on
		defer func() {
			// a failure before the stages ran (the model or the draft is missing) must not leave the ports reserved
			if retErr != nil && res.Status == StatusStarting {
				res.Status = StatusFailed
				res.Error = retErr.Error()
				_ = rec.RecordRun(*res)
			}
		}()
	}
	unlockPorts()
	unlockPorts = func() {}

	// the first stage loads the draft: on another machine it must be there at the same path (the head-embedded copy is
	// made here, under ~/.cache/fleet/dflash)
	if p.Draft != "" {
		st := p.Stages[0]
		if autoOn && !opts.DryRun {
			if err := shipDraft(ctx, runner, st.Node, p.Draft); err != nil {
				return nil, fmt.Errorf("stage 0 (%s): sending the draft %s: %w", st.Node, p.Draft, err)
			}
		}
		ok, err := runner.Exists(st.Node, p.Draft)
		if err != nil {
			return nil, fmt.Errorf("stage 0 (%s): check draft: %w", st.Node, err)
		}
		if !ok {
			return nil, fmt.Errorf("stage 0 (%s): draft missing on %s: %s; copy it there at the same path, or make it on %s with `fleet chain dflash-head DRAFT TARGET OUT` and pass that file as --draft-model", st.Node, st.Node, p.Draft, st.Node)
		}
	}
	for _, idx := range p.StartOrder {
		st := p.Stages[idx]
		ok, err := runner.Exists(st.Node, modelPath)
		if err != nil {
			return nil, fmt.Errorf("stage %d (%s): check model: %w", st.Index, st.Node, err)
		}
		if !ok {
			return nil, fmt.Errorf("stage %d (%s): model missing on %s: %s; copy it with `fleet store` or SCP so it is at the same path on all stage machines", st.Index, st.Node, st.Node, modelPath)
		}
	}

	readyTimeout := 120 * time.Second
	if opts.TimeoutS > 0 {
		readyTimeout = time.Duration(opts.TimeoutS) * time.Second
	}
	if opts.ReadyTimeout > 0 {
		readyTimeout = opts.ReadyTimeout // --ready-timeout was parsed but never used on this path
	}

	res.ReadyTimeout = readyTimeout
	err = RunStagePipe(p, runner, readyTimeout)
	if lr, ok := runner.(*LiveStageRunner); ok {
		res.Warnings = append(res.Warnings, lr.Warnings...)
	}
	if err != nil {
		res.Status = StatusFailed
		res.Error = err.Error()
		_ = rec.RecordRun(*res)
		return res, err
	}

	if opts.DryRun {
		res.Status = "dry-run"
		return res, nil
	}
	res.Status = StatusRunning

	var grp *config.Group
	if len(p.Stages) > 1 {
		groupAddrs := make(map[string]string)
		var stages []config.StageAddr
		for _, st := range p.Stages[1:] {
			a := net.JoinHostPort(opts.Host(st.Node), strconv.Itoa(st.Port))
			if _, seen := groupAddrs[st.Node]; !seen {
				groupAddrs[st.Node] = a
			}
			stages = append(stages, config.StageAddr{Node: st.Node, Addr: a})
		}
		grp = &config.Group{
			Engine:  "chain",
			Workers: workerNodes,
			Addrs:   groupAddrs,
			Stages:  stages,
		}
		for _, rel := range p.Relays {
			grp.Relays = append(grp.Relays, rel.Relay)
		}
	}

	ep := config.Endpoint{
		Name:        epName,
		Node:        masterNode,
		URL:         ingressAddr,
		Model:       filepath.Base(modelPath),
		ModelSHA256: res.ModelSHA256,
		Ctx:         ctxOrEngineDefault(opts.Ctx),
		Slots:       max(1, opts.Np),
		Group:       grp,
		Caps:        []string{"chat"},
		Tags:        []string{"distributed", "chain"},
	}
	// an ingress this machine cannot dial (a LAN on another site; E10: papa -> bravo) is registered the way an
	// off-LAN ring master is: an ssh tunnel to its node, so this machine's daemon can route to it
	if pr, ok := runner.(ingressProber); ok && !pr.DirectlyReachable(p.Stages[0]) {
		ep.URL = ""
		ep.SSH = masterNode
		ep.Remote = net.JoinHostPort("127.0.0.1", strconv.Itoa(p.Stages[0].Port))
		ep.Tags = append(ep.Tags, "lan-only")
		res.Reach = "ssh-only"
		res.Warnings = append(res.Warnings, fmt.Sprintf("this machine cannot reach the ingress %s directly: the endpoint is registered as ssh %s -> %s (this machine's fleet daemon tunnels to it)", ingressAddr, masterNode, ep.Remote))
	}
	savedName, epWarns, saveErr := RegisterEndpointInConfig(opts.ConfigPath, ep, opts.Role, nil, opts.EndpointName != "")
	res.EndpointName = savedName
	res.Warnings = append(res.Warnings, epWarns...)
	if saveErr != nil {
		res.Warnings = append(res.Warnings, saveErr.Error())
	}

	_ = rec.RecordRun(*res)
	return res, nil
}

// ctxOrEngineDefault is the context a chain stage runs with: -c when given, else fleet-engine-stage's default (2048)
func ctxOrEngineDefault(ctx int) int {
	if ctx > 0 {
		return ctx
	}
	return 2048
}

// sameMachine: two node names that are one machine (the same name, or both this machine).
func sameMachine(a, b string) bool {
	return a == b || (defaultIsThisMachine(a) && defaultIsThisMachine(b))
}

// planPorts: every port a chain deploy holds on a machine (the stages, the relay tunnel ends), for the run record.
func planPorts(p *StagePipePlan) []NodePort {
	var out []NodePort
	for _, st := range p.Stages {
		out = append(out, NodePort{Node: st.Node, Port: st.Port})
	}
	for _, r := range p.Relays {
		out = append(out, NodePort{Node: r.Relay.Via, Port: r.Relay.Port}, NodePort{Node: r.Relay.From, Port: r.Relay.Port})
	}
	return out
}

// ingressAnswers: does the ingress of a recorded deploy (http://host:port/v1) answer /health? Replaceable by tests.
var ingressAnswers = func(ingress string) bool {
	u, err := url.Parse(ingress)
	if err != nil || u.Host == "" {
		return true // not understood: keep the ports reserved
	}
	c := &http.Client{Timeout: 1500 * time.Millisecond}
	resp, err := c.Get(u.Scheme + "://" + u.Host + "/health")
	if err != nil {
		return false
	}
	resp.Body.Close()
	return true
}

// startingRecordTTL: how long a "starting" record keeps its ports reserved.
const startingRecordTTL = 45 * time.Minute

// recordedForeignPorts reads the run records: the ports of deploys that still run (starting/running) under another endpoint name.
func recordedForeignPorts(endpoint string) map[string]map[int]bool {
	out := map[string]map[int]bool{}
	files, _ := filepath.Glob(filepath.Join(runDir(), "*.json"))
	type held struct {
		r     DeployResult
		probe bool
	}
	var cands []held
	for _, f := range files {
		b, err := os.ReadFile(f)
		if err != nil {
			continue
		}
		var r DeployResult
		if json.Unmarshal(b, &r) != nil || r.EndpointName == endpoint || len(r.Ports) == 0 || (r.Status != StatusStarting && r.Status != StatusRunning) {
			continue // (no ports: a ring deploy or a record from before the ports were recorded)
		}
		// a deploy that was killed half way (Ctrl-C, a crash) leaves "starting" for ever: the longest ready wait is 30 min
		if r.Status == StatusStarting && !r.StartedAt.IsZero() && time.Since(r.StartedAt) > startingRecordTTL {
			continue
		}
		// A running deploy whose ingress no longer answers (its machine died, nobody ran `fleet run stop`) holds nothing.
		// Only an ingress this machine could reach when it was deployed is probed (Reach is "ssh-only" for the others);
		// if the probe is wrong the start script still refuses a port that is really busy (FLEET_BUSY).
		cands = append(cands, held{r, r.Status == StatusRunning && r.URL != "" && r.Reach == ""})
	}
	// all probes at once: the planning waits for the slowest one (1.5 s), not for the sum
	alive := make([]bool, len(cands))
	var wg sync.WaitGroup
	for i, c := range cands {
		alive[i] = true
		if !c.probe {
			continue
		}
		wg.Add(1)
		go func(i int, u string) {
			defer wg.Done()
			alive[i] = ingressAnswers(u)
		}(i, c.r.URL)
	}
	wg.Wait()
	for i, c := range cands {
		if !alive[i] {
			continue
		}
		for _, np := range c.r.Ports {
			k := canonicalNode(np.Node)
			if out[k] == nil {
				out[k] = map[int]bool{}
			}
			out[k][np.Port] = true
		}
	}
	return out
}

// loopbackReady is s with its readiness address moved to 127.0.0.1 (same port): the stages listen on every address.
func loopbackReady(s StageLaunch) StageLaunch {
	if _, port, err := net.SplitHostPort(s.Ready.Addr); err == nil {
		s.Ready.Addr = net.JoinHostPort("127.0.0.1", port)
	}
	return s
}

// uniqueStageNodes: the machines of a plan's stages, each once, in order.
func uniqueStageNodes(p *StagePipePlan) []string {
	seen := map[string]bool{}
	var out []string
	for _, st := range p.Stages {
		if !seen[st.Node] {
			seen[st.Node] = true
			out = append(out, st.Node)
		}
	}
	return out
}

// stageLogTail is the end of a stage's log on its machine ("" when it cannot be read).
func stageLogTail(s StageLaunch) string {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	out, _ := runOn(ctx, s.Node, fmt.Sprintf("tail -n 15 /tmp/fleet-stage-%d.log 2>/dev/null", s.Port))
	return strings.TrimSpace(string(out))
}
