package hostinfo

import "sync"

// Accelerator is a device with a memory pool of its own that fleet does not read by itself (an NPU board, for
// example): a stage module detects it and registers a probe (pkg/stagemod.RegisterAccelerator). The benchmark then
// counts its pool as fast memory and its bandwidth as the read speed of a model that fits.
type Accelerator struct {
	Name      string
	Kind      string // "npu": its pool cannot spill into system RAM (the only kind the scoring treats specially)
	PoolBytes int64
	ClockMHz  float64
	BW        float64 // GB/s weight read bandwidth
	Source    string  // where BW comes from: "probe", "config" or "estimate"
	Notes     []string
}

// BenchGPU is the entry the benchmark keeps for the accelerator: the pool as VRAM, not unified.
func (a Accelerator) BenchGPU() BenchGPU {
	return BenchGPU{Name: a.Name, VRAM: a.PoolBytes, Bandwidth: a.BW, Source: a.Source, Kind: a.Kind}
}

var (
	accelMu     sync.RWMutex
	accelProbes []func() (Accelerator, bool)
)

// RegisterAccelerator adds a probe; it is asked whenever the machine is benchmarked or fingerprinted.
func RegisterAccelerator(probe func() (Accelerator, bool)) {
	accelMu.Lock()
	defer accelMu.Unlock()
	accelProbes = append(accelProbes, probe)
}

// accelerators are what the registered probes find on this machine.
func accelerators() []Accelerator {
	accelMu.RLock()
	probes := append([]func() (Accelerator, bool){}, accelProbes...)
	accelMu.RUnlock()
	var out []Accelerator
	for _, p := range probes {
		if a, ok := p(); ok {
			out = append(out, a)
		}
	}
	return out
}
