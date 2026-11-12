// Package chatpick is the model picker of the fleet chat: what can be chosen (ring/distributed models, roles,
// plain endpoints — offline ones included and marked), the one-line state shown in the chat header, the
// "who really answered" note, the explanation (with alternatives) when a model cannot answer, and the
// conversation history that survives a model change. It is pure: the chat (main.go) draws it.
package chatpick

import (
	"fmt"
	"sort"
	"strings"

	"github.com/tevfik/fleet-router/internal/server"
)

type Kind int

const (
	Ring     Kind = iota // distributed / llama-ring group
	Role                 // a role: routes around members that are down
	Endpoint             // a plain endpoint
)

func (k Kind) String() string { return [...]string{"ring", "role", "endpoint"}[k] }

// Entry is one thing the picker offers.
type Entry struct {
	Name      string
	Kind      Kind
	Up        bool
	Node      string
	Model     string
	Detail    string // role: "→ endpoint" (now) ; endpoint: why it is down
	TPS       float64
	LatencyMS int64
}

func isRing(e server.EpStatus) bool {
	return strings.HasPrefix(e.Name, "distributed-") || (e.Group != nil && e.Group.Engine == "llama-ring")
}

// Entries lists the picker in its fixed order: ring models, roles, endpoints; inside a group the ones that are up
// first, then by name. Offline endpoints are listed (marked down) so they can be managed. Embedding models are
// not chat targets. filter keeps the entries that contain every word (name, machine, model, kind, state).
func Entries(st server.Status, filter string) []Entry {
	var out []Entry
	seen := map[string]bool{}
	for _, ep := range st.Endpoints {
		if ep.IsEmbed() || seen[ep.Name] {
			continue
		}
		seen[ep.Name] = true
		k := Endpoint
		if isRing(ep) {
			k = Ring
		}
		d := ""
		if !ep.OK {
			d = ep.Err
			if d == "" {
				d = "offline"
			}
		}
		out = append(out, Entry{Name: ep.Name, Kind: k, Up: ep.OK, Node: ep.Node, Model: ep.Model, Detail: d, TPS: ep.TPS, LatencyMS: ep.LatencyMS})
	}
	for r, rs := range st.Roles {
		if seen[r] {
			continue
		}
		seen[r] = true
		up := false
		for _, ep := range st.Endpoints {
			if ep.Name == rs.Active && ep.OK {
				up = true
			}
		}
		d := "no endpoint is up"
		if rs.Active != "" {
			d = "→ " + rs.Active
		}
		out = append(out, Entry{Name: r, Kind: Role, Up: up, Detail: d})
	}
	sort.Slice(out, func(i, j int) bool {
		a, b := out[i], out[j]
		if a.Kind != b.Kind {
			return a.Kind < b.Kind
		}
		if a.Up != b.Up {
			return a.Up
		}
		return a.Name < b.Name
	})
	if strings.TrimSpace(filter) == "" {
		return out
	}
	var keep []Entry
	for _, e := range out {
		if Matches(e, filter) {
			keep = append(keep, e)
		}
	}
	return keep
}

// Matches is the picker's search: every word must appear in the entry's text.
func Matches(e Entry, filter string) bool {
	state := "up"
	if !e.Up {
		state = "offline down"
	}
	hay := strings.ToLower(strings.Join([]string{e.Name, e.Kind.String(), e.Node, e.Model, e.Detail, state}, " "))
	for _, w := range strings.Fields(strings.ToLower(filter)) {
		if !strings.Contains(hay, w) {
			return false
		}
	}
	return true
}

// Find returns the entry called name.
func Find(st server.Status, name string) (Entry, bool) {
	for _, e := range Entries(st, "") {
		if e.Name == name {
			return e, true
		}
	}
	return Entry{}, false
}

// Label is the picker line of an entry: group, state, name, and what it is.
func Label(e Entry) string {
	mark := "● "
	if !e.Up {
		mark = "✗ "
	}
	s := fmt.Sprintf("[%s] %s%s", e.Kind, mark, e.Name)
	var bits []string
	switch {
	case !e.Up && e.Kind != Role:
		bits = append(bits, "OFFLINE")
	case e.Kind == Role:
		bits = append(bits, e.Detail)
	}
	if e.Kind != Role {
		if e.Node != "" {
			bits = append(bits, "@"+e.Node)
		}
		if e.Up && e.TPS > 0 {
			bits = append(bits, fmt.Sprintf("%.0f t/s", e.TPS))
		}
	}
	if len(bits) > 0 {
		s += "  " + strings.Join(bits, " · ")
	}
	return s
}

// Labels returns the labels of entries, in order.
func Labels(es []Entry) []string {
	out := make([]string, len(es))
	for i, e := range es {
		out[i] = Label(e)
	}
	return out
}

// Resolve turns what the user typed after /model into the entry to switch to: an exact name wins, otherwise the
// words filter the list and a single survivor is taken. The rest is returned for the picker to show.
func Resolve(st server.Status, arg string) (Entry, []Entry) {
	arg = strings.TrimSpace(arg)
	if e, ok := Find(st, arg); ok {
		return e, nil
	}
	hits := Entries(st, arg)
	if len(hits) == 1 {
		return hits[0], nil
	}
	return Entry{}, hits
}

// ExpectedEndpoint is the endpoint a request for target would reach right now ("" : none).
func ExpectedEndpoint(st server.Status, target string) string {
	if r, ok := st.Roles[target]; ok {
		return r.Active
	}
	return target
}

// Header is the chat header line: what the chat talks to, whether it is up, and how fast. last is the endpoint
// that answered the previous message ("" : none yet).
func Header(st server.Status, target, last string) string {
	e, ok := Find(st, target)
	if !ok {
		return target + " — not known to the fleet"
	}
	var ep *server.EpStatus
	name := ExpectedEndpoint(st, target)
	for i := range st.Endpoints {
		if st.Endpoints[i].Name == name {
			ep = &st.Endpoints[i]
		}
	}
	switch {
	case e.Kind == Role && ep == nil:
		return fmt.Sprintf("role %s ✗ no endpoint is up", target)
	case ep == nil: // a name that is no endpoint (a role and an endpoint with one name, or a stale entry)
		return fmt.Sprintf("%s ✗ not available", target)
	case e.Kind == Role:
		return fmt.Sprintf("role %s → %s", target, epLine(*ep, last))
	default:
		return epLine(*ep, last)
	}
}

func epLine(ep server.EpStatus, last string) string {
	s := ep.Name
	if ep.Node != "" {
		s += " @" + ep.Node
	}
	if !ep.OK {
		return s + " ✗ offline"
	}
	s += " ✓ up"
	if ep.TPS > 0 {
		s += fmt.Sprintf(" · %.0f t/s", ep.TPS)
	}
	if ep.LatencyMS > 0 {
		s += fmt.Sprintf(" · %dms", ep.LatencyMS)
	}
	if last != "" && last != ep.Name {
		s += " · last answer: " + last
	}
	return s
}

// Notice says which endpoint answered when that is not simply the one that was asked for, and why: a role
// whose preferred member is down fails over; a named endpoint that cannot answer is replaced by the router.
// "" when nothing is worth saying.
func Notice(st server.Status, asked, answered string) string {
	if answered == "" || answered == asked {
		return ""
	}
	if r, ok := st.Roles[asked]; ok {
		if r.Preferred != "" && r.Preferred != answered {
			down := false
			for _, ep := range st.Endpoints {
				if ep.Name == r.Preferred && !ep.OK {
					down = true
				}
			}
			if down {
				return fmt.Sprintf("%s answered for role %s — failover: preferred %s is down", answered, asked, r.Preferred)
			}
			return fmt.Sprintf("%s answered for role %s (preferred %s was busy or skipped — see the role's strategy)", answered, asked, r.Preferred)
		}
		return fmt.Sprintf("%s answered for role %s", answered, asked)
	}
	return fmt.Sprintf("rerouted: asked %s, answered by %s", asked, answered)
}

// Alternatives suggests up to n other targets that are up, best first: the same model elsewhere, then roles,
// then the fastest others. The target itself is never suggested.
func Alternatives(st server.Status, target string, n int) []string {
	cur, _ := Find(st, target)
	type cand struct {
		name  string
		score float64
	}
	var cs []cand
	for _, e := range Entries(st, "") {
		if e.Name == target || !e.Up {
			continue
		}
		sc := e.TPS
		switch {
		case cur.Model != "" && e.Model == cur.Model:
			sc += 2000
		case e.Kind == Role:
			sc += 1000
		}
		cs = append(cs, cand{e.Name, sc})
	}
	sort.SliceStable(cs, func(i, j int) bool { return cs[i].score > cs[j].score })
	var out []string
	for i := 0; i < len(cs) && i < n; i++ {
		out = append(out, cs[i].name)
	}
	return out
}

// Explain turns a failed request into a message a person can act on: why, and what to pick instead.
func Explain(st server.Status, target string, err error) string {
	e, known := Find(st, target)
	var why string
	switch {
	case !known:
		why = fmt.Sprintf("%q is not an endpoint or role of this fleet (it may have been removed)", target)
	case !e.Up && e.Kind == Role:
		why = fmt.Sprintf("role %s has no endpoint that is up", target)
	case !e.Up:
		why = fmt.Sprintf("%s is offline (%s)", target, e.Detail)
	default:
		why = fmt.Sprintf("%s is up but the request failed: %s", target, short(err))
	}
	if known && !e.Up && err != nil {
		why += " — " + short(err)
	}
	s := why
	if alt := Alternatives(st, target, 3); len(alt) > 0 {
		s += "\nTry instead: " + strings.Join(alt, ", ") + "  (/model <name> switches; the conversation is kept)"
	} else {
		s += "\nNo other model is up. /model → Manage models… can probe or start one."
	}
	return s
}

func short(err error) string {
	if err == nil {
		return "no details"
	}
	s := strings.Join(strings.Fields(err.Error()), " ")
	if r := []rune(s); len(r) > 160 {
		s = string(r[:159]) + "…"
	}
	return s
}

// Msg is one conversation message.
type Msg struct{ Role, Content string }

// History is the conversation as the model sees it. It belongs to the chat, not to the model being talked to,
// so changing the model keeps it.
type History struct {
	Msgs []Msg
	Max  int // character budget of what is sent (0: 60000)
	// trimmed counts the exchanges dropped from the front of Msgs to keep the memory of a very long session bounded.
	trimmed int
}

// maxStoredTurns bounds what is kept at all (Window only sends what fits the budget, so older turns are dead weight).
const maxStoredTurns = 400

// Add records a finished exchange.
func (h *History) Add(user, reply string) {
	h.Msgs = append(h.Msgs, Msg{"user", user}, Msg{"assistant", reply})
	if over := len(h.Msgs)/2 - maxStoredTurns; over > 0 {
		h.Msgs = append([]Msg(nil), h.Msgs[over*2:]...) // a copy: the old backing array must not stay alive
		h.trimmed += over
	}
}

// Clear forgets the conversation.
func (h *History) Clear() { h.Msgs, h.trimmed = nil, 0 }

// Turns is the number of exchanges kept.
func (h *History) Turns() int { return len(h.Msgs)/2 + h.trimmed }

// Window is the part of the history that is sent: the newest exchanges that fit the budget (never half an exchange).
// dropped says how many older exchanges were left out.
func (h *History) Window() (msgs []Msg, dropped int) {
	budget := h.Max
	if budget <= 0 {
		budget = 60000
	}
	used, start := 0, len(h.Msgs)
	for i := len(h.Msgs) - 2; i >= 0; i -= 2 {
		c := len(h.Msgs[i].Content) + len(h.Msgs[i+1].Content)
		if used+c > budget && start < len(h.Msgs) {
			break
		}
		used += c
		start = i
	}
	return append([]Msg(nil), h.Msgs[start:]...), start/2 + h.trimmed
}
