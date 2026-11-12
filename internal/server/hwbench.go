package server

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"sync"
	"time"

	"github.com/tevfik/fleet-router/internal/config"
	"github.com/tevfik/fleet-router/internal/hostinfo"
)

// Every fleet benchmarks its own machine — never another: the other machines' fleets do theirs, and their
// results arrive with their readings (GET /fleet/host). It runs by itself only when the hardware changed
// (a different CPU, RAM size or GPU than the stored result was measured on, or none stored yet), a little
// after the daemon starts; otherwise only when asked (fleet nodes bench, menu r).

type hwBenchState struct {
	mu      sync.Mutex
	res     *hostinfo.Bench
	running bool
	loaded  bool
}

func (h *hwBenchState) load() {
	if h.loaded {
		return
	}
	h.loaded = true
	if b, err := os.ReadFile(config.HWBenchPath()); err == nil {
		var r hostinfo.Bench
		if json.Unmarshal(b, &r) == nil && r.Fingerprint != "" {
			h.res = &r
		}
	}
}

// HWBench returns the stored result of this machine (nil: never measured) and whether one is running.
func (s *Server) HWBench() (*hostinfo.Bench, bool) {
	h := &s.hwb
	h.mu.Lock()
	defer h.mu.Unlock()
	h.load()
	if h.res == nil {
		return nil, h.running
	}
	c := *h.res
	return &c, h.running
}

// RunHWBench measures this machine now (in the background); an error if it already runs.
func (s *Server) RunHWBench(why string) error {
	h := &s.hwb
	h.mu.Lock()
	if h.running {
		h.mu.Unlock()
		return fmt.Errorf("the machine benchmark is already running")
	}
	h.running = true
	h.mu.Unlock()
	go func() {
		log.Printf("benchmarking this machine (%s)", why)
		r := hostinfo.RunBench(s.ctx, s.Config().GPUBandwidth)
		h.mu.Lock()
		h.res, h.running = &r, false
		h.mu.Unlock()
		if b, err := json.MarshalIndent(r, "", "  "); err == nil {
			p := config.HWBenchPath()
			_ = os.MkdirAll(filepath.Dir(p), 0o700)
			if err := os.WriteFile(p, b, 0o600); err != nil {
				log.Printf("could not save the machine benchmark: %v", err)
			}
		}
		log.Printf("machine benchmark: AI score %d · %.0f tok/s on an 8B Q4 model · up to %gB in %s (%.1fs)", r.Score, r.Speed8B, r.MaxFastB, r.FastWhere, r.Took)
	}()
	return nil
}

// hwBenchLoop benchmarks the machine at start when its hardware is not the one last measured.
func (s *Server) hwBenchLoop(ctx context.Context) {
	select {
	case <-ctx.Done():
		return
	case <-time.After(20 * time.Second): // let the tunnels and the first health round settle
	}
	fp, hw := hostinfo.Fingerprint(s.Config().GPUBandwidth)
	cur, _ := s.HWBench()
	switch {
	case cur == nil:
		_ = s.RunHWBench("first run: " + hw)
	case cur.Fingerprint != fp:
		_ = s.RunHWBench("hardware changed: " + cur.Hardware + " → " + hw)
	}
}
