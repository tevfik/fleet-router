package server

import (
	"net/http"
	"strings"
	"sync"
	"time"
)

// LogEntry is one request that went through the router (newest last in the ring).
type LogEntry struct {
	Time     time.Time `json:"time"`
	Path     string    `json:"path"`
	Client   string    `json:"client,omitempty"`   // who asked (X-Fleet-Client, else the User-Agent product)
	Via      string    `json:"via,omitempty"`      // "ollama" for /api/* calls
	Model    string    `json:"model"`              // what the client asked for (role, endpoint or id)
	Role     string    `json:"role,omitempty"`     // set when Model is a role
	Endpoint string    `json:"endpoint,omitempty"` // who answered
	Status   int       `json:"status"`
	Attempts int       `json:"attempts"` // >1 means failover happened
	QueueMS  int64     `json:"queue_ms"` // time waiting for a slot
	TTFTMS   int64     `json:"ttft_ms,omitempty"`
	TPS      float64   `json:"tps,omitempty"`
	In       int       `json:"in,omitempty"`
	Out      int       `json:"out,omitempty"`
	DurMS    int64     `json:"dur_ms"`
	Remote   bool      `json:"remote,omitempty"` // caller was not on this machine
	Err      string    `json:"err,omitempty"`
	ref      string    // "node/model" of the answering endpoint when the config does not list it (tunnel endpoints)
}

const logCap = 300

type reqLog struct {
	mu   sync.Mutex
	ring []LogEntry
}

func (l *reqLog) add(e LogEntry) {
	l.mu.Lock()
	l.ring = append(l.ring, e)
	if len(l.ring) > logCap {
		l.ring = l.ring[len(l.ring)-logCap:]
	}
	l.mu.Unlock()
}

// last returns up to n newest entries, oldest first.
func (l *reqLog) last(n int) []LogEntry {
	l.mu.Lock()
	defer l.mu.Unlock()
	if n <= 0 || n > len(l.ring) {
		n = len(l.ring)
	}
	return append([]LogEntry(nil), l.ring[len(l.ring)-n:]...)
}

// statusRec records the status code, the first bytes of an error body and passes everything else through.
type statusRec struct {
	http.ResponseWriter
	code int
	errB []byte
}

func (r *statusRec) WriteHeader(c int) { r.code = c; r.ResponseWriter.WriteHeader(c) }
func (r *statusRec) Write(p []byte) (int, error) {
	if r.code == 0 {
		r.code = 200
	}
	if r.code >= 400 && len(r.errB) < 300 {
		r.errB = append(r.errB, p[:min(len(p), 300-len(r.errB))]...)
	}
	return r.ResponseWriter.Write(p)
}
func (r *statusRec) Flush() {
	if f, ok := r.ResponseWriter.(http.Flusher); ok {
		f.Flush()
	}
}

// tunnelWriter wraps the ResponseWriter a hub request is forwarded to a node into, so that the answer that passes
// through is measured like a local one (tokens, time to first token, tokens/s). The observer is made on the first
// write, when the status and the Content-Type are known: only a 200 answer to a /completions request is measured, a
// text/event-stream one as a stream.
type tunnelWriter struct {
	http.ResponseWriter
	t0          time.Time
	completions bool
	status      int
	decided     bool
	obs         *observer
}

func newTunnelWriter(t0 time.Time, urlPath string, w http.ResponseWriter) *tunnelWriter {
	return &tunnelWriter{ResponseWriter: w, t0: t0, completions: strings.HasSuffix(urlPath, "/completions")}
}

func (t *tunnelWriter) WriteHeader(code int) {
	if t.status == 0 {
		t.status = code
	}
	t.ResponseWriter.WriteHeader(code)
}

func (t *tunnelWriter) Write(p []byte) (int, error) {
	if !t.decided {
		t.decided = true
		if st := t.status; (st == 0 || st == http.StatusOK) && t.completions {
			t.obs = newObserver(t.t0, strings.Contains(t.Header().Get("Content-Type"), "event-stream"))
		}
	}
	n, err := t.ResponseWriter.Write(p)
	if err == nil && t.obs != nil && n > 0 {
		t.obs.feed(p[:n]) // after the write: measuring never delays a token
	}
	return n, err
}

func (t *tunnelWriter) Flush() {
	if f, ok := t.ResponseWriter.(http.Flusher); ok {
		f.Flush()
	}
}

// Unwrap lets http.NewResponseController reach the underlying writer (write deadlines).
func (t *tunnelWriter) Unwrap() http.ResponseWriter { return t.ResponseWriter }
