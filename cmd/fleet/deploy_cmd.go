package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"os"
	"os/signal"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/tevfik/fleet-router/internal/client"
	"github.com/tevfik/fleet-router/internal/config"
	"github.com/tevfik/fleet-router/internal/deploy"
	"github.com/tevfik/fleet-router/internal/plan"
	"github.com/tevfik/fleet-router/internal/profile"
	"github.com/tevfik/fleet-router/internal/ringengine"
	"github.com/tevfik/fleet-router/internal/server"
)

const deployUsage = `usage: fleet deploy [<model.gguf>] [--engine ring|chain] [--profile NAME [--explore]] [--save-profile NAME]
       [--layout auto|fixed] [--strategy latency|throughput|memory|auto] [--role R] [--master M] [--workers W1,W2]
       [--port N] [--ctx N] [--np N] [--ngl N] [--cpu|--cpu-only] [--ts T] [--mmap] [--mmproj file.gguf] [--mmproj-cpu] [--mmdev D]
       [--wire f16|fp8] [--spec auto|draft-dflash|draft-mtp|draft-dspark|ngram-mod|none] [--draft-model file.gguf] [--draft-max N]
       [--chain] [--io-timeout S] [--worker-port P] [--worker-ready-timeout S] [--exit-on-fail] [--name ENDPOINT]
       [--local-weights [--worker-model NODE=PATH] [--shards-manifest F]] [--cache-type-k TYPE] [--cache-type-v TYPE]
       [--moe-cache-mib N] [--cpu-moe|--n-cpu-moe N] [--worker-cpu-experts NODE=FIRST-LAST[,...]]
       [--via-hub | --via-hub-node N1,N2 | --no-hub] [--hub URL] [--key|--hub-key KEY] [--hub-listen-base P] [--hub-rtt-ms MS]
       [--master-env KEY=VALUE]... [--worker-env NODE:KEY=VALUE]... [--master-arg ARG]...
       [--force] [--load-timeout S] [--force-split] [--dry-run] [--json]
       fleet deploy <model> --engine chain [--plan plan.json] [--pick N] [--model-dir D] [--proto csp1|v1|zmq]
       [--timeout-s S] [--embedding-offload] [--ready-timeout D] [--cpu-moe|--n-cpu-moe N] [--moe-cache-mib N]
       [--port P] [--master M] [--workers W] [--ctx C] [--np N] [--lanes L] [--role R] [--name N]
       [--spec S] [--draft-model D] [--draft-max K] [--wire W] [--relay | --via-hub [--hub URL]]
       [--auto] [--dry-run] [--json]
The model is the positional argument, or the profile's. With --profile, every flag given explicitly overrides the
profile's value. --layout auto lets the planner choose the split from the machines' free memory, rates, round trips
and the profile's measurements (docs/profiles.md); fixed (the default without a profile) uses --ts / the generic split.`

// deployFlags are the flags of `fleet deploy` (and of `fleet profile save`, which stores them).
type deployFlags struct {
	fs *flag.FlagSet

	strat, role, master, workers, ts, mmproj, mmdev, wire, spec, draftModel, cacheK, cacheV   *string
	workerCPUExperts, shardsManifest, viaHubNodes, hubURL, layout, profileName, saveProfile   *string
	hubKey, name                                                                              *string
	port, ctxLen, np, ngl, draftMax, workerPort, workerReady, ioTimeout, moeCacheMiB, ncpuMoE *int
	hubListenBase, loadTimeout                                                                *int
	hubRTT                                                                                    *float64
	cpu, cpuOnly, mmap, mmprojCPU, chain, exitOnFail, cpuMoE, dryRun, forceSplit, force       *bool
	localWeights, viaHub, noHub, asJSON, explore                                              *bool

	workerModels map[string]string
	masterEnv    map[string]string
	workerEnv    map[string]map[string]string
	masterArgs   []string
}

// deployValueFlags are the deploy flags that take a value (for flagsFirst).
var deployValueFlags = []string{"strategy", "role", "master", "workers", "worker-model", "shards-manifest", "port", "ctx", "np", "ngl", "ts", "mmproj", "mmdev", "wire", "spec", "draft-model", "draft-max", "cache-type-k", "cache-type-v", "moe-cache-mib", "n-cpu-moe", "worker-cpu-experts", "io-timeout", "load-timeout", "worker-port", "worker-ready-timeout", "name", "via-hub-node", "hub", "hub-listen-base", "hub-rtt-ms", "layout", "profile", "save-profile", "master-env", "worker-env", "master-arg", "engine", "key", "hub-key"}

func newDeployFlags(name string) *deployFlags {
	fs := flag.NewFlagSet(name, flag.ContinueOnError)
	d := &deployFlags{fs: fs, workerModels: map[string]string{}, masterEnv: map[string]string{}, workerEnv: map[string]map[string]string{}}
	fs.String("engine", "ring", "engine: ring (Protocol 1, default) | chain (Protocol 2 / stage-pipe)")
	d.hubKey = fs.String("key", "", "API key for the hub raw TCP stream connect (default: $FLEET_API_KEY, ~/.config/fleet/.env, or hub_api_key)")
	fs.StringVar(d.hubKey, "hub-key", "", "alias for --key")
	d.profileName = fs.String("profile", "", "deploy a saved profile (fleet profile list); flags given explicitly override its values")
	d.saveProfile = fs.String("save-profile", "", "save this deploy's flags (and the model) as a profile under this name")
	d.explore = fs.Bool("explore", false, "with an auto layout: deploy the planner's best candidate that has no measurement in the profile yet, to measure it")
	d.layout = fs.String("layout", "", "auto: the planner picks the split (machine resources, round trips, measurements); fixed: --ts / the generic split (default: the profile's, fixed without a profile)")
	d.strat = fs.String("strategy", "auto", "QoS strategy: latency | throughput | memory | auto")
	d.role = fs.String("role", "", "role to map the deployed model to (e.g. coder, architect)")
	d.master = fs.String("master", "", "master node (default: this machine)")
	d.workers = fs.String("workers", "", "comma-separated worker nodes (default: none, the model runs on the master alone; with --layout auto: the machines the planner may use)")
	d.port = fs.Int("port", 18090, "port for the master llama-server endpoint")
	d.ctxLen = fs.Int("ctx", 4096, "context length")
	d.np = fs.Int("np", 0, "parallel slots (0: determined by strategy)")
	d.ts = fs.String("ts", "", "custom tensor split (e.g. 15,13,12)")
	d.ngl = fs.Int("ngl", 0, "layers offloaded to GPU (0: determined by strategy)")
	d.cpu = fs.Bool("cpu", false, "force execution on CPU (disable GPU offload)")
	d.cpuOnly = fs.Bool("cpu-only", false, "force execution on CPU (alias for --cpu)")
	d.mmap = fs.Bool("mmap", true, "memory-map model from disk (demand paging)")
	d.mmproj = fs.String("mmproj", "", "path to multimodal projector GGUF file")
	d.mmprojCPU = fs.Bool("mmproj-cpu", false, "run multimodal projector on CPU instead of GPU VRAM")
	d.mmdev = fs.String("mmdev", "", "device for multimodal projector (e.g. none, RING0)")
	d.wire = fs.String("wire", "", "activation format between machines: f16 | fp8 (default: f32, exact; the smaller formats are lossy, use them only for models that passed ring-gate)")
	d.spec = fs.String("spec", "auto", "speculative decoding: auto | draft-dflash | draft-mtp | draft-dspark | ngram-mod | none (auto: draft-mtp when the GGUF has NextN heads, the draft model's type when one is given, else none)")
	d.draftModel = fs.String("draft-model", "", "path to draft GGUF model for speculative decoding")
	d.draftMax = fs.Int("draft-max", 0, "maximum draft tokens per speculative step (0: automatic, draft-dflash 2 and otherwise 4; larger windows lowered acceptance and speed, so raise it only on purpose)")
	d.chain = fs.Bool("chain", false, "EXPERIMENTAL worker-to-worker forwarding (needs >= 2 workers; verified on loopback/netsim, not on a real WAN)")
	d.workerPort = fs.Int("worker-port", 0, "ring port of the first worker, worker i listens on it+i (0: 50152)")
	d.name = fs.String("name", "", "endpoint name to register (default: distributed-<model>); an existing endpoint of that name is replaced")
	d.workerReady = fs.Int("worker-ready-timeout", 0, "seconds a started worker gets to listen on its port before the deploy fails with its log (0: 120; a worker indexing a large --local-weights file needs the time)")
	d.ioTimeout = fs.Int("io-timeout", 0, "GGML_RING_IO_TIMEOUT_S for master and workers: seconds a worker may stay silent before the request fails; for decode-sized ops the engine adds 0.1 s per token, uploads and first-use ops get 600 s (not given: engine default, 15)")
	d.exitOnFail = fs.Bool("exit-on-fail", false, "set GGML_RING_EXIT_ON_FAIL=1 on the master: ONE failed ring request makes the whole server exit (code 75, 2 s later; all other sessions and the loaded model are lost) so a supervisor (`fleet watchdog`, systemd) restarts it; without a supervisor the endpoint stays dead. Off by default; the watchdog's own redeploys always set it")
	d.cacheK = fs.String("cache-type-k", "", "KV cache data type for K (f16, q8_0, q4_0)")
	d.cacheV = fs.String("cache-type-v", "", "KV cache data type for V (f16, q8_0, q4_0)")
	d.moeCacheMiB = fs.Int("moe-cache-mib", 0, "GPU cache size in MiB for host-resident MoE experts (e.g. 3072)")
	d.cpuMoE = fs.Bool("cpu-moe", false, "keep all MoE expert weights in host CPU RAM (with hot expert cache on GPU)")
	d.ncpuMoE = fs.Int("n-cpu-moe", 0, "keep MoE expert weights of first N layers in host CPU RAM")
	d.workerCPUExperts = fs.String("worker-cpu-experts", "", "NODE=FIRST-LAST[,NODE2=FIRST-LAST]: keep the MoE expert weights of these blocks (inclusive) in that worker's CPU RAM; the worker runs with -d CUDA0,CPU and the blocks must lie in the range its GPU gets by -ts (with --ts list only the GPU devices, fleet appends the 0s)")
	d.dryRun = fs.Bool("dry-run", false, "print deployment plan and commands without launching")
	d.forceSplit = fs.Bool("force-split", false, "deploy the split given with --workers even when the planner estimates one machine is at least 1.3x faster (below 1.3x it only warns; --ts is always authoritative)")
	d.force = fs.Bool("force", false, "deploy although the memory pre-check says the model (weights + KV cache) does not fit a machine's free RAM + VRAM: the engine then pages the weights from disk (mmap), which is very slow and can make the machine unresponsive")
	d.loadTimeout = fs.Int("load-timeout", 0, "seconds the started master gets to become healthy (0: 1200). When it runs out, or on Ctrl+C while it loads, NOTHING is stopped (a large model may still be loading): the message says what runs and how to stop it")
	d.localWeights = fs.Bool("local-weights", false, "workers that already hold the model file (same absolute path as on the master, or --worker-model) serve the weights from it instead of receiving them over the wire; the file size is checked over SSH first, a different size is warned about and not used")
	fs.Func("worker-model", "NODE=PATH of the model file on a worker (repeatable; with --local-weights)", func(v string) error {
		n, p, ok := strings.Cut(v, "=")
		if !ok || n == "" || p == "" {
			return fmt.Errorf("--worker-model wants NODE=PATH, got %q", v)
		}
		d.workerModels[n] = p
		return nil
	})
	d.shardsManifest = fs.String("shards-manifest", "", "fleet-shards.json for the SHA-256 check on the workers (with --local-weights; default: fleet-shards.json next to the model)")
	d.viaHub = fs.Bool("via-hub", false, "reach every worker through the hub's raw TCP streams (fleet stream connect on the master; RING_SERVERS=127.0.0.1:<port>); adds latency. Without a direct LAN IP, fleet refuses unless --via-hub, --via-hub-node or --no-hub is given")
	d.viaHubNodes = fs.String("via-hub-node", "", "comma-separated workers to reach through the hub (the others stay direct)")
	d.noHub = fs.Bool("no-hub", false, "dial a worker without a direct IP by hostname instead of refusing (requires --via-hub otherwise)")
	d.hubURL = fs.String("hub", "", "hub for --via-hub (default: hub_url of the config, else "+config.HubDefaultHelp()+")")
	d.hubListenBase = fs.Int("hub-listen-base", 0, "first local port of the hub streams on the master; worker i of the ring gets base+i (0: 51000)")
	d.hubRTT = fs.Float64("hub-rtt-ms", 0, "round trip the auto layout assumes for a worker through the hub (0: hub_rtt_ms of the config, else an estimate of 10 ms)")
	fs.Func("master-env", "KEY=VALUE extra environment of the master (repeatable; plain values only)", func(v string) error {
		k, val, ok := strings.Cut(v, "=")
		if !ok || k == "" {
			return fmt.Errorf("--master-env wants KEY=VALUE, got %q", v)
		}
		d.masterEnv[k] = val
		return nil
	})
	fs.Func("worker-env", "NODE:KEY=VALUE extra environment of a worker (repeatable)", func(v string) error {
		n, kv, ok1 := strings.Cut(v, ":")
		k, val, ok2 := strings.Cut(kv, "=")
		if !ok1 || !ok2 || n == "" || k == "" {
			return fmt.Errorf("--worker-env wants NODE:KEY=VALUE, got %q", v)
		}
		if d.workerEnv[n] == nil {
			d.workerEnv[n] = map[string]string{}
		}
		d.workerEnv[n][k] = val
		return nil
	})
	fs.Func("master-arg", "extra llama-server argument(s) of the master, split on spaces (repeatable; e.g. --master-arg \"--temp 0\")", func(v string) error {
		d.masterArgs = append(d.masterArgs, strings.Fields(v)...)
		return nil
	})
	d.asJSON = fs.Bool("json", false, "print deployment result as JSON")
	return d
}

func splitList(s string) []string {
	var out []string
	for _, w := range strings.Split(s, ",") {
		if w = strings.TrimSpace(w); w != "" {
			out = append(out, w)
		}
	}
	return out
}

// options builds the deploy options of the parsed flags.
func (d *deployFlags) options(model string) (deploy.DeployOptions, error) {
	cpuExperts, err := deploy.ParseWorkerCPUExperts(*d.workerCPUExperts)
	if err != nil {
		return deploy.DeployOptions{}, err
	}
	var wm map[string]string
	if len(d.workerModels) > 0 {
		wm = d.workerModels
	}
	o := deploy.DeployOptions{
		ViaHub: *d.viaHub, ViaHubNodes: splitList(*d.viaHubNodes), NoHub: *d.noHub, HubURL: *d.hubURL, HubListenBase: *d.hubListenBase,
		HubRTTms:  *d.hubRTT,
		HubKey:    *d.hubKey,
		ModelPath: model, Strategy: deploy.ParseStrategy(*d.strat), MasterNode: *d.master, WorkerNodes: splitList(*d.workers),
		Role: *d.role, Port: *d.port, Ctx: *d.ctxLen, Np: *d.np, NGL: *d.ngl, CPUOnly: *d.cpu || *d.cpuOnly, TensorSplit: *d.ts,
		MMap: *d.mmap, MMProj: *d.mmproj, MMProjCPU: *d.mmprojCPU, MMProjDevice: *d.mmdev, Wire: *d.wire,
		SpecType: *d.spec, DraftModel: *d.draftModel, DraftMax: *d.draftMax, Chain: *d.chain, IOTimeoutS: *d.ioTimeout, EndpointName: *d.name,
		WorkerPort: *d.workerPort, WorkerReadyS: *d.workerReady, ExitOnFail: *d.exitOnFail, LocalWeights: *d.localWeights,
		WorkerModels: wm, ShardsManifest: *d.shardsManifest, CacheTypeK: *d.cacheK, CacheTypeV: *d.cacheV,
		MoECacheMiB: *d.moeCacheMiB, CPUMoE: *d.cpuMoE, NCPUMoE: *d.ncpuMoE, WorkerCPUExperts: cpuExperts,
		DryRun: *d.dryRun, Force: *d.force, LoadTimeoutS: *d.loadTimeout, ConfigPath: config.Path(),
	}
	if len(d.masterEnv) > 0 {
		o.MasterEnvExtra = d.masterEnv
	}
	if len(d.workerEnv) > 0 {
		o.WorkerEnvExtra = d.workerEnv
	}
	if len(d.masterArgs) > 0 {
		o.MasterArgs = d.masterArgs
	}
	return o, deploy.ValidateExtras(o)
}

// profileArgs turns a profile into deploy flags (the model is not among them). skipSplit leaves the pinned split
// (ts / worker_cpu_experts) out: the user asked for --layout auto.
func profileArgs(p *profile.Profile, skipSplit bool) []string {
	var a []string
	str := func(flag, v string) {
		if v != "" {
			a = append(a, "--"+flag, v)
		}
	}
	num := func(flag string, v int) {
		if v != 0 {
			a = append(a, "--"+flag, strconv.Itoa(v))
		}
	}
	on := func(flag string, v bool) {
		if v {
			a = append(a, "--"+flag)
		}
	}
	str("master", p.Master)
	str("workers", strings.Join(p.Workers, ","))
	str("role", p.Role)
	str("strategy", p.Strategy)
	num("port", p.Port)
	num("ctx", p.Ctx)
	num("np", p.Np)
	num("ngl", p.NGL)
	on("cpu", p.CPUOnly)
	str("layout", p.Layout.Mode)
	if !skipSplit {
		str("ts", p.Layout.TS)
		if len(p.Layout.WorkerCPUExperts) > 0 {
			var parts []string
			for _, n := range sortedKeys(p.Layout.WorkerCPUExperts) {
				parts = append(parts, n+"="+p.Layout.WorkerCPUExperts[n])
			}
			str("worker-cpu-experts", strings.Join(parts, ","))
		}
	}
	on("via-hub", p.Layout.ViaHub)
	str("via-hub-node", strings.Join(p.Layout.ViaHubNodes, ","))
	on("no-hub", p.Layout.NoHub)
	str("hub", p.Layout.Hub)
	num("hub-listen-base", p.Layout.HubListenBase)
	num("worker-port", p.Layout.WorkerPort)
	if p.Layout.HubRTTms > 0 {
		a = append(a, "--hub-rtt-ms", strconv.FormatFloat(p.Layout.HubRTTms, 'f', -1, 64))
	}
	str("spec", p.Spec.Type)
	str("draft-model", p.Spec.DraftModel)
	num("draft-max", p.Spec.DraftMax)
	str("cache-type-k", p.Cache.K)
	str("cache-type-v", p.Cache.V)
	on("cpu-moe", p.MoE.CPU)
	num("n-cpu-moe", p.MoE.NCPU)
	num("moe-cache-mib", p.MoE.CacheMiB)
	str("mmproj", p.MMProj.Path)
	on("mmproj-cpu", p.MMProj.CPU)
	str("mmdev", p.MMProj.Device)
	str("wire", p.Wire)
	if p.MMap != nil {
		a = append(a, "--mmap="+strconv.FormatBool(*p.MMap))
	}
	num("io-timeout", p.IOTimeout)
	num("worker-ready-timeout", p.WorkerReadyTimeout)
	on("exit-on-fail", p.ExitOnFail)
	on("local-weights", p.LocalWeights)
	for _, n := range sortedKeys(p.WorkerModels) {
		a = append(a, "--worker-model", n+"="+p.WorkerModels[n])
	}
	str("shards-manifest", p.ShardsManifest)
	on("chain", p.Chain)
	on("force", p.Force)
	for _, k := range sortedKeys(p.Env.Master) {
		a = append(a, "--master-env", k+"="+p.Env.Master[k])
	}
	nodes := make([]string, 0, len(p.Env.Workers))
	for n := range p.Env.Workers {
		nodes = append(nodes, n)
	}
	sort.Strings(nodes)
	for _, n := range nodes {
		for _, k := range sortedKeys(p.Env.Workers[n]) {
			a = append(a, "--worker-env", n+":"+k+"="+p.Env.Workers[n][k])
		}
	}
	for _, x := range p.ExtraMasterArgs {
		a = append(a, "--master-arg", x)
	}
	return a
}

func sortedKeys(m map[string]string) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

// profileFromFlags is the profile of the flags that were set (explicitly, or by a loaded profile) and the model.
func (d *deployFlags) profileFromFlags(name, model string) (*profile.Profile, error) {
	set := map[string]bool{}
	d.fs.Visit(func(f *flag.Flag) { set[f.Name] = true })
	p := &profile.Profile{Name: name, Model: model}
	if set["master"] {
		p.Master = *d.master
	}
	if set["workers"] {
		p.Workers = splitList(*d.workers)
	}
	if set["role"] {
		p.Role = *d.role
	}
	if set["strategy"] {
		p.Strategy = *d.strat
	}
	if set["port"] {
		p.Port = *d.port
	}
	if set["ctx"] {
		p.Ctx = *d.ctxLen
	}
	if set["np"] {
		p.Np = *d.np
	}
	if set["ngl"] {
		p.NGL = *d.ngl
	}
	p.CPUOnly = *d.cpu || *d.cpuOnly
	if set["layout"] {
		p.Layout.Mode = *d.layout
	}
	p.Layout.TS = *d.ts
	if *d.workerCPUExperts != "" {
		cpx, err := deploy.ParseWorkerCPUExperts(*d.workerCPUExperts)
		if err != nil {
			return nil, err
		}
		p.Layout.WorkerCPUExperts = map[string]string{}
		for n, r := range cpx {
			p.Layout.WorkerCPUExperts[n] = fmt.Sprintf("%d-%d", r[0], r[1])
		}
	}
	if p.Layout.Mode == "" {
		p.Layout.Mode = profile.LayoutAuto // a new profile plans its split unless it pins one
		if p.Layout.TS != "" || len(p.Layout.WorkerCPUExperts) > 0 {
			p.Layout.Mode = profile.LayoutFixed
		}
	}
	p.Layout.ViaHub, p.Layout.ViaHubNodes, p.Layout.NoHub, p.Layout.Hub = *d.viaHub, splitList(*d.viaHubNodes), *d.noHub, *d.hubURL
	p.Layout.HubListenBase, p.Layout.WorkerPort, p.Layout.HubRTTms = *d.hubListenBase, *d.workerPort, *d.hubRTT
	if set["spec"] {
		p.Spec.Type = *d.spec
	}
	p.Spec.DraftModel, p.Spec.DraftMax = *d.draftModel, *d.draftMax
	p.Cache = profile.Cache{K: *d.cacheK, V: *d.cacheV}
	p.MoE = profile.MoE{CPU: *d.cpuMoE, NCPU: *d.ncpuMoE, CacheMiB: *d.moeCacheMiB}
	p.MMProj = profile.MMProj{Path: *d.mmproj, CPU: *d.mmprojCPU, Device: *d.mmdev}
	p.Wire = *d.wire
	if set["mmap"] {
		v := *d.mmap
		p.MMap = &v
	}
	p.IOTimeout, p.WorkerReadyTimeout, p.ExitOnFail = *d.ioTimeout, *d.workerReady, *d.exitOnFail
	p.LocalWeights, p.ShardsManifest, p.Chain, p.Force = *d.localWeights, *d.shardsManifest, *d.chain, *d.force
	if len(d.workerModels) > 0 {
		p.WorkerModels = d.workerModels
	}
	if len(d.masterEnv) > 0 {
		p.Env.Master = d.masterEnv
	}
	if len(d.workerEnv) > 0 {
		p.Env.Workers = d.workerEnv
	}
	p.ExtraMasterArgs = d.masterArgs
	if o, err := d.options(model); err != nil {
		return nil, err
	} else if err := deploy.ValidateExtras(o); err != nil {
		return nil, err
	}
	return p, p.Validate()
}

// deployRequest is a parsed `fleet deploy`: flags, the profile it came from, the options.
type deployRequest struct {
	flags   *deployFlags
	prof    *profile.Profile // nil without --profile
	model   string
	mode    string // auto | fixed
	opts    deploy.DeployOptions
	userSet map[string]bool // flags the user gave (not from the profile)
}

// parseDeploy reads the deploy arguments: the profile's values first, the explicit flags over them.
func parseDeploy(args []string, store profile.Store) (*deployRequest, error) {
	req := &deployRequest{userSet: map[string]bool{}}
	user := newDeployFlags("deploy")
	user.fs.SetOutput(io.Discard)
	if err := user.fs.Parse(flagsFirst(args, deployValueFlags...)); err != nil {
		return nil, fmt.Errorf("%v\n%s", err, deployUsage)
	}
	user.fs.Visit(func(f *flag.Flag) { req.userSet[f.Name] = true })
	if user.fs.NArg() > 1 {
		return nil, fmt.Errorf("%s", deployUsage)
	}
	all := flagsFirst(args, deployValueFlags...)
	if name := *user.profileName; name != "" {
		p, err := store.Load(name)
		if err != nil {
			return nil, err
		}
		req.prof = p
		skip := *user.layout == profile.LayoutAuto
		all = append(profileArgs(p, skip), all...)
	}
	d := newDeployFlags("deploy")
	d.fs.SetOutput(io.Discard)
	if err := d.fs.Parse(all); err != nil {
		return nil, fmt.Errorf("%v\n%s", err, deployUsage)
	}
	req.flags = d
	req.model = d.fs.Arg(0)
	if req.model == "" && req.prof != nil {
		req.model = req.prof.Model
	}
	if req.model == "" {
		return nil, fmt.Errorf("%s", deployUsage)
	}
	// the layout mode: an explicit --layout; else a split given on the command line pins it; else the profile's;
	// else fixed (a plain deploy keeps the generic split)
	switch {
	case req.userSet["layout"]:
		req.mode = *d.layout
	case req.userSet["ts"] || req.userSet["worker-cpu-experts"]:
		req.mode = profile.LayoutFixed
	case req.prof != nil:
		req.mode = req.prof.EffectiveMode()
	case *d.layout != "":
		req.mode = *d.layout
	default:
		req.mode = profile.LayoutFixed
	}
	if req.mode != profile.LayoutAuto && req.mode != profile.LayoutFixed {
		return nil, fmt.Errorf("--layout %q: auto or fixed", req.mode)
	}
	o, err := d.options(req.model)
	if err != nil {
		return nil, err
	}
	req.opts = o
	return req, nil
}

// Test seams of the deploy command: nothing here may start a process or reach a network in tests.
var (
	executeDeployment = func(ctx context.Context, cfg *config.Config, o deploy.DeployOptions) (*deploy.DeployResult, error) {
		if !o.DryRun {
			// the run opens in the daemon's run store (fleet run list / stop / end find it); no daemon, no record there
			if cl := client.New(listen()); cl.Alive() {
				deploy.OnState = func(r deploy.DeployResult) { _ = cl.NoteDeploy(deployNote(r)) }
				defer func() { deploy.OnState = nil }()
			}
		}
		return deploy.ExecuteDeploymentWithProgress(ctx, cfg, o, newProgressPrinter(os.Stderr, isTerminal(os.Stderr), time.Now))
	}
	deployClient            = func() (*client.Client, error) { return mustClient() }
	profileStore            = profile.Default
	readModelMeta           = deploy.ReadModelMeta
	deployOut     io.Writer = os.Stdout
)

// isTerminal says whether f is a terminal (progress is redrawn in place there, line by line elsewhere).
func isTerminal(f *os.File) bool {
	st, err := f.Stat()
	return err == nil && st.Mode()&os.ModeCharDevice != 0
}

// loadingEvery is how often the "loading" progress line is repeated when output is not a terminal.
const loadingEvery = 30 * time.Second

// newProgressPrinter prints the deploy's progress to w as it happens. On a terminal the "loading … (Ns)" line is redrawn
// in place; on a pipe or a log file (ssh without a tty, nohup, CI) every step is its own line and the loading line
// comes every loadingEvery, so the output is not silent for the minutes a large model takes.
func newProgressPrinter(w io.Writer, tty bool, now func() time.Time) func(string) {
	var last time.Time
	inPlace := false
	return func(line string) {
		loading := strings.Contains(line, "Loading model layers")
		switch {
		case tty && loading:
			fmt.Fprintf(w, "\r\033[K%s", line)
			inPlace = true
		case tty:
			if inPlace {
				fmt.Fprintln(w)
				inPlace = false
			}
			fmt.Fprintln(w, line)
		case loading:
			if t := now(); last.IsZero() || t.Sub(last) >= loadingEvery {
				last = t
				fmt.Fprintln(w, line)
			}
		default:
			fmt.Fprintln(w, line)
		}
	}
}

// cmdDeploy launches a distributed model deployment across machines using QoS strategies.
func cmdDeploy(args []string) error {
	eng, err := engineOf(args)
	if err != nil {
		return err
	}
	if eng == "chain" || eng == "stage-pipe" {
		return cmdDeployStagePipe(args) // a separate path: the ring below is not touched
	}
	if len(args) == 0 {
		return fmt.Errorf("%s", deployUsage)
	}
	store := profileStore()
	req, err := parseDeploy(args, store)
	if err != nil {
		return err
	}
	d, opts := req.flags, req.opts

	cfg, err := config.Load(config.Path())
	if err != nil {
		cfg = config.Default()
	}

	if *d.saveProfile != "" {
		p, err := d.profileFromFlags(*d.saveProfile, req.model)
		if err != nil {
			return err
		}
		if old, err := store.Load(p.Name); err == nil {
			p.Runs = old.Runs // the measurements stay with the name
		}
		if err := store.Save(p); err != nil {
			return err
		}
		fmt.Fprintf(os.Stderr, "✓ profile %s saved (%s)\n", p.Name, store.Dir)
	}

	choice := "fixed"
	if req.mode == profile.LayoutAuto {
		why, err := autoLayout(cfg, &opts, req.prof, *d.explore, *d.hubRTT)
		if err != nil {
			return err
		}
		choice = why
	} else if len(opts.WorkerNodes) > 0 && opts.TensorSplit == "" {
		// The planner's verdict (single machine vs split) is checked before anything starts; an explicit --ts stays
		// authoritative and skips the check.
		if err := guardSplit(os.Stderr, req.model, opts.MasterNode, opts.WorkerNodes, opts.Ctx, opts.Np, *d.forceSplit); err != nil {
			return err
		}
	}

	// Ctrl+C / SIGTERM cancel the deploy's context: ExecuteDeploymentWithProgress decides what is stopped (nothing once
	// the master is loading: it says so and how to stop it) instead of the process dying with children half started.
	ctx, stopSignals := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stopSignals()
	res, err := executeDeployment(ctx, cfg, opts)
	if err != nil {
		return err
	}
	if !opts.DryRun {
		reloadDaemon()
	}
	recordName := *d.saveProfile
	if recordName == "" && req.prof != nil {
		recordName = req.prof.Name
	}
	var measuredLine string
	if recordName != "" {
		if p, err := store.Load(recordName); err == nil {
			measuredLine = p.MeasuredLine()
			if !opts.DryRun {
				p.AddRun(runOf(res, opts, choice))
				if err := store.Save(p); err != nil {
					fmt.Fprintln(os.Stderr, "warning: the run was not recorded in the profile:", err)
				}
			}
		}
	}
	printDeploy(deployOut, res, *d.asJSON, choice, measuredLine)
	return nil
}

// reloadDaemon tells the running fleet daemon that the configuration file changed: the deploy has just registered
// (or replaced) its endpoint there, and a daemon that keeps its old copy routes nothing to it, and `fleet run end` of a
// run bound to it would stop the processes of the previous registration (another port, other workers). Best effort.
func reloadDaemon() {
	if cl := clientIfAlive(); cl != nil {
		_ = cl.Reload()
	}
}

func printDeploy(w io.Writer, res *deploy.DeployResult, asJSON bool, choice, measured string) {
	if asJSON {
		b, _ := json.MarshalIndent(res.Redacted(), "", "  ")
		fmt.Fprintln(w, string(b))
		return
	}
	fmt.Fprintf(w, "\n🚀 Fleet Distributed Deploy: %s\n", res.EndpointName)
	fmt.Fprintf(w, "   Strategy:      %s\n", res.Strategy)
	fmt.Fprintf(w, "   URL:           %s\n", res.URL)
	if res.Role != "" {
		fmt.Fprintf(w, "   Role:          %s\n", res.Role)
	}
	fmt.Fprintf(w, "   Master [%s]:   PID %d\n", res.MasterNode, res.MasterPID)
	fmt.Fprintf(w, "   Devices / TS:  %s | %s\n", res.Devices, res.TensorSplit)
	fmt.Fprintf(w, "   Layout:        %s\n", choice)
	if res.EstimatedTPS > 0 {
		fmt.Fprintf(w, "   Baseline est.: %.1f tok/s (%.1f ms/token, the planner's estimate), no speculation gain included\n", res.EstimatedTPS, res.LatencyMS)
	} else {
		fmt.Fprintf(w, "   Baseline est.: none (no plan candidate and no weight_gbps in the nodes: section; fleet plan / fleet profile bench give figures)\n")
	}
	if measured != "" {
		fmt.Fprintf(w, "   Measured:      %s\n", measured)
	}
	if res.MasterEngine != "" {
		fmt.Fprintf(w, "   Master engine: %s\n", res.MasterEngine)
	}
	fmt.Fprintf(w, "   Status:        %s\n", res.Status)
	for _, x := range res.Warnings {
		fmt.Fprintf(w, "   WARNING:       %s\n", x)
	}
	fmt.Fprintln(w)
	if len(res.WorkerCmds) > 0 {
		fmt.Fprintln(w, "Worker commands:")
		for _, node := range sortedKeys(res.WorkerCmds) {
			fmt.Fprintf(w, "  [%s]: %s\n", node, res.WorkerCmds[node])
		}
		fmt.Fprintln(w)
	}
	if len(res.StreamCmds) > 0 {
		fmt.Fprintf(w, "Hub streams (on the master %s, in the background, before it starts; log ~/.cache/fleet/runs/%s-stream-<worker>.log):\n", res.MasterNode, res.RunID)
		for _, node := range sortedKeys(res.StreamCmds) {
			if pid := res.StreamPIDs[node]; pid > 0 {
				fmt.Fprintf(w, "  [%s]: %s   (PID %d)\n", node, res.StreamCmds[node], pid)
			} else {
				fmt.Fprintf(w, "  [%s]: %s\n", node, res.StreamCmds[node])
			}
		}
		fmt.Fprintln(w)
	}
	fmt.Fprintf(w, "Master command:\n  %s\n  %s\n", res.MasterEnv, res.MasterCmd)
}

// runOf is the profile record of a deploy: the concrete layout, in the order it was asked for (so that deploying it
// again gives the same plan).
func runOf(res *deploy.DeployResult, opts deploy.DeployOptions, choice string) profile.Run {
	l := profile.RunLayout{Master: res.MasterNode, Devices: res.Devices, SpecType: opts.SpecType, DraftMax: opts.DraftMax}
	if l.SpecType == "" {
		l.SpecType = "auto"
	}
	if c := opts.Candidate; c != nil && len(c.Parts) > 1 {
		cl := profile.CandidateLayout(*c, l.SpecType, l.DraftMax, nil)
		l.Workers, l.TS, l.CPUExperts, l.FromPlan, l.OutputOnMaster = cl.Workers, cl.TS, cl.CPUExperts, true, cl.OutputOnMaster
	} else {
		l.Workers = append([]string(nil), opts.WorkerNodes...)
		l.TS = opts.TensorSplit
		if l.TS == "" && len(l.Workers) > 0 {
			f := strings.Split(res.TensorSplit, ",")
			if len(f) > len(l.Workers)+1 {
				f = f[:len(l.Workers)+1] // the CPU devices of --worker-cpu-experts are appended with 0
			}
			if strings.HasPrefix(res.Devices, "RING0,CUDA0") && len(f) == 2 {
				f[0], f[1] = f[1], f[0] // the NextN order: recorded as asked (master first)
			}
			l.TS = strings.Join(f, ",")
		}
		for n, r := range opts.WorkerCPUExperts {
			if l.CPUExperts == nil {
				l.CPUExperts = map[string]string{}
			}
			l.CPUExperts[n] = fmt.Sprintf("%d-%d", r[0], r[1])
		}
	}
	for n := range res.HubWorkers {
		l.ViaHubNodes = append(l.ViaHubNodes, n)
	}
	sort.Strings(l.ViaHubNodes)
	return profile.Run{At: time.Now().UTC(), RunID: res.RunID, Endpoint: res.EndpointName, URL: res.URL, StatsPort: res.StatsPortUsed(), Layout: l,
		PlannedTPS: res.EstimatedTPS, Choice: choice, FleetVersion: version, EngineVersion: ringengine.Version()}
}

// autoLayout asks the daemon's planner for the candidates over the profile's machines, calibrates the plan with the
// profile's measured traces, and applies the choice (profile.Choose) to opts. It returns why the layout was chosen.
func autoLayout(cfg *config.Config, opts *deploy.DeployOptions, p *profile.Profile, explore bool, hubRTT float64) (string, error) {
	cl, err := deployClient()
	if err != nil {
		return "", fmt.Errorf("--layout auto needs the daemon (the machines' benchmarks and free memory): %w; use --layout fixed", err)
	}
	master := opts.MasterNode
	if master == "" {
		master = deploy.LocalHostname()
	}
	m, err := readModelMeta(master, opts.ModelPath)
	if err != nil {
		return "", fmt.Errorf("--layout auto: %w", err)
	}
	req := server.PlanRequest{
		Model: *m, Ctx: opts.Ctx, Np: max(opts.Np, 1), Master: master,
		ExpertTier:  m.Experts > 0,
		KVBlocks:    m.KVBlocks,
		KVFactor:    deploy.KVCacheFactor(opts.CacheTypeK, opts.CacheTypeV),
		CacheTypeK:  opts.CacheTypeK,
		CacheTypeV:  opts.CacheTypeV,
		CPUMoE:      opts.CPUMoE || opts.NCPUMoE > 0,
		MoECacheMiB: opts.MoECacheMiB,
	}
	if len(opts.WorkerNodes) > 0 {
		req.Nodes = append([]string{master}, opts.WorkerNodes...)
	}
	// a worker reached through the hub pays the hub round trip (declared, else the planner's estimate)
	if hubRTT <= 0 {
		hubRTT = cfg.HubRTTms
	}
	if hubRTT <= 0 {
		hubRTT = plan.DefaultHubRTTms
	}
	hubNodes := opts.ViaHubNodes
	if opts.ViaHub {
		hubNodes = opts.WorkerNodes
	}
	for _, w := range hubNodes {
		if req.RTT == nil {
			req.RTT = map[string]float64{}
		}
		req.RTT[master+">"+w] = hubRTT
	}
	if p == nil {
		p = &profile.Profile{Model: opts.ModelPath, Spec: profile.Spec{Type: opts.SpecType, DraftMax: opts.DraftMax}}
	}
	r, err := cl.Plan(req)
	if err != nil {
		return "", err
	}
	note := ""
	if k, run, ok := profile.CalibrationFor(r, p, 0); ok {
		req.FixedMS, req.RingMS = k.FixedMS, k.RingMS
		if r2, err := cl.Plan(req); err == nil {
			r = r2
			note = fmt.Sprintf("; plan calibrated by the trace of run %s (fixed %.1f ms, ring %.1f ms)", run.RunID, k.FixedMS, k.RingMS)
		}
	}
	if len(r.Candidates) == 0 && opts.Force {
		return fmt.Sprintf("fixed (forced: planner has no fitting candidate — %s)", r.Verdict), nil
	}
	ch, err := profile.Choose(r, p, explore, hubNodes)
	if err != nil {
		return "", err
	}
	if ch.Measured != nil {
		l := ch.Measured.Layout
		opts.MasterNode, opts.WorkerNodes = l.Master, append([]string(nil), l.Workers...)
		opts.Candidate, opts.WorkerCPUExperts, opts.TensorSplit = nil, nil, ""
		if l.FromPlan {
			c, err := l.Candidate()
			if err != nil {
				return "", err
			}
			opts.Candidate = c
		} else {
			opts.TensorSplit = l.TS
			for n, rg := range l.CPUExperts {
				f, e, err := profile.ParseRange(rg)
				if err != nil {
					return "", err
				}
				if opts.WorkerCPUExperts == nil {
					opts.WorkerCPUExperts = map[string][2]int{}
				}
				opts.WorkerCPUExperts[n] = [2]int{f, e}
			}
		}
	} else {
		c := *ch.Candidate
		opts.Candidate, opts.MasterNode, opts.TensorSplit, opts.WorkerCPUExperts = &c, c.Master, "", nil
		opts.WorkerNodes = nil
		for _, pt := range c.Parts {
			if pt.Node != c.Master {
				opts.WorkerNodes = append(opts.WorkerNodes, pt.Node)
			}
		}
	}
	return "auto, " + ch.Reason + note, nil
}

// deployNote is a deploy's record as the daemon's run store takes it.
func deployNote(r deploy.DeployResult) server.DeployNote {
	model := ""
	if r.Options != nil {
		model = filepath.Base(r.Options.ModelPath)
	}
	nodes := append([]string{r.MasterNode}, r.WorkerNodes...)
	engine := "llama-ring"
	if r.Engine != "" {
		engine = r.Engine
	}
	var parts []server.RunPart
	if r.Options != nil && r.Options.Candidate != nil {
		for _, p := range r.Options.Candidate.Parts {
			parts = append(parts, server.RunPart{
				Node:      p.Node,
				First:     p.First,
				Layers:    p.Layers,
				GB:        p.GB,
				Output:    p.Output,
				ComputeMS: p.ComputeMS,
				RTTms:     p.RTTms,
				SendMS:    p.SendMS,
			})
		}
	}
	return server.DeployNote{ID: r.RunID, Status: r.Status, Error: r.Error, Model: model, SHA256: r.ModelSHA256, Engine: engine,
		Endpoint: r.EndpointName, Master: r.MasterNode, Nodes: nodes, Parts: parts, Ctx: r.Ctx, Np: r.Np, TPS: r.EstimatedTPS}
}
