package tui

import (
	"context"
	"fmt"
	"github.com/charmbracelet/x/ansi"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"

	tea "charm.land/bubbletea/v2"

	"github.com/tevfik/fleet-router/internal/board"
	"github.com/tevfik/fleet-router/internal/config"
	"github.com/tevfik/fleet-router/internal/deploy"
	"github.com/tevfik/fleet-router/internal/plan"
	"github.com/tevfik/fleet-router/internal/server"
)

// The Plan screen answers "where would this model run best: on one machine, or split over several?" for a
// GGUF file (`fleet plan`): pick a file found nearby, or type a path; context, slots and the master machine
// can be changed and the answer is worked out again. Nothing is started.

var (
	planCtxs        = []int{2048, 4096, 8192, 16384, 32768, 65536, 131072, 262144, 524288, 1048576}
	planNps         = []int{1, 2, 4, 8}
	planStrats      = []string{"latency (Speed)", "throughput (Batch)", "memory (Low VRAM)", "auto (Balanced)"}
	planMoECaches   = []string{"Auto (Max VRAM)", "3300 MiB", "2048 MiB", "1024 MiB", "Disabled (0)"}
	planSpecModes   = []string{"Auto (Topology)", "DFlash (Diffusion)", "MTP (Native)", "Prompt Lookahead", "Disabled"}
	planEngineModes = []string{"Auto (RTT Smart)", "ring (Protocol 1)", "chain (Protocol 2 / FSP1)"}
	headS           = accS
)

// requestSizes are the shapes of request the role screen prices (`z` cycles them).
var requestSizes = []struct {
	name    string
	in, out int
}{
	{"short prompt, 300-token reply", 200, 300},
	{"long prompt (8k tokens), short reply", 8000, 300},
	{"long reply (2k tokens)", 500, 2000},
}

type planMsg struct {
	seq    int // runPlan's number: an older reply (fast ←/→, another file picked) is dropped
	path   string
	model  *plan.Model
	res    plan.Result
	err    error
	ctxIdx int
}

func calcAvailablePlanCtxs(maxCtx int) []int {
	if maxCtx <= 0 {
		maxCtx = 32768
	}
	var out []int
	for _, c := range planCtxs {
		if c < maxCtx {
			out = append(out, c)
		} else if c == maxCtx {
			out = append(out, c)
			return out
		} else {
			break
		}
	}
	if len(out) == 0 || out[len(out)-1] < maxCtx {
		out = append(out, maxCtx)
	}
	return out
}

// planCtxMax is the longest context the model allows: from the planned model, else from the header of the file in
// view (read once per file; a plan that is still running or failed must not make the choices shrink to 32k).
func (m model) planCtxMax() int {
	if m.planModel != nil && m.planModel.CtxMax > 0 {
		return m.planModel.CtxMax
	}
	path := m.planPath
	if path == "" && m.cur >= 0 && m.cur < len(m.planFiles) {
		path = m.planFiles[m.cur]
	}
	if path == "" {
		return 0
	}
	path = expandHome(path)
	if m.ctxMaxOf != nil {
		if v, ok := m.ctxMaxOf[path]; ok {
			return v
		}
		v := 0
		if mod, err := plan.ReadGGUF(path); err == nil && mod != nil {
			v = mod.CtxMax
		}
		m.ctxMaxOf[path] = v
		return v
	}
	return 0
}

func (m model) availablePlanCtxs() []int {
	maxCtx := m.planCtxMax()
	if maxCtx <= 0 {
		maxCtx = 32768
	}
	return calcAvailablePlanCtxs(maxCtx)
}

func (m model) currentPlanCtx() int {
	ctxs := m.availablePlanCtxs()
	if len(ctxs) == 0 {
		return 32768
	}
	idx := m.planCtx
	if idx < 0 {
		idx = 0
	}
	if idx >= len(ctxs) {
		idx = len(ctxs) - 1
	}
	return ctxs[idx]
}

// ggufFiles lists the GGUF files in the places models are usually kept (one level deep).
func ggufFiles() []string {
	home, _ := os.UserHomeDir()
	dirs := []string{".", "models"}
	if home != "" {
		dirs = append(dirs, filepath.Join(home, "models"), filepath.Join(home, "llm-cluster"), filepath.Join(home, "agent_ws"),
			filepath.Join(home, ".cache", "llama.cpp"),
			filepath.Join(home, "Downloads"), filepath.Join(home, ".lmstudio", "models"))
	}
	seen := map[string]bool{}
	var out []string
	addFile := func(p string) {
		if strings.Contains(strings.ToLower(filepath.Base(p)), "mmproj") { // a vision projector is not a model to plan
			return
		}
		if si := plan.DetectShards(p); si != nil {
			if si.FirstShard != "" && !seen[si.FirstShard] {
				seen[si.FirstShard] = true
				out = append(out, si.FirstShard)
			}
			return
		}
		if !seen[p] {
			seen[p] = true
			out = append(out, p)
		}
	}
	for _, d := range dirs {
		es, err := os.ReadDir(d)
		if err != nil {
			continue
		}
		for _, e := range es {
			p := filepath.Join(d, e.Name())
			if e.IsDir() { // one more level: models/<name>/<file>.gguf
				sub, _ := os.ReadDir(p)
				for _, s := range sub {
					if sp := filepath.Join(p, s.Name()); strings.HasSuffix(strings.ToLower(s.Name()), ".gguf") {
						addFile(sp)
					}
				}
				continue
			}
			if strings.HasSuffix(strings.ToLower(e.Name()), ".gguf") {
				addFile(p)
			}
		}
	}
	sort.Strings(out)
	return out
}

func expandHome(p string) string {
	if rest, ok := strings.CutPrefix(p, "~/"); ok {
		if h, err := os.UserHomeDir(); err == nil {
			return filepath.Join(h, rest)
		}
	}
	return p
}

// runPlan reads the file (unless its facts are already known) and asks the daemon.
func (m *model) runPlan(path string) tea.Cmd {
	m.planSeq++
	seq := m.planSeq
	cl, known := m.cl, m.planModel
	prevPath := m.planPath
	np, master := planNps[m.planNp], m.planMaster
	planCtxIdx := m.planCtx
	return func() tea.Msg {
		path = expandHome(path)
		mod := known
		if mod == nil || prevPath != path {
			var err error
			if mod, err = plan.ReadGGUF(path); err != nil {
				return planMsg{seq: seq, path: path, err: err}
			}
		}
		var maxCtx int
		if mod != nil && mod.CtxMax > 0 {
			maxCtx = mod.CtxMax
		} else {
			maxCtx = 32768
		}
		ctxs := calcAvailablePlanCtxs(maxCtx)
		idx := planCtxIdx
		if idx >= len(ctxs) {
			idx = len(ctxs) - 1
		}
		if idx < 0 {
			idx = 0
		}
		ctx := ctxs[idx]
		res, err := cl.Plan(server.PlanRequest{Model: *mod, Ctx: ctx, Np: np, Master: master})
		return planMsg{seq: seq, path: path, model: mod, res: res, err: err, ctxIdx: idx}
	}
}

func (m model) enterPlan() model {
	m.sc, m.cur, m.note = scPlan, 0, ""
	m.planSection, m.planParamCur = 0, 0
	m.ctxMaxOf = map[string]int{}
	m.planFiles = ggufFiles()
	if !m.planInit { // 8192 tokens, to begin with
		m.planInit, m.planCtx = true, 2
	}
	return m
}

type deployProgressMsg string

type deployDoneMsg struct {
	res  *deploy.DeployResult
	err  error
	warn string // the deploy worked but the daemon could not be told / the run could not be recorded
}

func waitForDeployStep(ch chan string) tea.Cmd {
	return func() tea.Msg {
		step, ok := <-ch
		if !ok {
			return nil
		}
		return deployProgressMsg(step)
	}
}

func (m model) deploySelectedCandidate() (tea.Model, tea.Cmd) { return m.deployCandidate(false) }

func (m model) chainSupportFor(cand plan.Candidate) deploy.ChainSupportResult {
	arch := ""
	if m.planModel != nil {
		arch = m.planModel.Arch
	} else if m.planRes != nil {
		arch = m.planRes.Arch
	}
	stages := len(cand.Parts)
	if stages == 0 {
		stages = 1
	}
	spec := "auto"
	switch m.planSpecMode {
	case 1:
		spec = "draft-dflash"
	case 2:
		spec = "draft-mtp"
	case 3:
		spec = "ngram-mod"
	case 4:
		spec = "none"
	default:
		if m.planDisableSpec {
			spec = "none"
		}
	}
	hasMMProj := findCompanionMMProj(m.planPath) != ""
	cpuMoE := m.planMoECache >= 1 && m.planMoECache <= 3 // 0 auto, 4 disabled
	return deploy.ChainSupport(deploy.ChainSupportInput{
		Architecture: arch,
		Stages:       stages,
		Spec:         spec,
		DraftModel:   findCompanionDFlash(m.planPath, true),
		MMProj:       hasMMProj,
		CPUMoE:       cpuMoE,
		Proto:        "csp1",
	})
}

// effectiveEngine determines the engine to use ("ring" vs "chain").
// It first checks deploy.ChainSupport: if chain is supported, Auto uses the RTT rule (>= 15ms -> chain).
// If chain is not supported, Auto falls back to ring and reports why in whyNotChain.
func (m model) effectiveEngine(cand plan.Candidate) (engine string, highRTT bool, rttVal float64, whyNotChain string) {
	maxRTT := m.candidateRTT(cand)
	highRTT = maxRTT >= chainRTTms
	cs := m.chainSupportFor(cand)
	if !cs.OK {
		whyNotChain = "chain not supported"
		if len(cs.Reasons) > 0 {
			whyNotChain = cs.Reasons[0]
		}
	}
	switch m.planEngineMode {
	case 1: // manual ring: highRTT still reported, the view warns
		return "ring", highRTT, maxRTT, ""
	case 2: // manual chain
		return "chain", highRTT, maxRTT, whyNotChain
	}
	if whyNotChain != "" { // Auto: ring when chain cannot run this
		return "ring", false, maxRTT, whyNotChain
	}
	if highRTT {
		return "chain", true, maxRTT, ""
	}
	return "ring", false, maxRTT, ""
}

// chainRTTms is the RTT from which Auto picks chain: a ring pays the RTT on every token, chain hides it better.
const chainRTTms = 15.0

// candidateRTT is the worst RTT to the candidate's machines: the planner's figure or the daemon's ping. Via the hub
// counts as at least chainRTTms.
func (m model) candidateRTT(cand plan.Candidate) float64 {
	maxRTT := 0.0
	for _, p := range cand.Parts {
		maxRTT = max(maxRTT, p.RTTms)
		for _, n := range m.st.Nodes {
			if n.Node == p.Node {
				maxRTT = max(maxRTT, float64(n.PingMS))
			}
		}
	}
	if m.planViaHub {
		maxRTT = max(maxRTT, chainRTTms)
	}
	return maxRTT
}

// deployCandidate deploys the chosen candidate; with dry nothing is started: the commands it would run are shown.
func (m model) deployCandidate(dry bool) (tea.Model, tea.Cmd) {
	if m.planRes == nil || len(m.planRes.Candidates) == 0 {
		m.note = helpS.Render("plan a file first")
		return m, nil
	}
	if !dry && m.deployChan != nil { // a second deploy would use the same port and overwrite the progress channel
		m.note = badS.Render("a deploy is already running: wait for it to finish")
		return m, nil
	}
	pick := m.planPick
	if pick >= len(m.planRes.Candidates) {
		pick = 0
	}
	path := m.planPath
	cand := m.planRes.Candidates[pick]
	cl, resPlan := m.cl, *m.planRes
	effEng, highRTT, rttVal, whyNotChain := m.effectiveEngine(cand)
	if m.planEngineMode == 2 && whyNotChain != "" {
		cs := m.chainSupportFor(cand)
		m.note = badS.Render("chain not supported: " + strings.Join(cs.Reasons, "; "))
		return m, nil
	}
	if highRTT && m.planEngineMode == 0 {
		m.note = okS.Render(fmt.Sprintf("high RTT (%.1f ms): chain (CSP1 stage pipeline) selected", rttVal))
	} else if m.planEngineMode == 0 && whyNotChain != "" {
		m.note = helpS.Render("ring: " + whyNotChain)
	}

	ch := make(chan string, 32)
	deployCtx, deployCancel := context.WithCancel(context.Background())
	if !dry {
		m.deployCancel = deployCancel // x cancels a running deploy (Ctrl+C of `fleet deploy`)
		m.deployChan = ch
		m.deployStatus = "[1/4] Preparing deployment pipeline..."
	}

	strat := deploy.StrategyLatency
	switch m.planStrat {
	case 1:
		strat = deploy.StrategyThroughput
	case 2:
		strat = deploy.StrategyMemory
	case 3:
		strat = deploy.StrategyAuto
	default:
		strat = deploy.StrategyLatency
		if planNps[m.planNp] > 1 {
			strat = deploy.StrategyThroughput
		}
	}

	masterNode := cand.Master
	if m.planMaster != "" {
		masterNode = m.planMaster
	}

	var workerNodes []string
	if cand.Kind != "single" {
		seenW := map[string]bool{}
		for _, p := range cand.Parts {
			if p.Node != masterNode && !seenW[p.Node] {
				seenW[p.Node] = true
				workerNodes = append(workerNodes, p.Node)
			}
		}
	}

	mmprojPath := findCompanionMMProj(path)
	var moeCacheMiB int
	switch m.planMoECache {
	case 1:
		moeCacheMiB = 3300
	case 2:
		moeCacheMiB = 2048
	case 3:
		moeCacheMiB = 1024
	case 4:
		moeCacheMiB = -1 // 0/disabled
	default:
		moeCacheMiB = 0 // Auto calculated based on GPU free VRAM and MoE architecture
	}

	specType := "auto"
	switch m.planSpecMode {
	case 1:
		specType = "draft-dflash"
	case 2:
		specType = "draft-mtp"
	case 3:
		specType = "ngram-mod"
	case 4:
		specType = "none"
	default:
		if m.planDisableSpec {
			specType = "none"
		} else {
			specType = "auto"
		}
	}

	draftPath := ""
	if specType == "draft-dflash" { // llama-server needs --spec-draft-model for it
		if draftPath = findCompanionDFlash(path, effEng == "chain"); draftPath == "" {
			deployCancel()
			m.deployCancel, m.deployChan, m.deployStatus = nil, nil, ""
			m.note = badS.Render("DFlash: no *dflash*.gguf draft next to the model (or one directory up): put the draft there or choose another speculation mode")
			return m, nil
		}
	}

	if effEng == "chain" {
		ctxLen := planCtxs[m.planCtx]
		stageOpts := chainStageOptions(path, ctxLen, m.planMoECache, moeCacheMiB, specType, draftPath, m.planViaHub)
		if dry {
			deployCancel()
			return m, func() tea.Msg {
				dryRunner := &deploy.DryRunner{}
				res, err := deploy.DeployChain(context.Background(), cand, path, stageOpts, dryRunner, nil)
				if err != nil {
					return outMsg{note: badS.Render("dry run failed: " + err.Error())}
				}
				return outMsg{note: okS.Render("✓ dry run (chain): nothing was started"), text: dryRunText(res)}
			}
		}
		runCmd := func() tea.Msg {
			defer deployCancel()
			runner := &deploy.LiveStageRunner{}
			res, err := deploy.DeployChain(deployCtx, cand, path, stageOpts, runner, nil)
			close(ch)
			warn := ""
			if err == nil && res != nil {
				if rerr := cl.Reload(); rerr != nil {
					warn += " · the daemon was not told to reload: " + rerr.Error()
				}
				if _, rerr := cl.RecordRun(resPlan, pick+1, res.EndpointName, "chain"); rerr != nil {
					warn += " · the run was not recorded: " + rerr.Error()
				}
			}
			return deployDoneMsg{res: res, err: err, warn: warn}
		}
		return m, tea.Batch(waitForDeployStep(ch), runCmd)
	}

	opts := deploy.DeployOptions{
		ModelPath:   path,
		Strategy:    strat,
		MasterNode:  masterNode,
		WorkerNodes: workerNodes,
		Candidate:   &cand,
		Port:        18090,
		Ctx:         m.currentPlanCtx(),
		Np:          planNps[m.planNp],
		CPUOnly:     m.planCPUOnly,
		MMProj:      mmprojPath,
		DraftModel:  draftPath,
		MMProjCPU:   m.planMMProjCPU,
		MMap:        !m.planNoMMap,
		SpecType:    specType,
		MoECacheMiB: moeCacheMiB,
		DraftMax:    0, // 0: automatic (draft-dflash 2, otherwise 4)
		CacheTypeK:  "q8_0",
		CacheTypeV:  "q8_0",
		ViaHub:      m.planViaHub, // independent of Chain (worker forwarding), as in `fleet deploy`
		DryRun:      dry,
	}

	if dry { // fleet deploy --dry-run: plan and commands, nothing started
		deployCancel() // not needed here
		return m, func() tea.Msg {
			cfg, err := config.Load(config.Path())
			if err != nil {
				cfg = config.Default()
			}
			res, err := deploy.ExecuteDeploymentWithProgress(context.Background(), cfg, opts, nil)
			if err != nil {
				return outMsg{note: badS.Render("dry run failed: " + err.Error())}
			}
			return outMsg{note: okS.Render("✓ dry run: nothing was started"), text: dryRunText(res)}
		}
	}

	runCmd := func() tea.Msg {
		defer deployCancel()
		cfg, err := config.Load(config.Path())
		if err != nil {
			cfg = config.Default()
		}
		res, err := deploy.ExecuteDeploymentWithProgress(deployCtx, cfg, opts, func(step string) {
			select {
			case ch <- step:
			default:
			}
		})
		close(ch)
		warn := ""
		if err == nil && res != nil {
			if rerr := cl.Reload(); rerr != nil {
				warn += " · the daemon was not told to reload: " + rerr.Error()
			}
			if _, rerr := cl.RecordRun(resPlan, pick+1, res.EndpointName, "llama-ring"); rerr != nil {
				warn += " · the run was not recorded: " + rerr.Error()
			}
		}
		return deployDoneMsg{res: res, err: err, warn: warn}
	}

	return m, tea.Batch(waitForDeployStep(ch), runCmd)
}

// planLastSection is the last section ↓/tab reach before the button bar: the deploy line once there are
// candidates, else ③ (the machines, with a hint to plan a file first).
func (m model) planLastSection() int {
	if m.planRes != nil && len(m.planRes.Candidates) > 0 {
		return 3
	}
	return 2
}

func (m model) keyPlan(s string) (tea.Model, tea.Cmd) {
	if s == "x" && m.deployChan != nil && m.deployCancel != nil { // a running deploy
		m.deployCancel()
		m.note = helpS.Render("cancelling the deploy … (what it started is stopped)")
		return m, nil
	}
	rerun := func() (tea.Model, tea.Cmd) {
		if m.planPath == "" {
			return m, nil
		}
		m.planBusy = true
		cmd := m.runPlan(m.planPath)
		return m, cmd
	}

	if m.profOpen {
		return m.keyProfiles(s)
	}
	if m.pick { // choosing the candidate f and w act on: ↑/↓ move, enter takes it, esc leaves it as it was
		if s == "enter" && m.planRes != nil && m.cur < len(m.planRes.Candidates) {
			m.planPick, m.pick, m.cur = m.cur, false, m.planCur
			m.note = helpS.Render(fmt.Sprintf("candidate %d chosen — f fetches what it needs, w records it as a run", m.planPick+1))
		}
		return m, nil
	}

	maxSec := m.planLastSection()

	switch s {
	case "tab":
		m.planSection = (m.planSection + 1) % (maxSec + 1)
		return m, nil
	case "shift+tab", "backtab":
		m.planSection = (m.planSection + maxSec) % (maxSec + 1)
		return m, nil
	case "up", "k":
		switch m.planSection {
		case 0:
			if m.cur > 0 {
				m.cur--
			}
		case 1:
			if i := m.planParamIdx(); i > 0 {
				m.planParamCur = m.planParamSeq()[i-1]
			} else {
				m.planSection = 0
			}
		case 2:
			if m.planPick > 0 {
				m.planPick--
			} else {
				m.planSection = 1
				m.planParamCur = m.planParamLast()
			}
		case 3:
			if m.planRes != nil && len(m.planRes.Candidates) > 0 {
				m.planSection = 2
			} else {
				m.planSection = 1
			}
		}
		return m, nil
	case "down", "j":
		switch m.planSection {
		case 0:
			if m.cur < len(m.planFiles)-1 {
				m.cur++
			} else {
				m.planSection = 1
				m.planParamCur = 0
			}
		case 1:
			if seq := m.planParamSeq(); m.planParamIdx() < len(seq)-1 {
				m.planParamCur = seq[m.planParamIdx()+1]
			} else if maxSec >= 2 {
				m.planSection = 2
			}
		case 2:
			if m.planRes != nil && m.planPick < len(m.planRes.Candidates)-1 {
				m.planPick++
			} else if maxSec >= 3 {
				m.planSection = 3
			}
		case 3:
			// already at deploy button
		}
		return m, nil
	case "left", "h", "right", "l":
		if m.planSection == 1 {
			dir := 1
			if s == "left" || s == "h" {
				dir = -1
			}
			switch m.planParamCur {
			case planAdvRow:
				m.planAdv = !m.planAdv
				return m, nil
			case 0:
				ctxs := m.availablePlanCtxs()
				m.planCtx = (m.planCtx + dir + len(ctxs)) % len(ctxs)
				return rerun()
			case 1:
				m.planNp = (m.planNp + dir + len(planNps)) % len(planNps)
				return rerun()
			case 2:
				m.planStrat = (m.planStrat + dir + len(planStrats)) % len(planStrats)
				m.note = helpS.Render(fmt.Sprintf("QoS mode set to: %s", planStrats[m.planStrat]))
				return m, nil
			case 3:
				opts := []string{""}
				for _, n := range m.st.Nodes {
					opts = append(opts, n.Node)
				}
				curIdx := 0
				for i, o := range opts {
					if o == m.planMaster {
						curIdx = i
						break
					}
				}
				nextIdx := (curIdx + dir + len(opts)) % len(opts)
				m.planMaster = opts[nextIdx]
				return rerun()
			case 4:
				m.planCPUOnly = !m.planCPUOnly
				m.note = helpS.Render(fmt.Sprintf("Hardware target: CPU-Only = %v", m.planCPUOnly))
				return rerun()
			case 5:
				m.planMoECache = (m.planMoECache + dir + len(planMoECaches)) % len(planMoECaches)
				m.note = helpS.Render(fmt.Sprintf("MoE Hot Expert Cache: %s", planMoECaches[m.planMoECache]))
				return m, nil
			case 6:
				m.planSpecMode = (m.planSpecMode + dir + len(planSpecModes)) % len(planSpecModes)
				m.note = helpS.Render(specModeNote(m.planSpecMode))
				return m, nil
			case 7:
				m.planMMProjCPU = !m.planMMProjCPU
				m.note = helpS.Render(fmt.Sprintf("Multimodal: CPU Offload = %v", m.planMMProjCPU))
				return m, nil
			case 8:
				m.planNoMMap = !m.planNoMMap
				m.note = helpS.Render(fmt.Sprintf("Demand Paging (mmap): Enabled = %v", !m.planNoMMap))
				return m, nil
			case 9:
				m.planEngineMode = (m.planEngineMode + dir + len(planEngineModes)) % len(planEngineModes)
				m.note = helpS.Render(fmt.Sprintf("Engine set to: %s", planEngineModes[m.planEngineMode]))
				return m, nil
			}
		}
		return m, nil
	case "enter":
		switch m.planSection {
		case 0:
			if m.cur < len(m.planFiles) {
				m.planPath, m.planModel, m.planRes, m.planErr, m.planBusy = m.planFiles[m.cur], nil, nil, "", true
				cmd := m.runPlan(m.planFiles[m.cur])
				return m, cmd
			}
			m.note = helpS.Render("no GGUF file found nearby — p types a path")
			return m, nil
		case 1:
			switch m.planParamCur {
			case planAdvRow:
				m.planAdv = !m.planAdv
				return m, nil
			case 0:
				ctxs := m.availablePlanCtxs()
				m.planCtx = (m.planCtx + 1) % len(ctxs)
				return rerun()
			case 1:
				m.planNp = (m.planNp + 1) % len(planNps)
				return rerun()
			case 2:
				m.planStrat = (m.planStrat + 1) % len(planStrats)
				m.note = helpS.Render(fmt.Sprintf("QoS mode set to: %s", planStrats[m.planStrat]))
				return m, nil
			case 3:
				opts := []string{""}
				for _, n := range m.st.Nodes {
					opts = append(opts, n.Node)
				}
				for i, o := range opts {
					if o == m.planMaster {
						m.planMaster = opts[(i+1)%len(opts)]
						break
					}
				}
				return rerun()
			case 4:
				m.planCPUOnly = !m.planCPUOnly
				m.note = helpS.Render(fmt.Sprintf("Hardware target: CPU-Only = %v", m.planCPUOnly))
				return rerun()
			case 5:
				m.planMoECache = (m.planMoECache + 1) % len(planMoECaches)
				m.note = helpS.Render(fmt.Sprintf("MoE Hot Expert Cache: %s", planMoECaches[m.planMoECache]))
				return m, nil
			case 6:
				m.planSpecMode = (m.planSpecMode + 1) % len(planSpecModes)
				m.note = helpS.Render(specModeNote(m.planSpecMode))
				return m, nil
			case 7:
				m.planMMProjCPU = !m.planMMProjCPU
				m.note = helpS.Render(fmt.Sprintf("Multimodal: CPU Offload = %v", m.planMMProjCPU))
				return m, nil
			case 8:
				m.planNoMMap = !m.planNoMMap
				m.note = helpS.Render(fmt.Sprintf("Demand Paging (mmap): Enabled = %v", !m.planNoMMap))
				return m, nil
			case 9:
				m.planEngineMode = (m.planEngineMode + 1) % len(planEngineModes)
				m.note = helpS.Render(fmt.Sprintf("Engine set to: %s", planEngineModes[m.planEngineMode]))
				return m, nil
			}
		case 2:
			if m.planRes == nil || len(m.planRes.Candidates) == 0 {
				m.note = helpS.Render("no candidates yet — press 1, pick a file and press enter to plan it")
				return m, nil
			}
			m.note = helpS.Render(fmt.Sprintf("candidate %d selected", m.planPick+1))
			return m, nil
		case 3:
			return m.deploySelectedCandidate()
		}
	case "e":
		m.planExpl = !m.planExpl
	case "S":
		return m.openLayers()
	case "a":
		if m.planRes == nil || len(m.planRes.Candidates) == 0 {
			m.note = helpS.Render("plan a file first")
			return m, nil
		}
		m.editing, m.editBuf = "planprofile", m.defaultProfileName()
	case "L":
		m = m.openProfiles()
		return m, nil
	case "c":
		if m.planRes == nil || len(m.planRes.Candidates) == 0 {
			m.note = helpS.Render("plan a file first")
			return m, nil
		}
		m.planSection = 2
		m.pick, m.pickMode, m.planCur, m.cur = true, "plancand", m.cur, m.planPick
	case "p":
		m.editing, m.editBuf = "planpath", m.planPath
	case "r":
		return rerun()
	case "f":
		if m.planRes == nil || !m.planRes.Known || len(m.planRes.Candidates) == 0 {
			m.note = helpS.Render("plan a registered file first (Model files → p adds one on the machine that has it)")
			return m, nil
		}
		res, cl := *m.planRes, m.cl
		pick := m.planPick
		m.note = helpS.Render("fetching what candidate " + fmt.Sprint(pick+1) + " needs — progress is on the Model files screen …")
		return m, func() tea.Msg {
			if err := cl.FetchPlan(res, pick+1, nil); err != nil {
				return noteMsg(badS.Render("error: " + err.Error()))
			}
			run, err := cl.RecordRun(res, pick+1, "", "")
			if err != nil {
				return noteMsg(badS.Render("error: " + err.Error()))
			}
			return noteMsg(okS.Render("✓ candidate " + fmt.Sprint(pick+1) + " is ready: the master has the file, the workers' caches are filled — recorded as run " + run.ID + " (Runs)"))
		}
	case "w":
		if m.planRes == nil || len(m.planRes.Candidates) == 0 {
			m.note = helpS.Render("plan a file first")
			return m, nil
		}
		res, cl := *m.planRes, m.cl
		pick := m.planPick
		return m, func() tea.Msg {
			run, err := cl.RecordRun(res, pick+1, "", "")
			if err != nil {
				return noteMsg(badS.Render("error: " + err.Error()))
			}
			return noteMsg(okS.Render("✓ recorded as run " + run.ID + " — bind it to its endpoint on the Runs screen"))
		}
	case "d":
		return m.deploySelectedCandidate()
	case "i": // fleet deploy --dry-run
		return m.deployCandidate(true)
	case "b": // fleet deploy --via-hub
		m.planViaHub = !m.planViaHub
		if m.planViaHub {
			m.note = helpS.Render("Workers are reached through the hub's raw TCP streams (fleet deploy --via-hub): adds latency; required when a worker has no direct LAN IP")
		} else {
			m.note = helpS.Render("Workers are reached directly over LAN (a worker without a direct LAN IP is refused without --via-hub)")
		}
		return m, nil
	case "t":
		if m.planPath != "" {
			slug := config.Slug(filepath.Base(m.planPath))
			target := fmt.Sprintf("distributed-%s", slug)
			for _, ep := range m.st.Endpoints {
				if ep.Model == filepath.Base(m.planPath) && strings.HasPrefix(ep.Name, "distributed-") {
					target = ep.Name
					break
				}
			}
			return m.launchAgentTUI(target)
		}
	}
	return m, nil
}

func renderSelector(label string, options []string, selectedIdx int, keyHint string, focused bool) string {
	var b strings.Builder
	lead := "  "
	lblStyle := descS
	if focused {
		lead = accS.Render("▸ ")
		lblStyle = headS
	}
	fmt.Fprintf(&b, "%s%s %s", lead, lblStyle.Render(fmt.Sprintf("%-11s", label)), helpS.Render("[◄ ")) // pad before styling: escapes do not count
	for i, opt := range options {
		if i == selectedIdx {
			b.WriteString(accS.Render("[ " + opt + " ]"))
		} else {
			b.WriteString(descS.Render(" " + opt + " "))
		}
		if i < len(options)-1 {
			b.WriteString(helpS.Render(" • "))
		}
	}
	b.WriteString(helpS.Render(" ►]  ") + helpS.Render(keyHint))
	return b.String()
}

// selector is renderSelector fitted to the terminal: a hint that does not fit goes on its own line below.
func (m model) selector(label string, options []string, selectedIdx int, keyHint string, focused bool) string {
	line := renderSelector(label, options, selectedIdx, keyHint, focused)
	if m.w <= 0 || ansi.StringWidth(line) < m.w { // m.w 0: not known yet
		return line
	}
	line = trunc(renderSelector(label, options, selectedIdx, "", focused), m.w-1)
	if keyHint == "" {
		return line
	}
	return line + "\n" + helpS.Render(trunc(strings.Repeat(" ", 15)+keyHint, m.w-1))
}

func (m model) viewPlan() string {
	var b strings.Builder
	width := m.w
	if width <= 0 {
		width = 100
	}

	currentSec := m.planSection
	currentParam := m.planParamCur
	if m.inBar {
		bs := m.buttons()
		if m.barCur < len(bs) {
			switch bs[m.barCur].label {
			case "Plan this file", "Type a path…":
				currentSec = 0
			case "Choose candidate…", "Explain":
				currentSec = 2
			case "Deploy":
				currentSec = 3
			}
		}
	}

	// Step 1: Model Selection
	sec1Header := "① MODEL FILE SELECTION: GGUF files nearby"
	if currentSec == 0 {
		b.WriteString(accS.Render("▸ ") + headS.Render(sec1Header) + helpS.Render("  (↑/↓: files · enter: plan)") + "\n")
	} else {
		b.WriteString("  " + groupS.Render(sec1Header) + "" + "\n")
	}
	if len(m.planFiles) == 0 {
		b.WriteString(descS.Render("  no GGUF file found in ., ./models, ~/models, ~/Downloads — p types a path") + "\n")
	} else {
		fileCur := m.cur
		if m.pick {
			fileCur = m.planCur
		}
		start, end := window(len(m.planFiles), fileCur, 6)
		for i := start; i < end; i++ {
			p := m.planFiles[i]
			size := ""
			shardNote := ""
			if si := plan.DetectShards(p); si != nil {
				size = board.Bytes(si.TotalBytes)
				if len(si.Missing) > 0 {
					shardNote = fmt.Sprintf(" (⚠ %d/%d shards)", len(si.ShardPaths), si.Total)
				} else {
					shardNote = fmt.Sprintf(" (%d shards)", si.Total)
				}
			} else if st, err := os.Stat(p); err == nil {
				size = board.Bytes(st.Size())
			}
			tag := ""
			if p == m.planPath {
				tag = okS.Render("  ← planned")
			}
			mark := "  "
			if currentSec == 0 && i == fileCur {
				mark = curMark(true)
			}
			displayName := tailTrunc(p, 55)
			if shardNote != "" {
				displayName = tailTrunc(p, 55-len(shardNote)) + shardNote
			}
			fmt.Fprintf(&b, "%s%-55s %8s%s\n", mark, displayName, size, tag)
		}
	}
	b.WriteString("\n")

	// Step 2: Runtime Parameters & QoS
	sec2Header := "② RUNTIME PARAMETERS & QOS STRATEGY"
	if currentSec == 1 {
		b.WriteString(accS.Render("▸ ") + headS.Render(sec2Header) + helpS.Render("  (↑/↓: fields · ←/→: change the value)") + "\n")
	} else {
		b.WriteString("  " + groupS.Render(sec2Header) + "" + "\n")
	}
	ctxs := m.availablePlanCtxs()
	curCtxIdx := m.planCtx
	if curCtxIdx >= len(ctxs) {
		curCtxIdx = len(ctxs) - 1
	}
	if curCtxIdx < 0 {
		curCtxIdx = 0
	}
	ctxOpts := make([]string, len(ctxs))
	for i, c := range ctxs {
		ctxOpts[i] = fmt.Sprint(c)
	}
	ctxHint := ""
	if cm := m.planCtxMax(); cm > 0 {
		maxStr := fmt.Sprintf("%d", cm)
		if cm >= 1048576 {
			maxStr = fmt.Sprintf("%dM", cm/1048576)
		} else if cm >= 1024 {
			maxStr = fmt.Sprintf("%dk", cm/1024)
		}
		ctxHint = "model max: " + maxStr
	}
	b.WriteString(m.selector("context", ctxOpts, curCtxIdx, ctxHint, currentSec == 1 && currentParam == 0) + "\n")

	slotOpts := make([]string, len(planNps))
	for i, n := range planNps {
		slotOpts[i] = fmt.Sprint(n)
	}
	b.WriteString(m.selector("slots", slotOpts, m.planNp, "", currentSec == 1 && currentParam == 1) + "\n")

	b.WriteString(m.selector("QoS mode", planStrats, m.planStrat, "", currentSec == 1 && currentParam == 2) + "\n")

	masterOpts := []string{"any machine"}
	selectedMasterIdx := 0
	for i, n := range m.st.Nodes {
		masterOpts = append(masterOpts, n.Node)
		if n.Node == m.planMaster {
			selectedMasterIdx = i + 1
		}
	}
	b.WriteString(m.selector("master", masterOpts, selectedMasterIdx, "", currentSec == 1 && currentParam == 3) + "\n")

	b.WriteString(m.advancedRow(currentSec == 1 && currentParam == planAdvRow) + "\n")
	if m.planAdv {
		hwOpts := []string{"Cluster / GPU (Auto)", "CPU only (-ngl 0)"}
		hwIdx := 0
		if m.planCPUOnly {
			hwIdx = 1
		}
		b.WriteString(m.selector("target", hwOpts, hwIdx, "", currentSec == 1 && currentParam == 4) + "\n")

		b.WriteString(m.selector("moe cache", planMoECaches, m.planMoECache, "", currentSec == 1 && currentParam == 5) + "\n")
		b.WriteString(m.selector("spec engine", planSpecModes, m.planSpecMode, "", currentSec == 1 && currentParam == 6) + "\n")

		mmOpts := []string{"CPU (0 VRAM)", "GPU VRAM"}
		mmIdx := 0
		if !m.planMMProjCPU {
			mmIdx = 1
		}
		b.WriteString(m.selector("vision", mmOpts, mmIdx, "", currentSec == 1 && currentParam == 7) + "\n")

		mmapOpts := []string{"mmap (Paging)", "RAM only"}
		mmapIdx := 0
		if m.planNoMMap {
			mmapIdx = 1
		}
		b.WriteString(m.selector("paging", mmapOpts, mmapIdx, "", currentSec == 1 && currentParam == 8) + "\n")

		engineHint := m.engineHintText()
		b.WriteString(m.selector("engine", planEngineModes, m.planEngineMode, engineHint, currentSec == 1 && currentParam == 9) + "\n")
	}
	b.WriteString("\n")

	// Step 3: Cluster Nodes & Candidates
	sec3Header := "③ CLUSTER DEVICES & ALLOCATION CANDIDATES"
	if currentSec == 2 {
		hint := "  (↑/↓: candidate)"
		if m.planRes == nil || len(m.planRes.Candidates) == 0 {
			hint = "  (nothing to choose yet: plan a file in ① first)"
		}
		b.WriteString(accS.Render("▸ ") + headS.Render(sec3Header) + helpS.Render(hint) + "\n")
	} else {
		b.WriteString("  " + groupS.Render(sec3Header) + "" + "\n")
	}
	var nodeStatus []string
	for _, n := range m.st.Nodes {
		isOnline := n.Up > 0 || n.Self || (n.Total > 0 && n.Up == n.Total)
		stMark := okS.Render("● " + n.Node)
		if !isOnline {
			stMark = descS.Render("○ " + n.Node + " (down)")
		}
		detail := ""
		if n.Self {
			detail = " (local)"
		} else if n.PingMS > 0 {
			detail = fmt.Sprintf(" (%dms)", n.PingMS)
		}
		nodeStatus = append(nodeStatus, fmt.Sprintf("%s%s", stMark, helpS.Render(detail)))
	}
	if len(nodeStatus) > 0 {
		b.WriteString("  " + strings.Join(nodeStatus, "   ") + "\n\n")
	}

	pick := m.planPick
	switch {
	case m.planBusy:
		b.WriteString(accS.Render("  working out the candidates …") + "\n\n")
	case m.planErr != "":
		b.WriteString(badS.Render("  "+m.planErr) + "\n\n")
	case m.planRes != nil:
		title, verdict := board.PlanHead(*m.planRes)
		b.WriteString(helpS.Render("  "+title) + "\n")
		vs := accS
		if len(m.planRes.Candidates) == 0 {
			vs = badS
		}
		b.WriteString("  " + vs.Render(trunc(verdict, width-4)) + "\n\n")

		table := board.PlanTable(*m.planRes)
		if pick >= len(m.planRes.Candidates) {
			pick = 0
		}
		for i := 1; i < len(table); i++ {
			mark := "  "
			switch {
			case m.pick && i-1 == m.cur:
				mark = curMark(true)
			case i-1 == pick:
				mark = okS.Render("✓ ")
			}
			table[i] = mark + strings.TrimPrefix(table[i], "  ")
		}
		budget := len(table)
		if m.h > 0 {
			budget = max(4, m.h-min(len(m.planFiles), 6)-26)
		}
		for i, l := range table {
			if i >= budget {
				b.WriteString(helpS.Render(fmt.Sprintf("  … %d more lines (fleet plan %s --json)", len(table)-i, filepath.Base(m.planPath))) + "\n")
				break
			}
			if i == 0 {
				l = helpS.Render(l)
			}
			b.WriteString(trunc(l, width-2) + "\n")
		}
		if m.planExpl && pick < len(m.planRes.Candidates) {
			b.WriteString("\n")
			for _, l := range board.PlanExplain(m.planRes.Candidates[pick], pick+1) {
				b.WriteString(accS.Render(trunc(l, width-2)) + "\n")
			}
		}
		for _, l := range board.PlanNotes(*m.planRes) {
			b.WriteString(helpS.Render(trunc(l, width-2)) + "\n")
		}
		b.WriteString("\n")

		// Step 4: Deploy Action
		if len(m.planRes.Candidates) > 0 {
			cand := m.planRes.Candidates[pick]
			workersSummary := "none (local single)"
			if cand.Kind != "single" {
				var wlist []string
				for _, p := range cand.Parts {
					if p.Node != cand.Master {
						wlist = append(wlist, p.Node)
					}
				}
				if len(wlist) > 0 {
					workersSummary = strings.Join(wlist, ", ")
				}
			}
			sec4Header := "④ DEPLOY & EXECUTE"
			if currentSec == 3 {
				b.WriteString(accS.Render("▸ ") + headS.Render(sec4Header) + "" + "\n")
			} else {
				b.WriteString("  " + groupS.Render(sec4Header) + "" + "\n")
			}
			effEng, highRTT, rttVal, _ := m.effectiveEngine(cand)
			engSummary := effEng
			if highRTT && m.planEngineMode == 0 {
				engSummary = fmt.Sprintf("chain (auto: high RTT %.0fms)", rttVal)
			}
			summary := fmt.Sprintf("  Ready: %s  •  Master: %s  •  Workers: [%s]  •  Engine: %s  •  %.1f tok/s (est., no speculation)  •  QoS: %s%s",
				filepath.Base(m.planPath), cand.Master, workersSummary, engSummary, cand.TPS, planStrats[m.planStrat], map[bool]string{true: "  •  via hub"}[m.planViaHub])
			b.WriteString(accS.Render(summary) + "\n")
			if m.deployStatus != "" {
				b.WriteString(accS.Render("  ▸ [ ⏳ DEPLOYING IN PROGRESS... ] ") + headS.Render(m.deployStatus) + helpS.Render("   x cancels") + "\n")
			} else if currentSec == 3 {
				b.WriteString(headS.Render("  ▸ [ 🚀 PRESS ENTER OR 'd' TO DEPLOY CANDIDATE ]") + "" + "\n")
			} else {
				b.WriteString(okS.Render("  [ 🚀 PRESS 'd' TO DEPLOY CANDIDATE ]") + "" + "\n")
			}
		}
	default:
		b.WriteString(helpS.Render("  Pick a model above or press enter on a file to work out distributed placement (L: saved profiles).") + "\n")
	}
	if m.lay != nil {
		b.WriteString("\n" + m.viewLayers(width))
	}
	if m.profOpen {
		b.WriteString("\n" + m.viewProfiles(width))
	}

	return b.String()
}

// routeSection is the part of the role screen that shows where a request would go now and what each model costs.
func (m model) routeSection(role string) string {
	var b strings.Builder
	sz := requestSizes[m.routeSize%len(requestSizes)]
	b.WriteString("\n" + groupS.Render("Where a request would go now — "+sz.name) + helpS.Render("   (z: other request size)") + "\n")
	if m.route == nil || m.route.Role != role {
		b.WriteString(helpS.Render("  …") + "\n")
		return b.String()
	}
	width := m.w
	if width <= 0 {
		width = 100
	}
	lines := board.RouteLines(*m.route)
	for i, l := range lines {
		if i == 0 {
			continue // the title says what the heading already does
		}
		if i == 1 {
			l = helpS.Render(trunc(l, width-2))
		} else {
			l = trunc(l, width-2)
		}
		b.WriteString(l + "\n")
	}
	return b.String()
}

// tailTrunc shortens a path from the left: the file name is what tells the files apart.
func tailTrunc(p string, n int) string {
	r := []rune(p)
	if len(r) <= n {
		return p
	}
	return "…" + string(r[len(r)-n+1:])
}

// findCompanionMMProj finds the vision projector of the model: an mmproj GGUF next to it or one directory up (split
// models sit in a quant subdirectory) whose name starts like the model's ("mmproj-Qwen3.8-Flash-Next-BF16" for
// "Qwen3.8-Flash-Next-IQ1_M-…"). A generic name ("mmproj-model-f16.gguf") counts only when no other model shares the
// directory: a directory with several models must not give one of them another's projector.
func findCompanionMMProj(modelPath string) string {
	if modelPath == "" {
		return ""
	}
	model := nameTokens(filepath.Base(modelPath))
	dir := filepath.Dir(modelPath)
	for _, d := range []string{dir, filepath.Dir(dir)} {
		entries, err := os.ReadDir(d)
		if err != nil {
			continue
		}
		generic := ""
		others := map[string]bool{} // models in d other than this one (shards of one model count once)
		for _, e := range entries {
			name := strings.ToLower(e.Name())
			if e.IsDir() || !strings.HasSuffix(name, ".gguf") {
				continue
			}
			if !strings.Contains(name, "mmproj") {
				if base := shardSuffix.ReplaceAllString(name, ""); !strings.Contains(name, "dflash") && base != shardSuffix.ReplaceAllString(strings.ToLower(filepath.Base(modelPath)), "") {
					others[base] = true
				}
				continue
			}
			proj := nameTokens(strings.NewReplacer("mmproj", "").Replace(name))
			if len(proj) >= 2 && len(model) >= 2 && proj[0] == model[0] && proj[1] == model[1] {
				return filepath.Join(d, e.Name())
			}
			if genericProj(proj) && generic == "" {
				generic = filepath.Join(d, e.Name())
			}
		}
		if generic != "" && len(others) == 0 {
			return generic
		}
	}
	return ""
}

var (
	shardSuffix   = regexp.MustCompile(`-\d{5}-of-\d{5}\.gguf$`)
	precisionWord = regexp.MustCompile(`^(model|f16|bf16|f32|q\d.*|\d+)$`)
)

// genericProj: an mmproj name that says nothing about its model ("mmproj-model-f16", "mmproj-f16").
func genericProj(tokens []string) bool {
	for _, t := range tokens {
		if !precisionWord.MatchString(t) {
			return false
		}
	}
	return true
}

// nameTokens splits a lower-cased file name on - and _ without its .gguf.
func nameTokens(name string) []string {
	name = strings.TrimSuffix(strings.ToLower(name), ".gguf")
	return strings.FieldsFunc(name, func(r rune) bool { return r == '-' || r == '_' })
}

// findCompanionDFlash looks for a DFlash draft GGUF next to the model or one directory up (split models often sit
// in a quant subdirectory). wantHead prefers a draft with an embedded output layer (fleet chain dflash-head, "-head"):
// chain stages without the target's output need it; llama-server shares the target's and saves the VRAM without it.
func findCompanionDFlash(modelPath string, wantHead bool) string {
	if modelPath == "" {
		return ""
	}
	dir := filepath.Dir(modelPath)
	var first, other string
	for _, d := range []string{dir, filepath.Dir(dir)} {
		entries, err := os.ReadDir(d)
		if err != nil {
			continue
		}
		for _, e := range entries {
			name := strings.ToLower(e.Name())
			if e.IsDir() || !strings.Contains(name, "dflash") || !strings.HasSuffix(name, ".gguf") {
				continue
			}
			full := filepath.Join(d, e.Name())
			if full == modelPath {
				continue
			}
			switch {
			case strings.Contains(name, "-head") == wantHead && first == "":
				first = full
			case other == "":
				other = full
			}
		}
	}
	if first != "" {
		return first
	}
	return other
}

// specModeNote names the chosen speculation mode and, for the draft modes, what has actually been measured: the
// planner's tok/s never includes a speculative gain.
func specModeNote(mode int) string {
	name := planSpecModes[mode]
	switch mode {
	case 1, 2:
		return fmt.Sprintf("Speculation Engine: %s - gain not guaranteed: measured break-even on one machine (n-max 4), about +50%% median on a WAN ring (n-max 2, n=15, indicative, prompt dependent)", name)
	}
	return fmt.Sprintf("Speculation Engine: %s", name)
}

// dryRunText is what a dry-run deploy would do: the placement and the commands, nothing started.
func dryRunText(r *deploy.DeployResult) string {
	if r == nil {
		return ""
	}
	var b strings.Builder
	fmt.Fprintf(&b, "dry run: strategy %s, master %s, workers %s, endpoint %s (%s)\n", r.Strategy, r.MasterNode, strings.Join(r.WorkerNodes, ","), r.EndpointName, r.URL)
	names := make([]string, 0, len(r.WorkerCmds))
	for n := range r.WorkerCmds {
		names = append(names, n)
	}
	sort.Strings(names)
	for _, n := range names {
		fmt.Fprintf(&b, "worker %s: %s\n", n, r.WorkerCmds[n])
	}
	fmt.Fprintf(&b, "master: %s\n", r.MasterCmd)
	return b.String()
}

// chainStageOptions are the options of a chain deploy from the plan screen. moeSetting is the screen's MoE cache
// setting (0 auto, 1-3 a size, 4 disabled): "auto" also lets the deploy size the experts and the cache from the free
// VRAM and give every machine one model path (--auto); a size or "disabled" is what the user chose and stays.
func chainStageOptions(path string, ctxLen, moeSetting, moeCacheMiB int, spec, draft string, viaHub bool) deploy.StagePipeOptions {
	return deploy.StagePipeOptions{
		ModelDir:    path,
		TimeoutS:    600,
		Host:        deploy.ResolveNodeIP,
		CPUMoE:      moeSetting >= 1 && moeSetting <= 3,
		MoECacheMiB: moeCacheMiB,
		Ctx:         ctxLen,
		Spec:        spec,
		DraftModel:  draft,
		ViaHub:      viaHub, // the stages' links through the hub's raw TCP streams (fleet deploy --engine chain --via-hub)
		Auto:        moeSetting == 0,
	}
}

// planAdvRow is the cursor value of the "advanced" line that folds the expert runtime settings.
const planAdvRow = 10

// planParamSeq lists the settings the cursor visits, top to bottom: the four everyday ones, the advanced line, and
// (when it is open) target, moe cache, spec engine, vision, paging and engine.
func (m model) planParamSeq() []int {
	seq := []int{0, 1, 2, 3, planAdvRow}
	if m.planAdv {
		seq = append(seq, 4, 5, 6, 7, 8, 9)
	}
	return seq
}

func (m model) planParamIdx() int {
	for i, p := range m.planParamSeq() {
		if p == m.planParamCur {
			return i
		}
	}
	return 0
}

func (m model) planParamLast() int {
	seq := m.planParamSeq()
	return seq[len(seq)-1]
}

// advancedRow is the folded line: what it hides, and which of those settings are not at their default.
func (m model) advancedRow(focused bool) string {
	lead, lbl := "  ", descS
	if focused {
		lead, lbl = accS.Render("▸ "), headS
	}
	mark := "▸"
	hint := "target · moe cache · spec engine · vision · paging · engine"
	tail := "  (enter or ←/→ opens)"
	if m.planAdv {
		mark, hint, tail = "▾", "enter or ←/→ folds them again", ""
	} else {
		var parts []string
		if h := m.engineHintText(); strings.Contains(h, "warning") || strings.Contains(h, "recommended") {
			parts = append(parts, "engine: "+h) // what matters must stay visible while the line is folded
		}
		if ch := m.advancedChanged(); len(ch) > 0 {
			parts = append(parts, "changed: "+strings.Join(ch, ", "))
		}
		if len(parts) > 0 {
			hint = strings.Join(parts, " · ")
		}
	}
	return lead + lbl.Render(fmt.Sprintf("%-11s", "advanced")) + " " + accS.Render(mark) + " " + helpS.Render(hint+tail)
}

// advancedChanged names the folded settings that are not at their default.
func (m model) advancedChanged() []string {
	var out []string
	if m.planCPUOnly {
		out = append(out, "CPU only")
	}
	if m.planMoECache != 0 {
		out = append(out, "moe cache "+planMoECaches[m.planMoECache])
	}
	if m.planSpecMode != 0 {
		out = append(out, "spec "+planSpecModes[m.planSpecMode])
	}
	if m.planMMProjCPU {
		out = append(out, "vision on CPU")
	}
	if m.planNoMMap {
		out = append(out, "RAM only")
	}
	if m.planEngineMode != 0 {
		out = append(out, "engine "+planEngineModes[m.planEngineMode])
	}
	return out
}

// engineHintText is what the engine line says about the plan's candidate (RTT, why not chain …).
func (m model) engineHintText() string {
	engineHint := ""
	if m.planRes != nil && len(m.planRes.Candidates) > 0 {
		cand := m.planRes.Candidates[m.planPick%len(m.planRes.Candidates)]
		effEng, highRTT, rttVal, whyNotChain := m.effectiveEngine(cand)
		if m.planEngineMode == 0 {
			if whyNotChain != "" {
				engineHint = fmt.Sprintf("ring: %s", whyNotChain)
			} else if highRTT {
				engineHint = fmt.Sprintf("high RTT (%.0fms) → chain recommended", rttVal)
			} else {
				engineHint = "low RTT → ring default"
			}
		} else if m.planEngineMode == 2 {
			if whyNotChain != "" {
				engineHint = fmt.Sprintf("warning: %s", whyNotChain)
			}
		} else {
			if highRTT && effEng == "ring" {
				engineHint = fmt.Sprintf("warning: high RTT (%.0fms), chain recommended", rttVal)
			}
		}
	}
	return engineHint
}
