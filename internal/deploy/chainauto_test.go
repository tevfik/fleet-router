package deploy

import (
	"path/filepath"

	"context"
	"github.com/tevfik/fleet-router/internal/ggufhead"
	"strings"
	"testing"
	"time"

	"github.com/tevfik/fleet-router/internal/plan"
)

// autoModel: 48 layers of 1 GiB, a share of it experts.
func autoModel(expertGiB float64) *plan.Model {
	m := &plan.Model{Name: "moe", Layers: 48, KVPerTokenLayer: 0}
	for i := 0; i < 48; i++ {
		m.LayerBytes = append(m.LayerBytes, 1<<30)
		m.ExpertBytes = append(m.ExpertBytes, int64(expertGiB*float64(1<<30)))
	}
	return m
}

func autoCand() plan.Candidate {
	return plan.Candidate{Parts: []plan.Part{
		{Node: "big", First: 0, Layers: 24},
		{Node: "gpu", First: 24, Layers: 24, Output: true},
	}}
}

func stubMem(t *testing.T, mem map[string]MemFree) {
	t.Helper()
	old := probeMemory
	probeMemory = func(_ context.Context, node string, _ bool) (MemFree, error) {
		f, ok := mem[node]
		if !ok {
			return MemFree{}, context.DeadlineExceeded
		}
		return f, nil
	}
	t.Cleanup(func() { probeMemory = old })
}

// the GPU cannot hold the stage: experts on the CPU, the cache is what the VRAM leaves (3.9 GiB free, 1.2 dense,
// 2.0 of buffers and headroom: 0.7 GiB, 512 MiB in steps of 256)
func TestAutoTuneChainSizesTheMoECacheFromFreeVRAM(t *testing.T) {
	stubMem(t, map[string]MemFree{"big": {RAMGiB: 100}, "gpu": {RAMGiB: 50, VRAMGiB: 3.9}})
	o := &StagePipeOptions{}
	notes, err := AutoTuneChain(context.Background(), autoCand(), autoModel(0.95), o)
	if err != nil {
		t.Fatal(err)
	}
	if !o.CPUMoE || o.MoECacheMiB != 512 {
		t.Errorf("cpu-moe %v cache %d, want true 512 (notes %v)", o.CPUMoE, o.MoECacheMiB, notes)
	}
	if o.ReadyTimeout != 120*time.Second+24*15*time.Second {
		t.Errorf("ready wait %s", o.ReadyTimeout)
	}
	if o.HeadDir != chainHeadDir {
		t.Errorf("head dir %q", o.HeadDir)
	}
}

func TestAutoTuneChainLeavesAStageThatFitsAlone(t *testing.T) {
	stubMem(t, map[string]MemFree{"big": {RAMGiB: 100}, "gpu": {RAMGiB: 50, VRAMGiB: 80}})
	o := &StagePipeOptions{}
	if _, err := AutoTuneChain(context.Background(), autoCand(), autoModel(0.95), o); err != nil {
		t.Fatal(err)
	}
	if o.CPUMoE || o.MoECacheMiB != 0 {
		t.Errorf("a stage that fits the GPU got cpu-moe %v cache %d", o.CPUMoE, o.MoECacheMiB)
	}
}

// the dense weights, KV and buffers do not fit even with the experts on the CPU: said before anything starts
func TestAutoTuneChainRefusesAStageThatCannotFit(t *testing.T) {
	stubMem(t, map[string]MemFree{"big": {RAMGiB: 100}, "gpu": {RAMGiB: 50, VRAMGiB: 3.9}})
	o := &StagePipeOptions{}
	_, err := AutoTuneChain(context.Background(), autoCand(), autoModel(0.5), o) // 12 GiB dense
	if err == nil || !strings.Contains(err.Error(), "stage 1 (gpu)") || !strings.Contains(err.Error(), "3.9 GiB is free") {
		t.Fatalf("err = %v", err)
	}
}

// what the user gave is kept (a note when it may not fit); a model without experts is not probed at all
func TestAutoTuneChainKeepsTheUsersValues(t *testing.T) {
	stubMem(t, map[string]MemFree{"big": {RAMGiB: 100}, "gpu": {RAMGiB: 50, VRAMGiB: 3.9}})
	o := &StagePipeOptions{CPUMoE: true, MoECacheMiB: 2000, ReadyTimeout: time.Minute}
	notes, err := AutoTuneChain(context.Background(), autoCand(), autoModel(0.95), o)
	if err != nil {
		t.Fatal(err)
	}
	if o.MoECacheMiB != 2000 || o.ReadyTimeout != time.Minute {
		t.Errorf("changed the user's values: %+v", o)
	}
	if !strings.Contains(strings.Join(notes, "\n"), "may not fit on gpu") {
		t.Errorf("no note: %v", notes)
	}
	old := probeMemory
	probeMemory = func(context.Context, string, bool) (MemFree, error) {
		t.Error("a dense model was probed")
		return MemFree{}, nil
	}
	defer func() { probeMemory = old }()
	if _, err := AutoTuneChain(context.Background(), autoCand(), autoModel(0), &StagePipeOptions{}); err != nil {
		t.Fatal(err)
	}
}

// the layers follow the memory each machine has free
func TestAutoSplitPartsByFreeMemory(t *testing.T) {
	stubMem(t, map[string]MemFree{"big": {RAMGiB: 100}, "gpu": {RAMGiB: 40, VRAMGiB: 10}})
	parts, notes := AutoSplitParts(context.Background(), autoModel(0.9), []string{"big", "gpu"})
	if len(parts) != 2 || parts[0].Layers != 32 || parts[1].Layers != 16 || parts[1].First != 32 || !parts[1].Output || parts[0].Output {
		t.Errorf("parts %+v notes %v", parts, notes)
	}
	stubMem(t, map[string]MemFree{"big": {RAMGiB: 100}}) // gpu cannot be read
	if parts, _ := AutoSplitParts(context.Background(), autoModel(0.9), []string{"big", "gpu"}); parts != nil {
		t.Errorf("a machine that could not be read still got a split: %+v", parts)
	}
}

func TestLinkScriptKeepsTheShardsTogether(t *testing.T) {
	s := linkScript("/home/x/models/q/Qwen-IQ3_S-00001-of-00003.gguf", "/tmp/fleet-models/ab")
	if !strings.Contains(s, "/home/x/models/q/Qwen-IQ3_S-*-of-00003.gguf") {
		t.Errorf("no shard glob: %s", s)
	}
	if s := linkScript("/m/a.gguf", "/tmp/fleet-models/ab"); strings.Contains(s, "*") {
		t.Errorf("a single file got a glob: %s", s)
	}
}

func stubScripts(t *testing.T, local, remote func(script string) string) (calls *[]string) {
	t.Helper()
	oldL, oldS := localScript, sshScript
	var log []string
	localScript = func(_ context.Context, s string) ([]byte, error) {
		log = append(log, "local")
		return []byte(local(s)), nil
	}
	sshScript = func(_ context.Context, node, s string) ([]byte, error) {
		log = append(log, node)
		return []byte(remote(s)), nil
	}
	t.Cleanup(func() { localScript, sshScript = oldL, oldS })
	return &log
}

// every machine has the file at the path: nothing is linked
func TestResolveCommonModelPathKeepsAPathEveryoneHas(t *testing.T) {
	calls := stubScripts(t, func(string) string { return "FLEET_AT /m/a.gguf\n" }, func(string) string { return "FLEET_AT /m/a.gguf\n" })
	got, notes, err := ResolveCommonModelPath(context.Background(), "/m/a.gguf", []string{"localhost", "far"}, false)
	if err != nil || got != "/m/a.gguf" || len(notes) != 0 || len(*calls) != 2 {
		t.Fatalf("%q %v %v %v", got, notes, err, *calls)
	}
}

// one machine keeps it elsewhere: every machine gets a link under the same path
func TestResolveCommonModelPathLinksWhereItDiffers(t *testing.T) {
	found := func(s string) string {
		if strings.Contains(s, "FLEET_LINKED") {
			return "FLEET_LINKED\n"
		}
		return "FLEET_FOUND /home/b/models/ornith/a.gguf\n"
	}
	here := func(s string) string {
		if strings.Contains(s, "FLEET_LINKED") {
			return "FLEET_LINKED\n"
		}
		return "FLEET_AT /m/a.gguf\n"
	}
	calls := stubScripts(t, here, found)
	got, notes, err := ResolveCommonModelPath(context.Background(), "/m/a.gguf", []string{"localhost", "far"}, false)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(got, commonModelDir+"/") || !strings.HasSuffix(got, "/a.gguf") || len(notes) != 1 {
		t.Errorf("%q %v", got, notes)
	}
	if strings.Join(*calls, ",") != "local,far,local,far" {
		t.Errorf("calls %v", *calls)
	}
}

func TestResolveCommonModelPathNamesTheMachineWithoutIt(t *testing.T) {
	stubScripts(t, func(string) string { return "FLEET_AT /m/a.gguf\n" }, func(string) string { return "FLEET_NONE\n" })
	_, _, err := ResolveCommonModelPath(context.Background(), "/m/a.gguf", []string{"localhost", "far"}, false)
	if err == nil || !strings.Contains(err.Error(), "not on far") {
		t.Fatalf("err = %v", err)
	}
}

// the master on another machine gets the head-embedded draft at the very same path, once
func TestShipDraftSendsOnlyWhatIsMissingOnARemoteMaster(t *testing.T) {
	var sent []string
	old := shipFile
	shipFile = func(_ context.Context, node, src, dst string) error {
		sent = append(sent, node+" "+src+" "+dst)
		return nil
	}
	t.Cleanup(func() { shipFile = old })
	ctx := context.Background()
	if err := shipDraft(ctx, &DryRunner{}, "localhost", "/tmp/fleet-models/heads/h.gguf"); err != nil || len(sent) != 0 {
		t.Fatalf("local master: %v %v", err, sent)
	}
	if err := shipDraft(ctx, &DryRunner{FakeExists: func(string, string) bool { return true }}, "far", "/tmp/fleet-models/heads/h.gguf"); err != nil || len(sent) != 0 {
		t.Fatalf("already there: %v %v", err, sent)
	}
	if err := shipDraft(ctx, &DryRunner{FakeExists: func(string, string) bool { return false }}, "far", "/tmp/fleet-models/heads/h.gguf"); err != nil {
		t.Fatal(err)
	}
	if len(sent) != 1 || sent[0] != "far /tmp/fleet-models/heads/h.gguf /tmp/fleet-models/heads/h.gguf" {
		t.Errorf("sent %v", sent)
	}
}

// HeadDir moves the cached head out of ~/.cache/fleet/dflash
func TestEnsureCachedHeadUsesTheGivenDirectory(t *testing.T) {
	dir := t.TempDir()
	tgt, draft := filepath.Join(dir, "model.gguf"), filepath.Join(dir, "draft.gguf")
	writeTestGGUF(t, tgt, "qwen2", nil, "output.weight")
	writeTestGGUF(t, draft, "dflash", []uint32{1, 2}, "fc.weight")
	old := embedHeadFn
	embedHeadFn = func(_, _, out string) (ggufhead.Result, error) {
		writeTestGGUF(t, out, "dflash", []uint32{1, 2}, "fc.weight", "output.weight")
		return ggufhead.Result{}, nil
	}
	t.Cleanup(func() { embedHeadFn = old })
	heads := filepath.Join(dir, "heads")
	got, err := ensureCachedHead(draft, tgt, heads)
	if err != nil || filepath.Dir(got) != heads {
		t.Fatalf("head at %q (%v), want a file in %s", got, err, heads)
	}
}

// a dry run looks and names the common path, but links nothing
func TestResolveCommonModelPathDryRunLinksNothing(t *testing.T) {
	calls := stubScripts(t, func(string) string { return "FLEET_AT /m/a.gguf\n" }, func(string) string { return "FLEET_FOUND /home/b/a.gguf\n" })
	got, notes, err := ResolveCommonModelPath(context.Background(), "/m/a.gguf", []string{"localhost", "far"}, true)
	if err != nil || !strings.HasPrefix(got, commonModelDir+"/") || len(*calls) != 2 || !strings.Contains(notes[0], "would be linked") {
		t.Fatalf("%q %v %v %v", got, notes, err, *calls)
	}
}
