package tui

import (
	"fmt"
	"math/rand"
	"runtime/debug"
	"strings"
	"testing"

	"github.com/tevfik/fleet-router/internal/client"
	"github.com/tevfik/fleet-router/internal/config"
	"github.com/tevfik/fleet-router/internal/server"
)

// fuzzModels are screens' starting states: odd role shapes must never crash the menu.
func fuzzModels() map[string]model {
	eps := []server.EpStatus{
		{Endpoint: config.Endpoint{Name: "a", Node: "dino", Model: "gemma", Slots: 2, Tags: []string{"fast"}}, Known: true, OK: true},
		{Endpoint: config.Endpoint{Name: "b", Node: "victor", Model: "bonsai"}, Known: true},
		{Endpoint: config.Endpoint{Name: "e", Node: "dino", Model: "bge", Caps: []string{"embed"}}, Known: true, OK: true},
	}
	mk := func(roles map[string]server.RoleStatus, eps []server.EpStatus) model {
		m := model{w: 100, h: 30, sel: map[int]bool{}, cl: client.New("127.0.0.1:1")}
		m.st = server.Status{Endpoints: eps, Roles: roles}
		m.reqlog = []server.LogEntry{{Model: "scout", Endpoint: "a", Status: 200}, {Model: "x", Status: 404, Err: "nope"}}
		m.usage = []server.UsageRow{{Key: "opencode", Requests: 3}}
		m.bWeek = m.usage
		m.bData = []server.UsageRow{{Key: "scout", Sub: "dino/gemma", Requests: 2, In: 5}, {Key: "dino/gemma", Requests: 1}}
		m.capName = "a"
		return m
	}
	rs := func(r config.Role, active string) server.RoleStatus {
		return server.RoleStatus{Role: r, Active: active}
	}
	return map[string]model{
		"no roles":     mk(map[string]server.RoleStatus{}, eps),
		"no endpoints": mk(map[string]server.RoleStatus{"scout": rs(config.Role{Preferred: "a"}, "a")}, nil),
		"normal": mk(map[string]server.RoleStatus{
			"scout":     rs(config.Role{Preferred: "a", Fallback: []string{"b"}}, "a"),
			"architect": rs(config.Role{Preferred: "b"}, "b"),
		}, eps),
		"match only":   mk(map[string]server.RoleStatus{"pool": rs(config.Role{Match: []string{"tag:fast"}, Strategy: "fastest"}, "a")}, eps),
		"unknown refs": mk(map[string]server.RoleStatus{"scout": rs(config.Role{Preferred: "gone", Fallback: []string{"gone2", "a"}}, "")}, eps),
		"empty active": mk(map[string]server.RoleStatus{"scout": rs(config.Role{Preferred: "a"}, "")}, eps),
	}
}

func TestFuzzRolesScreens(t *testing.T) {
	keys := []string{"up", "down", "enter", "esc", "p", "f", "x", "t", "[", "]", "a", "-", "+", "<", ">", "o", "s", "tab", "left", "right", " ", "j", "k", "q",
		"1", "2", "3", "4", "n", "g", "b", "B", "c", "y", "w", "i", "d", "L", "/", "backspace", "h", "l", "e"}
	for name, base := range fuzzModels() {
		for _, start := range []screen{scHome, scDash, scRoles, scRoleEdit, scDiscover, scBoard, scInteg, scCaps, scActivity, scUsage, scNodes} {
			for seed := int64(0); seed < 8; seed++ {
				rng := rand.New(rand.NewSource(seed))
				m := base
				m.sc = start
				var trail []string
				func() {
					defer func() {
						if r := recover(); r != nil {
							t.Fatalf("%s / screen %d: panic %v after keys %s\n%s", name, start, r, strings.Join(trail, " "), debug.Stack())
						}
					}()
					m.View()
					for i := 0; i < 30; i++ {
						k := keys[rng.Intn(len(keys))]
						trail = append(trail, k)
						nm, _ := m.keyStr(k, k)
						m = nm.(model)
						m.View()
					}
				}()
			}
		}
	}
	_ = fmt.Sprint
}

// An "enter" button on the focused bar used to press itself forever (stack overflow = the menu died).
func TestEnterButtonDoesNotRecurse(t *testing.T) {
	m := fuzzModels()["normal"]
	m.sc, m.inBar, m.barCur = scRoles, true, 0 // [Edit role] is the first button and its key is "enter"
	if got := m.buttons()[0].key; got != "enter" {
		t.Fatalf("first Roles button key = %q, the test needs an enter button", got)
	}
	nm, _ := m.keyStr("enter", "enter")
	if nm.(model).sc != scRoleEdit {
		t.Fatalf("pressing [Edit role] should open the role editor, screen = %d", nm.(model).sc)
	}
}
