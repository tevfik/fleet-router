// Package rtt measures the round trip between this machine and a worker and keeps the result for the planner.
//
// The probe is one TCP handshake to a port the worker already listens on (SSH, 22): connect() returns after one
// round trip, so its duration is the network round trip. It never spawns a process: timing `ssh host true`
// measured process start, key exchange and auth, and inflated the figure about 3x (measured on the WAN). The name
// is resolved before the clock starts so DNS does not count either.
package rtt

import (
	"context"
	"encoding/json"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"sort"
	"sync"
	"time"
)

// Probe makes one round-trip measurement to addr ("host:port").
type Probe func(ctx context.Context, addr string) (time.Duration, error)

// TCPConnect is the real probe: the time of one TCP handshake, name resolution excluded.
func TCPConnect(ctx context.Context, addr string) (time.Duration, error) {
	host, port, err := net.SplitHostPort(addr)
	if err != nil {
		return 0, err
	}
	ip := host
	if net.ParseIP(host) == nil {
		ips, err := net.DefaultResolver.LookupHost(ctx, host)
		if err != nil || len(ips) == 0 {
			return 0, fmt.Errorf("resolve %s: %v", host, err)
		}
		ip = ips[0]
	}
	var d net.Dialer
	t0 := time.Now()
	c, err := d.DialContext(ctx, "tcp", net.JoinHostPort(ip, port))
	if err != nil {
		return 0, err
	}
	rt := time.Since(t0)
	_ = c.Close()
	return rt, nil
}

// Sample is the result of measuring one worker.
type Sample struct {
	MS      float64   `json:"ms"` // median of the successful probes
	N       int       `json:"n"`  // how many probes succeeded
	MinMS   float64   `json:"min_ms,omitempty"`
	MaxMS   float64   `json:"max_ms,omitempty"`
	Addr    string    `json:"addr,omitempty"`
	At      time.Time `json:"at"`
	Cached  bool      `json:"-"` // came from the cache
	Skipped string    `json:"-"`
}

// Measure probes addr n times (default 5; a failed probe is dropped) and returns the median. It fails when no
// probe succeeded.
func Measure(ctx context.Context, p Probe, addr string, n int) (Sample, error) {
	if n <= 0 {
		n = 5
	}
	var v []float64
	var lastErr error
	for i := 0; i < n; i++ {
		if ctx.Err() != nil {
			break
		}
		d, err := p(ctx, addr)
		if err != nil {
			lastErr = err
			continue
		}
		v = append(v, float64(d)/float64(time.Millisecond))
	}
	if len(v) == 0 {
		if lastErr == nil {
			lastErr = fmt.Errorf("no sample")
		}
		return Sample{}, lastErr
	}
	sort.Float64s(v)
	m := v[len(v)/2]
	if len(v)%2 == 0 {
		m = (v[len(v)/2-1] + v[len(v)/2]) / 2
	}
	return Sample{MS: m, N: len(v), MinMS: v[0], MaxMS: v[len(v)-1], Addr: addr}, nil
}

// Cache keeps measured round trips between runs, keyed "from>to", in a JSON file.
type Cache struct {
	Path string
	mu   sync.Mutex
}

// Load reads the file (a missing or broken file is an empty cache).
func (c *Cache) Load() map[string]Sample {
	out := map[string]Sample{}
	b, err := os.ReadFile(c.Path)
	if err == nil {
		_ = json.Unmarshal(b, &out)
	}
	return out
}

// Store writes the file atomically.
func (c *Cache) Store(m map[string]Sample) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	if err := os.MkdirAll(filepath.Dir(c.Path), 0o755); err != nil {
		return err
	}
	b, _ := json.MarshalIndent(m, "", "  ")
	tmp := c.Path + ".tmp"
	if err := os.WriteFile(tmp, b, 0o644); err != nil {
		return err
	}
	return os.Rename(tmp, c.Path)
}

// Target is a worker to measure: the node name the planner knows it by, and where to connect.
type Target struct {
	Node string
	Addr string // "host:port"
}

// Collector measures a set of workers from this machine and caches the results.
type Collector struct {
	From    string // this machine's node name (the "a" of "a>b")
	Probe   Probe  // nil: TCPConnect
	Cache   *Cache // nil: no caching
	Now     func() time.Time
	MaxAge  time.Duration   // a cached value older than this is measured again (0: DefaultMaxAge)
	Samples int             // probes per worker (0: 5)
	Timeout time.Duration   // per worker (0: 5 s)
	Refresh bool            // ignore the cache
	Skip    map[string]bool // pair keys ("a>b" or "b>a") that are given by hand: not measured
}

// DefaultMaxAge is how long a cached round trip is trusted.
const DefaultMaxAge = 15 * time.Minute

// Key is the pair key the planner uses (Options.RTT).
func Key(from, to string) string { return from + ">" + to }

// Collect returns the round trips by pair key, and the per-target detail (Cached says whether it was reused; an
// unreachable worker has Skipped set and is left out of the map, so the planner falls back to its ping).
func (c *Collector) Collect(ctx context.Context, targets []Target) (map[string]float64, map[string]Sample) {
	probe := c.Probe
	if probe == nil {
		probe = TCPConnect
	}
	now := c.Now
	if now == nil {
		now = time.Now
	}
	maxAge := c.MaxAge
	if maxAge <= 0 {
		maxAge = DefaultMaxAge
	}
	tmo := c.Timeout
	if tmo <= 0 {
		tmo = 5 * time.Second
	}
	var cached map[string]Sample
	if c.Cache != nil {
		cached = c.Cache.Load()
	} else {
		cached = map[string]Sample{}
	}
	out := map[string]float64{}
	detail := map[string]Sample{}
	dirty := false
	for _, t := range targets {
		k := Key(c.From, t.Node)
		if c.Skip[k] || c.Skip[Key(t.Node, c.From)] {
			detail[k] = Sample{Skipped: "given with --rtt"}
			continue
		}
		if s, ok := cached[k]; ok && !c.Refresh && now().Sub(s.At) < maxAge && s.MS > 0 {
			s.Cached = true
			out[k], detail[k] = s.MS, s
			continue
		}
		cctx, cancel := context.WithTimeout(ctx, tmo)
		s, err := Measure(cctx, probe, t.Addr, c.Samples)
		cancel()
		if err != nil {
			detail[k] = Sample{Addr: t.Addr, Skipped: err.Error()}
			continue
		}
		s.At = now()
		out[k], detail[k] = s.MS, s
		cached[k] = s
		dirty = true
	}
	if dirty && c.Cache != nil {
		_ = c.Cache.Store(cached)
	}
	return out, detail
}
