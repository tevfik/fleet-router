package deploy

import (
	"fmt"
	"strings"
	"testing"

	"github.com/tevfik/fleet-router/internal/plan"
)

func stubSizes(t *testing.T, files map[string]int64) {
	t.Helper()
	old := fileSizeOn
	fileSizeOn = func(node, path string) (int64, error) {
		if n, ok := files[node+":"+path]; ok {
			return n, nil
		}
		return 0, fmt.Errorf("no such file")
	}
	t.Cleanup(func() { fileSizeOn = old })
}

func TestLocalWeightsOffByDefault(t *testing.T) {
	stubPlanner(t, "llama")
	stubSizes(t, map[string]int64{"dino:/models/ornith-122b.gguf": 100, "victor:/models/ornith-122b.gguf": 100})
	res := dry(t, DeployOptions{WorkerNodes: []string{"victor"}})
	if strings.Contains(res.WorkerEnv["victor"], "SHARD") {
		t.Errorf("default must not pass shard env: %v", res.WorkerEnv)
	}
}

// A model with NextN heads in its GGUF puts the master on the tail (RING0,CUDA0), so the worker holds the head blocks
// 0..11 (+ margin). (This used to hang on "ornith" in the file name; the metadata says it now.)
func TestLocalWeightsPassesPathManifestBlocks(t *testing.T) {
	stubPlanner(t, "llama")
	stubMeta(t, &plan.Model{Name: "m", Arch: "anyarch", Layers: 48, HasMTP: true, MTPLayers: 1})
	stubSizes(t, map[string]int64{
		"dino:/models/ornith-122b.gguf":    100,
		"dino:/models/fleet-shards.json":   5,
		"victor:/models/ornith-122b.gguf":  100,
		"victor:/models/fleet-shards.json": 5,
	})
	res := dry(t, DeployOptions{WorkerNodes: []string{"victor"}, LocalWeights: true, TensorSplit: "36,12"})
	env := res.WorkerEnv["victor"]
	for _, want := range []string{"GGML_RING_SHARD_GGUF=/models/ornith-122b.gguf", "GGML_RING_SHARD_MANIFEST=/models/fleet-shards.json", "GGML_RING_SHARD_BLOCKS=0-13"} {
		if !strings.Contains(env, want) {
			t.Errorf("missing %s in %q", want, env)
		}
	}
}

func TestLocalWeightsSizeMismatchWarnsAndSkips(t *testing.T) {
	stubPlanner(t, "llama")
	stubSizes(t, map[string]int64{"dino:/models/ornith-122b.gguf": 100, "victor:/models/ornith-122b.gguf": 99})
	res := dry(t, DeployOptions{WorkerNodes: []string{"victor"}, LocalWeights: true})
	if strings.Contains(res.WorkerEnv["victor"], "SHARD") {
		t.Errorf("a file of a different size must not be used: %v", res.WorkerEnv)
	}
	if !strings.Contains(strings.Join(res.Warnings, "\n"), "99 bytes") {
		t.Errorf("no warning: %v", res.Warnings)
	}
}

func TestLocalWeightsPerNodePathAndMissingFile(t *testing.T) {
	stubPlanner(t, "llama")
	stubSizes(t, map[string]int64{"dino:/models/ornith-122b.gguf": 100, "victor:/data/o.gguf": 100})
	res := dry(t, DeployOptions{WorkerNodes: []string{"victor", "papa"}, LocalWeights: true, WorkerModels: map[string]string{"victor": "/data/o.gguf"}})
	if !strings.Contains(res.WorkerEnv["victor"], "GGML_RING_SHARD_GGUF=/data/o.gguf") {
		t.Errorf("per-node path: %v", res.WorkerEnv)
	}
	if strings.Contains(res.WorkerEnv["victor"], "MANIFEST") {
		t.Errorf("no manifest exists, none must be passed: %v", res.WorkerEnv)
	}
	if strings.Contains(res.WorkerEnv["papa"], "SHARD") || !strings.Contains(strings.Join(res.Warnings, "\n"), "not found on the worker") {
		t.Errorf("missing file: env %v warnings %v", res.WorkerEnv, res.Warnings)
	}
}

func TestWorkerBlockRanges(t *testing.T) {
	// 48 blocks, ts 36,12: the worker takes roughly the last quarter of 49 units; range widened by one, ends at the last block
	r := workerBlockRanges("CUDA0,RING0", "36,12", []string{"w"}, 48, 0)["w"]
	if r != [2]int{35, 47} {
		t.Errorf("tail worker: %v", r)
	}
	// 3 stages, the worker in the middle: [16,32) of 48 -> 16..32 plus margin, never beyond the layers
	r = workerBlockRanges("CUDA0,RING0,RING1", "16,16,16", []string{"a", "b"}, 48, 0)["a"]
	if r[0] != 15 || r[1] != 33 {
		t.Errorf("middle worker: %v", r)
	}
	// the master holds the tail (MTP order RING0,CUDA0): worker starts at block 0, nothing clamps below 0; MTP head blocks are added at the end only for the last device
	r = workerBlockRanges("RING0,CUDA0", "12,36", []string{"w"}, 48, 1)["w"]
	if r != [2]int{0, 13} {
		t.Errorf("head worker: %v", r)
	}
	r = workerBlockRanges("CUDA0,RING0", "36,12", []string{"w"}, 48, 1)["w"]
	if r != [2]int{35, 48} {
		t.Errorf("tail worker with MTP block: %v", r)
	}
	// devices that do not match the nodes one to one: no range
	if len(workerBlockRanges("CUDA0,RING0,RING1", "1,1,1", []string{"a"}, 48, 0)) != 0 {
		t.Error("mismatch must give no range")
	}
}

func TestLocalWeightsIgnoredForMultiFileModels(t *testing.T) {
	stubMeta(t, &plan.Model{Name: "m", Arch: "llama", Layers: 8, Shards: &plan.ShardInfo{Total: 3, FirstShard: "/m/X-00001-of-00003.gguf"}})
	old := fileSizeOn
	fileSizeOn = func(string, string) (int64, error) {
		t.Error("no size must be asked for a multi-file model")
		return 0, nil
	}
	t.Cleanup(func() { fileSizeOn = old })
	res := dry(t, DeployOptions{ModelPath: "/m/X-00001-of-00003.gguf", WorkerNodes: []string{"papa"}, LocalWeights: true})
	if !strings.Contains(strings.Join(res.Warnings, "\n"), "--local-weights ignored: the model is 3 GGUF files") || strings.Contains(res.WorkerEnv["papa"], "GGML_RING_SHARD_GGUF") {
		t.Fatalf("%v %v", res.Warnings, res.WorkerEnv)
	}
}
