package deploy

import (
	"fmt"
	"math"
	"strconv"
	"strings"
)

// --worker-cpu-experts NODE=FIRST-LAST: the MoE expert weights of blocks FIRST..LAST (inclusive) live in that worker's
// CPU RAM. The worker is started with -d CUDA0,CPU and so shows the master two devices (its GPU, then its CPU); the
// master numbers the RING devices in RING_SERVERS order, each worker's devices in its -d order. The CPU device goes
// last in -dev with -ts 0 (it gets no layers by the split) and receives only the expert tensors of those blocks through
// -ot …=RINGk[<the worker's RING_SERVERS entry>] (the engine wants the buffer type name; a bare RINGk is refused). The
// rest of those blocks (attention, norms, the shared expert) stays on the worker's GPU, so the blocks must lie in the
// range its GPU device gets by -ts.

// ParseWorkerCPUExperts reads NODE=FIRST-LAST[,NODE2=FIRST-LAST...] ("" = none).
func ParseWorkerCPUExperts(s string) (map[string][2]int, error) {
	out := map[string][2]int{}
	for _, item := range strings.Split(s, ",") {
		item = strings.TrimSpace(item)
		if item == "" {
			continue
		}
		node, rng, ok := strings.Cut(item, "=")
		node = strings.TrimSpace(node)
		a, b, ok2 := strings.Cut(strings.TrimSpace(rng), "-")
		first, err1 := strconv.Atoi(strings.TrimSpace(a))
		last, err2 := strconv.Atoi(strings.TrimSpace(b))
		if !ok || !ok2 || node == "" || err1 != nil || err2 != nil {
			return nil, fmt.Errorf("--worker-cpu-experts wants NODE=FIRST-LAST[,NODE2=FIRST-LAST] (blocks inclusive), got %q", item)
		}
		if first < 0 || last < first {
			return nil, fmt.Errorf("--worker-cpu-experts %s: block range %d-%d is empty or negative", item, first, last)
		}
		if _, dup := out[node]; dup {
			return nil, fmt.Errorf("--worker-cpu-experts: %s is given twice", node)
		}
		out[node] = [2]int{first, last}
	}
	return out, nil
}

// cpuExpertPlan is what --worker-cpu-experts makes of the -dev/-ts the planner built.
type cpuExpertPlan struct {
	devList []string
	ts      string
	ot      string            // " -ot '…'" per worker (in RING_SERVERS order), "" without the flag
	devNode []string          // the worker node of each RING index (RINGk -> devNode[k])
	devices map[string]string // node -> its -d list, only for workers that differ from CUDA0
}

// planCPUExperts renumbers the RING devices for the workers that also offer their CPU, appends those CPU devices with
// -ts 0 and builds the expert overrides. devList is the -dev list with one RINGk per worker (k = its index in
// ringNodes / ringServers), ts the -ts value for it (an explicit --ts lists only these devices; trailing 0s for the
// CPU devices are accepted too). Without spec nothing changes.
func planCPUExperts(spec map[string][2]int, ringNodes, ringServers, devList []string, ts string, layers int) (cpuExpertPlan, error) {
	p := cpuExpertPlan{devList: devList, ts: ts, devNode: ringNodes}
	if len(spec) == 0 {
		return p, nil
	}
	pos := map[string]int{}
	for i, n := range ringNodes {
		pos[n] = i
	}
	for n := range spec {
		if _, ok := pos[n]; !ok {
			return p, fmt.Errorf("--worker-cpu-experts %s: not a worker of this ring (workers: %s)", n, strings.Join(ringNodes, ","))
		}
	}
	// every worker must be exactly one RINGk of devList, k its ring index
	seen := map[int]bool{}
	for _, d := range devList {
		if !strings.HasPrefix(d, "RING") {
			continue
		}
		k, err := strconv.Atoi(strings.TrimPrefix(d, "RING"))
		if err != nil || k < 0 || k >= len(ringNodes) || seen[k] {
			return p, fmt.Errorf("--worker-cpu-experts needs every worker as one RING device in -dev; -dev %s does not match the workers %s", strings.Join(devList, ","), strings.Join(ringNodes, ","))
		}
		seen[k] = true
	}
	if len(seen) != len(ringNodes) {
		return p, fmt.Errorf("--worker-cpu-experts needs every worker as one RING device in -dev; -dev %s does not match the workers %s", strings.Join(devList, ","), strings.Join(ringNodes, ","))
	}

	// new RING numbering: RING_SERVERS order, each worker's devices in its -d order (GPU, then CPU)
	gpuIdx := map[string]int{}
	cpuIdx := map[string]int{}
	var devNode []string
	for _, n := range ringNodes {
		gpuIdx[n] = len(devNode)
		devNode = append(devNode, n)
		if _, ok := spec[n]; ok {
			cpuIdx[n] = len(devNode)
			devNode = append(devNode, n)
		}
	}

	tsl := strings.Split(ts, ",")
	if len(tsl) == len(devList)+len(spec) {
		zeros := true
		for _, v := range tsl[len(devList):] {
			if f, err := strconv.ParseFloat(strings.TrimSpace(v), 64); err != nil || f != 0 {
				zeros = false
			}
		}
		if zeros {
			tsl = tsl[:len(devList)]
		}
	}
	if len(tsl) != len(devList) {
		return p, fmt.Errorf("--ts %s has %d values but -dev has %d GPU devices (%s); with --worker-cpu-experts list only the GPU devices, fleet appends a 0 for each worker CPU", ts, len(tsl), len(devList), strings.Join(devList, ","))
	}
	vals := make([]float64, len(tsl))
	sum := 0.0
	for i, s := range tsl {
		v, err := strconv.ParseFloat(strings.TrimSpace(s), 64)
		if err != nil || v < 0 {
			return p, fmt.Errorf("--ts %s: %q is not a share >= 0", ts, s)
		}
		vals[i] = v
		sum += v
	}
	if sum <= 0 {
		return p, fmt.Errorf("--ts %s gives no device any layer", ts)
	}

	var newDev, newTS []string
	gpuRange := map[string][2]int{} // node -> blocks its GPU device gets by -ts (last < first: none)
	cum := 0.0
	for i, d := range devList {
		lo := int(math.Round(cum / sum * float64(layers)))
		cum += vals[i]
		hi := int(math.Round(cum/sum*float64(layers))) - 1
		if strings.HasPrefix(d, "RING") {
			k, _ := strconv.Atoi(strings.TrimPrefix(d, "RING"))
			n := ringNodes[k]
			d = fmt.Sprintf("RING%d", gpuIdx[n])
			gpuRange[n] = [2]int{lo, hi}
		}
		newDev = append(newDev, d)
		newTS = append(newTS, strings.TrimSpace(tsl[i]))
	}

	p.devices = map[string]string{}
	var ot strings.Builder
	for _, n := range ringNodes {
		r, ok := spec[n]
		if !ok {
			continue
		}
		g := gpuRange[n]
		if r[1] >= layers {
			return p, fmt.Errorf("--worker-cpu-experts %s=%d-%d: the model has %d blocks (0-%d)", n, r[0], r[1], layers, layers-1)
		}
		if g[1] < g[0] {
			return p, fmt.Errorf("--worker-cpu-experts %s=%d-%d: %s's GPU gets no blocks by -dev %s -ts %s; the blocks whose experts sit in its RAM must run on its GPU", n, r[0], r[1], n, strings.Join(newDev, ","), strings.Join(newTS, ","))
		}
		if r[0] < g[0] || r[1] > g[1] {
			return p, fmt.Errorf("--worker-cpu-experts %s=%d-%d: outside the blocks %s's GPU runs, %d-%d (-dev %s -ts %s); the blocks whose experts sit in its RAM must run on its GPU", n, r[0], r[1], n, g[0], g[1], strings.Join(newDev, ","), strings.Join(newTS, ","))
		}
		newDev = append(newDev, fmt.Sprintf("RING%d", cpuIdx[n]))
		newTS = append(newTS, "0")
		blocks := make([]string, 0, r[1]-r[0]+1)
		for b := r[0]; b <= r[1]; b++ {
			blocks = append(blocks, strconv.Itoa(b))
		}
		fmt.Fprintf(&ot, ` -ot 'blk\.(%s)\.ffn_(up|down|gate)_exps\.weight=RING%d[%s]'`, strings.Join(blocks, "|"), cpuIdx[n], ringServers[pos[n]])
		p.devices[n] = cpuExpertDevices
	}
	p.devList, p.ts, p.ot, p.devNode = newDev, strings.Join(newTS, ","), ot.String(), devNode
	return p, nil
}
