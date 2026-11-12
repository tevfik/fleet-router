package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"os"
	"strings"
	"time"

	"github.com/tevfik/fleet-router/internal/client"
	"github.com/tevfik/fleet-router/internal/deploy"
	"github.com/tevfik/fleet-router/internal/plan"
	"github.com/tevfik/fleet-router/internal/server"
)

// engineOf finds --engine in a deploy command line: "" or "ring" (Protocol 1, default) or "chain" / "stage-pipe" (Protocol 2).
func engineOf(args []string) (string, error) {
	eng := ""
	for i, a := range args {
		switch {
		case a == "--engine" || a == "-engine":
			if i+1 < len(args) {
				eng = args[i+1]
			}
		case strings.HasPrefix(a, "--engine="):
			eng = strings.TrimPrefix(a, "--engine=")
		}
	}
	switch eng {
	case "", "ring", "chain", "stage-pipe":
		return eng, nil
	}
	return "", fmt.Errorf("--engine %q: ring or chain (or stage-pipe)", eng)
}

// cmdDeployStagePipe shows how a stage chain would be started for a chosen plan candidate. It starts nothing:
// until the runtime side is verified on boards only --dry-run is accepted.
func cmdDeployStagePipe(args []string) error {
	fs := flag.NewFlagSet("deploy --engine stage-pipe", flag.ContinueOnError)
	fs.String("engine", "stage-pipe", "")
	planFile := fs.String("plan", "", "the result of `fleet plan <model> --json` (- reads stdin)")
	pick := fs.Int("pick", 1, "the candidate of the plan to deploy (default: the best)")
	modelDir := fs.String("model-dir", "", "the model directory on the boards (default: the adapter's model root + the model's name)")
	timeoutS := fs.Int("timeout-s", 0, "seconds a stage waits for the next one (default: the runtime's own)")
	embOff := fs.Bool("embedding-offload", false, "keep the embedding tables of the first stage out of the NPU pool")
	ready := fs.Duration("ready-timeout", 120*time.Second, "how long to wait for each stage to answer")
	dryRun := fs.Bool("dry-run", false, "print the stage commands and the start order without starting them")
	asJSON := fs.Bool("json", false, "print the stage plan as JSON")
	proto := fs.String("proto", "csp1", "stage protocol: csp1 (Chain Stage Protocol v1, default; alias: v1) or a linked stage module")
	cpuMoE := fs.Bool("cpu-moe", false, "keep all MoE expert weights in host CPU RAM (with hot expert cache on GPU)")
	ncpuMoE := fs.Int("n-cpu-moe", 0, "keep MoE expert weights of first N layers in host CPU RAM")
	moeCacheMiB := fs.Int("moe-cache-mib", 0, "GPU cache size in MiB for host-resident MoE experts")
	port := fs.Int("port", 0, "ingress port (default: 52310 for csp1, the module's own for a stage module)")
	master := fs.String("master", "", "master/ingress node (default: this machine)")
	workers := fs.String("workers", "", "comma-separated worker nodes")
	ctxLen := fs.Int("ctx", 0, "context length (0: engine default 2048)")
	name := fs.String("name", "", "endpoint name to register (default: distributed-<model>)")
	spec := fs.String("spec", "", "speculative decoding: auto | draft-dflash | none")
	draftModel := fs.String("draft-model", "", "path to draft GGUF model for speculative decoding")
	draftMax := fs.Int("draft-max", 0, "maximum draft tokens per speculative step (0: automatic, 2 for CPU-MoE, 4 for GPU)")
	mmproj := fs.String("mmproj", "", "vision projector GGUF, a path on the master (ingress) machine: images in chat requests (plain-RoPE models; not with a draft)")
	wire := fs.String("wire", "", "hidden states between stages: f32 | bf16 | auto (engine default: auto = f32 for hyper-connection models, else bf16)")
	role := fs.String("role", "", "role to map the deployed model to")
	np := fs.Int("np", 0, "concurrent requests (a sequence each, the KV cache grows with it; not with speculative decoding)")
	lanes := fs.Int("lanes", 0, "parallel pipeline connections to the next stage (needs --np >= 2)")
	relay := fs.Bool("relay", false, "join stages on different machines with ssh tunnels through this machine (sites that cannot reach each other; this machine must stay up)")
	viaHub := fs.Bool("via-hub", false, "join stages on different machines through the hub (raw TCP stream mode, like a ring worker behind the hub): no machine of the deploy carries the traffic")
	auto := fs.Bool("auto", false, "set the deploy up from what the machines have: the model at one path on every machine, the draft sent to the master, --cpu-moe and --moe-cache-mib from the free VRAM, layers split by free memory, a ready wait that follows the stage size")
	hubURL := fs.String("hub", "", "hub for --via-hub (default: hub_url of the config, else the default hub)")

	force := fs.Bool("force", false, "deploy although the memory check says a stage does not fit its machine's free VRAM / RAM (a GPU that runs out fails the load; a machine whose RAM runs out pages or locks up)")
	unsupportedBoolFlags := []string{"cpu", "cpu-only", "mmap", "mmproj-cpu", "explore", "force-split", "exit-on-fail", "chain"}
	for _, u := range unsupportedBoolFlags {
		fs.Bool(u, false, "")
	}
	unsupportedValueFlags := []string{
		"ts", "ngl", "mmdev", "worker-port", "io-timeout", "cache-type-k",
		"cache-type-v", "worker-cpu-experts", "load-timeout", "local-weights", "worker-model",
		"layout", "strategy", "key", "hub-key", "profile", "save-profile",
	}
	for _, u := range unsupportedValueFlags {
		fs.String(u, "", "")
	}

	valFlags := []string{
		"engine", "plan", "pick", "model-dir", "timeout-s", "ready-timeout", "proto",
		"n-cpu-moe", "moe-cache-mib", "port", "master", "workers", "ctx", "name",
		"spec", "draft-model", "draft-max", "wire", "role", "hub",
		"np", "lanes", "ts", "ngl", "mmproj", "mmdev", "worker-port", "io-timeout", "cache-type-k",
		"cache-type-v", "worker-cpu-experts", "load-timeout", "local-weights", "worker-model",
		"layout", "strategy", "key", "hub-key", "profile", "save-profile",
	}

	if err := fs.Parse(flagsFirst(args, valFlags...)); err != nil {
		return err
	}

	var rejected string
	fs.Visit(func(f *flag.Flag) {
		for _, u := range unsupportedBoolFlags {
			if f.Name == u && rejected == "" {
				rejected = f.Name
			}
		}
		for _, u := range unsupportedValueFlags {
			if f.Name == u && rejected == "" {
				rejected = f.Name
			}
		}
	})
	if rejected != "" {
		return fmt.Errorf("--%s: not supported with --engine chain", rejected)
	}

	if fs.NArg() != 1 {
		return fmt.Errorf("usage: fleet deploy <fleet-model.json | model-dir | model.gguf> --engine chain [--plan plan.json] [--pick N] [--model-dir D] [--proto csp1|v1|<module>] [--timeout-s S] [--embedding-offload] [--ready-timeout D] [--cpu-moe|--n-cpu-moe N] [--moe-cache-mib N] [--port P] [--master M] [--workers W] [--ctx C] [--np N] [--lanes L] [--role R] [--name N] [--spec S] [--draft-model D] [--draft-max K] [--wire W] [--relay | --via-hub [--hub URL]] [--auto] [--dry-run] [--json]")
	}
	m, err := plan.LoadModel(fs.Arg(0))
	if err != nil {
		return err
	}
	var cand plan.Candidate
	if *master != "" || *workers != "" {
		mNode := *master
		if mNode == "" {
			mNode = deploy.LocalHostname()
		}
		nodes := []string{mNode}
		if *workers != "" {
			for _, w := range strings.Split(*workers, ",") {
				w = strings.TrimSpace(w)
				if w != "" {
					nodes = append(nodes, w)
				}
			}
		}
		nNodes := len(nodes)
		var parts []plan.Part
		if *auto {
			var notes []string
			parts, notes = deploy.AutoSplitParts(context.Background(), m, nodes)
			for _, n := range notes {
				fmt.Println("•", n)
			}
		}
		if parts == nil {
			base := m.Layers / nNodes
			rem := m.Layers % nNodes
			parts = make([]plan.Part, nNodes)
			first := 0
			for i, n := range nodes {
				layers := base
				if i < rem {
					layers++
				}
				parts[i] = plan.Part{
					Node:   n,
					First:  first,
					Layers: layers,
					Output: i == nNodes-1,
				}
				first += layers
			}
		}
		kind := "split"
		if nNodes == 1 {
			kind = "single"
		}
		cand = plan.Candidate{
			Kind:   kind,
			Master: mNode,
			Parts:  parts,
		}
	} else if *planFile != "" {
		var in io.Reader = os.Stdin
		if *planFile != "-" {
			f, err := os.Open(*planFile)
			if err != nil {
				return err
			}
			defer f.Close()
			in = f
		}
		var res plan.Result
		if err := json.NewDecoder(in).Decode(&res); err != nil {
			return fmt.Errorf("--plan: %w", err)
		}
		if *pick < 1 || *pick > len(res.Candidates) {
			return fmt.Errorf("--pick %d: the plan has %d candidates", *pick, len(res.Candidates))
		}
		cand = res.Candidates[*pick-1]
	} else {
		cand, err = planCandidateForModel(m, *pick)
		if err != nil {
			return err
		}
	}

	var adapter deploy.StagePipeAdapter
	var module *deploy.StageModule // a registered runtime module (internal/stagemod/*), nil for the built-in ones
	switch *proto {
	case "csp1", "v1", "":
		adapter = deploy.ChainStageV1
	default:
		m, ok := deploy.LookupStageModule(*proto)
		if !ok {
			return fmt.Errorf("--proto %q: expected csp1, v1%s", *proto, moduleNamesSuffix())
		}
		module, adapter = &m, m.Adapter
		if err := m.Refuses(map[string]bool{"moe": *cpuMoE || *ncpuMoE > 0 || *moeCacheMiB > 0, "lanes": *lanes > 1, "np": *np > 1}); err != nil {
			return err
		}
	}

	stages := len(cand.Parts)
	if stages == 0 {
		stages = 1
	}
	cs := deploy.ChainSupport(deploy.ChainSupportInput{
		Architecture: m.Arch,
		Stages:       stages,
		Spec:         *spec,
		DraftModel:   *draftModel,
		MMProj:       false,
		CPUMoE:       *cpuMoE,
		NCPUMoE:      *ncpuMoE,
		Proto:        *proto,
	})
	if !cs.OK {
		return fmt.Errorf("chain deploy not supported: %s", strings.Join(cs.Reasons, "; "))
	}

	stageOpts := deploy.StagePipeOptions{
		ModelDir:         *modelDir,
		TimeoutS:         *timeoutS,
		EmbeddingOffload: *embOff,
		Host:             deploy.ResolveStageHost,
		CPUMoE:           *cpuMoE,
		NCPUMoE:          *ncpuMoE,
		MoECacheMiB:      *moeCacheMiB,
		Port:             *port,
		Ctx:              *ctxLen,
		Np:               *np,
		Lanes:            *lanes,
		Relay:            *relay,
		ViaHub:           *viaHub,
		HubURL:           *hubURL,
		EndpointName:     *name,
		Spec:             *spec,
		DraftModel:       *draftModel,
		DraftMax:         *draftMax,
		Wire:             *wire,
		MMProj:           *mmproj,
		Role:             *role,
		DryRun:           *dryRun,
		Auto:             *auto,
		Force:            *force,
	}
	fs.Visit(func(f *flag.Flag) {
		if f.Name == "ready-timeout" { // only when given: the default must not override --timeout-s
			stageOpts.ReadyTimeout = *ready
		}
	})

	if module != nil {
		sp, err := deploy.PlanStagePipe(cand, m, adapter, stageOpts)
		if err != nil {
			return err
		}
		var dryRunner *deploy.DryRunner
		var runner deploy.StageRunner
		if *dryRun {
			dryRunner = &deploy.DryRunner{}
			runner = dryRunner
		} else if module != nil && module.NewRunner != nil {
			if runner, err = module.NewRunner(); err != nil {
				return fmt.Errorf("--proto %s: %w", module.Name, err)
			}
		} else {
			runner = &deploy.LiveStageRunner{}
		}
		if err := deploy.RunStagePipe(sp, runner, *ready); err != nil {
			return err
		}
		if *asJSON {
			b, _ := json.MarshalIndent(sp, "", "  ")
			fmt.Println(string(b))
			return nil
		}
		if *dryRun && dryRunner != nil {
			fmt.Printf("# stage-pipe dry run: %s, %d stages, start order last to first\n", sp.Model, len(sp.Stages))
			for _, w := range sp.Warnings {
				fmt.Println("# WARNING:", w)
			}
			for _, l := range dryRunner.Lines {
				fmt.Println(l)
			}
			return nil
		}
		fmt.Printf("✓ stage-pipe deployed: %s (%d stages, ingress on %s:%d)\n", sp.Model, len(sp.Stages), sp.Stages[0].Host, sp.Stages[0].Port)
		return nil
	}

	var dryRunner *deploy.DryRunner
	var runner deploy.StageRunner
	if *dryRun {
		dryRunner = &deploy.DryRunner{}
		runner = dryRunner
	} else {
		runner = &deploy.LiveStageRunner{}
		cl := client.New(listen())
		if cl.Alive() {
			deploy.OnState = func(r deploy.DeployResult) { _ = cl.NoteDeploy(deployNote(r)) }
			defer func() { deploy.OnState = nil }()
		}
	}

	res, err := deploy.DeployChain(context.Background(), cand, fs.Arg(0), stageOpts, runner, nil)
	if err != nil {
		return err
	}
	if *asJSON {
		sp, _ := deploy.PlanStagePipe(cand, m, deploy.ChainStageV1, stageOpts)
		b, _ := json.MarshalIndent(sp, "", "  ")
		fmt.Println(string(b))
		return nil
	}
	if *dryRun && dryRunner != nil {
		fmt.Printf("# stage-pipe dry run: %s, %d stages, start order last to first\n", m.Name, len(cand.Parts))
		for _, w := range res.Warnings {
			fmt.Println("# WARNING:", w)
		}
		for _, l := range dryRunner.Lines {
			fmt.Println(l)
		}
		return nil
	}
	// the endpoint is in the configuration now: a daemon that keeps its old copy routes nothing to it
	reloadDaemon()
	fmt.Printf("✓ stage-pipe deployed: %s (%d stages, ingress on %s, endpoint %s)\n", m.Name, len(cand.Parts), res.URL, res.EndpointName)
	for _, w := range res.Warnings {
		if isDeployNote(w) {
			fmt.Println("•", w)
			continue
		}
		fmt.Println("⚠", w)
	}
	return nil
}

// isDeployNote: a line of the deploy result that informs (what --auto or the memory check chose) rather than warns.
func isDeployNote(w string) bool {
	if strings.HasPrefix(w, "auto: ") {
		return true
	}
	return strings.HasPrefix(w, "memory check: ") && !strings.Contains(w, "could not be read") && !strings.HasPrefix(w, "memory check (--force)")
}

func planCandidateForModel(m *plan.Model, pick int) (plan.Candidate, error) {
	cl := client.New(listen())
	if cl.Alive() {
		res, err := cl.Plan(server.PlanRequest{Model: *m})
		if err == nil && len(res.Candidates) > 0 {
			if pick < 1 || pick > len(res.Candidates) {
				return plan.Candidate{}, fmt.Errorf("--pick %d: the plan has %d candidates", pick, len(res.Candidates))
			}
			return res.Candidates[pick-1], nil
		}
	}
	host := deploy.LocalHostname()
	return plan.Candidate{
		Kind:   "single",
		Master: host,
		Parts: []plan.Part{
			{Node: host, First: 0, Layers: m.Layers, Output: true},
		},
	}, nil
}

// moduleNamesSuffix is ", a, b" for the registered stage modules, "" without any.
func moduleNamesSuffix() string {
	var b strings.Builder
	for _, n := range deploy.StageModuleNames() {
		b.WriteString(", " + n)
	}
	return b.String()
}
