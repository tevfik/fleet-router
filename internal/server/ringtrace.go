package server

import (
	"context"
	"fmt"
	"net"
	"net/url"
	"sort"
	"strconv"
	"strings"

	"github.com/tevfik/fleet-router/internal/config"
	"github.com/tevfik/fleet-router/internal/ringengine"
)

// RingTrace is what llama-ring's live statistics say about the last tokens (GET /ring/trace on the master's
// machine, RING_STATS_PORT): the time of a token, how much of it is the master's own, and for every worker the
// time it computed and the time the bytes spent on the wire.
type RingTrace struct {
	StepsTotal  int        `json:"steps_total"`   // every step since the engine started: stops growing when it freezes
	Steps       int        `json:"steps"`         // decode steps in the window
	TokenMeanMS float64    `json:"token_mean_ms"` // mean time of a token
	TokenP50MS  float64    `json:"token_p50_ms"`  // the median: one slow step does not move it
	TokenP95MS  float64    `json:"token_p95_ms,omitempty"`
	RingMS      float64    `json:"ring_ms"`                  // the part that went through the workers
	LocalMS     float64    `json:"local_ms"`                 // the master's own blocks, output, sampling
	PromptMS    float64    `json:"prompt_mean_ms,omitempty"` // a prompt chunk (batch steps), for the first-token time
	Nodes       []RingNode `json:"nodes,omitempty"`
	// Failed: the workers whose connection the engine lost (GET /ring/info, failed_workers). llama-server's own /health
	// stays "ok" then and the requests fail at once, so no step ever stalls: this is the signal.
	Failed []string `json:"failed_workers,omitempty"`
	// Report: why each failed connection failed (GET /ring/info, failure_report). Empty on an engine that predates it.
	Report []FailureEntry `json:"failure_report,omitempty"`
	// Features: the HELLO feature byte of every worker that connected ("host:port" -> bits, see Feat*). nil means the
	// engine does not say (older build): unknown, not "none".
	Features map[string]int `json:"worker_features,omitempty"`
	// Reconnects: per-worker session resume telemetry from GET /ring/info ("host:port" -> ReconnectStat, patch 6+).
	Reconnects map[string]ReconnectStat `json:"reconnects,omitempty"`
}

// ReconnectStat is one worker's session-resume counters from GET /ring/info reconnects.
type ReconnectStat struct {
	Resumed        uint64  `json:"resumed"`
	Replayed       uint64  `json:"replayed"`
	ReplyResent    uint64  `json:"reply_resent"`
	LastDowntimeMS float64 `json:"last_downtime_ms"`
}

// The engine's HELLO feature bits (fleet-engine RING_FEAT_*), re-exported from ringengine.
const (
	FeatWireF16   = ringengine.FeatWireF16   // F16 / Q8_0 activations on the wire
	FeatAsyncHash = ringengine.FeatAsyncHash // SET_TENSOR_HASH_ASYNC
	FeatAsyncCopy = ringengine.FeatAsyncCopy // COPY_TENSOR_ASYNC
	FeatStateSnap = ringengine.FeatStateSnap // recurrent-state checkpoints stay on the worker
	FeatPeerFwd   = ringengine.FeatPeerFwd   // worker-to-worker forwarding (chain)
	FeatNoStore   = ringengine.FeatNoStore   // negative bit: worker keeps no weights by hash
	FeatSession   = ringengine.FeatSession   // SESSION_ATTACH / SESSION_CLOSE: lost connection resumes with replay
)

// FailureEntry is one line of /ring/info failure_report. Role is "culprit" (this worker failed), "victim" (its
// connection dropped because of a neighbour in a chain) or "link" (the link between two workers broke: both are
// link). Cause names the worker that caused a victim's loss, when the engine knows.
type FailureEntry struct {
	Worker string `json:"worker"`
	Role   string `json:"role"`
	Reason string `json:"reason,omitempty"`
	Cause  string `json:"cause,omitempty"`
}

// Blame says whether the report puts this worker's failure on it (a culprit or a broken link), as opposed to a victim.
func (f FailureEntry) Blame() bool { return f.Role != "victim" }

// Down says whether the engine's report means the endpoint is unusable: the dispatcher stays failed until restart, so
// any entry (failed_workers, culprit, link, even victims only) means a broken ring. Victims alone are never blamed.
func (t *RingTrace) Down() bool {
	if t == nil {
		return false
	}
	if len(t.Failed) > 0 {
		return true
	}
	return len(t.Report) > 0
}

// FailureWhy words the failure for st.Err and the notes: the workers to blame with the engine's reason, then the
// victims as a side remark. "" when Down is false.
func (t *RingTrace) FailureWhy() string {
	if !t.Down() {
		return ""
	}
	byWorker := map[string]FailureEntry{}
	var victims []string
	for _, f := range t.Report {
		if f.Role == "victim" {
			victims = append(victims, f.Worker)
		} else {
			byWorker[f.Worker] = f
		}
	}
	if len(t.Failed) == 0 && len(victims) == len(t.Report) {
		return "engine ring broken: victim only, culprit unknown (dropped: " + strings.Join(victims, ", ") + ")"
	}
	var parts []string
	seen := map[string]bool{}
	add := func(w string) {
		if seen[w] {
			return
		}
		seen[w] = true
		if f, ok := byWorker[w]; ok {
			role := f.Role
			if role == "" {
				role = "culprit"
			}
			x := w + " (" + role
			if f.Reason != "" {
				x += ": " + f.Reason
			}
			if f.Cause != "" {
				x += "; cause " + f.Cause
			}
			parts = append(parts, x+")")
		} else {
			parts = append(parts, w)
		}
	}
	for _, w := range t.Failed {
		add(w)
	}
	for _, f := range t.Report {
		if f.Blame() {
			add(f.Worker)
		}
	}
	why := "engine lost its worker(s): " + strings.Join(parts, ", ")
	if len(victims) > 0 {
		why += " [dropped as a consequence, not at fault: " + strings.Join(victims, ", ") + "]"
	}
	return why
}

// ReconnectSummary formats a concise summary of workers that have resumed sessions via SESSION_ATTACH ("" when none).
// If g is non-nil, worker addresses are mapped to node names when possible.
func (t *RingTrace) ReconnectSummary(g *config.Group) string {
	if t == nil || len(t.Reconnects) == 0 {
		return ""
	}
	return FormatReconnects(t.Reconnects, g)
}

// FormatReconnects formats a map of worker reconnect counters into a human-readable summary ("" when no worker has Resumed > 0).
func FormatReconnects(reconnects map[string]ReconnectStat, g *config.Group) string {
	if len(reconnects) == 0 {
		return ""
	}
	addrs := make([]string, 0, len(reconnects))
	for a, st := range reconnects {
		if st.Resumed > 0 {
			addrs = append(addrs, a)
		}
	}
	if len(addrs) == 0 {
		return ""
	}
	sort.Strings(addrs)
	var parts []string
	for _, a := range addrs {
		st := reconnects[a]
		label := a
		if g != nil {
			for node, ad := range g.Addrs {
				if ad == a {
					label = node
					break
				}
			}
			if label == a && len(g.Workers) == 1 && len(reconnects) == 1 {
				label = g.Workers[0]
			}
		}
		detail := fmt.Sprintf("resumed %d×", st.Resumed)
		if st.Replayed > 0 || st.ReplyResent > 0 {
			detail += fmt.Sprintf(", replayed %d, resent %d", st.Replayed, st.ReplyResent)
		}
		if st.LastDowntimeMS > 0 {
			detail += fmt.Sprintf(", last downtime %.0f ms", st.LastDowntimeMS)
		}
		parts = append(parts, fmt.Sprintf("%s (%s)", label, detail))
	}
	return "session reconnects: " + strings.Join(parts, "; ")
}

// RingNode is one worker as the engine measured it, per token.
type RingNode struct {
	Addr    string  `json:"addr"`           // as in RING_SERVERS
	Node    string  `json:"node,omitempty"` // the machine, when the group says which address is which
	SendMS  float64 `json:"send_ms"`
	GetMS   float64 `json:"get_ms"`  // waiting for the worker's answer (its compute + the wire)
	BusyMS  float64 `json:"busy_ms"` // the worker's own compute, as it reported it
	WireMS  float64 `json:"wire_ms"` // get − busy: what the bytes and the queue cost
	Samples int     `json:"samples"` // fewer than 3: do not trust busy and wire
}

// Trusted: busy and wire rest on enough samples.
func (n RingNode) Trusted() bool { return n.Samples >= 3 }

// The engine's net_ms is get minus the worker's *GET* time, which leaves the worker's compute in it (on a
// loopback link net_ms came out equal to the whole wait). The wire is what is left after the compute.
func wireOf(get, busy float64) float64 {
	if w := get - busy; w > 0 {
		return w
	}
	return 0
}

type ringTraceReply struct {
	StepsTotal int `json:"steps_total"`
	Decode     struct {
		Steps     int     `json:"steps"`
		MeanMS    float64 `json:"mean_ms"`
		P50MS     float64 `json:"p50_ms"`
		P95MS     float64 `json:"p95_ms"`
		RingMS    float64 `json:"ring_ms"`
		LocalMS   float64 `json:"local_ms"`
		Endpoints map[string]struct {
			SendMS  float64 `json:"send_ms"`
			GetMS   float64 `json:"get_ms"`
			BusyMS  float64 `json:"worker_busy_ms"`
			Samples int     `json:"worker_samples"`
		} `json:"endpoints"`
	} `json:"decode"`
	Batch struct {
		MeanMS float64 `json:"mean_ms"`
	} `json:"batch"`
}

// ringTrace reads the engine's profile. The statistics listen on 127.0.0.1 of the master's machine, so this works
// where the endpoint is reached by a plain address on that machine (not through an SSH tunnel to another one).
func (s *Server) ringTrace(ctx context.Context, e *config.Endpoint, root string, g *config.Group) *RingTrace {
	host, port := "", g.StatsPort
	if sr := e.StatsRemote(); sr != "" { // a master on another machine: through the tunnel to its statistics port
		p := s.tunnels.Port(e.SSH, sr)
		if p == 0 {
			return nil
		}
		host, port = "127.0.0.1", p
	} else {
		u, err := url.Parse(root)
		if err != nil || u.Hostname() == "" {
			return nil
		}
		host = u.Hostname()
	}
	statsURL := "http://" + net.JoinHostPort(host, strconv.Itoa(port))
	var r ringTraceReply
	if err := s.engineJSON(ctx, e, statsURL+"/ring/trace?last=256", &r); err != nil {
		return nil
	}
	var info struct {
		Failed     []string                 `json:"failed_workers"`
		Report     []FailureEntry           `json:"failure_report"`
		Features   map[string]int           `json:"worker_features"`
		Reconnects map[string]ReconnectStat `json:"reconnects"`
	}
	_ = s.engineJSON(ctx, e, statsURL+"/ring/info", &info) // an engine without the fields (older build) just has none: unknown, never an error
	if r.Decode.Steps == 0 {
		return &RingTrace{StepsTotal: r.StepsTotal, Failed: info.Failed, Report: info.Report, Features: info.Features, Reconnects: info.Reconnects} // the engine answers, nothing was generated yet
	}
	tr := &RingTrace{StepsTotal: r.StepsTotal, Failed: info.Failed, Report: info.Report, Features: info.Features, Reconnects: info.Reconnects, Steps: r.Decode.Steps, TokenMeanMS: r.Decode.MeanMS, TokenP50MS: r.Decode.P50MS, TokenP95MS: r.Decode.P95MS,
		RingMS: r.Decode.RingMS, LocalMS: r.Decode.LocalMS, PromptMS: r.Batch.MeanMS}
	addrs := make([]string, 0, len(r.Decode.Endpoints))
	for a := range r.Decode.Endpoints {
		addrs = append(addrs, a)
	}
	sort.Strings(addrs)
	for _, a := range addrs {
		x := r.Decode.Endpoints[a]
		n := RingNode{Addr: a, SendMS: x.SendMS, GetMS: x.GetMS, BusyMS: x.BusyMS, Samples: x.Samples}
		n.WireMS = wireOf(x.GetMS, x.BusyMS)
		for node, ad := range g.Addrs {
			if ad == a {
				n.Node = node
			}
		}
		if n.Node == "" && len(g.Workers) == 1 && len(addrs) == 1 {
			n.Node = g.Workers[0]
		}
		tr.Nodes = append(tr.Nodes, n)
	}
	return tr
}
