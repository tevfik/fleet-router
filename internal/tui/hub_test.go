package tui

import (
	"strings"
	"testing"

	"github.com/tevfik/fleet-router/internal/config"
	"github.com/tevfik/fleet-router/internal/server"
)

func TestTUI_Hub_ViewAndKeys(t *testing.T) {
	cfg := config.Config{
		Name: "papa",
		Endpoints: []config.Endpoint{
			{Name: "papa-test", Node: "papa", Model: "test-model"},
		},
	}
	m := model{
		sc:            scHub,
		cfg:           cfg,
		w:             120,
		hubURL:        "https://hub.example.com",
		hubShowModels: true,
		hubRemoteModels: []hubModelInfo{
			{ID: "papa/test-model", OwnedBy: "papa", Caps: []string{"chat"}, Up: true},
		},
	}

	view := m.viewHub()
	if !strings.Contains(view, "FLEET HUB & REVERSE TUNNEL STATUS") {
		t.Fatalf("expected hub status header, got: %s", view)
	}
	if !strings.Contains(view, "https://hub.example.com") {
		t.Fatalf("expected hub url, got: %s", view)
	}
	if !strings.Contains(view, "MODELS EXPORTED TO HUB") {
		t.Fatalf("expected exported models section, got: %s", view)
	}
	if !strings.Contains(view, "REMOTE MODELS ON FLEET HUB") {
		t.Fatalf("expected remote models section, got: %s", view)
	}
	if !strings.Contains(view, "papa") || !strings.Contains(view, "Daemon port 4711") {
		t.Fatalf("expected local node name in status, got: %s", view)
	}

	// Test fallback node name when cfg.Name is empty
	mEmptyName := model{
		sc: scHub,
		st: server.Status{
			Nodes: []server.NodeStatus{{Node: "fallback-node", Self: true}},
		},
	}
	if got := mEmptyName.localNodeName(); got != "fallback-node" {
		t.Fatalf("expected fallback-node, got: %s", got)
	}

	// the models view is toggled from the list (cursor on "Toggle Models View", enter)
	m.cur = 4
	m2, _ := m.keyHub("enter")
	mod := m2.(model)
	if mod.hubShowModels {
		t.Fatal("expected hubShowModels to be false after toggle")
	}

	// Test key refresh shortcut
	m3, cmd := m.keyHub("r")
	mod3 := m3.(model)
	if !mod3.hubShowModels || !mod3.hubRemoteBusy || cmd == nil {
		t.Fatal("expected hubShowModels=true, hubRemoteBusy=true and cmd != nil on 'r'")
	}
}

func TestTUI_Hub_APIKeyDistinctFromToken(t *testing.T) {
	m := model{sc: scHub, w: 120, hubURL: "https://h", cfg: config.Config{HubToken: "tunneltok", HubAPIKey: "sk-apikey"}}
	v := m.viewHub()
	if !strings.Contains(v, "Set Auth Token") || !strings.Contains(v, "Set API Key") {
		t.Fatalf("both actions expected: %s", v)
	}
	// editing the API key must not touch the join token, and vice versa
	m.cur = 3
	m2, _ := m.keyHub("enter")
	mm := m2.(model)
	if mm.editing != "hubapikey" || mm.editBuf != "" {
		t.Fatalf("api key edit not started blank: %q %q", mm.editing, mm.editBuf)
	}
	m.cur = 2
	m3, _ := m.keyHub("enter")
	if mt := m3.(model); mt.editing != "hubtoken" {
		t.Fatalf("enter on Set Auth Token should edit the join token, got %q", mt.editing)
	}
	mm.editBuf = "newkey"
	r, _ := mm.finishEdit()
	rm := r.(model)
	if rm.cfg.HubAPIKey != "newkey" || rm.cfg.HubToken != "tunneltok" || rm.hubToken != "" {
		t.Fatalf("api key edit leaked: %+v %q", rm.cfg, rm.hubToken)
	}
	if m.hubAPIKeyVal() != "sk-apikey" {
		t.Fatal("api key should come from cfg")
	}
}

// The Hub is a list: its help line names only enter/↑↓/esc and the few letters that are kept (c k r), the
// rows of those say so, and no other letter does anything.
func TestTUI_Hub_HelpLineIsShortAndMatchesTheKeys(t *testing.T) {
	m := model{sc: scHub, w: 120, hubURL: "https://h"}
	help := m.help()
	for _, want := range []string{"↑/↓ select", "enter run", "c connect/disconnect", "k API key", "r refresh models", "esc back"} {
		if !strings.Contains(help, want) {
			t.Errorf("help lacks %q: %s", want, help)
		}
	}
	if n := strings.Count(help, " • "); n > 6 {
		t.Errorf("help line too long (%d items): %s", n+1, help)
	}
	v := m.viewHub()
	for _, want := range []string{"Connect to Fleet Hub ] (c)", "(k)", "Refresh Models from Hub ] (r)"} {
		if !strings.Contains(v, want) {
			t.Errorf("list row lacks %q", want)
		}
	}
	for _, k := range strings.Split("abdefghijlmnopqstuvwxyz", "") {
		nm, cmd := m.keyHub(k)
		if cmd != nil || nm.(model).editing != "" || nm.(model).hubShowModels != m.hubShowModels {
			t.Errorf("letter %q still does something on the Hub screen", k)
		}
	}
	for _, k := range []string{"k"} {
		if nm, _ := m.keyHub(k); nm.(model).editing != "hubapikey" {
			t.Errorf("%q should edit the API key", k)
		}
	}
	if nm, cmd := m.keyHub("r"); !nm.(model).hubRemoteBusy || cmd == nil {
		t.Error("r refreshes the models")
	}
}
