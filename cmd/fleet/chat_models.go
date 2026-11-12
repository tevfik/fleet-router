package main

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"time"

	"github.com/tevfik/agenttui"

	"github.com/tevfik/fleet-router/internal/board"
	"github.com/tevfik/fleet-router/internal/chatpick"
	"github.com/tevfik/fleet-router/internal/client"
	"github.com/tevfik/fleet-router/internal/config"
	"github.com/tevfik/fleet-router/internal/deploy"
	"github.com/tevfik/fleet-router/internal/server"
)

// The chat's model handling: the picker behind /model and Ctrl+S (ring models, roles, endpoints — offline ones
// marked), the model-management steps behind its "Manage models…" entry, the header/sidebar state of the model
// in use, the conversation history that a model change keeps, and the explanation when a model cannot answer.
// The pure parts live in internal/chatpick.

// manageEntry is the last line of the picker; picking it opens the management steps.
const manageEntry = "⚙ Manage models…  probe · role · slots · tags · files · start · stop"

// What the model is told about an answer that was cut short.
const (
	interruptedMark  = "[response interrupted by the user]"
	interruptedEmpty = "[interrupted by the user before any answer]"
	// lostMark closes a partial answer whose connection broke mid-response (error or EOF, not the user).
	lostMark = "[connection lost mid-response]"
)

// chatAPI is what the chat needs from the daemon (*client.Client; tests use a fake).
type chatAPI interface {
	chatpick.API
	Status() (server.Status, error)
	StreamChatMsgs(cx context.Context, model string, conv []client.ChatMessage, emit func(string)) (client.StreamResult, error)
}

// chatUI is the part of agenttui.App the chat flows use.
type chatUI interface {
	Ask(ctx context.Context, q agenttui.Question) (string, error)
	SetModelName(name string)
}

type chatCtl struct {
	api    chatAPI
	system string
	run    chatpick.RunOn

	// turn serialises exchanges: an interrupted exchange is recorded (with its partial answer) before the next one
	// reads the history, so "send now" never races the cancelled request.
	turn sync.Mutex

	mu     sync.Mutex
	ui     chatUI
	target string
	st     server.Status
	lost   string // prompt whose answer was cut by a lost connection (its partial is the last history entry)
	last   string // the endpoint that answered the previous message
	note   string // how it got there (failover / rerouting), "" when as asked
	hist   chatpick.History

	// the exchange in flight: where its time goes (connecting, thinking, answering) so a slow remote model is not
	// mistaken for a hung one, and so the first-token delay, the thinking time and the speed are told apart.
	now  func() time.Time
	live *chatLive
}

// chatLive is the timing of one exchange.
type chatLive struct {
	start, first, answer time.Time // request sent; first chunk of any kind; first chunk of the answer proper
	thinkEnd             time.Time
	lastText             time.Time // latest answer chunk: the speed is measured up to it, not to the end of the stream
	inThink              bool
	think, text          int // chunks (about one token each) of reasoning and of answer
}

func (c *chatCtl) clock() time.Time {
	if c.now != nil {
		return c.now()
	}
	return time.Now()
}

func secs(d time.Duration) string {
	if d < 10*time.Second {
		return fmt.Sprintf("%.1fs", d.Seconds())
	}
	return fmt.Sprintf("%ds", int(d.Round(time.Second).Seconds()))
}

// phaseText says what the request in flight is doing; "" when none is.
func (c *chatCtl) phaseText() string {
	l := c.live
	if l == nil {
		return ""
	}
	now := c.clock()
	switch {
	case l.first.IsZero():
		s := "connecting… waiting for the first token " + secs(now.Sub(l.start))
		if now.Sub(l.start) > 10*time.Second {
			s += " (slow link, remote model or the model is loading — Esc cancels)"
		}
		return s
	case l.inThink:
		return fmt.Sprintf("thinking… %s (first token after %s)", secs(now.Sub(l.first)), secs(l.first.Sub(l.start)))
	case l.answer.IsZero():
		return "answering…"
	}
	s := fmt.Sprintf("answering · %d tok", l.text)
	if d := now.Sub(l.answer); d > time.Second && l.text > 1 {
		s += fmt.Sprintf(" · %.1f tok/s", float64(l.text-1)/d.Seconds()) // the clock starts at the first token: it is not in the interval
	}
	return s
}

// observe records one streamed chunk (the client wraps reasoning in <think>…</think> chunks).
func (c *chatCtl) observe(chunk string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	l := c.live
	if l == nil {
		return
	}
	now := c.clock()
	switch chunk {
	case "<think>":
		l.inThink = true
		return
	case "</think>":
		l.inThink, l.thinkEnd = false, now
		return
	}
	if chunk == "" {
		return
	}
	if l.first.IsZero() {
		l.first = now
	}
	if l.inThink {
		l.think++
		return
	}
	if l.answer.IsZero() {
		l.answer = now
	}
	l.lastText = now
	l.text++
}

// summary is the closing line of an exchange: the three delays a slow model hides in one number.
func (l *chatLive) summary(end time.Time) string {
	if l == nil || l.first.IsZero() {
		return ""
	}
	parts := []string{"first token " + secs(l.first.Sub(l.start))}
	if l.think > 0 {
		te := l.thinkEnd
		if te.IsZero() {
			te = end
		}
		parts = append(parts, "thinking "+secs(te.Sub(l.first)))
	}
	if !l.answer.IsZero() && l.text > 1 {
		if d := l.lastText.Sub(l.answer); d > 0 { // n chunks span n-1 intervals
			parts = append(parts, fmt.Sprintf("%.1f tok/s (%d tok)", float64(l.text-1)/d.Seconds(), l.text))
		}
	}
	return strings.Join(parts, " · ")
}

// stripThink removes the reasoning blocks of a (partial) streamed answer: the history keeps what was answered.
func stripThink(s string) string {
	for {
		i := strings.Index(s, "<think>")
		if i < 0 {
			return s
		}
		j := strings.Index(s[i:], "</think>")
		if j < 0 {
			return s[:i]
		}
		s = s[:i] + s[i+j+len("</think>"):]
	}
}

// historyBudget is how much of the conversation is sent: half of the target's context window when it is known
// (about 3.5 characters a token), never more than the default.
func (c *chatCtl) historyBudget(st server.Status, target string) int {
	name := chatpick.ExpectedEndpoint(st, target)
	for _, e := range st.Endpoints {
		if e.Name == name && e.Ctx > 0 {
			return min(60000, max(4000, e.Ctx*7/4))
		}
	}
	return 0
}

func newChatCtl(api chatAPI, target, system string, run chatpick.RunOn) *chatCtl {
	c := &chatCtl{api: api, target: target, system: system, run: run}
	c.refresh()
	return c
}

// refresh reads the cluster state for the header (a failed read keeps the last one).
func (c *chatCtl) refresh() server.Status {
	st, err := c.api.Status()
	c.mu.Lock()
	defer c.mu.Unlock()
	if err == nil {
		c.st = st
	}
	return c.st
}

func (c *chatCtl) snapshot() (server.Status, string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.st, c.target
}

// header is the one-line state of the model in use.
func (c *chatCtl) header() string {
	c.mu.Lock()
	defer c.mu.Unlock()
	h := chatpick.Header(c.st, c.target, c.last)
	if p := c.phaseText(); p != "" {
		h += " · " + p
	}
	return h
}

// infoRows are the sidebar rows: model, who serves it, whether it is up and how fast, the last routing, the context.
func (c *chatCtl) infoRows() [][2]string {
	c.mu.Lock()
	defer c.mu.Unlock()
	rows := [][2]string{{"Model", c.target}, {"State", chatpick.Header(c.st, c.target, "")}}
	if p := c.phaseText(); p != "" {
		rows = append(rows, [2]string{"Now", p})
	}
	if c.last != "" {
		rows = append(rows, [2]string{"Answered by", c.last})
	}
	if c.note != "" {
		rows = append(rows, [2]string{"Routing", c.note})
	}
	return append(rows, [2]string{"Context", fmt.Sprintf("%d exchanges kept", c.hist.Turns())})
}

func (c *chatCtl) setTarget(name string) {
	c.mu.Lock()
	c.target, c.last, c.note = name, "", ""
	ui := c.ui
	c.mu.Unlock()
	if ui != nil {
		ui.SetModelName(name)
	}
}

// switchTo changes the target; the conversation stays, and an offline target is said to be one.
func (c *chatCtl) switchTo(e chatpick.Entry, st server.Status) string {
	c.setTarget(e.Name)
	c.mu.Lock()
	turns := c.hist.Turns()
	c.mu.Unlock()
	msg := fmt.Sprintf("Active target: **%s** — %s", e.Name, chatpick.Header(st, e.Name, ""))
	if turns > 0 {
		msg += fmt.Sprintf("\nThe conversation (%d earlier exchange(s)) is sent along, so the new model continues it. /clear starts afresh.", turns)
	}
	if !e.Up {
		msg += "\n⚠ " + chatpick.Explain(st, e.Name, nil)
	}
	return msg
}

// modelCmd is /model, /role and Ctrl+S: /model opens the picker, /model <words> filters it (an exact name or a
// single match switches at once), /model manage opens the management steps.
func (c *chatCtl) modelCmd(ctx context.Context, args []string, a chatUI) (string, bool, error) {
	c.mu.Lock()
	c.ui = a
	c.mu.Unlock()
	st := c.refresh()
	arg := strings.TrimSpace(strings.Join(args, " "))
	if arg == "manage" {
		return c.manage(ctx, a, st)
	}
	var list []chatpick.Entry
	if arg != "" {
		e, hits := chatpick.Resolve(st, arg)
		if e.Name != "" {
			return c.switchTo(e, st), true, nil
		}
		if len(hits) == 0 {
			return fmt.Sprintf("Nothing matches %q. /model lists everything; offline models are marked ✗.", arg), true, nil
		}
		list = hits
	} else {
		list = chatpick.Entries(st, "")
	}
	if len(list) == 0 {
		return "No endpoints or roles found — `fleet discover` finds models.", true, nil
	}
	_, cur := c.snapshot()
	labels := append(chatpick.Labels(list), manageEntry)
	def := ""
	for _, e := range list {
		if e.Name == cur {
			def = chatpick.Label(e)
		}
	}
	text := "Switch model (● up, ✗ offline — the conversation is kept):"
	if arg != "" {
		text = fmt.Sprintf("%d matches for %q:", len(list), arg)
	}
	choice, err := a.Ask(ctx, agenttui.Question{Text: text, Options: labels, Default: def})
	if errors.Is(err, agenttui.ErrAskCancelled) {
		return "Model selection cancelled.", true, nil
	}
	if err != nil {
		return "", false, err
	}
	if choice == manageEntry {
		return c.manage(ctx, a, st)
	}
	for _, e := range list {
		if chatpick.Label(e) == choice {
			return c.switchTo(e, st), true, nil
		}
	}
	return "", false, fmt.Errorf("unknown choice %q", choice)
}

// manage asks which model, then which step, then runs it; it repeats until the user leaves (esc).
func (c *chatCtl) manage(ctx context.Context, a chatUI, st server.Status) (string, bool, error) {
	var done []string
	for {
		list := chatpick.Entries(st, "")
		if len(list) == 0 {
			return "No endpoints or roles to manage.", true, nil
		}
		_, cur := c.snapshot()
		def := ""
		for _, e := range list {
			if e.Name == cur {
				def = chatpick.Label(e)
			}
		}
		pick, err := a.Ask(ctx, agenttui.Question{Text: "Manage which model or role? (esc: done)", Options: chatpick.Labels(list), Default: def})
		if errors.Is(err, agenttui.ErrAskCancelled) {
			break
		}
		if err != nil {
			return "", false, err
		}
		var e chatpick.Entry
		for _, x := range list {
			if chatpick.Label(x) == pick {
				e = x
			}
		}
		acts := chatpick.Actions(e)
		var al []string
		for _, x := range acts {
			al = append(al, x.Label)
		}
		al = append([]string{"Use it in this chat"}, al...)
		ch, err := a.Ask(ctx, agenttui.Question{Text: fmt.Sprintf("%s — %s", e.Name, chatpick.Header(st, e.Name, "")), Options: al})
		if errors.Is(err, agenttui.ErrAskCancelled) {
			continue
		}
		if err != nil {
			return "", false, err
		}
		if ch == al[0] {
			done = append(done, c.switchTo(e, st))
			break
		}
		for _, act := range acts {
			if act.Label != ch {
				continue
			}
			out, err := c.runAction(ctx, a, st, act, e)
			switch {
			case errors.Is(err, agenttui.ErrAskCancelled):
				done = append(done, "cancelled: "+act.Label)
			case err != nil:
				done = append(done, fmt.Sprintf("✗ %s: %v", act.Label, err))
			default:
				done = append(done, fmt.Sprintf("✓ %s: %s", act.Label, out))
			}
			st = c.refresh()
		}
	}
	if len(done) == 0 {
		return "Model management closed.", true, nil
	}
	return strings.Join(done, "\n"), true, nil
}

// runAction collects the step's argument (a choice from the daemon's own lists, or text) and runs it.
func (c *chatCtl) runAction(ctx context.Context, a chatUI, st server.Status, act chatpick.Action, e chatpick.Entry) (string, error) {
	arg := ""
	switch act.Needs {
	case "tags":
		v, err := a.Ask(ctx, agenttui.Question{Text: "Tags of " + e.Name + " (space separated, empty clears):", Default: ""})
		if err != nil {
			return "", err
		}
		arg = v
	case "role", "endpoint", "node":
		opts := chatpick.Choices(st, act.Needs)
		if act.Needs == "endpoint" {
			opts = filterOut(opts, e.Name)
		}
		if len(opts) == 0 {
			return "", fmt.Errorf("no %s to choose from", act.Needs)
		}
		v, err := a.Ask(ctx, agenttui.Question{Text: fmt.Sprintf("%s — choose a %s:", act.Label, act.Needs), Options: opts})
		if err != nil {
			return "", err
		}
		arg = v
	}
	if act.ID == "start" {
		path, err := a.Ask(ctx, agenttui.Question{Text: fmt.Sprintf("Path of the .gguf file on %s:", arg)})
		if err != nil {
			return "", err
		}
		plan, err := c.run(arg, path, true, false)
		if err != nil {
			return "", err
		}
		ok, err := a.Ask(ctx, agenttui.Question{Text: "Start it now?", Detail: plan, Options: []string{"Yes, start and register it as an endpoint", "Yes, start only", "No"}, Default: "No"})
		if err != nil {
			return "", err
		}
		switch ok {
		case "Yes, start and register it as an endpoint":
			arg = arg + "\x00" + path + "\x00register"
		case "Yes, start only":
			arg = arg + "\x00" + path
		default:
			return "", agenttui.ErrAskCancelled
		}
	}
	if act.ID == "stop" {
		dry, err := c.api.StopEndpoint(e.Name, server.StopOptions{DryRun: true})
		if err != nil {
			return "", err
		}
		detail := strings.Join(board.StopLines(dry), "\n")
		opts := []string{"Yes, stop them", "No"}
		if dry.InUse > 0 {
			opts = []string{"Yes, stop them although requests are running", "No"}
			arg = "force"
		}
		ok, err := a.Ask(ctx, agenttui.Question{Text: "Stop the processes behind " + e.Name + "?", Detail: detail, Options: opts, Default: "No"})
		if err != nil {
			return "", err
		}
		if ok == "No" {
			return "", agenttui.ErrAskCancelled
		}
	}
	if act.ID == "remove" {
		ok, err := a.Ask(ctx, agenttui.Question{Text: "Remove the endpoint " + e.Name + " from the configuration? (its process keeps running; roles that name it are mended)", Options: []string{"Yes, remove", "No"}, Default: "No"})
		if err != nil {
			return "", err
		}
		if ok != "Yes, remove" {
			return "", agenttui.ErrAskCancelled
		}
		arg = "mend"
	}
	if act.ID == "forget" {
		ok, err := a.Ask(ctx, agenttui.Question{Text: "Forget the model file of " + e.Name + " in the store? (the file itself stays)", Options: []string{"Yes, forget", "No"}, Default: "No"})
		if err != nil {
			return "", err
		}
		if ok != "Yes, forget" {
			return "", agenttui.ErrAskCancelled
		}
	}
	return chatpick.Do(c.api, st, c.run, act, e, arg)
}

func filterOut(l []string, x string) []string {
	var out []string
	for _, s := range l {
		if s != x {
			out = append(out, s)
		}
	}
	return out
}

// clear forgets the conversation (the model stays).
func (c *chatCtl) clear() {
	c.mu.Lock()
	c.hist.Clear()
	c.mu.Unlock()
}

// stream sends the prompt with the conversation so far to the active target, then records the exchange. When
// the router answered with another endpoint it says so under the answer; when nothing answers, the error says why
// and what to pick instead.
//
// A request cut short (Esc, or Ctrl+X "send now") is kept in the history with the partial answer and an
// interruptedMark, so the next message sees what the user saw. A connection that breaks after tokens arrived is kept the same way with lostMark (and /retry replaces it).
// A real failure with no token received is not recorded.
func (c *chatCtl) stream(ctx context.Context, prompt string, emit func(string)) (int, error) {
	c.turn.Lock()
	defer c.turn.Unlock()
	var partial strings.Builder
	userEmit := emit
	emit = func(s string) { partial.WriteString(s); c.observe(s); userEmit(s) }
	c.mu.Lock()
	if c.lost != "" {
		// Asking the same thing again replaces the kept partial instead of piling up a duplicate exchange.
		if n := len(c.hist.Msgs); prompt == c.lost && n >= 2 && strings.HasSuffix(c.hist.Msgs[n-1].Content, lostMark) {
			c.hist.Msgs = c.hist.Msgs[:n-2]
		}
		c.lost = ""
	}
	tgt := c.target
	c.hist.Max = c.historyBudget(c.st, tgt)
	win, dropped := c.hist.Window()
	live := &chatLive{start: c.clock()}
	c.live = live
	c.mu.Unlock()
	defer func() { c.mu.Lock(); c.live = nil; c.mu.Unlock() }()
	var past []client.ChatMessage
	for _, m := range win {
		past = append(past, client.ChatMessage{Role: m.Role, Content: m.Content})
	}
	res, err := c.api.StreamChatMsgs(ctx, tgt, client.ChatMessages(c.system, past, prompt), emit)
	st := c.refresh()
	if err != nil {
		if ctx.Err() != nil {
			c.mu.Lock()
			if p := strings.TrimSpace(stripThink(partial.String())); p != "" {
				c.hist.Add(prompt, p+"\n\n"+interruptedMark)
			} else {
				c.hist.Add(prompt, interruptedEmpty)
			}
			c.mu.Unlock()
			return res.Tokens, err
		}
		if p := strings.TrimSpace(stripThink(partial.String())); p != "" {
			// The connection broke after tokens arrived: keep what was shown, marked, so the next message
			// sees it; the same prompt sent again (/retry) replaces it.
			c.mu.Lock()
			c.hist.Add(prompt, p+"\n\n"+lostMark)
			c.lost = prompt
			c.mu.Unlock()
			return res.Tokens, errors.New("connection lost, partial answer kept (marked as cut in the conversation) — /retry asks again: " + chatpick.Explain(st, tgt, err))
		}
		return res.Tokens, errors.New(chatpick.Explain(st, tgt, err))
	}
	c.mu.Lock()
	c.hist.Add(prompt, res.Content)
	c.last = res.Endpoint
	c.note = chatpick.Notice(st, tgt, res.Endpoint)
	note := c.note
	c.mu.Unlock()
	var foot []string
	c.mu.Lock()
	sum := live.summary(c.clock())
	c.mu.Unlock()
	if sum != "" {
		foot = append(foot, sum)
	}
	if note != "" {
		foot = append(foot, "↪ "+note)
	}
	if dropped > 0 {
		foot = append(foot, fmt.Sprintf("(%d older exchange(s) not sent: the context budget)", dropped))
	}
	if len(foot) > 0 {
		emit("\n\n_" + strings.Join(foot, " · ") + "_")
	}
	return res.Tokens, nil
}

// watch keeps the header's cluster state fresh until ctx ends.
func (c *chatCtl) watch(ctx context.Context) {
	t := time.NewTicker(4 * time.Second)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			c.refresh()
		}
	}
}

// runOnFromConfig is `fleet run-on <node> <model.gguf>` for the chat (default port, context and GPU layers);
// dry only reports what would be run.
func runOnFromConfig(node, path string, dry, register bool) (string, error) {
	cfg, err := config.Load(config.Path())
	if err != nil {
		return "", err
	}
	res, err := deploy.RunOnNode(context.Background(), cfg, node, path, 0, 0, 0, dry)
	if err != nil {
		return "", err
	}
	if dry {
		return fmt.Sprintf("on %s: %s\n→ endpoint %s at %s", node, res.MasterCmd, res.EndpointName, res.URL), nil
	}
	state := "running"
	if res.Status != deploy.StatusRunning {
		state = "starting (not healthy yet)"
	}
	reg := fmt.Sprintf("not registered (fleet endpoint add %s %s, or start it again with registration)", res.EndpointName, res.URL)
	if register {
		name, err := registerRunOn(node, path, res, 0)
		if err != nil {
			reg = "registering it failed: " + err.Error()
		} else {
			reg = "registered as endpoint " + name + " (probe it, then route to it from a role)"
		}
	}
	return fmt.Sprintf("%s on %s: %s (PID %d) — %s%s", state, node, res.URL, res.MasterPID, reg, strings.Join(append([]string{""}, res.Warnings...), "\n! ")), nil
}
