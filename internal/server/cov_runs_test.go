package server

import (
	"encoding/json"
	"testing"

	"github.com/tevfik/fleet-router/internal/config"
	"github.com/tevfik/fleet-router/internal/plan"
)

func TestCovRunsAPIAndMethods(t *testing.T) {
	flEnv(t)
	cfg := &config.Config{
		Name:   "node1",
		Listen: "127.0.0.1:4711",
		Endpoints: []config.Endpoint{
			{Name: "ep1", Node: "node1", URL: "http://127.0.0.1:8000/v1", Model: "model-x"},
		},
	}
	s, _ := flFleet(t, cfg, nil)

	// 1. Run unit methods: Active, MeasuredTPS, Nodes
	r := Run{
		ID:    "run-test-1",
		Parts: []RunPart{{Node: "node1"}, {Node: "node2"}},
		Samples: []RunSample{
			{TPS: 10.0},
			{TPS: 20.0},
			{TPS: 0.0}, // ignored in mean
		},
	}
	if !r.Active() {
		t.Fatal("run without Ended should be active")
	}
	if tps := r.MeasuredTPS(); tps != 15.0 {
		t.Fatalf("expected TPS 15.0, got %f", tps)
	}
	emptyRun := Run{}
	if tps := emptyRun.MeasuredTPS(); tps != 0.0 {
		t.Fatalf("expected 0 TPS for empty run, got %f", tps)
	}
	nodes := r.Nodes()
	if len(nodes) != 2 || nodes[0] != "node1" || nodes[1] != "node2" {
		t.Fatalf("unexpected nodes: %v", nodes)
	}

	// 2. GET /fleet/runs
	rec := rtDo(t, s, "GET", "/fleet/runs", nil)
	if rec.Code != 200 {
		t.Fatalf("GET /fleet/runs got %d", rec.Code)
	}

	// 3. POST /fleet/runs: bad JSON
	rec = rtDo(t, s, "POST", "/fleet/runs", "{bad")
	if rec.Code != 400 {
		t.Fatalf("expected 400 on bad json, got %d", rec.Code)
	}

	// POST /fleet/runs: record a run
	planRes := plan.Result{
		Model:  "model-x",
		SHA256: "hash123",
		Candidates: []plan.Candidate{
			{Master: "node1", TPS: 25.0, Parts: []plan.Part{{Node: "node1", Layers: 30}}},
		},
	}
	rec = rtDo(t, s, "POST", "/fleet/runs", map[string]any{
		"plan":     planRes,
		"pick":     1,
		"endpoint": "ep1",
	})
	if rec.Code != 200 {
		t.Fatalf("POST /fleet/runs got %d: %s", rec.Code, rec.Body.String())
	}
	var createdRun Run
	json.NewDecoder(rec.Body).Decode(&createdRun)
	if createdRun.ID == "" {
		t.Fatal("created run has no ID")
	}

	// 4. POST /fleet/runs/deploy: bad JSON
	rec = rtDo(t, s, "POST", "/fleet/runs/deploy", map[string]any{"id": ""})
	if rec.Code != 400 {
		t.Fatalf("expected 400 on empty id, got %d", rec.Code)
	}

	// Deploy notes: starting, running, stopped, failed
	deployNote := DeployNote{
		ID:       "deploy-run-100",
		Status:   "running",
		Model:    "model-x",
		Endpoint: "ep1",
		Master:   "node1",
		Nodes:    []string{"node1"},
		Ctx:      2048,
		Np:       1,
		TPS:      30.5,
	}
	rec = rtDo(t, s, "POST", "/fleet/runs/deploy", deployNote)
	if rec.Code != 200 {
		t.Fatalf("POST /fleet/runs/deploy got %d: %s", rec.Code, rec.Body.String())
	}

	// Failed deploy note
	deployNote.Status = "failed"
	deployNote.Error = "engine crash"
	rec = rtDo(t, s, "POST", "/fleet/runs/deploy", deployNote)
	if rec.Code != 200 {
		t.Fatalf("POST /fleet/runs/deploy failed status got %d", rec.Code)
	}

	// 5. POST /fleet/runs/set: bad JSON or missing ID
	rec = rtDo(t, s, "POST", "/fleet/runs/set", map[string]any{"id": ""})
	if rec.Code != 400 {
		t.Fatalf("expected 400 on empty id, got %d", rec.Code)
	}

	// Nonexistent run -> 404
	nonexistentID := "nonexistent-run-id"
	rec = rtDo(t, s, "POST", "/fleet/runs/set", map[string]any{"id": nonexistentID})
	if rec.Code != 404 {
		t.Fatalf("expected 404 on nonexistent run, got %d", rec.Code)
	}

	// Bind endpoint & end run
	epName := "ep1"
	rec = rtDo(t, s, "POST", "/fleet/runs/set", map[string]any{
		"id":             createdRun.ID,
		"endpoint":       &epName,
		"end":            true,
		"keep_processes": true,
	})
	if rec.Code != 200 {
		t.Fatalf("POST /fleet/runs/set got %d: %s", rec.Code, rec.Body.String())
	}

	// 6. POST /fleet/runs/delete
	// Missing id and prune/stale -> 400
	rec = rtDo(t, s, "POST", "/fleet/runs/delete", map[string]any{})
	if rec.Code != 400 {
		t.Fatalf("expected 400 when neither id nor prune/stale is given, got %d", rec.Code)
	}

	// Delete by ID (not found) -> 404
	rec = rtDo(t, s, "POST", "/fleet/runs/delete", map[string]any{"id": "does-not-exist"})
	if rec.Code != 404 {
		t.Fatalf("expected 404 for deleting unknown run, got %d", rec.Code)
	}

	// Delete by ID (exists)
	rec = rtDo(t, s, "POST", "/fleet/runs/delete", map[string]any{"id": createdRun.ID})
	if rec.Code != 200 {
		t.Fatalf("POST /fleet/runs/delete got %d", rec.Code)
	}

	// Prune with dry_run
	rec = rtDo(t, s, "POST", "/fleet/runs/delete", map[string]any{"prune": true, "dry_run": true})
	if rec.Code != 200 {
		t.Fatalf("prune dry_run got %d", rec.Code)
	}

	// Stale with dry_run
	rec = rtDo(t, s, "POST", "/fleet/runs/delete", map[string]any{"stale": true, "dry_run": true})
	if rec.Code != 200 {
		t.Fatalf("stale dry_run got %d", rec.Code)
	}

	// 7. Drain API
	if s.Draining() {
		t.Fatal("should not be draining initially")
	}
	rec = rtDo(t, s, "POST", "/fleet/drain", map[string]any{"on": true})
	if rec.Code != 200 {
		t.Fatalf("POST /fleet/drain on got %d", rec.Code)
	}
	if !s.Draining() {
		t.Fatal("should be draining after setting drain: true")
	}
	rec = rtDo(t, s, "POST", "/fleet/drain", map[string]any{"on": false})
	if rec.Code != 200 {
		t.Fatalf("POST /fleet/drain off got %d", rec.Code)
	}
	if s.Draining() {
		t.Fatal("should not be draining after setting drain: false")
	}
}
