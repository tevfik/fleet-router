package server

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"
)

// Live is the snapshot of speed figures collected from real traffic (no benchmark needed).
type Live struct {
	TPS        float64 // decode speed, tokens/s
	TTFTMS     float64 // time to first token
	PrefillTPS float64 // prompt tokens processed per second (only from prompts ≥ prefillMin tokens)
	LastPrompt int     // prompt tokens of the latest request
	MaxPrompt  int     // largest prompt seen
	Samples    int     // decode samples currently in the window
	// lifetime totals (kept across restarts, exported on /metrics)
	Requests, Errors    int
	TokensIn, TokensOut int
	At                  time.Time
}

const (
	liveVersion = 2   // 2: decode/prefill samples come only from streams or the server's own timings
	prefillMin  = 300 // shorter prompts say more about overhead than about prefill speed
	window      = 20  // samples the speed estimate is built from
	maxAge      = 7 * 24 * time.Hour
	shiftRun    = 3    // this many consecutive samples far off the estimate = the speed really changed
	shiftBand   = 1.35 // "far off" = outside estimate/1.35 … estimate*1.35
)

// sample is one measured stream: tokens produced (or prompt tokens) over the seconds it took.
type sample struct {
	T   time.Time `json:"t"`
	Tok float64   `json:"n"`
	Sec float64   `json:"s"`
}

func (a sample) rate() float64 { return a.Tok / a.Sec }

// liveData is everything remembered about one endpoint; it is what gets written to disk.
type liveData struct {
	Model      string    `json:"model"` // stats are dropped when the endpoint is re-pointed at another model
	Decode     []sample  `json:"decode,omitempty"`
	Prefill    []sample  `json:"prefill,omitempty"`
	TTFT       []sample  `json:"ttft,omitempty"` // Tok holds milliseconds, Sec is unused
	LastPrompt int       `json:"last_prompt,omitempty"`
	MaxPrompt  int       `json:"max_prompt,omitempty"`
	Requests   int       `json:"requests,omitempty"`
	Errors     int       `json:"errors,omitempty"`
	TokensIn   int       `json:"tokens_in,omitempty"`
	TokensOut  int       `json:"tokens_out,omitempty"`
	lastOK     time.Time // last real request it answered (not persisted): the health loop need not ask it meanwhile
	At         time.Time `json:"at"`
}

type liveStore struct {
	mu    sync.Mutex
	m     map[string]*liveData
	usage map[usageKey]*usageRec
	dirty bool
}

func median(v []float64) float64 {
	if len(v) == 0 {
		return 0
	}
	c := append([]float64(nil), v...)
	sort.Float64s(c)
	if len(c)%2 == 1 {
		return c[len(c)/2]
	}
	return (c[len(c)/2-1] + c[len(c)/2]) / 2
}

// estimate is the throughput of the window: total tokens ÷ total seconds (a true average, so one long stream
// counts for more than one short one), after dropping samples more than 2× away from the median (cold starts,
// stalls, tiny replies). 0 when there is nothing to go on.
func estimate(ss []sample) float64 {
	if len(ss) == 0 {
		return 0
	}
	rates := make([]float64, len(ss))
	for i, x := range ss {
		rates[i] = x.rate()
	}
	med := median(rates)
	var tok, sec float64
	for _, x := range ss {
		if r := x.rate(); r >= med/2 && r <= med*2 {
			tok += x.Tok
			sec += x.Sec
		}
	}
	if sec <= 0 {
		return med
	}
	return tok / sec
}

// push appends a sample and keeps the window. Adaptation is fast: if the newest shiftRun samples all lie on the
// same side, beyond shiftBand of what the older ones predicted, the old ones are forgotten at once (a model that
// was swapped, a GPU that is now shared, a server that finished warming up) instead of being averaged away slowly.
func push(ss []sample, x sample) []sample {
	ss = append(ss, x)
	if len(ss) > window {
		ss = ss[len(ss)-window:]
	}
	if len(ss) >= shiftRun+3 {
		old, recent := ss[:len(ss)-shiftRun], ss[len(ss)-shiftRun:]
		est := estimate(old)
		up, down := true, true
		for _, r := range recent {
			up = up && r.rate() > est*shiftBand
			down = down && r.rate() < est/shiftBand
		}
		if est > 0 && (up || down) {
			ss = append([]sample(nil), recent...)
		}
	}
	return ss
}

func (l *liveStore) entry(name string) *liveData {
	if l.m == nil {
		l.m = map[string]*liveData{}
	}
	x := l.m[name]
	if x == nil {
		x = &liveData{}
		l.m[name] = x
	}
	return x
}

// record merges one finished request. model is the endpoint's current model id.
func (l *liveStore) record(name, model string, o obsResult) {
	l.mu.Lock()
	defer l.mu.Unlock()
	x := l.entry(name)
	x.forgetIfModelChanged(model)
	now := time.Now()
	x.At, l.dirty = now, true
	x.TokensIn += o.Prompt
	x.TokensOut += o.Out
	if o.Prompt > 0 {
		x.LastPrompt = o.Prompt
		x.MaxPrompt = max(x.MaxPrompt, o.Prompt)
	}
	if o.TTFT > 0 {
		x.TTFT = push1(x.TTFT, sample{T: now, Tok: float64(o.TTFT.Milliseconds()), Sec: 1})
	}
	switch {
	case o.PreTok > 0 && o.PreSec > 0: // the server's own count of what it computed (cached tokens excluded)
		if o.PreTok >= prefillMin {
			x.Prefill = push(x.Prefill, sample{T: now, Tok: float64(o.PreTok), Sec: o.PreSec})
		}
	case o.TTFT > 0:
		// tokens served from the prompt cache cost nothing: counting them would make prefill look many times faster
		if fresh := o.Prompt - o.Cached; fresh >= prefillMin {
			x.Prefill = push(x.Prefill, sample{T: now, Tok: float64(fresh), Sec: o.TTFT.Seconds()})
		}
	}
	if o.DecTok > 0 && o.DecSec > 0 {
		x.Decode = push(x.Decode, sample{T: now, Tok: float64(o.DecTok), Sec: o.DecSec})
	}
}

// push1 is push for values that are not rates (time to first token): a plain sliding window.
func push1(ss []sample, x sample) []sample {
	ss = append(ss, x)
	if len(ss) > window {
		ss = ss[len(ss)-window:]
	}
	return ss
}

func (x *liveData) forgetIfModelChanged(model string) {
	if model != "" && x.Model != "" && x.Model != model {
		*x = liveData{}
	}
	if model != "" {
		x.Model = model
	}
}

func (x *liveData) snapshot() Live {
	var ttft []float64
	for _, s := range x.TTFT {
		ttft = append(ttft, s.Tok)
	}
	return Live{
		TPS: estimate(x.Decode), TTFTMS: median(ttft), PrefillTPS: estimate(x.Prefill),
		LastPrompt: x.LastPrompt, MaxPrompt: x.MaxPrompt, Samples: len(x.Decode),
		Requests: x.Requests, Errors: x.Errors, TokensIn: x.TokensIn, TokensOut: x.TokensOut, At: x.At,
	}
}

func (l *liveStore) get(name string) (Live, bool) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if x := l.m[name]; x != nil {
		return x.snapshot(), true
	}
	return Live{}, false
}

// lastOK: when the endpoint last answered a real request (zero: not since the daemon started).
func (l *liveStore) lastOK(name string) time.Time {
	l.mu.Lock()
	defer l.mu.Unlock()
	if x := l.m[name]; x != nil {
		return x.lastOK
	}
	return time.Time{}
}

// bump counts one request outcome; ok=false is a failed attempt (the request may still succeed elsewhere).
func (l *liveStore) bump(name string, ok bool) {
	l.mu.Lock()
	defer l.mu.Unlock()
	x := l.entry(name)
	l.dirty = true
	if ok {
		x.Requests++
		x.lastOK = time.Now()
	} else {
		x.Errors++
	}
}

// save writes the store atomically (private file); a no-op when nothing changed since the last save.
func (l *liveStore) save(path string) error {
	l.mu.Lock()
	if !l.dirty || path == "" {
		l.mu.Unlock()
		return nil
	}
	l.dirty = false // cleared first: a change made while we write marks it again
	l.mu.Unlock()
	recs := l.usageRecs() // takes the lock itself
	l.mu.Lock()
	b, err := json.Marshal(map[string]any{"version": liveVersion, "saved": time.Now(), "endpoints": l.m, "usage": recs})
	l.mu.Unlock()
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, b, 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}

// load restores what an earlier run measured; samples older than maxAge are ignored.
func (l *liveStore) load(path string) error {
	b, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return err
	}
	var f struct {
		Version   int                  `json:"version"`
		Endpoints map[string]*liveData `json:"endpoints"`
		Usage     []usageRec           `json:"usage"`
	}
	if err := json.Unmarshal(b, &f); err != nil {
		return err
	}
	cut := time.Now().Add(-maxAge)
	fresh := func(ss []sample) (out []sample) {
		for _, s := range ss {
			if s.T.After(cut) && s.Sec > 0 {
				out = append(out, s)
			}
		}
		return
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	l.m = map[string]*liveData{}
	l.restoreUsage(f.Usage)
	for k, x := range f.Endpoints {
		if x == nil {
			continue
		}
		x.Decode, x.Prefill, x.TTFT = fresh(x.Decode), fresh(x.Prefill), fresh(x.TTFT)
		if f.Version < 2 { // measured the old way: non-stream replies (load + prefill in their time) and cached prompt tokens
			x.Decode, x.Prefill = nil, nil
		}
		l.m[k] = x
	}
	return nil
}

type obsResult struct {
	TPS    float64
	TTFT   time.Duration
	Prompt int
	Cached int     // prompt tokens the server took from its cache (usage.prompt_tokens_details.cached_tokens)
	Out    int     // completion tokens (server's usage figure, else chunk count)
	DecTok int     // decode tokens: the server's timings when given, else between first and last chunk (tokens-1)
	DecSec float64 // seconds those took
	PreTok int     // prompt tokens the server says it computed (llama.cpp timings.prompt_n); 0: not reported
	PreSec float64 // seconds that took
}

// timingsJSON is llama.cpp's (and fleet-engine's) own measurement, sent with the reply: exact, no network or queue in it.
type timingsJSON struct {
	PromptN     int     `json:"prompt_n"`
	PromptMS    float64 `json:"prompt_ms"`
	PredictedN  int     `json:"predicted_n"`
	PredictedMS float64 `json:"predicted_ms"`
}

// apply takes the server's figures over the ones guessed from the outside.
func (t *timingsJSON) apply(r *obsResult) {
	if t == nil {
		return
	}
	if t.PredictedN > 1 && t.PredictedMS > 0 {
		r.DecTok, r.DecSec = t.PredictedN, t.PredictedMS/1000
		r.TPS = float64(r.DecTok) / r.DecSec
	}
	if t.PromptN > 0 && t.PromptMS > 0 {
		r.PreTok, r.PreSec = t.PromptN, t.PromptMS/1000
	}
}

// observer taps a response while it is forwarded: it sees every chunk but never changes it.
type observer struct {
	t0          time.Time
	stream      bool
	partial     []byte
	body        bytes.Buffer // non-stream replies (capped)
	first, last time.Time
	chunks      int
	usageOut    int
	usagePrompt int
	usageCached int
	timings     *timingsJSON
}

func newObserver(t0 time.Time, stream bool) *observer { return &observer{t0: t0, stream: stream} }

func (o *observer) feed(p []byte) {
	if !o.stream {
		if o.body.Len() < 1<<20 {
			o.body.Write(p)
		}
		return
	}
	o.partial = append(o.partial, p...)
	for {
		i := bytes.IndexByte(o.partial, '\n')
		if i < 0 {
			break
		}
		o.line(o.partial[:i])
		o.partial = o.partial[i+1:]
	}
	if len(o.partial) > 1<<20 {
		o.partial = nil
	}
}

type usageJSON struct {
	Prompt        int `json:"prompt_tokens"`
	Completion    int `json:"completion_tokens"`
	PromptDetails *struct {
		Cached int `json:"cached_tokens"`
	} `json:"prompt_tokens_details"`
}

func (u *usageJSON) cached() int {
	if u == nil || u.PromptDetails == nil || u.PromptDetails.Cached < 0 || u.PromptDetails.Cached > u.Prompt {
		return 0
	}
	return u.PromptDetails.Cached
}

func (o *observer) line(b []byte) {
	s := strings.TrimSpace(string(b))
	if !strings.HasPrefix(s, "data:") {
		return
	}
	s = strings.TrimSpace(strings.TrimPrefix(s, "data:"))
	if s == "" || s == "[DONE]" {
		return
	}
	var ev struct {
		Choices []struct {
			Delta struct {
				Content   string `json:"content"`
				Reasoning string `json:"reasoning_content"` // llama.cpp, vLLM, DeepSeek
				Thinking  string `json:"reasoning"`         // Ollama
			} `json:"delta"`
		} `json:"choices"`
		Usage   *usageJSON   `json:"usage"`
		Timings *timingsJSON `json:"timings"`
	}
	if json.Unmarshal([]byte(s), &ev) != nil {
		return
	}
	if ev.Timings != nil {
		o.timings = ev.Timings // llama.cpp sends them on every chunk; the last one covers the whole reply
	}
	if ev.Usage != nil {
		if ev.Usage.Completion > 0 {
			o.usageOut = ev.Usage.Completion
		}
		if ev.Usage.Prompt > 0 {
			o.usagePrompt = ev.Usage.Prompt
			o.usageCached = ev.Usage.cached()
		}
	}
	if len(ev.Choices) > 0 && (ev.Choices[0].Delta.Content != "" || ev.Choices[0].Delta.Reasoning != "" || ev.Choices[0].Delta.Thinking != "") {
		now := time.Now()
		if o.chunks == 0 {
			o.first = now
		}
		o.last = now
		o.chunks++
	}
}

// result turns what was seen into figures; estPrompt is used when the server reports no usage.
func (o *observer) result(estPrompt int) (r obsResult) {
	if !o.stream {
		var v struct {
			Usage   *usageJSON   `json:"usage"`
			Timings *timingsJSON `json:"timings"`
		}
		if json.Unmarshal(o.body.Bytes(), &v) == nil {
			if v.Usage != nil {
				r.Prompt, r.Out, r.Cached = v.Usage.Prompt, v.Usage.Completion, v.Usage.cached()
			}
			// Without the server's own timings a whole reply says nothing about decode speed: its time holds the
			// queue, a model load and the prefill too (a 2-token reply after a cold load read as 0.4 tok/s).
			v.Timings.apply(&r)
		}
		return r
	}
	r.Prompt, r.Cached = o.usagePrompt, o.usageCached
	if r.Prompt == 0 {
		r.Prompt = estPrompt
	}
	defer o.timings.apply(&r) // exact figures win over the chunk clock
	if o.chunks == 0 {
		return r
	}
	r.TTFT = o.first.Sub(o.t0)
	tokens := o.chunks // one chunk ≈ one token; the server's count is exact when present
	if o.usageOut > o.chunks/2 && o.usageOut < o.chunks*4 {
		tokens = o.usageOut
	}
	r.Out = tokens
	if dur := o.last.Sub(o.first).Seconds(); o.chunks >= 4 && dur >= 0.05 {
		r.TPS = float64(tokens-1) / dur
		r.DecTok, r.DecSec = tokens-1, dur
	}
	return r
}
