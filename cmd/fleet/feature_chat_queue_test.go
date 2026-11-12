package main

import (
	"context"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	tea "charm.land/bubbletea/v2"
	"github.com/tevfik/agenttui"

	"github.com/tevfik/fleet-router/internal/client"
)

// gatedAPI answers every prompt with a first chunk, then waits for release(prompt) or for the cancel.
type gatedAPI struct {
	*fakeChatAPI
	mu      sync.Mutex
	convs   [][]client.ChatMessage
	gates   map[string]chan struct{}
	started chan string
}

func newGatedAPI() *gatedAPI {
	return &gatedAPI{fakeChatAPI: &fakeChatAPI{}, gates: map[string]chan struct{}{}, started: make(chan string, 16)}
}

func (g *gatedAPI) gate(p string) chan struct{} {
	g.mu.Lock()
	defer g.mu.Unlock()
	if g.gates[p] == nil {
		g.gates[p] = make(chan struct{})
	}
	return g.gates[p]
}
func (g *gatedAPI) release(p string) { close(g.gate(p)) }
func (g *gatedAPI) conv(i int) []client.ChatMessage {
	g.mu.Lock()
	defer g.mu.Unlock()
	return g.convs[i]
}

func (g *gatedAPI) StreamChatMsgs(ctx context.Context, _ string, conv []client.ChatMessage, emit func(string)) (client.StreamResult, error) {
	p := conv[len(conv)-1].Content
	g.mu.Lock()
	g.convs = append(g.convs, conv)
	g.mu.Unlock()
	g.started <- p
	emit("part:" + p)
	select {
	case <-g.gate(p):
		emit(" end:" + p)
		return client.StreamResult{Tokens: 1, Endpoint: "ep", Content: "part:" + p + " end:" + p}, nil
	case <-ctx.Done():
		return client.StreamResult{}, ctx.Err()
	}
}

// chatRig runs the real agenttui App on the chat controller with a tiny event loop.
type chatRig struct {
	t    *testing.T
	app  *agenttui.App
	api  *gatedAPI
	ctl  *chatCtl
	msgs chan tea.Msg
}

func newChatRig(t *testing.T) *chatRig {
	isolateChat(t)
	api := newGatedAPI()
	ctl := newChatCtl(api, "m", "", nil)
	app := agenttui.New(agenttui.Config{Theme: agenttui.ThemeByName(""), Stream: ctl.stream, ShowFooter: true})
	r := &chatRig{t: t, app: app, api: api, ctl: ctl, msgs: make(chan tea.Msg, 1024)}
	r.run(app.Init())
	r.send(tea.WindowSizeMsg{Width: 110, Height: 32})
	r.pumpFor(1000 * time.Millisecond) // the app ignores keys until its init timer fired
	return r
}

func (r *chatRig) run(cmd tea.Cmd) {
	if cmd == nil {
		return
	}
	go func() {
		m := cmd()
		if b, ok := m.(tea.BatchMsg); ok {
			for _, c := range b {
				r.run(c)
			}
			return
		}
		if m != nil {
			r.msgs <- m
		}
	}()
}

func (r *chatRig) send(m tea.Msg) {
	_, cmd := r.app.Update(m)
	r.run(cmd)
}

func (r *chatRig) pumpFor(d time.Duration) {
	end := time.After(d)
	for {
		select {
		case m := <-r.msgs:
			if strings.Contains(strings.ToLower(typeName(m)), "tick") || typeName(m) == "cursor.BlinkMsg" {
				continue
			}
			r.send(m)
		case <-end:
			return
		}
	}
}

func typeName(m tea.Msg) string { return fmt.Sprintf("%T", m) }

func (r *chatRig) typeLine(s string) {
	for _, c := range s {
		r.send(tea.KeyPressMsg{Code: c, Text: string(c)})
	}
	r.send(tea.KeyPressMsg{Code: tea.KeyEnter})
}

func (r *chatRig) typeText(s string) {
	for _, c := range s {
		r.send(tea.KeyPressMsg{Code: c, Text: string(c)})
	}
}

func (r *chatRig) ctrlX() { r.send(tea.KeyPressMsg{Code: 'x', Mod: tea.ModCtrl}) }

// until pumps events until cond holds; a hang (the old lock-up) fails after 5s.
func (r *chatRig) until(what string, cond func() bool) {
	r.t.Helper()
	deadline := time.After(5 * time.Second)
	for !cond() {
		select {
		case m := <-r.msgs:
			if strings.Contains(strings.ToLower(typeName(m)), "tick") || typeName(m) == "cursor.BlinkMsg" {
				continue
			}
			r.send(m)
		case <-time.After(10 * time.Millisecond):
		case <-deadline:
			r.t.Fatalf("timed out waiting for %s\n%s", what, r.app.View().Content)
		}
	}
}

func (r *chatRig) started(want string) {
	r.t.Helper()
	select {
	case got := <-r.api.started:
		if got != want {
			r.t.Fatalf("request %q started, want %q", got, want)
		}
	case <-time.After(5 * time.Second):
		r.t.Fatalf("request %q never started", want)
	}
}

func (r *chatRig) screen() string { return r.app.View().Content }

func (r *chatRig) histLen() int {
	r.ctl.mu.Lock()
	defer r.ctl.mu.Unlock()
	return len(r.ctl.hist.Msgs)
}

func TestChatQueuesWhileAnswering(t *testing.T) {
	r := newChatRig(t)
	r.typeLine("one")
	r.pumpFor(50 * time.Millisecond)
	r.started("one")

	r.typeLine("two")
	r.typeLine("three")
	r.pumpFor(50 * time.Millisecond)
	if s := r.screen(); !strings.Contains(s, "queued 1/2: two") || !strings.Contains(s, "part:one") {
		t.Fatalf("queued indicator / running answer missing:\n%s", s)
	}
	select {
	case p := <-r.api.started:
		t.Fatalf("%q was sent while the first answer was still running", p)
	default:
	}

	r.api.release("one")
	r.until("two sent", func() bool { return len(r.api.started) == 1 })
	r.started("two")
	if c := r.api.conv(1); len(c) < 3 || c[len(c)-3].Content != "one" || !strings.Contains(c[len(c)-2].Content, "end:one") {
		t.Fatalf("second request lacks the first exchange: %+v", c)
	}
	r.api.release("two")
	r.until("three sent", func() bool { return len(r.api.started) == 1 })
	r.started("three")
	r.api.release("three")
	r.until("history", func() bool { return r.histLen() == 6 })

	want := []string{"one", "part:one end:one", "two", "part:two end:two", "three", "part:three end:three"}
	for i, w := range want {
		if r.ctl.hist.Msgs[i].Content != w {
			t.Fatalf("hist[%d] = %q, want %q", i, r.ctl.hist.Msgs[i].Content, w)
		}
	}
}

func TestChatSendNowInterruptsAndKeepsPartial(t *testing.T) {
	r := newChatRig(t)
	r.typeLine("one")
	r.pumpFor(50 * time.Millisecond)
	r.started("one")

	r.typeText("two")
	r.ctrlX()
	r.until("two sent", func() bool { return len(r.api.started) == 1 })
	r.started("two")

	c := r.api.conv(1) // what the model gets for "two": the interrupted exchange first
	if len(c) < 3 || c[len(c)-3].Content != "one" || c[len(c)-2].Role != "assistant" ||
		c[len(c)-2].Content != "part:one\n\n"+interruptedMark || c[len(c)-1].Content != "two" {
		t.Fatalf("history sent with the new message is inconsistent: %+v", c)
	}
	r.pumpFor(100 * time.Millisecond)
	if s := r.screen(); !strings.Contains(s, "part:two") || !strings.Contains(s, "interrupted") {
		t.Fatalf("the new answer must be running and the cut one marked:\n%s", s)
	}
	r.api.release("two")
	r.until("history", func() bool { return r.histLen() == 4 })
	if got := r.ctl.hist.Msgs[3].Content; got != "part:two end:two" {
		t.Fatalf("second answer = %q", got)
	}
}

func TestChatEscCancelsAndDropsQueue(t *testing.T) {
	r := newChatRig(t)
	r.typeLine("one")
	r.pumpFor(50 * time.Millisecond)
	r.started("one")
	r.typeLine("two")
	r.send(tea.KeyPressMsg{Code: tea.KeyEscape})
	r.until("recorded", func() bool { return r.histLen() == 2 })
	r.pumpFor(150 * time.Millisecond)
	if s := r.screen(); !strings.Contains(s, "cancelled") || !strings.Contains(s, "1 queued message(s) discarded") {
		t.Fatalf("cancel must say what happened to the queue:\n%s", s)
	}
	select {
	case p := <-r.api.started:
		t.Fatalf("discarded message %q was sent", p)
	default:
	}
	if got := r.ctl.hist.Msgs[1].Content; !strings.HasSuffix(got, interruptedMark) {
		t.Fatalf("cancelled answer not marked in the history: %q", got)
	}
}

func TestChatInterruptBeforeAnyAnswerIsRecorded(t *testing.T) {
	isolateChat(t)
	ctl := newChatCtl(&fakeChatAPI{}, "m", "", nil)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	ctl.api.(*fakeChatAPI).err = context.Canceled
	ctl.stream(ctx, "q", func(string) {})
	if len(ctl.hist.Msgs) != 2 || ctl.hist.Msgs[1].Content != interruptedEmpty {
		t.Fatalf("history %+v", ctl.hist.Msgs)
	}
}
