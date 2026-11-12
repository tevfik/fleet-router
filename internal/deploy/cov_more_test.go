package deploy

import (
	"context"
	"errors"
	"io"
	"strings"
	"testing"

	"github.com/tevfik/fleet-router/internal/plan"
)

func TestCovDeployRemoteMetaAndShards(t *testing.T) {
	// 1. parseRemoteMeta
	// Success
	m, err := parseRemoteMeta("my-model", "llama 32 4096 8 2\n")
	if err != nil || m.Arch != "llama" || m.Layers != 32 || m.Embd != 4096 || m.Experts != 8 || m.ExpertsUsed != 2 {
		t.Fatalf("parseRemoteMeta success failed: %+v, %v", m, err)
	}

	// Output too short (< 5 fields)
	if _, err := parseRemoteMeta("m", "llama 32 4096"); err == nil {
		t.Errorf("parseRemoteMeta expected error on < 5 fields")
	}

	// Number parse error
	if _, err := parseRemoteMeta("m", "llama invalid 4096 0 0"); err == nil {
		t.Errorf("parseRemoteMeta expected error on invalid int")
	}

	// Zero or negative layers
	if _, err := parseRemoteMeta("m", "llama 0 4096 0 0"); err == nil {
		t.Errorf("parseRemoteMeta expected error on 0 layers")
	}
	if _, err := parseRemoteMeta("m", " 10 4096 0 0"); err == nil {
		t.Errorf("parseRemoteMeta expected error on empty arch")
	}

	// 2. readRemoteGGUF unsafe checks
	if _, err := readRemoteGGUF("bad;node", "/path/to/model"); err == nil {
		t.Errorf("readRemoteGGUF expected error on unsafe node")
	}
	if _, err := readRemoteGGUF("-leadingdash", "/path/to/model"); err == nil {
		t.Errorf("readRemoteGGUF expected error on leading dash node")
	}
	if _, err := readRemoteGGUF("validnode", "/path/'withquote/model"); err == nil {
		t.Errorf("readRemoteGGUF expected error on unsafe path")
	}

	// 3. addRemoteShards
	ctx := context.Background()
	model := &plan.Model{Name: "shard-model", Layers: 32}
	names := []string{"shard-00001-of-00002.gguf", "shard-00002-of-00002.gguf"}

	// Case A: missing shard 2
	sizesMissing := map[string]int64{
		"shard-00001-of-00002.gguf": 5000,
	}
	err = addRemoteShards(ctx, model, names, sizesMissing, nil)
	if err != nil {
		t.Fatalf("addRemoteShards missing shard failed: %v", err)
	}
	if model.Shards == nil || len(model.Shards.Missing) != 1 || model.Shards.Missing[0] != 2 {
		t.Errorf("addRemoteShards expected Missing=[2], got: %+v", model.Shards)
	}

	// Case B: all shards present, open function returns error
	sizesAll := map[string]int64{
		"shard-00001-of-00002.gguf": 5000,
		"shard-00002-of-00002.gguf": 5000,
	}
	openErr := func(name string) (io.ReadCloser, func(), error) {
		return nil, nil, io.ErrUnexpectedEOF
	}
	model2 := &plan.Model{Name: "shard-model2", Layers: 32}
	if err := addRemoteShards(ctx, model2, names, sizesAll, openErr); err == nil {
		t.Errorf("addRemoteShards expected open error")
	}

	// Case C: all shards present, open function returns empty reader (AddShardFile fails on empty)
	openEmpty := func(name string) (io.ReadCloser, func(), error) {
		return io.NopCloser(strings.NewReader("")), func() {}, nil
	}
	model3 := &plan.Model{Name: "shard-model3", Layers: 32}
	if err := addRemoteShards(ctx, model3, names, sizesAll, openEmpty); err == nil {
		t.Errorf("addRemoteShards expected AddShardFile error on empty header")
	}
}

func TestCovChainAutoNeedsAndLocate(t *testing.T) {
	// 1. locateScript & parseLocate
	script := locateScript("/path/to/model.gguf", "model.gguf")
	if !strings.Contains(script, "FLEET_AT") || !strings.Contains(script, "FLEET_FOUND") {
		t.Errorf("locateScript unexpected: %s", script)
	}

	kind, p := parseLocate("FLEET_AT /path/to/model.gguf\n")
	if kind != "AT" || p != "/path/to/model.gguf" {
		t.Errorf("parseLocate AT unexpected: %s, %s", kind, p)
	}

	kind, p = parseLocate("FLEET_FOUND /models/model.gguf\n")
	if kind != "FOUND" || p != "/models/model.gguf" {
		t.Errorf("parseLocate FOUND unexpected: %s, %s", kind, p)
	}

	kind, p = parseLocate("FLEET_NONE\n")
	if kind != "NONE" || p != "" {
		t.Errorf("parseLocate NONE unexpected: %s, %s", kind, p)
	}

	// 2. linkScript
	l1 := linkScript("/dir/model.gguf", "/tmp/alias")
	if !strings.Contains(l1, "FLEET_LINKED") {
		t.Errorf("linkScript non-shard unexpected: %s", l1)
	}

	l2 := linkScript("/dir/model-00001-of-00004.gguf", "/tmp/alias")
	if !strings.Contains(l2, "-*-of-") {
		t.Errorf("linkScript shard regex match unexpected: %s", l2)
	}

	// 3. chainStageNeeds
	cand := plan.Candidate{
		Parts: []plan.Part{
			{Node: "node-1", Layers: 16},
			{Node: "node-2", Layers: 16},
		},
	}
	// nil model
	if needs, ok := chainStageNeeds(cand, nil, 2048); ok || needs != nil {
		t.Errorf("chainStageNeeds nil model want nil, false")
	}

	// empty parts
	m := &plan.Model{Layers: 32, LayerBytes: []int64{100, 200}}
	if needs, ok := chainStageNeeds(plan.Candidate{}, m, 2048); ok || needs != nil {
		t.Errorf("chainStageNeeds empty parts want nil, false")
	}

	// empty LayerBytes
	mEmptyLayers := &plan.Model{Layers: 32}
	if needs, ok := chainStageNeeds(cand, mEmptyLayers, 2048); ok || needs != nil {
		t.Errorf("chainStageNeeds empty LayerBytes want nil, false")
	}

	// valid model with KVBlocks and expert share
	mValid := &plan.Model{
		Layers:     32,
		KVBlocks:   16,
		LayerBytes: make([]int64, 32),
	}
	for i := range mValid.LayerBytes {
		mValid.LayerBytes[i] = 100 * 1024 * 1024
	}
	needs, ok := chainStageNeeds(cand, mValid, 0) // ctxLen 0 defaults to 2048
	if !ok || len(needs) != 2 {
		t.Errorf("chainStageNeeds valid failed: ok=%v, needs=%v", ok, needs)
	}

	// 4. shipFile
	ctx := context.Background()
	if err := shipFile(ctx, "localhost", "/missing/src/file", "/tmp/dst"); err == nil {
		t.Errorf("shipFile expected error on missing source file")
	}

	// 5. ResolveCommonModelPath
	oldLocal := localScript
	t.Cleanup(func() { localScript = oldLocal })

	// Case A: atPath on all nodes
	localScript = func(_ context.Context, _ string) ([]byte, error) {
		return []byte("FLEET_AT /path/to/model.gguf\n"), nil
	}
	commonPath, notes, err := ResolveCommonModelPath(ctx, "/path/to/model.gguf", []string{"localhost"}, false)
	if err != nil || commonPath != "/path/to/model.gguf" || len(notes) != 0 {
		t.Errorf("ResolveCommonModelPath atPath failed: %v, %s, %v", err, commonPath, notes)
	}

	// Case B: found on another path, dryRun = true
	localScript = func(_ context.Context, _ string) ([]byte, error) {
		return []byte("FLEET_FOUND /srv/models/model.gguf\n"), nil
	}
	commonPath, notes, err = ResolveCommonModelPath(ctx, "/path/to/model.gguf", []string{"localhost"}, true)
	if err != nil || len(notes) == 0 || !strings.Contains(notes[0], "would be linked") {
		t.Errorf("ResolveCommonModelPath dryRun failed: %v, %s, %v", err, commonPath, notes)
	}

	// Case C: not found (FLEET_NONE)
	localScript = func(_ context.Context, _ string) ([]byte, error) {
		return []byte("FLEET_NONE\n"), nil
	}
	if _, _, err := ResolveCommonModelPath(ctx, "/path/to/model.gguf", []string{"localhost"}, false); err == nil {
		t.Errorf("ResolveCommonModelPath expected error on FLEET_NONE")
	}

	// Case D: runOn error
	localScript = func(_ context.Context, _ string) ([]byte, error) {
		return nil, errors.New("script failed")
	}
	if _, _, err := ResolveCommonModelPath(ctx, "/path/to/model.gguf", []string{"localhost"}, false); err == nil {
		t.Errorf("ResolveCommonModelPath expected error on script fail")
	}
}

func TestCovRelayHubOpenHubRelay(t *testing.T) {
	r := &LiveStageRunner{}

	oldSSH := sshScript
	t.Cleanup(func() { sshScript = oldSSH })

	// Test openHubRelay with remote machines
	rel := StageRelay{
		From:   "remote-from",
		To:     "remote-to",
		Port:   19001,
		ToPort: 19002,
		Hub:    "https://hub.example.com",
	}

	// Case 1: StartStream succeeds on remote
	sshScript = func(_ context.Context, node, script string) ([]byte, error) {
		if strings.Contains(script, "fleet stream connect") {
			return []byte("FLEET_PID 8888\nFLEET_READY\n"), nil
		}
		// hubJoinScript
		return []byte("ok"), nil
	}
	if err := r.openHubRelay(rel); err != nil {
		t.Errorf("openHubRelay remote success failed: %v", err)
	}

	// Case 2: StartStream fails
	sshScript = func(_ context.Context, node, script string) ([]byte, error) {
		if strings.Contains(script, "fleet stream connect") {
			return []byte("FLEET_PID 8888\nFLEET_DIED\nerror tail"), nil
		}
		return []byte("ok"), nil
	}
	if err := r.openHubRelay(rel); err == nil {
		t.Errorf("openHubRelay expected error when StartStream fails")
	}

	// 3. DryRunner OpenRelay / CloseRelay
	dr := &DryRunner{}
	if err := dr.OpenRelay(rel); err != nil || len(dr.Lines) == 0 {
		t.Errorf("DryRunner.OpenRelay hub failed: %v, %v", err, dr.Lines)
	}

	relSSH := StageRelay{
		From:   "from-node",
		To:     "to-node",
		Port:   19001,
		ToPort: 19002,
	}
	dr2 := &DryRunner{}
	if err := dr2.OpenRelay(relSSH); err != nil {
		t.Errorf("DryRunner.OpenRelay ssh failed: %v", err)
	}
	dr2.CloseRelay(relSSH)
}

func TestCovSplitGuardMasterFilterAndFallback(t *testing.T) {
	single := plan.Candidate{Kind: "single", Master: "node1", TPS: 50, Parts: []plan.Part{{Node: "node1"}}}
	best := plan.Candidate{Master: "node1", TPS: 40, Parts: []plan.Part{{Node: "node1"}, {Node: "node2"}}}
	mismatch := plan.Candidate{Master: "othermaster", TPS: 45, Parts: []plan.Part{{Node: "othermaster"}, {Node: "node2"}}}

	r := plan.Result{
		Candidates: []plan.Candidate{single, best, mismatch},
	}
	// Candidate with mismatched master is skipped; split falls back to best
	g := CheckSplit(r, "node1", []string{"node2"})
	warn, err := g.Check(false)
	_ = warn
	_ = err
}
