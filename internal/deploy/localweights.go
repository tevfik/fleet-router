package deploy

import (
	"context"
	"fmt"
	"math"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

// fileSizeOn is the test seam for "how big is this file on that machine": size, or an error when it is missing or the
// machine cannot be asked. Local nodes use os.Stat, the others one SSH call (no data transfer).
var fileSizeOn = func(node, path string) (int64, error) {
	hostName, _ := os.Hostname()
	if node == "" || node == "localhost" || node == "127.0.0.1" || node == hostName {
		st, err := os.Stat(path)
		if err != nil {
			return 0, err
		}
		return st.Size(), nil
	}
	if !safeEnvText(path) {
		return 0, fmt.Errorf("path %q has characters that are not passed to a remote shell", path)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 8*time.Second)
	defer cancel()
	out, err := exec.CommandContext(ctx, "ssh", "-o", "BatchMode=yes", "-o", "ConnectTimeout=5", "--", node,
		"stat -c %s "+path).Output()
	if err != nil {
		return 0, err
	}
	return strconv.ParseInt(strings.TrimSpace(string(out)), 10, 64)
}

// localWeightsEnv decides, per worker, whether it already holds the model file and returns the environment that makes
// ring-worker serve the weights from it (GGML_RING_SHARD_GGUF / _MANIFEST / _BLOCKS: the same thing as --shard-gguf,
// --shard-manifest, --shard-blocks). A worker whose file is missing or differs in size from the master's gets a
// warning and nothing else: the master pushes its weights over the wire as before.
//
// devs/ts are the final -dev and -ts values; the block range is derived from them (see workerBlockRanges). devNode is
// the worker node of each RING index (a worker with -d CUDA0,CPU has two).
func localWeightsEnv(opts DeployOptions, masterNode string, ringNodes, devNode []string, devs, ts string, layers, extra int, workerEnv map[string]string) []string {
	var warns []string
	masterSize, err := fileSizeOn(masterNode, opts.ModelPath)
	if err != nil || masterSize <= 0 {
		return []string{fmt.Sprintf("--local-weights: cannot read the size of %s on %s (%v); no worker is told to use a local copy", opts.ModelPath, masterNode, err)}
	}
	ranges := workerBlockRanges(devs, ts, devNode, layers, extra)
	for _, node := range ringNodes {
		path := opts.ModelPath
		if p := opts.WorkerModels[node]; p != "" {
			path = p
		}
		if !safeEnvText(path) {
			warns = append(warns, fmt.Sprintf("--local-weights: %s: path %q has characters outside [A-Za-z0-9_.:,/-]; not used", node, path))
			continue
		}
		size, err := fileSizeOn(node, path)
		if err != nil {
			warns = append(warns, fmt.Sprintf("--local-weights: %s: %s not found on the worker (%v); the master will push the weights", node, path, err))
			continue
		}
		if size != masterSize {
			warns = append(warns, fmt.Sprintf("--local-weights: %s: %s is %d bytes, the master's file is %d; not used (a different file would only fail the hash check)", node, path, size, masterSize))
			continue
		}
		workerEnv[node] = joinEnv(workerEnv[node], "GGML_RING_SHARD_GGUF="+path)

		mpath := opts.ShardsManifest
		if mpath == "" {
			mpath = filepath.Join(filepath.Dir(path), "fleet-shards.json")
			// default manifest sits next to the model: only on the master it is known to exist for sure; the worker is asked below
			if _, merr := fileSizeOn(masterNode, filepath.Join(filepath.Dir(opts.ModelPath), "fleet-shards.json")); merr != nil {
				mpath = ""
			}
		}
		if mpath != "" {
			if !safeEnvText(mpath) {
				warns = append(warns, fmt.Sprintf("--local-weights: %s: manifest path %q not passed (unsupported characters)", node, mpath))
			} else if _, merr := fileSizeOn(node, mpath); merr != nil {
				warns = append(warns, fmt.Sprintf("--local-weights: %s: manifest %s not found on the worker; weights are matched by content hash only, without the SHA-256 check", node, mpath))
			} else {
				workerEnv[node] = joinEnv(workerEnv[node], "GGML_RING_SHARD_MANIFEST="+mpath)
			}
		}
		if r, ok := ranges[node]; ok {
			workerEnv[node] = joinEnv(workerEnv[node], fmt.Sprintf("GGML_RING_SHARD_BLOCKS=%d-%d", r[0], r[1]))
		}
	}
	return warns
}

// workerBlockRanges derives the blocks each worker node runs from -dev/-ts, which split the layers in order in
// proportion to ts (llama.cpp spreads n_layer + 1 units: the blocks and the output layer). The range is widened by one
// block on both sides because a range that is too wide costs nothing (a block the worker never asks for is never read;
// one outside the range is simply pushed by the master) while one too narrow loses the saving. A worker at the end of
// the split also gets the blocks beyond block_count (extra: MTP/nextn heads). devNode is the worker node of each RING
// index (RINGk runs on devNode[k]; a node with several devices appears several times). A device with -ts 0 (a worker's
// CPU holding only overridden expert tensors) adds no blocks: they lie in the range of that worker's GPU. Nodes are
// left out (no range: all blocks are indexed) when the devices cannot be matched to them.
func workerBlockRanges(devs, ts string, devNode []string, layers, extra int) map[string][2]int {
	out := map[string][2]int{}
	dl := strings.Split(devs, ",")
	tl := strings.Split(ts, ",")
	if layers <= 0 || len(dl) != len(tl) {
		return out
	}
	nRing := 0
	for _, d := range dl {
		if strings.HasPrefix(d, "RING") {
			nRing++
		}
	}
	if nRing != len(devNode) {
		return out
	}
	vals := make([]float64, len(tl))
	sum := 0.0
	for i, s := range tl {
		v, err := strconv.ParseFloat(strings.TrimSpace(s), 64)
		if err != nil || v < 0 {
			return out
		}
		vals[i] = v
		sum += v
	}
	if sum <= 0 {
		return out
	}
	units := float64(layers + 1)
	cum := 0.0
	for i, d := range dl {
		lo := cum / sum
		cum += vals[i]
		hi := cum / sum
		if !strings.HasPrefix(d, "RING") {
			continue
		}
		k, err := strconv.Atoi(strings.TrimPrefix(d, "RING"))
		if err != nil || k < 0 || k >= len(devNode) {
			return map[string][2]int{}
		}
		if vals[i] == 0 {
			continue
		}
		first := int(math.Floor(lo*units)) - 1
		last := int(math.Ceil(hi * units)) // exclusive end, plus one block of margin = inclusive last
		if first < 0 {
			first = 0
		}
		if i == len(dl)-1 || last >= layers {
			last = layers - 1 + extra
		}
		if last > layers-1+extra {
			last = layers - 1 + extra
		}
		node := devNode[k]
		if old, ok := out[node]; ok { // a node with several devices: the union
			if old[0] < first {
				first = old[0]
			}
			if old[1] > last {
				last = old[1]
			}
		}
		out[node] = [2]int{first, last}
	}
	return out
}
