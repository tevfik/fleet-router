package board

import (
	"strings"
	"testing"
	"time"

	"github.com/tevfik/fleet-router/internal/config"
	"github.com/tevfik/fleet-router/internal/server"
)

func TestRows(t *testing.T) {
	// Input for testing: 1 embed (skipped), 4 chat (ranked), 1 chat (zero TPS)
	eps := []server.EpStatus{
		{Endpoint: config.Endpoint{Name: "E1", Kind: "embed", TPS: 100}}, // Skip
		{Endpoint: config.Endpoint{Name: "E2", Kind: "chat", TPS: 50}},   // Rank 1, Medal 🥇
		{Endpoint: config.Endpoint{Name: "E3", Kind: "chat", TPS: 40}},   // Rank 2, Medal 🥈
		{Endpoint: config.Endpoint{Name: "E4", Kind: "chat", TPS: 30}},   // Rank 3, Medal 🥉
		{Endpoint: config.Endpoint{Name: "E5", Kind: "chat", TPS: 20}},   // Rank 4, No medal
		{Endpoint: config.Endpoint{Name: "E6", Kind: "chat", TPS: 0}},    // Rank 0, No medal
	}

	rows := Rows(eps)

	if len(rows) != 5 {
		t.Fatalf("expected 5 rows, got %d", len(rows))
	}

	if rows[0].Name != "E2" || rows[0].Medal != "🥇" || rows[0].Rank != 1 {
		t.Errorf("E2 wrong: %+v", rows[0])
	}
	if rows[1].Name != "E3" || rows[1].Medal != "🥈" || rows[1].Rank != 2 {
		t.Errorf("E3 wrong: %+v", rows[1])
	}
	if rows[2].Name != "E4" || rows[2].Medal != "🥉" || rows[2].Rank != 3 {
		t.Errorf("E4 wrong: %+v", rows[2])
	}
	if rows[3].Name != "E5" || rows[3].Medal != "" || rows[3].Rank != 4 {
		t.Errorf("E5 wrong: %+v", rows[3])
	}
	if rows[4].Name != "E6" || rows[4].Medal != "" || rows[4].Rank != 0 {
		t.Errorf("E6 wrong: %+v", rows[4])
	}
}

// a slow but measured model (under 1 tok/s) is still ranked; only a zero means "not measured"
func TestRowsRanksSlowButMeasured(t *testing.T) {
	rows := Rows([]server.EpStatus{
		{Endpoint: config.Endpoint{Name: "unmeasured"}},
		{Endpoint: config.Endpoint{Name: "slow", TPS: 0.5}},
	})
	if rows[0].Name != "slow" || rows[0].Rank != 1 || rows[1].Rank != 0 {
		t.Errorf("rows: %+v", rows)
	}
}

// the log line shows a wait only when it was noticeable (500 ms and up)
func TestLogLineQueueNote(t *testing.T) {
	if got := LogLine(server.LogEntry{Model: "m", Status: 200, QueueMS: 499}); strings.Contains(got, "queued") {
		t.Errorf("499 ms should not be reported: %s", got)
	}
	if got := LogLine(server.LogEntry{Model: "m", Status: 200, QueueMS: 1500}); !strings.Contains(got, "queued 1.5s") {
		t.Errorf("1.5 s wait missing: %s", got)
	}
}

// the in/out columns must not be swapped
func TestRenderUsageColumns(t *testing.T) {
	res := RenderUsage([]server.UsageRow{{Key: "k", Requests: 3, In: 1500, Out: 25000}}, "model")
	f := strings.Fields(res[1])
	if len(f) < 5 || f[3] != "1.5k" || f[4] != "25k" {
		t.Errorf("tokens in/out columns wrong: %v", f)
	}
}

func TestCombineUsageAndRenderSource(t *testing.T) {
	local := []server.UsageRow{
		{Key: "client-local", Requests: 5, In: 500, Out: 1000},
	}
	hub := []server.UsageRow{
		{Key: "client-hub", Requests: 10, In: 2000, Out: 4000},
	}

	combined := CombineUsage(local, hub)
	if len(combined) != 3 {
		t.Fatalf("expected 3 rows (local + hub + total), got %d", len(combined))
	}
	if combined[0].Source != "local" || combined[0].Requests != 5 {
		t.Errorf("row 0: %+v", combined[0])
	}
	if combined[1].Source != "hub" || combined[1].Requests != 10 {
		t.Errorf("row 1: %+v", combined[1])
	}
	if combined[2].Key != "TOTAL" || combined[2].Source != "combined" || combined[2].Requests != 15 || combined[2].In != 2500 || combined[2].Out != 5000 {
		t.Errorf("row 2 (total): %+v", combined[2])
	}

	lines := RenderUsage(combined, "client")
	header := lines[0]
	if !strings.Contains(header, "SOURCE") {
		t.Fatalf("expected SOURCE in header, got: %s", header)
	}
	body := strings.Join(lines[1:], "\n")
	if !strings.Contains(body, "local") || !strings.Contains(body, "hub") || !strings.Contains(body, "combined") {
		t.Fatalf("expected source tags in body:\n%s", body)
	}
}

func TestMax(t *testing.T) {
	if Max([]Row{}) != 0.0 {
		t.Errorf("empty rows max should be 0")
	}
	rows := []Row{{TPS: 10.5}, {TPS: 25.0}, {TPS: 5.0}}
	if Max(rows) != 25.0 {
		t.Errorf("expected 25.0, got %f", Max(rows))
	}
}

func TestBar(t *testing.T) {
	tests := []struct {
		v, max float64
		w      int
		want   string
	}{
		{0, 10, 10, "··········"},
		{5, 0, 10, "··········"},
		{10, 10, 10, "██████████"},
		{5, 10, 10, "█████·····"},
		{1, 10, 10, "█·········"},
		{15, 10, 10, "██████████"},  // full > w case
		{0.5, 10, 10, "▌·········"}, // partial[4] is ▌, combined with 9 dots = 10 width
	}
	for _, tt := range tests {
		got := Bar(tt.v, tt.max, tt.w)
		if got != tt.want {
			t.Errorf("Bar(%f, %f, %d) = %q; want %q", tt.v, tt.max, tt.w, got, tt.want)
		}
	}
}

func TestCompact(t *testing.T) {
	tests := []struct {
		n    int
		want string
	}{
		{5, "5"},
		{999, "999"},
		{1000, "1.0k"},
		{1500, "1.5k"},
		{10000, "10k"},
		{999499, "999k"},
		{999999, "1.0M"}, // used to print "1000k"
		{1000000, "1.0M"},
		{2500000, "2.5M"},
	}
	for _, tt := range tests {
		if got := Compact(tt.n); got != tt.want {
			t.Errorf("Compact(%d) = %q; want %q", tt.n, got, tt.want)
		}
	}
}

func TestLogLine(t *testing.T) {
	now := time.Date(2023, 1, 1, 12, 0, 0, 0, time.UTC)
	e := server.LogEntry{
		Time:     now,
		Model:    "gpt-4",
		Endpoint: "node-1",
		Status:   200,
		DurMS:    1300, // 1300ms -> 1.3s
		TPS:      10.5,
		TTFTMS:   200,
		In:       10,
		Out:      20,
		Attempts: 2,
		Via:      "ollama",
		Remote:   true,
		Err:      " connection reset ",
	}

	got := LogLine(e)
	if !strings.Contains(got, "12:00:00") {
		t.Errorf("missing time: %s", got)
	}
	if !strings.Contains(got, "gpt-4 → node-1") {
		t.Errorf("missing who: %s", got)
	}
	if !strings.Contains(got, "200") {
		t.Errorf("missing status: %s", got)
	}
	if !strings.Contains(got, "1.3s") {
		t.Errorf("missing duration: %s", got)
	}
	if !strings.Contains(got, "ttft 200ms") || !strings.Contains(got, "10.5 t/s") {
		t.Errorf("missing speed: %s", got)
	}
	if !strings.Contains(got, "in 10 out 20") {
		t.Errorf("missing tokens: %s", got)
	}
	if !strings.Contains(got, "↪2") {
		t.Errorf("missing attempts: %s", got)
	}
	if !strings.Contains(got, "(ollama)") {
		t.Errorf("missing via: %s", got)
	}
	if !strings.Contains(got, "↗") {
		t.Errorf("missing remote: %s", got)
	}
	if !strings.Contains(got, "connection reset") {
		t.Errorf("missing error: %s", got)
	}
}

func TestRenderLog(t *testing.T) {
	e1 := server.LogEntry{Time: time.Now(), Model: "A"}
	e2 := server.LogEntry{Time: time.Now().Add(time.Second), Model: "B"}
	logs := []server.LogEntry{e1, e2}

	// newestFirst = false
	res1 := RenderLog(logs, false)
	if !strings.Contains(res1[0], "A") || !strings.Contains(res1[1], "B") {
		t.Errorf("order wrong (false): %v", res1)
	}

	// newestFirst = true
	res2 := RenderLog(logs, true)
	if !strings.Contains(res2[0], "B") || !strings.Contains(res2[1], "A") {
		t.Errorf("order wrong (true): %v", res2)
	}
}

func TestRenderUsage(t *testing.T) {
	// Empty
	res := RenderUsage([]server.UsageRow{}, "model")
	if res[0] != "no traffic recorded in this period" {
		t.Errorf("empty message wrong: %s", res[0])
	}

	// Populate
	rows := []server.UsageRow{
		{Key: "very-long-model-name-that-should-be-truncated", Requests: 100, Errors: 0, In: 1000, Out: 2000, AvgMS: 500},
		{Key: "short", Requests: 50, Errors: 5, In: 500, Out: 500, AvgMS: 200, TPS: 1.5, Failover: 1},
	}

	res = RenderUsage(rows, "By Model")
	if len(res) < 3 {
		t.Fatalf("too few lines: %d", len(res))
	}

	// Check header
	if !strings.Contains(res[0], "BY MODEL") {
		t.Errorf("header wrong: %s", res[0])
	}

	// Check truncation
	if !strings.Contains(res[1], "…") {
		t.Errorf("truncation failed: %s", res[1])
	}

	// Check standard row
	if !strings.Contains(res[2], "short") || !strings.Contains(res[2], "1.5") || !strings.Contains(res[2], "↪1") {
		t.Errorf("row 2 wrong: %s", res[2])
	}

	// Check bar presence
	if !strings.Contains(res[1], "██") {
		t.Errorf("bar missing: %s", res[1])
	}
}
