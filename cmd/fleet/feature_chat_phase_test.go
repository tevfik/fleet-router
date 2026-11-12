package main

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/tevfik/fleet-router/internal/client"
	"github.com/tevfik/fleet-router/internal/config"
	"github.com/tevfik/fleet-router/internal/server"
)

// phaseAPI streams a slow thinking model on a fake clock: the first token after 20 s, 30 s of reasoning, then the answer.
type phaseAPI struct {
	*fakeChatAPI
	clk    *time.Time
	atWait func()
	mid    func()
}

func (p *phaseAPI) StreamChatMsgs(_ context.Context, _ string, conv []client.ChatMessage, emit func(string)) (client.StreamResult, error) {
	p.atWait()
	*p.clk = p.clk.Add(20 * time.Second)
	emit("<think>")
	emit("hmm")
	*p.clk = p.clk.Add(30 * time.Second)
	p.mid()
	emit("</think>")
	for i := 0; i < 20; i++ {
		emit("w")
		*p.clk = p.clk.Add(500 * time.Millisecond)
	}
	*p.clk = p.clk.Add(4500 * time.Millisecond) // the stream closes 5 s after the last token (usage chunk, a slow link)
	return client.StreamResult{Tokens: 21, Endpoint: "ep", Content: strings.Repeat("w", 20)}, nil
}

func TestChat_PhasesAndSummaryTellTheDelaysApart(t *testing.T) {
	clk := time.Unix(1000, 0)
	var ctl *chatCtl
	var waitH, thinkH string
	api := &phaseAPI{fakeChatAPI: &fakeChatAPI{st: server.Status{Endpoints: []server.EpStatus{{Endpoint: config.Endpoint{Name: "ornith"}, OK: true}}}}, clk: &clk}
	api.atWait = func() { clk = clk.Add(12 * time.Second); waitH = ctl.header() }
	api.mid = func() { thinkH = ctl.header() }
	ctl = newChatCtl(api, "ornith", "", nil)
	ctl.now = func() time.Time { return clk }
	var out strings.Builder
	if _, err := ctl.stream(context.Background(), "hi", func(s string) { out.WriteString(s) }); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(waitH, "connecting… waiting for the first token 12s") || !strings.Contains(waitH, "slow link") {
		t.Errorf("connect phase: %q", waitH)
	}
	if !strings.Contains(thinkH, "thinking… 30s (first token after 32s)") {
		t.Errorf("thinking phase: %q", thinkH)
	}
	// 20 answer tokens, one every 0.5 s: 2.0 tok/s (not 20/10 s counted to the end of the stream, nor from the request)
	for _, want := range []string{"first token 32s", "thinking 30s", "2.0 tok/s (20 tok)"} {
		if !strings.Contains(out.String(), want) {
			t.Errorf("summary lacks %q: %q", want, out.String())
		}
	}
	if h := ctl.header(); strings.Contains(h, "thinking") || strings.Contains(h, "connecting") {
		t.Errorf("phase must clear after the exchange: %q", h)
	}
}

func TestChat_InterruptedThinkingIsNotKeptAndBudgetFollowsContext(t *testing.T) {
	if got := stripThink("<think>secret</think>answer <think>half"); got != "answer " {
		t.Errorf("stripThink = %q", got)
	}
	st := server.Status{Endpoints: []server.EpStatus{{Endpoint: config.Endpoint{Name: "small", Ctx: 4096}}, {Endpoint: config.Endpoint{Name: "big", Ctx: 262144}}, {Endpoint: config.Endpoint{Name: "none"}}}}
	c := &chatCtl{}
	if b := c.historyBudget(st, "small"); b != 7168 {
		t.Errorf("small ctx budget %d", b)
	}
	if b := c.historyBudget(st, "big"); b != 60000 {
		t.Errorf("big ctx budget %d", b)
	}
	if b := c.historyBudget(st, "none"); b != 0 {
		t.Errorf("unknown ctx keeps the default, got %d", b)
	}
}
