package deploy

import (
	"context"
	"fmt"
	"io"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync/atomic"
	"time"

	"github.com/tevfik/fleet-router/internal/archcert"
	"github.com/tevfik/fleet-router/internal/config"
	"github.com/tevfik/fleet-router/internal/plan"
	"github.com/tevfik/fleet-router/internal/ringengine"
)

// DeployOptions specifies parameters for distributed or single-node model deployment.
type DeployOptions struct {
	ModelPath      string
	Strategy       Strategy
	MasterNode     string
	WorkerNodes    []string
	Candidate      *plan.Candidate
	Role           string
	Port           int
	StatsPort      int
	Ctx            int
	Np             int
	NGL            int               // Layers offloaded to GPU (0: auto/strategy)
	CPUOnly        bool              // Force execution on CPU (NGL=0, no GPU offload)
	TensorSplit    string            // Custom tensor split (e.g. "15,13,12")
	MMap           bool              // Memory-map model file for demand-paging from disk (default: true)
	MMProj         string            // Path to multimodal projector file (e.g. mmproj-*.gguf)
	MMProjCPU      bool              // Run multimodal projector on CPU to conserve GPU VRAM
	MMProjDevice   string            // Custom device for multimodal projector (e.g. none, RING0)
	Wire           string            // activation format between machines: "" (f32, exact) | f16 | fp8 (smaller, lossy: only for models that passed `ring-gate`)
	SpecType       string            // draft-dflash | draft-mtp | draft-dspark | ngram-mod | auto | none
	DraftModel     string            // Path to draft model gguf
	DraftMax       int               // Max draft tokens; 0 = automatic (draft-dflash 2, everything else 4: never more unless asked for)
	LocalWeights   bool              // --local-weights: workers that already hold the model file serve the weights from it (nothing is sent over the wire for them)
	WorkerModels   map[string]string // node -> path of the model file on that worker (default: the master's path)
	ShardsManifest string            // fleet-shards.json for the SHA-256 check on the workers (default: next to the model)
	Chain          bool              // worker-to-worker forwarding (opt-in; needs >= 2 workers; verified on loopback/netsim only, not on a real WAN)
	IOTimeoutS     int               // GGML_RING_IO_TIMEOUT_S for master and workers (0 = engine default: 15 s + 0.1 s per token for decode-sized ops, 600 s for uploads)
	WorkerPort     int               // ring port of the first worker (worker i: WorkerPort+i; 0: 50152)
	WorkerReadyS   int               // seconds a started worker gets to listen on its port (0: DefaultWorkerReady, 120)
	EndpointName   string            // --name: endpoint name to register (empty: "distributed-" + a slug of the model)
	ExitOnFail     bool              // GGML_RING_EXIT_ON_FAIL=1 on the master: a failed ring exits with code 75 so a supervisor (fleet watchdog) redeploys it
	CacheTypeK     string            // KV cache data type for K (f16, q8_0, q4_0)
	CacheTypeV     string            // KV cache data type for V (f16, q8_0, q4_0)
	MoECacheMiB    int               // GPU cache size in MiB for host-resident MoE experts (e.g. 3072)
	CPUMoE         bool              // Keep all MoE expert weights in host CPU RAM (with hot expert cache on GPU)
	NCPUMoE        int               // Keep MoE expert weights of first N layers in host CPU RAM
	// WorkerCPUExperts (--worker-cpu-experts NODE=FIRST-LAST): the MoE expert weights of blocks FIRST..LAST (inclusive)
	// stay in that worker's CPU RAM; the worker runs with -d CUDA0,CPU (see cpuexperts.go).
	WorkerCPUExperts map[string][2]int
	// Hub path (docs/networking_and_tunneling.md, Senaryo B2): workers named by --via-hub / --via-hub-node are reached
	// through the hub's raw TCP streams (fleet stream connect on the master, RING_SERVERS=127.0.0.1:<listen port>).
	// A worker without a direct LAN IP is refused unless --via-hub, --via-hub-node, --no-hub or --chain is given.
	ViaHub        bool     // every worker through the hub
	ViaHubNodes   []string // these workers through the hub
	NoHub         bool     // never choose the hub: a worker without a direct address is dialed by name
	HubURL        string   // hub (default: hub_url of the config, else config.DefaultHubURL)
	HubListenBase int      // first listen port of the streams on the master (0: DefaultHubListenBase, 51000; worker i gets base + i)
	HubRTTms      float64  // assumed hub round trip in ms (0: config hub_rtt_ms, else plan.DefaultHubRTTms)
	HubKey        string   `json:"-"` // API key for hub raw TCP streams (default: $FLEET_API_KEY, config hub_api_key, or api_key_env)
	// Extra environment and arguments (a profile's env: / extra_master_args:, or --master-env / --worker-env /
	// --master-arg): validated by ValidateExtras, appended after everything fleet sets itself.
	MasterEnvExtra map[string]string            // KEY -> VALUE for the master
	WorkerEnvExtra map[string]map[string]string // node -> KEY -> VALUE for that worker
	MasterArgs     []string                     // extra llama-server arguments of the master, one word each
	// Replace: this deploy replaces the endpoint's own previous processes on the same ports (a watchdog redeploy): the
	// memory they still hold is about to be freed, so a memory shortfall is a warning, not a refusal. Never persisted.
	Replace bool `json:"-"`
	DryRun  bool
	// Force goes on although the memory pre-check says the model does not fit a machine (it then pages from disk).
	Force bool
	// LoadTimeoutS is how long the started master gets to become healthy (0: DefaultLoadTimeout, 20 min). When it
	// runs out nothing is stopped (see WaitError).
	LoadTimeoutS int
	ConfigPath   string
}

// DeployResult contains execution commands, process metadata, and endpoint information.
// NodePort is a TCP port a deploy holds on a machine.
type NodePort struct {
	Node string `json:"node"`
	Port int    `json:"port"`
}

type DeployResult struct {
	RunID        string            `json:"run_id"`
	EndpointName string            `json:"endpoint_name"`
	ModelSHA256  string            `json:"model_sha256,omitempty"` // content hash of the model file when already known (never computed here)
	URL          string            `json:"url"`
	Role         string            `json:"role,omitempty"`
	Strategy     string            `json:"strategy"`
	MasterNode   string            `json:"master_node"`
	WorkerNodes  []string          `json:"worker_nodes"`
	Engine       string            `json:"engine,omitempty"` // "ring" | "chain"
	MasterPID    int               `json:"master_pid,omitempty"`
	WorkerPIDs   map[string]int    `json:"worker_pids,omitempty"`
	MasterEnv    string            `json:"master_env,omitempty"`
	MasterCmd    string            `json:"master_cmd"`
	WorkerCmds   map[string]string `json:"worker_cmds,omitempty"`
	WorkerEnv    map[string]string `json:"worker_env,omitempty"`   // environment each worker is started with (space separated KEY=VALUE)
	WorkerPorts  map[string]int    `json:"worker_ports,omitempty"` // ring port of each worker node (the port in RING_SERVERS)
	Ports        []NodePort        `json:"ports,omitempty"`        // chain: every port the deploy holds per machine (stages, relay ends): other deploys keep off them
	Relays       []config.Relay    `json:"relays,omitempty"`       // chain --relay: the ssh tunnels between stages (closed with the run)
	// Stages (chain): every stage as it was started (node, port, command line, readiness): the supervisor starts a stage
	// that died again from this, alone; the other stages keep their loaded weights and the driver reconnects.
	Stages []StageLaunch `json:"stages,omitempty"`
	// ReadyTimeout (chain): how long a stage took to be allowed to come up (the supervisor waits as long for a restarted one)
	ReadyTimeout time.Duration `json:"ready_timeout,omitempty"`
	// WorkerDevices is the -d list of the workers that do not run with the default CUDA0 (--worker-cpu-experts: CUDA0,CPU).
	WorkerDevices map[string]string `json:"worker_devices,omitempty"`
	Devices       string            `json:"devices"`
	TensorSplit   string            `json:"tensor_split"`
	EstimatedTPS  float64           `json:"estimated_tps,omitempty"`
	LatencyMS     float64           `json:"latency_ms,omitempty"`
	Status        string            `json:"status"`
	Warnings      []string          `json:"warnings,omitempty"`
	// Memory: what each machine of the ring must hold (weights + KV cache) and, once checked, what it has.
	Memory []NodeMem `json:"memory,omitempty"`
	// Ctx / Np: the context and slots the master really runs with (-c / -np), after the defaults and the strategy.
	Ctx int `json:"ctx,omitempty"`
	Np  int `json:"np,omitempty"`
	// Hub path: HubWorkers maps each worker reached through the hub to its listen port on the master (its
	// RING_SERVERS entry is 127.0.0.1:<port>), StreamCmds is the `fleet stream connect` run there for it, StreamPIDs
	// the started processes. The API key is never in any of them.
	HubURL     string            `json:"hub_url,omitempty"`
	HubWorkers map[string]int    `json:"hub_workers,omitempty"`
	StreamCmds map[string]string `json:"stream_cmds,omitempty"`
	StreamPIDs map[string]int    `json:"stream_pids,omitempty"`
	// hybridSpec: the plan carries the generic "cannot check STATE_SNAP" warning (hybridSpecGeneric); after the ring is
	// up, checkSpecFeatures replaces it with what the workers really advertise.
	hybridSpec bool
	statsPort  int
	hubBlocker string
	// Status as the record file shows it: planned | starting | running | failed | timeout | stopped (see Status*).
	StartedAt time.Time `json:"started_at,omitempty"`
	Error     string    `json:"error,omitempty"` // why a failed/stopped run ended
	// Reach: "ssh-only" when this machine could not reach the master's URL directly (the master answered over ssh).
	Reach string `json:"reach,omitempty"`
	// MasterEngine: for a remote master, how its engine is found there (a dry run cannot show a path of that machine).
	MasterEngine string `json:"master_engine,omitempty"`
	// RingServers: list of worker addresses dialed by the master (RING_SERVERS)
	RingServers []string `json:"ring_servers,omitempty"`
	// Options are what produced this run: the watchdog redeploys a ring that lost a worker from them.
	Options *DeployOptions `json:"options,omitempty"`
}

// RingServersList returns the list of worker dial targets in RING_SERVERS.
func (r *DeployResult) RingServersList() []string {
	if r == nil {
		return nil
	}
	if len(r.RingServers) > 0 {
		return r.RingServers
	}
	for _, env := range strings.Fields(r.MasterEnv) {
		if strings.HasPrefix(env, "RING_SERVERS=") {
			v := strings.TrimPrefix(env, "RING_SERVERS=")
			if v != "" {
				return strings.Split(v, ",")
			}
		}
	}
	return nil
}

// Test seams: the planner must be runnable without a model on disk, SSH or a network.
var (
	findBackendPath = ringengine.FindBackendPath
	readGGUF        = plan.ReadGGUF
	readRemoteMeta  = readRemoteGGUF
	localHostname   = defaultLocalHostname
	lanProbe        = func(rwm *RemoteWorkerManager, node string) bool {
		_, ok := rwm.ResolveLANAddress(node, "50152")
		return ok
	}
)

const (
	baseWorkerPort = 50152
	// peerPortOffset is the chain rule: worker i listens for its peers on ring port + 100 (50252, 50253, ...). The ring
	// ports are 50152 + i, so the two ranges cannot meet below 100 workers; PlanDeployment also refuses a peer port that
	// equals the master or stats port.
	peerPortOffset = 100

	// maxAutoDraft is the largest n-max fleet picks by itself. Measured (see docs/deploy-spec-and-chain.md): larger
	// draft windows lowered acceptance and throughput.
	maxAutoDraft    = 4
	dflashAutoDraft = 2

	// defaultMoECacheMiB is the GPU cache for host-resident MoE experts when --cpu-moe / --n-cpu-moe asks for host
	// experts and --moe-cache-mib is not given. A tuning default, not a property of any model.
	defaultMoECacheMiB = 3300
)

// exitOnFailWarning is what GGML_RING_EXIT_ON_FAIL=1 costs (engine: ggml-ring.cpp, mark_failed: std::_Exit(75) from a
// thread 2 s after the failing request was answered). Verified in the engine source; see docs/deploy-spec-and-chain.md.
const exitOnFailWarning = "exit-on-fail: ONE failed ring request (a lost worker, a link timeout) makes the WHOLE model server exit (code 75, 2 s after that request was answered): every other request and session in flight and the loaded model are lost, and the reload takes as long as the first load. It is only worth it with a supervisor that restarts the master (fleet watchdog, systemd Restart=on-failure); without one the endpoint stays dead. Off by default"

// recurrent says whether the GGUF metadata describes blocks that keep a recurrent state (plan.Model.Recurrent).
func recurrent(m *plan.Model) bool { return m != nil && m.Recurrent }

// moe says whether the GGUF metadata describes a mixture of experts (<arch>.expert_count > 0).
func moe(m *plan.Model) bool { return m != nil && m.Experts > 0 }

// nextN says whether the model carries NextN / MTP heads (blk.N.nextn.* tensors).
func nextN(m *plan.Model) bool { return m != nil && (m.HasMTP || m.MTPLayers > 0) }

func defaultLocalHostname() string {
	h, err := os.Hostname()
	if err != nil || h == "" {
		return "localhost"
	}
	short, _, _ := strings.Cut(h, ".")
	return short
}

// isLocalNode says whether a master/worker name means this machine.
func isLocalNode(n string) bool {
	return isThisMachine(n) || strings.EqualFold(n, localHostname())
}

// proportional splits total blocks over the stages in proportion to weights (largest remainders; every stage at
// least one block when there are enough).
func proportional(total int, weights []float64) []int {
	out := make([]int, len(weights))
	var sum float64
	for _, w := range weights {
		sum += w
	}
	if sum <= 0 || total <= 0 {
		return out
	}
	type rem struct {
		i int
		r float64
	}
	var rems []rem
	used := 0
	for i, w := range weights {
		x := float64(total) * w / sum
		out[i] = int(x)
		used += out[i]
		rems = append(rems, rem{i, x - float64(out[i])})
	}
	sort.SliceStable(rems, func(a, b int) bool { return rems[a].r > rems[b].r })
	for k := 0; used < total; k++ {
		out[rems[k%len(rems)].i]++
		used++
	}
	if total >= len(out) {
		for i := range out {
			for out[i] == 0 {
				j := 0
				for k := range out {
					if out[k] > out[j] {
						j = k
					}
				}
				out[j]--
				out[i]++
			}
		}
	}
	return out
}

// memorySplit is the generic fixed split when every machine of the ring declares mem_gib in the nodes: section:
// blocks in proportion to the declared memory. ok is false when one of them does not declare it.
func memorySplit(cfg *config.Config, nodes []string, total int) ([]int, bool) {
	ws := make([]float64, len(nodes))
	for i, n := range nodes {
		spec, ok := cfg.Node(n)
		if !ok || spec.MemGiB <= 0 {
			return nil, false
		}
		ws[i] = spec.MemGiB
	}
	return proportional(total, ws), true
}

// PlanDeployment calculates optimal tensor split, worker endpoints, and execution commands.
func PlanDeployment(cfg *config.Config, opts DeployOptions) (*DeployResult, error) {
	wBase := baseWorkerPort
	if opts.WorkerPort > 0 {
		if opts.WorkerPort > 65535-100 {
			return nil, fmt.Errorf("--worker-port %d is out of range", opts.WorkerPort)
		}
		wBase = opts.WorkerPort
	}
	if opts.ModelPath == "" {
		return nil, fmt.Errorf("model path is required")
	}
	// These paths are put into the master's command line, which a remote master's shell reads: a space splits it and
	// ; $( ) ` run code there. RunOnNode already refuses them.
	for name, p := range map[string]string{"model path": opts.ModelPath, "--mmproj": opts.MMProj, "--draft-model": opts.DraftModel, "--shards-manifest": opts.ShardsManifest} {
		if err := checkShellSafePath(name, p); err != nil {
			return nil, err
		}
	}

	if cfg == nil {
		cfg = config.Default()
	}
	// The master is the machine fleet runs on unless one is named; there is no built-in machine name.
	masterNode := opts.MasterNode
	if masterNode == "" {
		masterNode = localHostname()
	}
	if err := ValidateExtras(opts); err != nil {
		return nil, err
	}

	// Every model fact below comes from the GGUF metadata: read here, or on the master over ssh.
	var metaWarn []string
	modelInfo, err := readGGUF(opts.ModelPath)
	if err != nil {
		if isLocalNode(masterNode) {
			return nil, fmt.Errorf("reading gguf failed: %w", err)
		}
		remote, rerr := readRemoteMeta(masterNode, opts.ModelPath)
		switch {
		case rerr == nil && remote != nil:
			modelInfo = remote
		case opts.TensorSplit != "":
			// --ts names every block: the count is its sum; nothing else about the model is known
			n := 0
			for _, f := range strings.Split(opts.TensorSplit, ",") {
				v, _ := strconv.Atoi(strings.TrimSpace(f))
				n += v
			}
			modelInfo = &plan.Model{Name: filepath.Base(opts.ModelPath), Layers: n}
			metaWarn = append(metaWarn, fmt.Sprintf("the GGUF metadata of %s could not be read here or on %s (%v): the plan uses the block count of --ts (%d) and knows nothing else about the model (no MoE / hybrid / NextN rules apply)", opts.ModelPath, masterNode, rerr, n))
		default:
			return nil, fmt.Errorf("reading the GGUF metadata of %s failed here (%v) and on the master %s (%v); fleet does not guess a model's layout: make the file readable or give --ts", opts.ModelPath, err, masterNode, rerr)
		}
	}
	if modelInfo != nil && modelInfo.Shards != nil && modelInfo.Shards.FirstShard != "" {
		opts.ModelPath = modelInfo.Shards.FirstShard
	}

	rwm := NewRemoteWorkerManager()
	// Workers are exactly the ones given (flag or profile): none are discovered or added.
	workerNodes := append([]string(nil), opts.WorkerNodes...)

	isLAN := false
	for _, w := range workerNodes {
		if lanProbe(rwm, w) {
			isLAN = true
			break
		}
	}

	// No workers: one machine, no network between ring members. Without this the Auto strategy took the
	// "WAN" (throughput) profile: np=8 slots sharing -c, i.e. a 512-token context per slot for the default 4096.
	singleMachine := len(workerNodes) == 0
	stratParams := ParamsForStrategy(opts.Strategy, modelInfo.Layers, isLAN || singleMachine)
	if singleMachine && stratParams.Strategy == StrategyAuto {
		stratParams.Description = "Auto-adaptive (no workers: single machine -> low-latency mode selected)"
	}

	port := opts.Port
	if port <= 0 {
		port = 18090
	}
	statsPort := opts.StatsPort
	if statsPort <= 0 {
		statsPort = 50155
	}
	ctxLen := opts.Ctx
	if ctxLen <= 0 {
		ctxLen = 4096
	}
	np := opts.Np
	if np <= 0 {
		np = stratParams.Np
	}

	totalLayers := modelInfo.Layers
	if totalLayers <= 0 {
		return nil, fmt.Errorf("%s: the GGUF metadata has no block count (<arch>.block_count)", opts.ModelPath)
	}

	// Calculate Tensor Split (TS) and Devices
	var devList []string
	var tsList []string
	workerPorts := make(map[string]int)
	ringServers := make([]string, 0)

	var ringNodes []string // worker node of each RING_SERVERS entry, in order

	if opts.Candidate != nil && len(opts.Candidate.Parts) > 1 {
		ringIdx := 0
		workerPortMap := make(map[string]int)
		for _, p := range opts.Candidate.Parts {
			if p.Node == masterNode {
				devList = append(devList, "CUDA0")
				tsList = append(tsList, strconv.Itoa(p.Layers))
			} else {
				devName := fmt.Sprintf("RING%d", ringIdx)
				devList = append(devList, devName)
				tsList = append(tsList, strconv.Itoa(p.Layers))
				ringIdx++

				wPort, exists := workerPortMap[p.Node]
				if !exists {
					wPort = wBase + len(workerPortMap)
					workerPortMap[p.Node] = wPort
					wIP := ResolveNodeIP(p.Node)
					ringServers = append(ringServers, fmt.Sprintf("%s:%d", wIP, wPort))
					ringNodes = append(ringNodes, p.Node)
					workerPorts[p.Node] = wPort
				}
			}
		}
	} else if len(workerNodes) == 0 {
		// Single node
		devList = []string{"CUDA0"}
		tsList = []string{strconv.Itoa(totalLayers)}
	} else if len(workerNodes) == 1 {
		// Master + 1 worker
		wNode := workerNodes[0]
		wIP := ResolveNodeIP(wNode)
		wAddr := fmt.Sprintf("%s:%d", wIP, wBase)
		ringServers = append(ringServers, wAddr)
		ringNodes = append(ringNodes, wNode)

		workerPorts[wNode] = wBase

		var masterLayers, workerLayers int
		if split, ok := memorySplit(cfg, []string{masterNode, wNode}, totalLayers); ok {
			// both machines declare mem_gib (nodes: section): blocks in proportion to the memory
			devList = []string{"CUDA0", "RING0"}
			tsList = []string{strconv.Itoa(split[0]), strconv.Itoa(split[1])}
		} else if stratParams.AsymSplit {
			// Master (laptop) takes 1/3, worker takes 2/3
			masterLayers = totalLayers / 3
			workerLayers = totalLayers - masterLayers
			devList = []string{"CUDA0", "RING0"}
			tsList = []string{strconv.Itoa(masterLayers), strconv.Itoa(workerLayers)}
		} else {
			// Balanced split
			masterLayers = totalLayers / 2
			workerLayers = totalLayers - masterLayers
			devList = []string{"CUDA0", "RING0"}
			tsList = []string{strconv.Itoa(masterLayers), strconv.Itoa(workerLayers)}
		}
	} else {
		// Multi-worker cluster
		numStages := len(workerNodes) + 1
		layersPerStage := totalLayers / numStages
		rem := totalLayers % numStages

		devList = append(devList, "CUDA0")
		tsList = append(tsList, strconv.Itoa(layersPerStage+rem))

		for i, wNode := range workerNodes {
			devName := fmt.Sprintf("RING%d", i)
			devList = append(devList, devName)
			tsList = append(tsList, strconv.Itoa(layersPerStage))

			wPort := wBase + i
			wIP := ResolveNodeIP(wNode)
			wAddr := fmt.Sprintf("%s:%d", wIP, wPort)
			ringServers = append(ringServers, wAddr)
			ringNodes = append(ringNodes, wNode)
			workerPorts[wNode] = wPort
		}

		// every machine declares mem_gib (nodes: section): blocks in proportion to the memory
		if split, ok := memorySplit(cfg, append([]string{masterNode}, workerNodes...), totalLayers); ok {
			for i := range tsList {
				tsList[i] = strconv.Itoa(split[i])
			}
		}
	}

	// A draft model is used only when given (flag or profile): nothing is picked up from the model's directory.
	isMoE := moe(modelInfo)

	// Single-node MoE topology automation:
	// When running a single node and model exceeds VRAM, or Candidate specifies Hot-MoE / GPU+RAM:
	// 1. Enable host CPU MoE offload (-cmoe)
	// 2. Maximize Hot Expert Cache (SLRU) on GPU
	// 3. Disable speculation by default because local PCIe has 0ms wire latency and draft models
	//    cannibalize 1.2+ GB VRAM and add compute overhead.
	isSingleNodeMoE := len(workerNodes) == 0 && isMoE && (opts.CPUMoE || opts.NCPUMoE > 0 || (opts.Candidate != nil && (opts.Candidate.Where == "GPU+RAM" || opts.Candidate.Where == "RAM")) || opts.Strategy == StrategyMemory)
	if isSingleNodeMoE && !opts.CPUMoE && opts.NCPUMoE == 0 {
		opts.CPUMoE = true
	}

	specType := opts.SpecType
	if specType == "auto" || specType == "" {
		specType, metaWarn = autoSpec(opts, masterNode, modelInfo, isSingleNodeMoE, metaWarn)
	}

	// Draft window: an explicit --draft-max is authoritative. Otherwise 2 for draft-dflash and 4 for everything else:
	// measured acceptance falls and throughput with it as the window grows, and fleet no longer widens it on slow links.
	draftMax := opts.DraftMax
	if draftMax <= 0 {
		draftMax = maxAutoDraft
		if specType == "draft-dflash" {
			draftMax = dflashAutoDraft
		}
	}

	devsStr := strings.Join(devList, ",")
	tsStr := strings.Join(tsList, ",")
	if opts.TensorSplit != "" {
		tsStr = opts.TensorSplit
	}

	// A NextN / MTP head (blk.N.nextn.* tensors in the GGUF, or --spec draft-mtp) shares the final blocks' KV cache,
	// so the master (which runs the head) must hold the final blocks: the order becomes RING0,CUDA0. A plan candidate
	// keeps its own order.
	if len(workerNodes) == 1 && (opts.Candidate == nil || len(opts.Candidate.Parts) <= 1) && (specType == "draft-mtp" || nextN(modelInfo)) {
		devList = []string{"RING0", "CUDA0"}
		if opts.TensorSplit != "" {
			parts := strings.Split(opts.TensorSplit, ",")
			if len(parts) == 2 {
				tsStr = fmt.Sprintf("%s,%s", strings.TrimSpace(parts[1]), strings.TrimSpace(parts[0]))
			}
		} else if len(tsList) == 2 {
			tsStr = fmt.Sprintf("%s,%s", tsList[1], tsList[0])
		}
		devsStr = strings.Join(devList, ",")
	}
	hp, err := planHub(cfg, opts, masterNode, ringNodes, workerPorts, port, statsPort)
	if err != nil {
		return nil, err
	}
	var streamCmds map[string]string
	if len(hp.listen) > 0 {
		streamCmds = make(map[string]string)
		for i, node := range ringNodes {
			if l, ok := hp.listen[node]; ok {
				sl := streamLaunch{Node: node, Port: workerPorts[node], ListenPort: l, Hub: hp.hub}
				ringServers[i] = sl.Listen()
				streamCmds[node] = sl.ShellLine()
			}
		}
	}
	// --worker-cpu-experts: renumber the RING devices for the workers that also offer their CPU (RING_SERVERS order,
	// GPU before CPU), append those CPU devices with -ts 0 and point their expert tensors at them. Built after the hub
	// step: the -ot target names the final RING_SERVERS entry (127.0.0.1:<listen port> for a worker behind the hub).
	// a plan candidate that uses the CPU-expert tier brings its block ranges (an explicit --worker-cpu-experts wins)
	if len(opts.WorkerCPUExperts) == 0 && opts.Candidate != nil && len(opts.Candidate.Parts) > 1 {
		for _, p := range opts.Candidate.Parts {
			if f, l, ok := p.CPUExpertBlocks(); ok && p.Node != masterNode {
				if opts.WorkerCPUExperts == nil {
					opts.WorkerCPUExperts = map[string][2]int{}
				}
				opts.WorkerCPUExperts[p.Node] = [2]int{f, l}
			}
		}
	}
	if len(opts.WorkerCPUExperts) > 0 && opts.CPUOnly {
		return nil, fmt.Errorf("--worker-cpu-experts cannot be combined with --cpu (there is no -dev/-ts split to put the experts into)")
	}
	cpx, err := planCPUExperts(opts.WorkerCPUExperts, ringNodes, ringServers, devList, tsStr, totalLayers)
	if err != nil {
		return nil, err
	}
	if cpx.ot != "" {
		devList, tsStr = cpx.devList, cpx.ts
		devsStr = strings.Join(devList, ",")
	}
	serversEnv := strings.Join(ringServers, ",")

	if opts.IOTimeoutS < 0 {
		return nil, fmt.Errorf("--io-timeout must be >= 1 seconds (not given: engine default, 15 s + 0.1 s per token)")
	}
	if opts.Chain && len(ringServers) < 2 {
		return nil, fmt.Errorf("--chain needs at least 2 workers (worker-to-worker forwarding has nothing to forward between with %d); drop --chain or add workers", len(ringServers))
	}

	var envParts []string
	if serversEnv != "" {
		envParts = append(envParts, fmt.Sprintf("RING_SERVERS=%s", serversEnv))
	}
	// Ports on one machine: the master's server, its statistics port and a worker's ring port must differ (the default
	// statistics port 50155 is the ring port of a 4th worker; --worker-port near --port collides too).
	if port == statsPort {
		return nil, fmt.Errorf("--port %d is also the statistics port: pick another --port", port)
	}
	for node, wp := range workerPorts {
		if (node == masterNode || (isThisMachine(node) && isThisMachine(masterNode))) && (wp == port || wp == statsPort) {
			return nil, fmt.Errorf("the ring port %d of worker %s is also the master's %s on that machine: pick another --worker-port / --port", wp, node, map[bool]string{true: "port", false: "statistics port"}[wp == port])
		}
	}
	workerEnv := make(map[string]string)
	if opts.Chain {
		peers := make([]string, len(ringServers))
		for i, node := range ringNodes {
			pp := wBase + peerPortOffset + i
			if pp == port || pp == statsPort {
				return nil, fmt.Errorf("--chain: peer port %d of worker %s collides with the master/stats port; pick another --port", pp, node)
			}
			peers[i] = fmt.Sprintf("%s:%d", ResolveNodeIP(node), pp)
			workerEnv[node] = joinEnv(workerEnv[node], fmt.Sprintf("GGML_RING_PEER_LISTEN=0.0.0.0:%d", pp))
		}
		envParts = append(envParts, "RING_PEER_SERVERS="+strings.Join(peers, ","))
	}
	if opts.IOTimeoutS > 0 && len(ringServers) > 0 {
		v := fmt.Sprintf("GGML_RING_IO_TIMEOUT_S=%d", opts.IOTimeoutS)
		envParts = append(envParts, v)
		for _, node := range ringNodes {
			workerEnv[node] = joinEnv(workerEnv[node], v)
		}
	}
	if opts.ExitOnFail && len(ringServers) > 0 {
		envParts = append(envParts, "GGML_RING_EXIT_ON_FAIL=1")
	}
	// The backend library is this machine's path: only a master that runs here gets it. A remote master resolves its
	// own (remoteMasterScript); this machine's /home/... path in its environment would be wrong there.
	masterEngine := ""
	if !isLocalNode(masterNode) {
		masterEngine = describeRemoteMaster(masterNode)
	} else if bPath, ok := findBackendPath(); ok {
		envParts = append(envParts, fmt.Sprintf("GGML_BACKEND_PATH=%s", bPath))
		envParts = append(envParts, fmt.Sprintf("LD_LIBRARY_PATH=%s:$LD_LIBRARY_PATH", filepath.Dir(bPath)))
	}
	switch wire := strings.ToLower(strings.TrimSpace(opts.Wire)); {
	case wire == "fp8" || stratParams.WireFP8:
		envParts = append(envParts, "GGML_RING_WIRE_FP8=1")
	case wire == "f16":
		envParts = append(envParts, "GGML_RING_WIRE_F16=1")
	}
	// Worker-side recurrent-state snapshots are opt-in in the engine (an evicted snapshot restored later fails the ring);
	// speculation on a hybrid model over the ring is what needs them (otherwise every draft checkpoint crosses the wire)
	if specType != "" && specType != "none" && len(ringServers) > 0 && recurrent(modelInfo) {
		envParts = append(envParts, "GGML_RING_STATE_SNAP=1")
	}
	envParts = append(envParts, fmt.Sprintf("RING_STATS_PORT=%d", statsPort))
	// the architecture's ring gate passed only with some engine variables set (arch-cert.json requires_env): they go to
	// the master and to every worker, before the user's own extras so that those can still override
	certEnv := ringCertEnv(modelInfo, len(ringServers) > 0)
	envParts = append(envParts, certEnv...)
	envParts = append(envParts, sortedEnv(opts.MasterEnvExtra)...)
	masterEnv := strings.Join(envParts, " ")

	faFlag := ""
	if stratParams.FlashAttn {
		faFlag = " -fa on"
	}

	devFlags := ""
	if len(workerNodes) > 0 {
		devFlags = fmt.Sprintf(" -dev %s -ts %s", devsStr, tsStr) + cpx.ot
		// the plan keeps the output projection on the master: without this llama.cpp puts it on the last device
		// and the logits (~1 MB) cross the link every token
		if keepOutputOnMaster(opts.Candidate, devList, devsStr) {
			devFlags += " -ot '^output\\.weight$=CUDA0'"
		}
	}

	ngl := stratParams.NGL
	if opts.NGL > 0 {
		ngl = opts.NGL
	} else if len(workerNodes) > 0 && totalLayers > 0 {
		// block_count is not the offload count: a model with extra layers beyond block_count (NextN heads) would leave
		// its tail on the CPU when ngl equals block_count, so offload everything
		ngl = 99
	}
	if opts.CPUOnly {
		ngl = 0
		devFlags = ""
		opts.MMProjCPU = true
	}

	mmprojFlag := ""
	if opts.MMProj != "" {
		mmprojFlag = fmt.Sprintf(" --mmproj %s", opts.MMProj)
		if opts.MMProjDevice != "" && !opts.CPUOnly {
			mmprojFlag += fmt.Sprintf(" -mmdev %s", opts.MMProjDevice)
		} else if opts.MMProjCPU || opts.CPUOnly {
			mmprojFlag += " --no-mmproj-offload"
		}
	}

	mmapFlag := ""
	if opts.MMap {
		mmapFlag = " --load-mode mmap"
	}

	specFlag := ""

	if specType != "" && specType != "none" {
		specFlag = fmt.Sprintf(" --spec-type %s --spec-draft-n-max %d", specType, draftMax)
		if specType == "ngram-mod" {
			specFlag += fmt.Sprintf(" --spec-ngram-mod-n-max %d", draftMax)
		}
		if opts.DraftModel != "" {
			specFlag += fmt.Sprintf(" --spec-draft-model %s --spec-draft-ngl 99 --spec-draft-device CUDA0", opts.DraftModel)
		}
	}

	kvCacheFlag := ""
	if opts.CacheTypeK != "" {
		kvCacheFlag += fmt.Sprintf(" --cache-type-k %s", opts.CacheTypeK)
	}
	if opts.CacheTypeV != "" {
		kvCacheFlag += fmt.Sprintf(" --cache-type-v %s", opts.CacheTypeV)
	}

	moeFlag := ""
	if opts.CPUMoE {
		moeFlag += " -cmoe"
	} else if opts.NCPUMoE > 0 {
		moeFlag += fmt.Sprintf(" -ncmoe %d", opts.NCPUMoE)
	}

	moeCacheMiB := opts.MoECacheMiB
	if moeCacheMiB == 0 && (opts.CPUMoE || opts.NCPUMoE > 0 || isSingleNodeMoE) {
		moeCacheMiB = defaultMoECacheMiB
	}
	if moeCacheMiB > 0 {
		moeFlag += fmt.Sprintf(" --moe-cache-mib %d", moeCacheMiB)
	}

	extraArgs := ""
	if len(opts.MasterArgs) > 0 {
		extraArgs = " " + strings.Join(opts.MasterArgs, " ")
	}
	masterCmd := fmt.Sprintf("fleet ring serve -m %s -ngl %d%s -c %d -np %d -ub %d%s%s%s%s%s%s%s --fit off --host 0.0.0.0 --port %d",
		opts.ModelPath, ngl, devFlags, ctxLen, np, stratParams.MicroBatch, faFlag, mmprojFlag, mmapFlag, specFlag, kvCacheFlag, moeFlag, extraArgs, port)

	modelName := ""
	if modelInfo != nil {
		modelName = modelInfo.Name
	}
	endpointName := "distributed-" + endpointSlug(opts.ModelPath, modelName)
	if opts.EndpointName != "" {
		endpointName = opts.EndpointName
	}

	runID := newRunID()

	// The estimate is the planner's (plan package), never a built-in figure: a plan candidate's own, else the cost
	// model over the fixed split when every machine declares weight_gbps in the nodes: section, else none (0).
	hubRTT := effectiveHubRTT(cfg, opts)
	estTPS, latMS, directTPS := estimateWithHub(cfg, opts.Candidate, modelInfo, masterNode, ringNodes, devList, tsStr, len(workerNodes) == 0 && !opts.CPUOnly, hp.listen, hubRTT)

	masterHost := "127.0.0.1"
	if !isLocalNode(masterNode) {
		masterHost = ResolveNodeIP(masterNode)
	}
	url := fmt.Sprintf("http://%s:%d/v1", masterHost, port)

	warnings := append([]string(nil), metaWarn...)
	hybridSpecIdx := -1
	if len(workerNodes) > 0 {
		wire := strings.ToLower(strings.TrimSpace(opts.Wire))
		if stratParams.WireFP8 {
			wire = "fp8"
		}
		arch := ""
		if modelInfo != nil {
			arch = modelInfo.Arch
		}
		if w := archcert.Warn(arch, wire); w != "" {
			warnings = append(warnings, w)
		}
	}

	if specFlag != "" {
		warnings = append(warnings, fmt.Sprintf("estimated tok/s is the baseline WITHOUT speculation: what %s gains (or loses) depends on the model, the draft acceptance and the prompt, so it is not estimated (indicative only until measured: fleet profile bench records it)", specType))
		if len(ringServers) > 0 && recurrent(modelInfo) {
			hybridSpecIdx = len(warnings)
			warnings = append(warnings, hybridSpecGeneric)
		}
	}
	if opts.ExitOnFail && len(ringServers) > 0 {
		warnings = append(warnings, exitOnFailWarning)
	}
	if opts.Chain {
		warnings = append(warnings, "--chain (worker-to-worker forwarding) is experimental: verified on loopback/netsim only, not on a real WAN. Peer ports are ring port + 100 and must be reachable between the workers")
	}

	if opts.LocalWeights && len(ringNodes) > 0 && modelInfo != nil && modelInfo.Shards != nil {
		// GGML_RING_SHARD_GGUF names ONE file: a worker would index the first shard only and the sizes compared would be
		// the first shard's; the master pushes the weights over the wire instead
		warnings = append(warnings, fmt.Sprintf("--local-weights ignored: the model is %d GGUF files (%s ...) and the worker's local-weights mode reads a single file; the master sends the weights", modelInfo.Shards.Total, filepath.Base(modelInfo.Shards.FirstShard)))
	} else if opts.LocalWeights && len(ringNodes) > 0 {
		extra := 0
		if modelInfo != nil {
			extra = modelInfo.MTPLayers
		}
		warnings = append(warnings, localWeightsEnv(opts, masterNode, ringNodes, cpx.devNode, devsStr, tsStr, totalLayers, extra, workerEnv)...)
	}

	// The worker commands are built last, from the final per-worker environment, by the same builder StartWorkerEnv
	// runs: the plan shows exactly what is started.
	workerCmds := make(map[string]string)
	for _, node := range ringNodes {
		if len(certEnv) > 0 {
			workerEnv[node] = joinEnv(workerEnv[node], strings.Join(certEnv, " "))
		}
		if extra := sortedEnv(opts.WorkerEnvExtra[node]); len(extra) > 0 {
			workerEnv[node] = joinEnv(workerEnv[node], strings.Join(extra, " "))
		}
	}
	if len(certEnv) > 0 {
		warnings = append(warnings, fmt.Sprintf("architecture %q passed the ring correctness gate only with %s: set on the master and on every worker (arch-cert.json requires_env). It can cost speed", modelInfo.Arch, strings.Join(certEnv, " ")))
	}
	for _, node := range ringNodes {
		workerCmds[node] = workerCommandLine(node, workerPorts[node], strings.Fields(workerEnv[node]), cpx.devices[node])
		if l, ok := hp.listen[node]; ok {
			// the same line hubJoinScript runs in the worker's ssh session once the worker listens
			workerCmds[node] += "\n      then, only if the fleet daemon answers there: " + hubJoinCommand(hp.hub, workerPorts[node])
			warnings = append(warnings, fmt.Sprintf("worker %s goes through the hub %s (%s): on the master `%s` runs in the background and RING_SERVERS names 127.0.0.1:%d for it; %s", node, hp.hub, hp.reasons[node], streamCmds[node], l, hubLatencyNote))
			continue
		}
		if a := ResolveNodeIP(node); net.ParseIP(a) == nil && !(isThisMachine(node) && isThisMachine(masterNode)) {
			warnings = append(warnings, fmt.Sprintf("worker %s has no known direct address (none in the fleet config, its ssh config entry gives no IP or goes through a proxy): the master will dial %q. Give the worker as an IP reachable from the master, or bring it in through a tunnel", node, net.JoinHostPort(a, strconv.Itoa(workerPorts[node]))))
		}
	}

	var hubWorkers map[string]int
	hubURL := ""
	hubBlocker := ""
	if len(hp.listen) > 0 {
		hubWorkers, hubURL = hp.listen, hp.hub
		if w, b := checkHubSpeedLoss(directTPS, estTPS, hubRTT, hp.listen); b != "" {
			hubBlocker = b
		} else if w != "" {
			warnings = append(warnings, w)
		}
		if isThisMachine(masterNode) {
			warnings = append(warnings, "the hub streams on this machine get the hub API key as FLEET_API_KEY in their environment (from $FLEET_API_KEY or api_key_env of this fleet's config); it is never printed")
		} else {
			warnings = append(warnings, fmt.Sprintf("the hub streams run on the master %s and use ITS fleet's hub API key ($FLEET_API_KEY there, else api_key_env of its config): nothing is sent over ssh, so set it up there first", masterNode))
		}
	}

	memory := memNeeds(modelInfo, devList, tsStr, masterNode, cpx.devNode, ctxLen, np, opts.CacheTypeK, opts.CacheTypeV)
	if !opts.CPUOnly {
		memory = applyMoECacheBudget(memory, modelInfo, masterNode, opts.CPUMoE, opts.NCPUMoE, moeCacheMiB)
	}
	if mm := modelInfo; mm != nil && mm.Weights() <= 0 && mm.Layers > 0 {
		warnings = append(warnings, "the size of the model's weights is unknown (only the block count could be read): the memory pre-check is skipped")
	}
	warnings = append(warnings, plan.ShardWarnings(modelInfo)...)

	return &DeployResult{
		Ctx:          ctxLen,
		Np:           np,
		RingServers:  ringServers,
		MasterEngine: masterEngine,
		Memory:       memory,
		HubURL:       hubURL,
		HubWorkers:   hubWorkers,
		StreamCmds:   streamCmds,
		Warnings:     warnings,
		hybridSpec:   hybridSpecIdx >= 0,
		statsPort:    statsPort,
		hubBlocker:   hubBlocker,
		WorkerEnv:    workerEnv,
		WorkerPorts:  workerPorts,
		WorkerDevices: func() map[string]string {
			if len(cpx.devices) == 0 {
				return nil
			}
			return cpx.devices
		}(),
		RunID:        runID,
		EndpointName: endpointName,
		ModelSHA256:  knownModelSHA(opts.ModelPath),
		URL:          url,
		Role:         opts.Role,
		Strategy:     string(stratParams.Strategy),
		MasterNode:   masterNode,
		WorkerNodes:  workerNodes,
		MasterEnv:    masterEnv,
		MasterCmd:    masterCmd,
		WorkerCmds:   workerCmds,
		Devices:      devsStr,
		TensorSplit:  tsStr,
		EstimatedTPS: estTPS,
		LatencyMS:    latMS,
		Status:       "planned",
	}, nil
}

// autoSpec is --spec auto, from metadata only: a single machine keeping MoE experts in host RAM gets none (the draft
// competes with the expert cache for VRAM and the link has no latency to hide); a given draft model gets draft-dflash
// when its GGUF architecture says dflash, else draft-simple; a model with NextN heads gets draft-mtp; anything else
// none (ngram-mod and the others only when asked for).
func autoSpec(opts DeployOptions, master string, m *plan.Model, singleNodeMoE bool, warns []string) (string, []string) {
	switch {
	case singleNodeMoE:
		return "none", warns
	case opts.DraftModel != "":
		d, err := readGGUF(opts.DraftModel)
		if err != nil && !isLocalNode(master) {
			d, err = readRemoteMeta(master, opts.DraftModel)
		}
		if err != nil || d == nil {
			return "draft-simple", append(warns, fmt.Sprintf("--spec auto: the draft model's GGUF metadata could not be read (%v), so it runs as draft-simple; give --spec draft-dflash / draft-mtp if it is one", err))
		}
		if strings.Contains(strings.ToLower(d.Arch), "dflash") {
			return "draft-dflash", warns
		}
		return "draft-simple", warns
	case nextN(m):
		return "draft-mtp", warns
	}
	return "none", warns
}

// estimateWithHub computes the baseline token rate (tps, ms) and, when hubWorkers is non-empty, the direct LAN
// baseline (directTPS) for speed-loss comparison.
func estimateWithHub(cfg *config.Config, c *plan.Candidate, m *plan.Model, master string, ringNodes, devList []string, ts string, single bool, hubWorkers map[string]int, hubRTT float64) (tps, ms, directTPS float64) {
	isHub := func(node string) bool {
		if len(hubWorkers) == 0 {
			return false
		}
		if _, ok := hubWorkers[node]; ok {
			return true
		}
		for k := range hubWorkers {
			if strings.EqualFold(k, node) {
				return true
			}
		}
		return false
	}
	if c != nil && c.TPS > 0 {
		if len(hubWorkers) == 0 || c.TokenMS <= 0 {
			return c.TPS, c.TokenMS, c.TPS
		}
		hubMS := c.TokenMS
		directMS := c.TokenMS
		for _, pt := range c.Parts {
			if strings.EqualFold(pt.Node, c.Master) || !isHub(pt.Node) {
				continue
			}
			if pt.RTTms > plan.DefaultRTTms && pt.RTTms >= hubRTT*0.9 {
				directMS -= pt.RTTms - plan.DefaultRTTms
			} else {
				baseRTT := pt.RTTms
				if baseRTT <= 0 {
					baseRTT = plan.DefaultRTTms
				}
				if hubRTT > baseRTT {
					hubMS += hubRTT - baseRTT
				}
			}
		}
		if directMS > 0 {
			directTPS = 1000 / directMS
		}
		if hubMS > 0 {
			return 1000 / hubMS, hubMS, directTPS
		}
		return c.TPS, c.TokenMS, directTPS
	}
	if m == nil || len(m.LayerRead) == 0 {
		return 0, 0, 0
	}
	rate := func(n string) float64 {
		s, _ := cfg.Node(n)
		return s.WeightGBps
	}
	var directStages, hubStages []plan.Stage
	if single {
		st := plan.Stage{Node: master, Layers: m.Layers, WeightGBps: rate(master)}
		directStages = []plan.Stage{st}
		hubStages = []plan.Stage{st}
	} else {
		fields := strings.Split(ts, ",")
		ring := 0
		for i, d := range devList {
			if i >= len(fields) {
				break
			}
			n, _ := strconv.Atoi(strings.TrimSpace(fields[i]))
			node := master
			if strings.HasPrefix(d, "RING") {
				if ring >= len(ringNodes) {
					break
				}
				node = ringNodes[ring]
				ring++
			}
			ds := plan.Stage{Node: node, Layers: n, WeightGBps: rate(node)}
			hs := ds
			if i > 0 && n > 0 && isHub(node) && hubRTT > 0 {
				hs.RTTms = hubRTT
			}
			directStages = append(directStages, ds)
			hubStages = append(hubStages, hs)
		}
	}
	dtok, dok := plan.RoughTokenMS(m, directStages, plan.Options{})
	if dok && dtok > 0 {
		directTPS = 1000 / dtok
	}
	tok, ok := plan.RoughTokenMS(m, hubStages, plan.Options{})
	if !ok || tok <= 0 {
		return 0, 0, directTPS
	}
	return 1000 / tok, tok, directTPS
}

func joinEnv(cur, add string) string {
	if cur == "" {
		return add
	}
	return cur + " " + add
}

// keepOutputOnMaster says whether the output projection is pinned to the master's GPU. A plan candidate decides for
// itself (Output on the master part). Without a plan (the fixed splits) the master is first and a worker last, so
// llama.cpp would put the projection on the worker and ship vocab x 4 bytes of logits back every token; the pin costs
// the master one tensor of VRAM. When the master is not first (NextN/MTP order RING0,CUDA0) the last device is the
// master already.
func keepOutputOnMaster(c *plan.Candidate, devList []string, devsStr string) bool {
	if !strings.HasPrefix(devsStr, "CUDA0") {
		return false
	}
	if c != nil && len(c.Parts) > 1 {
		return c.Parts[0].Output
	}
	return len(devList) > 1 && devList[0] == "CUDA0" && devList[len(devList)-1] != "CUDA0"
}

// startOrder lists the worker nodes to start: the ring's workers in port order (a plan candidate's workers need not be
// the --workers list), else WorkerNodes.
func (r *DeployResult) startOrder() []string {
	if len(r.WorkerPorts) == 0 {
		return r.WorkerNodes
	}
	nodes := make([]string, 0, len(r.WorkerPorts))
	for n := range r.WorkerPorts {
		nodes = append(nodes, n)
	}
	sort.Slice(nodes, func(i, j int) bool { return r.WorkerPorts[nodes[i]] < r.WorkerPorts[nodes[j]] })
	return nodes
}

// workerPort is the ring port of node (the port its RING_SERVERS entry names).
func (r *DeployResult) workerPort(node string) int {
	if p, ok := r.WorkerPorts[node]; ok && p > 0 {
		return p
	}
	for i, n := range r.WorkerNodes {
		if n == node {
			return baseWorkerPort + i
		}
	}
	return baseWorkerPort
}

// localArgs turns the words of MasterCmd into exec arguments: MasterCmd is a shell line (a remote master runs it
// through sh), so a single-quoted word such as '^output\.weight$=CUDA0' loses its quotes here as the shell would drop
// them; otherwise llama-server gets the quotes as part of the -ot pattern and of the buffer type name.
func localArgs(words []string) []string {
	out := make([]string, len(words))
	for i, w := range words {
		if len(w) >= 2 && w[0] == '\'' && w[len(w)-1] == '\'' && !strings.Contains(w[1:len(w)-1], "'") {
			w = w[1 : len(w)-1]
		}
		out[i] = w
	}
	return out
}

// readRemoteGGUF reads a model's GGUF metadata on node over ssh: the header is streamed (the file is never copied) into
// the same reader as a local file; when that fails, a generic python one-liner reads general.architecture and then
// <arch>.block_count, <arch>.embedding_length, <arch>.expert_count, <arch>.expert_used_count. Nothing is guessed: an
// error is returned when neither works.
func readRemoteGGUF(node, path string) (*plan.Model, error) {
	if strings.ContainsAny(path, "'\n") || strings.ContainsAny(node, " \t\n'\"$;&|<>`\\") || strings.HasPrefix(node, "-") {
		return nil, fmt.Errorf("unsafe node or path")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	if m, err := streamRemoteHeader(ctx, node, path); err == nil {
		if names := plan.ShardNames(path); names != nil {
			return m, addRemoteShards(ctx, m, names, remoteShardSizes(ctx, node, names), func(name string) (io.ReadCloser, func(), error) {
				return remoteHeaderReader(ctx, node, name)
			})
		}
		return m, nil
	}
	py := "import sys\nfrom gguf import GGUFReader\nr=GGUFReader(sys.argv[1])\n" +
		"def v(k):\n f=r.fields.get(k)\n return '' if f is None else (bytes(f.parts[-1]).decode() if k=='general.architecture' else str(int(f.parts[-1][0])))\n" +
		"a=v('general.architecture')\nprint(a)\n" +
		"for k in ('block_count','embedding_length','expert_count','expert_used_count'): print(v(a+'.'+k) or 0)\n"
	cmd := exec.CommandContext(ctx, "ssh", "-o", "BatchMode=yes", "-o", "ConnectTimeout=5", "--", node,
		fmt.Sprintf("python3 -c %s '%s'", shellQuote(py), path))
	out, err := cmd.Output()
	if err != nil {
		return nil, fmt.Errorf("ssh %s: %w", node, err)
	}
	return parseRemoteMeta(filepath.Base(path), string(out))
}

// remoteShardSizes asks node for the sizes of the shard files that exist (name -> bytes).
func remoteShardSizes(ctx context.Context, node string, names []string) map[string]int64 {
	quoted := make([]string, len(names))
	for i, n := range names {
		quoted[i] = "'" + n + "'"
	}
	out, _ := exec.CommandContext(ctx, "ssh", "-o", "BatchMode=yes", "-o", "ConnectTimeout=5", "--", node,
		"stat -L -c '%s %n' "+strings.Join(quoted, " ")+" 2>/dev/null").Output()
	sizes := map[string]int64{}
	for _, ln := range strings.Split(string(out), "\n") {
		sz, name, ok := strings.Cut(strings.TrimSpace(ln), " ")
		if v, err := strconv.ParseInt(sz, 10, 64); ok && err == nil {
			sizes[name] = v
		}
	}
	return sizes
}

// addRemoteShards completes m (read from the first file of a multi-file GGUF on another machine) with the tensor
// tables of the other files: m.Weights() is then the whole model's. sizes says which files exist; open streams the
// header of one (the second func ends the stream). Missing files are recorded in m.Shards, not an error: the plan and
// the memory check report them.
func addRemoteShards(ctx context.Context, m *plan.Model, names []string, sizes map[string]int64, open func(name string) (io.ReadCloser, func(), error)) error {
	si := &plan.ShardInfo{Total: len(names), FirstShard: names[0]}
	for i, n := range names {
		sz, ok := sizes[n]
		if !ok {
			si.Missing = append(si.Missing, i+1)
			continue
		}
		si.ShardPaths = append(si.ShardPaths, n)
		si.TotalBytes += sz
		if i == 0 {
			continue
		}
		r, stop, err := open(n)
		if err != nil {
			return err
		}
		err = m.AddShardFile(r, sz, len(si.ShardPaths)-1)
		stop()
		_ = r.Close()
		if err != nil {
			return fmt.Errorf("%s: %w", n, err)
		}
	}
	m.Shards = si
	if len(si.Missing) == 0 || si.Missing[0] != 1 {
		m.FileBytes = si.TotalBytes
	}
	return nil
}

// remoteHeaderReader streams the first 256 MiB of file name on node.
func remoteHeaderReader(ctx context.Context, node, name string) (io.ReadCloser, func(), error) {
	cctx, cancel := context.WithCancel(ctx)
	cmd := exec.CommandContext(cctx, "ssh", "-o", "BatchMode=yes", "-o", "ConnectTimeout=5", "--", node, fmt.Sprintf("head -c 268435456 '%s'", name))
	pipe, err := cmd.StdoutPipe()
	if err != nil {
		cancel()
		return nil, nil, err
	}
	if err := cmd.Start(); err != nil {
		cancel()
		return nil, nil, err
	}
	return pipe, func() { cancel(); _ = cmd.Wait() }, nil
}

// parseRemoteMeta reads the python one-liner's output: the architecture, then block_count, embedding_length,
// expert_count, expert_used_count, one per line.
func parseRemoteMeta(name, out string) (*plan.Model, error) {
	lines := strings.Fields(strings.TrimSpace(out))
	if len(lines) < 5 {
		return nil, fmt.Errorf("unexpected GGUF metadata output %q", out)
	}
	n := make([]int, 4)
	for i := range n {
		v, err := strconv.Atoi(lines[i+1])
		if err != nil {
			return nil, fmt.Errorf("unexpected GGUF metadata output %q", out)
		}
		n[i] = v
	}
	if lines[0] == "" || n[0] <= 0 {
		return nil, fmt.Errorf("%s: no %s.block_count in the metadata", name, lines[0])
	}
	return &plan.Model{Name: name, Arch: lines[0], Layers: n[0], Embd: n[1], Experts: n[2], ExpertsUsed: n[3]}, nil
}

// streamRemoteHeader parses the GGUF header of path on node as it arrives over ssh and stops the transfer once the
// tensor table is read.
func streamRemoteHeader(ctx context.Context, node, path string) (*plan.Model, error) {
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	sizeOut, err := exec.CommandContext(ctx, "ssh", "-o", "BatchMode=yes", "-o", "ConnectTimeout=5", "--", node, fmt.Sprintf("stat -L -c %%s '%s'", path)).Output()
	if err != nil {
		return nil, err
	}
	size, err := strconv.ParseInt(strings.TrimSpace(string(sizeOut)), 10, 64)
	if err != nil {
		return nil, err
	}
	cmd := exec.CommandContext(ctx, "ssh", "-o", "BatchMode=yes", "-o", "ConnectTimeout=5", "--", node, fmt.Sprintf("head -c 268435456 '%s'", path))
	pipe, err := cmd.StdoutPipe()
	if err != nil {
		return nil, err
	}
	if err := cmd.Start(); err != nil {
		return nil, err
	}
	m, perr := plan.ParseGGUF(pipe, size)
	cancel()
	_ = cmd.Wait()
	if perr != nil {
		return nil, perr
	}
	m.Name = filepath.Base(path)
	return m, nil
}

// remoteHealthy asks the machine that runs the master whether its llama-server answers /health with 200.
func remoteHealthy(ctx context.Context, node string, port int) bool {
	cctx, cancel := context.WithTimeout(ctx, 8*time.Second)
	defer cancel()
	out, err := sshScript(cctx, node, fmt.Sprintf("curl -s -m 4 -o /dev/null -w '%%{http_code}' http://127.0.0.1:%d/health\n", port))
	return err == nil && strings.TrimSpace(string(out)) == "200"
}

var runSeq atomic.Uint64

// newRunID is "run-<unix ms>": two deploys in one second (two watchdog redeploys, a script) used to share a record
// file and logs. Older "run-<unix s>" records sort before these (LastRunOptions sorts by name), and a same-ms
// collision is excluded by the counter.
func newRunID() string {
	ms := time.Now().UnixMilli()
	for {
		last := runSeq.Load()
		if uint64(ms) <= last {
			ms = int64(last) + 1
		}
		if runSeq.CompareAndSwap(last, uint64(ms)) {
			return fmt.Sprintf("run-%d", ms)
		}
	}
}
