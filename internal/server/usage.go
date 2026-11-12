package server

import (
	"net/http"
	"sort"
	"strconv"
	"strings"
	"time"
)

// Usage statistics: who asked for what and how much. Counted per hour, kept for usageKeep and
// saved in the same file as the speed figures.
const usageKeep = 30 * 24 * time.Hour

// UsageRow is one aggregated line of the usage report.
type UsageRow struct {
	Key      string  `json:"key"`              // the grouping value (client, role, model, endpoint or day, depending on `by`)
	Sub      string  `json:"sub,omitempty"`    // second level when a split was asked for (e.g. role → model)
	Source   string  `json:"source,omitempty"` // "local" | "hub" | "combined"
	Requests int     `json:"requests"`
	Errors   int     `json:"errors"` // requests that ended with an HTTP error
	In       int     `json:"tokens_in"`
	Out      int     `json:"tokens_out"`
	AvgMS    int64   `json:"avg_ms"`   // mean request duration
	QueueMS  int64   `json:"queue_ms"` // mean wait for a slot
	TPS      float64 `json:"tps,omitempty"`
	Failover int     `json:"failovers"` // requests that needed more than one attempt
}

// usageRec is one stored bucket: all requests of one hour with the same client, role and model.
type usageRec struct {
	Hour     int64  `json:"h"` // unix hour
	Client   string `json:"c"`
	Role     string `json:"r,omitempty"`
	Model    string `json:"m"` // node/model of the endpoint that answered ("-" when none did)
	Endpoint string `json:"e,omitempty"`
	Req      int    `json:"n"`
	Err      int    `json:"x,omitempty"`
	Failover int    `json:"f,omitempty"`
	In       int    `json:"i,omitempty"`
	Out      int    `json:"o,omitempty"`
	DurMS    int64  `json:"d,omitempty"`
	QueueMS  int64  `json:"q,omitempty"`
	GenMS    int64  `json:"g,omitempty"` // time spent generating (for the mean tok/s)
}

type usageKey struct {
	hour                          int64
	client, role, model, endpoint string
}

// clientLabel names the caller: Bearer API key identity, else the X-Fleet-Client header, else User-Agent.
func clientLabel(r *http.Request) string {
	if token := ExtractBearer(r); token != "" {
		if ak, ok := GetKeyStore().Lookup(token); ok {
			return ak.Name
		}
	}
	v := strings.TrimSpace(r.Header.Get("X-Fleet-Client"))
	if v == "" {
		v = strings.TrimSpace(r.Header.Get("User-Agent"))
		if i := strings.IndexAny(v, "/ ("); i > 0 {
			v = v[:i]
		}
	}
	v = strings.ToLower(v)
	if v == "" {
		return "unknown"
	}
	if len(v) > 40 {
		v = v[:40]
	}
	return v
}

// addUsage counts one finished request. ref is the "node/model" of the answering endpoint.
func (l *liveStore) addUsage(e LogEntry, ref string) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.usage == nil {
		l.usage = map[usageKey]*usageRec{}
	}
	if ref == "" {
		ref = "-"
	}
	k := usageKey{e.Time.Unix() / 3600, e.Client, e.Role, ref, e.Endpoint}
	u := l.usage[k]
	if u == nil {
		u = &usageRec{Hour: k.hour, Client: k.client, Role: k.role, Model: k.model, Endpoint: k.endpoint}
		l.usage[k] = u
	}
	u.Req++
	if e.Status >= 400 {
		u.Err++
	}
	if e.Attempts > 1 {
		u.Failover++
	}
	u.In += e.In
	u.Out += e.Out
	u.DurMS += e.DurMS
	u.QueueMS += e.QueueMS
	if e.TPS > 0 && e.Out > 0 {
		u.GenMS += int64(float64(e.Out) / e.TPS * 1000)
	}
	l.dirty = true
}

// usageReport aggregates the stored buckets newer than since, grouped by "client", "role", "model",
// "endpoint" or "day"; most requests first (days newest first).
func (l *liveStore) usageReport(by, split string, since time.Duration) []UsageRow {
	return l.usageReportFiltered(by, split, since, "")
}

// usageReportFiltered is usageReport with an optional client excluded (e.g. "hub-tunnel").
func (l *liveStore) usageReportFiltered(by, split string, since time.Duration, excludeClient string) []UsageRow {
	l.mu.Lock()
	defer l.mu.Unlock()
	cut := time.Now().Add(-since).Unix() / 3600
	type acc struct {
		UsageRow
		gen int64
	}
	rows := map[string]*acc{}
	for _, u := range l.usage {
		if u.Hour < cut {
			continue
		}
		if excludeClient != "" && u.Client == excludeClient {
			continue
		}
		key, sub := u.dim(by), ""
		if split != "" {
			sub = u.dim(split)
		}
		a := rows[key+"\x00"+sub]
		if a == nil {
			a = &acc{UsageRow: UsageRow{Key: key, Sub: sub}}
			rows[key+"\x00"+sub] = a
		}
		a.Requests += u.Req
		a.Errors += u.Err
		a.Failover += u.Failover
		a.In += u.In
		a.Out += u.Out
		a.AvgMS += u.DurMS // sums for now
		a.QueueMS += u.QueueMS
		a.gen += u.GenMS
	}
	var out []UsageRow
	for _, a := range rows {
		if a.Requests > 0 {
			a.AvgMS /= int64(a.Requests)
			a.QueueMS /= int64(a.Requests)
		}
		if a.gen > 0 {
			a.TPS = float64(int(float64(a.Out)/(float64(a.gen)/1000)*10+0.5)) / 10
		}
		out = append(out, a.UsageRow)
	}
	sort.Slice(out, func(i, j int) bool {
		if by == "day" && out[i].Key != out[j].Key {
			return out[i].Key > out[j].Key
		}
		if split != "" && out[i].Key != out[j].Key {
			return out[i].Key < out[j].Key
		}
		if out[i].Requests != out[j].Requests {
			return out[i].Requests > out[j].Requests
		}
		return out[i].Key+out[i].Sub < out[j].Key+out[j].Sub
	})
	return out
}

// dim reads one grouping dimension of a bucket.
func (u *usageRec) dim(by string) string {
	v := ""
	switch by {
	case "role":
		v = u.Role
	case "model":
		v = u.Model
	case "endpoint":
		v = u.Endpoint
	case "day":
		v = time.Unix(u.Hour*3600, 0).Format("2006-01-02")
	default:
		v = u.Client
	}
	if v == "" {
		return "-"
	}
	return v
}

func (l *liveStore) usageRecs() []usageRec {
	l.mu.Lock()
	defer l.mu.Unlock()
	cut := time.Now().Add(-usageKeep).Unix() / 3600
	var out []usageRec
	for k, u := range l.usage {
		if u.Hour < cut {
			delete(l.usage, k) // expired: saving is also where old buckets leave memory
			continue
		}
		out = append(out, *u)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Hour < out[j].Hour })
	return out
}

func (l *liveStore) restoreUsage(recs []usageRec) {
	cut := time.Now().Add(-usageKeep).Unix() / 3600
	l.usage = map[usageKey]*usageRec{}
	for _, u := range recs {
		if u.Hour < cut || u.Req <= 0 {
			continue
		}
		u := u
		l.usage[usageKey{u.Hour, u.Client, u.Role, u.Model, u.Endpoint}] = &u
	}
}

// Usage is the programmatic form of GET /fleet/usage.
func (s *Server) Usage(by, split string, since time.Duration) []UsageRow {
	return s.live.usageReport(by, split, since)
}

// UsageFiltered is Usage with an optional client excluded.
func (s *Server) UsageFiltered(by, split string, since time.Duration, excludeClient string) []UsageRow {
	return s.live.usageReportFiltered(by, split, since, excludeClient)
}

// ParseSince reads "24h", "7d" or "30d" (default 7 days, at most usageKeep).
func ParseSince(v string) time.Duration {
	d := 7 * 24 * time.Hour
	if n, err := strconv.Atoi(strings.TrimSuffix(v, "d")); err == nil && strings.HasSuffix(v, "d") && n > 0 {
		d = time.Duration(n) * 24 * time.Hour
	} else if x, err := time.ParseDuration(v); err == nil && x > 0 {
		d = x
	}
	return min(d, usageKeep)
}
