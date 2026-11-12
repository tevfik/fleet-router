package deploy

import (
	"context"
	"crypto/sha256"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"time"

	"github.com/tevfik/fleet-router/internal/plan"
)

// E13: `fleet deploy --engine chain --auto` sets a chain up from what the machines have, the steps that were done by
// hand for Ornith-122B over dino and papa:
//   - the model at the same absolute path on every stage machine (a stage is started with the same -m everywhere, the
//     home directories differ): found on each machine by name, linked under /tmp/fleet-models/<id>/;
//   - the head-embedded draft kept (and shipped to the master) under /tmp/fleet-models/heads, a path every machine has;
//   - experts on the CPU only when the GPU cannot hold the stage, and the MoE cache sized from the VRAM that is free
//     now (another program holding the GPU made `cudaMalloc failed` two minutes into the start);
//   - layers split in proportion to the memory each machine has free;
//   - a ready wait that follows the size of the stage, not a fixed two minutes.
// Without --auto nothing here runs: the deploy does exactly what the flags say.

const (
	commonModelDir       = "/tmp/fleet-models"
	chainHeadDir         = commonModelDir + "/heads"
	chainComputeGiB      = 1.5 // a stage's compute buffer (a 23-layer Qwen3.5 MoE stage reported 1.3 GiB)
	chainVRAMHeadroomGiB = 0.5
	maxAutoMoECacheMiB   = 8192
	minAutoMoECacheMiB   = 512
	autoCacheStepMiB     = 256
)

// runOn runs a sh script on node: here, or over ssh.
func runOn(ctx context.Context, node, script string) ([]byte, error) {
	if defaultIsThisMachine(node) {
		return localScript(ctx, script)
	}
	return sshScript(ctx, node, script)
}

// shipFile copies the local file src to dst on node (a temporary name first, so a half-sent file is never used).
var shipFile = func(ctx context.Context, node, src, dst string) error {
	f, err := os.Open(src)
	if err != nil {
		return err
	}
	defer f.Close()
	dir := filepath.Dir(dst)
	script := fmt.Sprintf("mkdir -p %s && cat > %s && mv %s %s", shellQuote(dir), shellQuote(dst+".part"), shellQuote(dst+".part"), shellQuote(dst))
	cmd := exec.CommandContext(ctx, "ssh", "-o", "BatchMode=yes", "-o", "ConnectTimeout=5", "--", node, script)
	cmd.Stdin = f
	if out, err := cmd.CombinedOutput(); err != nil {
		return fmt.Errorf("%w: %s", err, strings.TrimSpace(string(out)))
	}
	return nil
}

func chainNodes(c plan.Candidate) []string {
	var out []string
	seen := map[string]bool{}
	withLayers := partLayers(c) > 0
	for _, p := range c.Parts {
		if withLayers && p.Layers <= 0 {
			continue // a master without layers is not a stage of the chain (PlanStagePipe leaves it out)
		}
		if !seen[p.Node] {
			seen[p.Node] = true
			out = append(out, p.Node)
		}
	}
	return out
}

// locateScript says where the model file is on a machine: at path itself (FLEET_AT), under one of the usual model
// directories by its name (FLEET_FOUND <path>), or nowhere (FLEET_NONE).
func locateScript(path, base string) string {
	return fmt.Sprintf(`P=%s
B=%s
if test -f "$P"; then echo "FLEET_AT $P"; exit 0; fi
for d in "$HOME/models" "$HOME/llm-cluster" "$HOME/.cache/fleet" /opt/models /srv/models /data/models; do
  test -d "$d" || continue
  f=$(find -L "$d" -maxdepth 4 -name "$B" -type f 2>/dev/null | head -n 1)
  if test -n "$f"; then echo "FLEET_FOUND $f"; exit 0; fi
done
echo FLEET_NONE
`, shellQuote(path), shellQuote(base))
}

func parseLocate(out string) (kind, path string) {
	for _, ln := range strings.Split(out, "\n") {
		ln = strings.TrimSpace(ln)
		switch {
		case strings.HasPrefix(ln, "FLEET_AT "):
			return "AT", strings.TrimPrefix(ln, "FLEET_AT ")
		case strings.HasPrefix(ln, "FLEET_FOUND "):
			return "FOUND", strings.TrimPrefix(ln, "FLEET_FOUND ")
		}
	}
	return "NONE", ""
}

var shardRe = regexp.MustCompile(`^(.*)-\d{5}-of-(\d{5})\.gguf$`)

// linkScript links the model file src, and the other shards of its set, into aliasDir.
func linkScript(src, aliasDir string) string {
	dir, name := filepath.Split(src)
	pattern := shellQuote(src)
	if m := shardRe.FindStringSubmatch(name); m != nil {
		pattern = shellQuote(dir+m[1]) + "-*-of-" + shellQuote(m[2]+".gguf")
	}
	return fmt.Sprintf(`set -e
mkdir -p %s
n=0
for f in %s; do
  test -e "$f" || continue
  ln -sfn "$f" %s/"$(basename "$f")"
  n=$((n+1))
done
test "$n" -gt 0 && echo FLEET_LINKED
`, shellQuote(aliasDir), pattern, shellQuote(aliasDir))
}

// ResolveCommonModelPath returns a path the model has on every node. modelPath itself when every machine has it
// there; else a link under /tmp/fleet-models/<id>/ made on every machine (the file is found by name in the usual
// model directories). A machine that has the model nowhere is an error naming it. dryRun only looks: nothing is linked.
func ResolveCommonModelPath(ctx context.Context, modelPath string, nodes []string, dryRun bool) (string, []string, error) {
	base := filepath.Base(modelPath)
	src := map[string]string{}
	atPath := true
	for _, n := range nodes {
		cctx, cancel := context.WithTimeout(ctx, 60*time.Second)
		out, err := runOn(cctx, n, locateScript(modelPath, base))
		cancel()
		if err != nil {
			return "", nil, fmt.Errorf("%s: looking for %s: %w", n, base, err)
		}
		kind, found := parseLocate(string(out))
		switch kind {
		case "AT":
			src[n] = modelPath
		case "FOUND":
			src[n] = found
			atPath = false
		default:
			return "", nil, fmt.Errorf("the model %s is not on %s (looked at %s and in its model directories); copy it with `fleet store` or scp", base, n, modelPath)
		}
	}
	if atPath {
		return modelPath, nil, nil
	}
	id := fmt.Sprintf("%x", sha256.Sum256([]byte(base)))[:12]
	aliasDir := commonModelDir + "/" + id
	var where []string
	for _, n := range nodes {
		if !dryRun {
			cctx, cancel := context.WithTimeout(ctx, 60*time.Second)
			out, err := runOn(cctx, n, linkScript(src[n], aliasDir))
			cancel()
			if err != nil || !strings.Contains(string(out), "FLEET_LINKED") {
				return "", nil, fmt.Errorf("%s: linking %s into %s failed: %v", n, src[n], aliasDir, err)
			}
		}
		where = append(where, fmt.Sprintf("%s: %s", n, src[n]))
	}
	alias := aliasDir + "/" + base
	verb := "linked"
	if dryRun {
		verb = "would be linked"
	}
	return alias, []string{fmt.Sprintf("auto: the model is not at %s on every machine: %s as %s (%s)", modelPath, verb, alias, strings.Join(where, "; "))}, nil
}

// stageNeed is what one stage holds.
type stageNeed struct {
	node                        string
	layers                      int
	weightsGiB, denseGiB, kvGiB float64
}

// chainStageNeeds sizes the stages of c the way the engine will cut them (the NextN tail is not loaded). false: the
// GGUF listed no per-layer sizes.
func chainStageNeeds(c plan.Candidate, m *plan.Model, ctxLen int) ([]stageNeed, bool) {
	if t := trimNextNTail(c, m); t.Parts != nil {
		c = t
	}
	if m == nil || len(c.Parts) == 0 || len(m.LayerBytes) == 0 {
		return nil, false
	}
	expShare := 0.0
	if m.ExpertWeights() > 0 && m.Weights() > 0 {
		expShare = float64(m.ExpertWeights()) / float64(m.Weights())
	}
	kvScale := 1.0
	if m.KVBlocks > 0 && m.KVBlocks < m.Layers {
		kvScale = float64(m.KVBlocks) / float64(m.Layers)
	}
	if ctxLen <= 0 {
		ctxLen = 2048
	}
	out := make([]stageNeed, len(c.Parts))
	for i, p := range c.Parts {
		if p.First+p.Layers > len(m.LayerBytes) {
			return nil, false
		}
		w := 0.0
		for l := p.First; l < p.First+p.Layers; l++ {
			w += float64(m.LayerBytes[l])
		}
		// the embedding is read on the first stage, the output head on the last
		switch {
		case len(c.Parts) == 1:
			w += float64(m.OtherBytes)
		case i == 0 || i == len(c.Parts)-1:
			w += float64(m.OtherBytes) / 2
		}
		out[i] = stageNeed{
			node:       p.Node,
			layers:     p.Layers,
			weightsGiB: w / gib,
			denseGiB:   w * (1 - expShare) / gib,
			kvGiB:      float64(m.KVPerTokenLayer) * float64(ctxLen) * float64(p.Layers) * kvScale / gib,
		}
	}
	return out, true
}

// AutoTuneChain fills in what the options left open from the machines' memory now (see the top of the file). It
// changes o; the notes say what it chose and why. An error is a stage that cannot fit even with the experts on the
// CPU, said before anything is started.
func AutoTuneChain(ctx context.Context, c plan.Candidate, m *plan.Model, o *StagePipeOptions) ([]string, error) {
	var notes []string
	if o.HeadDir == "" {
		o.HeadDir = chainHeadDir
	}
	needs, ok := chainStageNeeds(c, m, o.Ctx)
	if !ok {
		return append(notes, "auto: the GGUF lists no per-layer sizes: ready wait and MoE settings are left as given"), nil
	}
	if o.ReadyTimeout == 0 {
		top := 0.0
		for _, n := range needs {
			top = max(top, n.weightsGiB)
		}
		// a cold read of the stage's weights from disk: about 15 s per GiB at worst
		o.ReadyTimeout = min(120*time.Second+time.Duration(top*15)*time.Second, 30*time.Minute)
		notes = append(notes, fmt.Sprintf("auto: ready wait %s per stage (largest stage %.0f GiB)", o.ReadyTimeout.Round(time.Second), top))
	}
	if m.ExpertWeights() <= 0 {
		return notes, nil
	}
	free := map[string]MemFree{}
	for _, n := range needs {
		f, err := probeMemory(ctx, n.node, isLocalNode(n.node))
		if err != nil {
			notes = append(notes, fmt.Sprintf("auto: free memory of %s not read (%v): its stage is not sized", n.node, err))
			continue
		}
		free[n.node] = f
	}
	if !o.CPUMoE && o.NCPUMoE == 0 {
		for _, n := range needs {
			if f, ok := free[n.node]; ok && f.VRAMGiB > 0 && n.weightsGiB+n.kvGiB+chainComputeGiB+chainVRAMHeadroomGiB > f.VRAMGiB {
				o.CPUMoE = true
				notes = append(notes, fmt.Sprintf("auto: --cpu-moe: stage %s holds %.1f GiB of weights, %.1f GiB of VRAM is free", n.node, n.weightsGiB, f.VRAMGiB))
				break
			}
		}
	}
	if !o.CPUMoE && o.NCPUMoE == 0 {
		return notes, nil
	}
	cache, sized := maxAutoMoECacheMiB, false
	for i, n := range needs {
		f, ok := free[n.node]
		if !ok || f.VRAMGiB <= 0 {
			continue
		}
		avail := f.VRAMGiB - n.denseGiB - n.kvGiB - chainComputeGiB - chainVRAMHeadroomGiB
		if avail < 0 {
			return notes, fmt.Errorf("stage %d (%s) needs about %.1f GiB of VRAM for its dense weights, KV cache and buffers even with the experts on the CPU, and %.1f GiB is free: something else holds the GPU (nvidia-smi), or use a smaller --ctx or more machines",
				i, n.node, n.denseGiB+n.kvGiB+chainComputeGiB+chainVRAMHeadroomGiB, f.VRAMGiB)
		}
		cache = min(cache, int(avail*1024)/autoCacheStepMiB*autoCacheStepMiB)
		sized = true
		if o.MoECacheMiB > 0 && float64(o.MoECacheMiB)/1024 > avail {
			notes = append(notes, fmt.Sprintf("auto: --moe-cache-mib %d may not fit on %s: %.1f GiB of VRAM is left for it", o.MoECacheMiB, n.node, avail))
		}
	}
	if sized && o.MoECacheMiB == 0 {
		if cache >= minAutoMoECacheMiB {
			o.MoECacheMiB = cache
			notes = append(notes, fmt.Sprintf("auto: --moe-cache-mib %d (the VRAM left on the tightest machine)", cache))
		} else {
			notes = append(notes, "auto: no VRAM left for an MoE cache: experts are read from RAM")
		}
	}
	return notes, nil
}

// AutoSplitParts cuts the model's layers over nodes in proportion to the memory each has free (RAM and the first
// GPU's VRAM). nil: a machine could not be read; the caller splits evenly.
func AutoSplitParts(ctx context.Context, m *plan.Model, nodes []string) ([]plan.Part, []string) {
	if m == nil || m.Layers < len(nodes) || len(nodes) < 2 {
		return nil, nil
	}
	weights := make([]float64, len(nodes))
	var desc []string
	for i, n := range nodes {
		f, err := probeMemory(ctx, n, isLocalNode(n))
		if err != nil {
			return nil, []string{fmt.Sprintf("auto: free memory of %s not read (%v): layers split evenly", n, err)}
		}
		weights[i] = max(f.RAMGiB+f.VRAMGiB-chainVRAMHeadroomGiB, 0.1)
		desc = append(desc, fmt.Sprintf("%s %.0f GiB", n, f.RAMGiB+f.VRAMGiB))
	}
	counts := proportional(m.Layers, weights)
	parts := make([]plan.Part, len(nodes))
	first := 0
	for i, n := range nodes {
		parts[i] = plan.Part{Node: n, First: first, Layers: counts[i], Output: i == len(nodes)-1}
		first += counts[i]
	}
	return parts, []string{fmt.Sprintf("auto: layers split by free memory (%s): %v", strings.Join(desc, ", "), counts)}
}

// shipDraft makes sure the first stage's machine has the head-embedded draft at the path the plan names.
func shipDraft(ctx context.Context, runner StageRunner, node, path string) error {
	if defaultIsThisMachine(node) {
		return nil
	}
	if ok, err := runner.Exists(node, path); err == nil && ok {
		return nil
	}
	return shipFile(ctx, node, path, path)
}
