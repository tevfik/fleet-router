package chatpick

import (
	"fmt"
	"path/filepath"
	"sort"
	"strings"

	"github.com/tevfik/fleet-router/internal/board"
	"github.com/tevfik/fleet-router/internal/client"
	"github.com/tevfik/fleet-router/internal/config"
	"github.com/tevfik/fleet-router/internal/server"
	"github.com/tevfik/fleet-router/internal/store"
)

// Action is one model-management step offered for a picker entry. Needs says what the step still asks for:
// "" nothing, "role", "endpoint", "node" (a machine), "tags" (free text), "path" (a model file on a machine).
type Action struct {
	ID    string
	Label string
	Needs string
}

// Actions lists what can be done to an entry, using only what the daemon and the CLI offer
// (fleet probe / bench / role set|slots|tag|match / store / run-on).
func Actions(e Entry) []Action {
	if e.Kind == Role {
		return []Action{
			{"test", "Test the role (ping every member)", ""},
			{"route", "Where would a request go now?", ""},
			{"setpref", "Set its preferred endpoint…", "endpoint"},
			{"addfb", "Add a fallback endpoint…", "endpoint"},
			{"clearfb", "Clear its fallbacks", ""},
		}
	}
	return []Action{
		{"probe", "Probe capabilities", ""},
		{"bench", "Benchmark speed", ""},
		{"slotsup", "Slots +1", ""},
		{"slotsdown", "Slots −1", ""},
		{"tags", "Edit tags…", "tags"},
		{"bindpref", "Make it a role's preferred endpoint…", "role"},
		{"bindfb", "Add it as a role's fallback…", "role"},
		{"where", "Model file: which machines have it", ""},
		{"pull", "Model file: fetch to a machine…", "node"},
		{"forget", "Model file: forget it in the store (file stays)", ""},
		{"start", "Start a model on a machine (run-on)…", "node"},
		{"stop", "Stop its processes (master, streams, workers)", ""},
		{"remove", "Remove the endpoint from the configuration", ""},
	}
}

// API is what the actions need from the daemon (the client satisfies it; tests use a fake).
type API interface {
	Probe(names []string) error
	Bench(names []string) error
	SetSlots(endpoint string, slots int) error
	SetTags(endpoint string, tags []string) error
	SetRole(role, preferred string, fallback []string) error
	StoreAll() ([]server.NodeStore, error)
	StorePull(node, from, sha string) error
	StoreRemove(ref string) (store.Entry, error)
	TestRole(role string, members []string) []client.TestResult
	Route(model string, in, out int) (server.RouteExplain, error)
	StopEndpoint(name string, opt server.StopOptions) (server.StopReport, error)
	EndpointsNotes(add []config.Endpoint, remove []string, mendRoles bool) ([]string, error)
}

// RunOn starts a model file on a machine (deploy.RunOnNode); dry only says what would run; register also records
// the started server as an endpoint.
type RunOn func(node, path string, dry, register bool) (string, error)

// Choices lists what a step's argument can be.
func Choices(st server.Status, needs string) []string {
	var out []string
	switch needs {
	case "role":
		for r := range st.Roles {
			out = append(out, r)
		}
	case "endpoint":
		for _, e := range Entries(st, "") {
			if e.Kind != Role {
				out = append(out, e.Name)
			}
		}
		return out
	case "node":
		for _, n := range st.Nodes {
			out = append(out, n.Node)
		}
	}
	sort.Strings(out)
	return out
}

// RebindPreferred makes ep the preferred member of role r; the old preferred one stays as the first fallback.
func RebindPreferred(r config.Role, ep string) (string, []string) {
	var fb []string
	if r.Preferred != "" && r.Preferred != ep {
		fb = append(fb, r.Preferred)
	}
	for _, f := range r.Fallback {
		if f != ep && f != r.Preferred {
			fb = append(fb, f)
		}
	}
	return ep, fb
}

// AddFallback appends ep to the role's fallbacks (no-op when it is already a member).
func AddFallback(r config.Role, ep string) (string, []string, bool) {
	if r.Preferred == ep {
		return r.Preferred, r.Fallback, false
	}
	for _, f := range r.Fallback {
		if f == ep {
			return r.Preferred, r.Fallback, false
		}
	}
	return r.Preferred, append(append([]string{}, r.Fallback...), ep), true
}

// FilesFor finds the model files of the store that an endpoint serves, by name (the endpoint's model is matched
// to a file name with or without ".gguf", either way round).
func FilesFor(nodes []server.NodeStore, ep Entry) []board.StoreRow {
	want := norm(ep.Model)
	if want == "" {
		want = norm(ep.Name)
	}
	var out []board.StoreRow
	for _, r := range board.StoreRows(nodes) {
		n := norm(r.Name)
		if n != "" && want != "" && (strings.Contains(want, n) || strings.Contains(n, want)) {
			out = append(out, r)
		}
	}
	return out
}

func norm(s string) string {
	s = strings.ToLower(filepath.Base(strings.TrimSpace(s)))
	return strings.TrimSuffix(s, ".gguf")
}

// WhereLines says, per matching file, which machines have it and which do not.
func WhereLines(nodes []server.NodeStore, rows []board.StoreRow) []string {
	var out []string
	for _, r := range rows {
		var has, lacks []string
		for _, n := range nodes {
			switch {
			case n.Store == nil:
			case r.Has[n.Node]:
				has = append(has, n.Node)
			default:
				lacks = append(lacks, n.Node)
			}
		}
		l := fmt.Sprintf("%s (%s, sha %s): on %s", r.Name, board.Bytes(r.Size), r.SHA256[:min(8, len(r.SHA256))], orNone(has))
		if len(lacks) > 0 {
			l += "; not on " + strings.Join(lacks, ", ")
		}
		out = append(out, l)
	}
	return out
}

func orNone(l []string) string {
	if len(l) == 0 {
		return "no machine"
	}
	return strings.Join(l, ", ")
}

// Do runs action a on entry e with the step's argument arg (Choices / free text) and returns what to tell the
// user. st is the latest status (for roles); run is only used by "start".
func Do(api API, st server.Status, run RunOn, a Action, e Entry, arg string) (string, error) {
	if a.Needs != "" && strings.TrimSpace(arg) == "" && a.ID != "tags" {
		return "", fmt.Errorf("%s needs a %s", a.Label, a.Needs)
	}
	switch a.ID {
	case "probe":
		if err := api.Probe([]string{e.Name}); err != nil {
			return "", err
		}
		return "probe started for " + e.Name + " — its capabilities show in the status once it ends", nil
	case "bench":
		if err := api.Bench([]string{e.Name}); err != nil {
			return "", err
		}
		return "benchmark started for " + e.Name + " — the speed in the header updates when it ends", nil
	case "slotsup", "slotsdown":
		cur := 0
		for _, ep := range st.Endpoints {
			if ep.Name == e.Name {
				cur = ep.Slots
			}
		}
		n := cur + 1
		if a.ID == "slotsdown" {
			n = max(0, cur-1)
		}
		if err := api.SetSlots(e.Name, n); err != nil {
			return "", err
		}
		return fmt.Sprintf("%s slots %d → %d (0 = unlimited)", e.Name, cur, n), nil
	case "tags":
		tags := strings.Fields(strings.ReplaceAll(arg, ",", " "))
		if err := api.SetTags(e.Name, tags); err != nil {
			return "", err
		}
		if len(tags) == 0 {
			return e.Name + ": tags cleared", nil
		}
		return e.Name + ": tags = " + strings.Join(tags, " "), nil
	case "bindpref", "bindfb":
		r, ok := st.Roles[arg]
		if !ok {
			return "", fmt.Errorf("no such role: %s", arg)
		}
		if a.ID == "bindpref" {
			p, fb := RebindPreferred(r.Role, e.Name)
			if err := api.SetRole(arg, p, fb); err != nil {
				return "", err
			}
			return fmt.Sprintf("role %s: preferred %s, fallbacks %v", arg, p, fb), nil
		}
		p, fb, changed := AddFallback(r.Role, e.Name)
		if !changed {
			return fmt.Sprintf("%s is already in role %s", e.Name, arg), nil
		}
		if err := api.SetRole(arg, p, fb); err != nil {
			return "", err
		}
		return fmt.Sprintf("role %s: preferred %s, fallbacks %v", arg, p, fb), nil
	case "setpref":
		p, fb := RebindPreferred(st.Roles[e.Name].Role, arg)
		if err := api.SetRole(e.Name, p, fb); err != nil {
			return "", err
		}
		return fmt.Sprintf("role %s: preferred %s, fallbacks %v", e.Name, p, fb), nil
	case "addfb":
		r := st.Roles[e.Name]
		p, fb, changed := AddFallback(r.Role, arg)
		if !changed {
			return fmt.Sprintf("%s is already in role %s", arg, e.Name), nil
		}
		if err := api.SetRole(e.Name, p, fb); err != nil {
			return "", err
		}
		return fmt.Sprintf("role %s: preferred %s, fallbacks %v", e.Name, p, fb), nil
	case "clearfb":
		r := st.Roles[e.Name]
		if err := api.SetRole(e.Name, r.Preferred, nil); err != nil {
			return "", err
		}
		return "role " + e.Name + ": fallbacks cleared", nil
	case "test":
		var members []string
		r := st.Roles[e.Name]
		members = append(members, r.Preferred)
		members = append(members, r.Fallback...)
		return strings.Join(client.FormatTest(api.TestRole(e.Name, members)), "\n"), nil
	case "route":
		x, err := api.Route(e.Name, 0, 0)
		if err != nil {
			return "", err
		}
		if len(x.Order) == 0 {
			return "a request for " + e.Name + " has no endpoint to go to", nil
		}
		var order []string
		for _, c := range x.Order {
			order = append(order, c.Endpoint)
		}
		return fmt.Sprintf("a request for %s goes to %s (order tried: %s)", e.Name, order[0], strings.Join(order, " → ")), nil
	case "where", "pull", "forget":
		nodes, err := api.StoreAll()
		if err != nil {
			return "", err
		}
		rows := FilesFor(nodes, e)
		if len(rows) == 0 {
			return "", fmt.Errorf("no model file in the store matches %s (model %q) — fleet store add <file> registers one", e.Name, e.Model)
		}
		switch a.ID {
		case "where":
			return strings.Join(WhereLines(nodes, rows), "\n"), nil
		case "pull":
			if len(rows) > 1 {
				return "", fmt.Errorf("%d files match %s (%s) — use `fleet store pull <sha> --node %s`", len(rows), e.Name, rows[0].Name, arg)
			}
			if rows[0].Has[arg] {
				return arg + " already has " + rows[0].Name, nil
			}
			if err := api.StorePull(arg, "", rows[0].SHA256); err != nil {
				return "", err
			}
			return fmt.Sprintf("%s is fetching %s in the background (progress: fleet store status)", arg, rows[0].Name), nil
		default:
			if len(rows) > 1 {
				return "", fmt.Errorf("%d files match %s — use `fleet store rm <sha>`", len(rows), e.Name)
			}
			ent, err := api.StoreRemove(rows[0].SHA256)
			if err != nil {
				return "", fmt.Errorf("%v (only this machine's files can be forgotten here)", err)
			}
			return "forgot " + ent.Name + " — the file itself is untouched", nil
		}
	case "start":
		if run == nil {
			return "", fmt.Errorf("starting models is not available here")
		}
		node, rest, _ := strings.Cut(arg, "\x00")
		path, reg, _ := strings.Cut(rest, "\x00")
		if strings.TrimSpace(path) == "" {
			return "", fmt.Errorf("give the model file path on %s", node)
		}
		return run(node, path, false, reg == "register")
	case "stop":
		rep, err := api.StopEndpoint(e.Name, server.StopOptions{Force: arg == "force", MendRoles: false})
		if err != nil {
			return "", err
		}
		out := strings.Join(board.StopLines(rep), "\n")
		if rep.Failed() {
			return "", fmt.Errorf("%s", out)
		}
		return out, nil
	case "remove":
		notes, err := api.EndpointsNotes(nil, []string{e.Name}, arg == "mend")
		if err != nil {
			return "", err
		}
		return strings.Join(append([]string{"removed " + e.Name + " from the configuration (no process was stopped; fleet config undo restores)"}, notes...), "\n"), nil
	}
	return "", fmt.Errorf("unknown action %q", a.ID)
}
