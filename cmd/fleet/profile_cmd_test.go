package main

import (
	"bytes"
	"context"
	"encoding/binary"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/tevfik/fleet-router/internal/client"
	"github.com/tevfik/fleet-router/internal/config"
	"github.com/tevfik/fleet-router/internal/deploy"
	"github.com/tevfik/fleet-router/internal/plan"
	"github.com/tevfik/fleet-router/internal/profile"
	"github.com/tevfik/fleet-router/internal/server"
	"github.com/tevfik/fleet-router/internal/setup"
)

// tinyGGUF writes a dense GGUF of the given architecture and block count (one small tensor per block).
func tinyGGUF(t *testing.T, dir, arch string, blocks int) string {
	t.Helper()
	var w bytes.Buffer
	u32 := func(v uint32) { binary.Write(&w, binary.LittleEndian, v) }
	u64 := func(v uint64) { binary.Write(&w, binary.LittleEndian, v) }
	str := func(s string) { u64(uint64(len(s))); w.WriteString(s) }
	w.WriteString("GGUF")
	u32(3)
	u64(uint64(blocks))
	u64(3)
	str("general.architecture")
	u32(8)
	str(arch)
	str(arch + ".block_count")
	u32(4)
	u32(uint32(blocks))
	str(arch + ".embedding_length")
	u32(4)
	u32(64)
	for i := 0; i < blocks; i++ {
		str(fmt.Sprintf("blk.%d.attn_q.weight", i))
		u32(2)
		u64(8)
		u64(8)
		u32(0)
		u64(uint64(i * 256))
	}
	for w.Len()%32 != 0 {
		w.WriteByte(0)
	}
	w.Write(make([]byte, blocks*256))
	p := filepath.Join(dir, "tiny.gguf")
	if err := os.WriteFile(p, w.Bytes(), 0o644); err != nil {
		t.Fatal(err)
	}
	return p
}

// profileEnv isolates the configuration and the profiles in a temporary directory; no daemon, no split guard.
func profileEnv(t *testing.T) (string, profile.Store) {
	t.Helper()
	dir := t.TempDir()
	t.Setenv("FLEET_CONFIG", filepath.Join(dir, "config.yaml"))
	t.Setenv("FLEET_PROFILES", filepath.Join(dir, "profiles"))
	t.Setenv("FLEET_LISTEN", "127.0.0.1:1") // nothing listens: no daemon
	old := splitPlanFn
	splitPlanFn = func(string, string, int, int) (plan.Result, error) {
		return plan.Result{}, fmt.Errorf("no daemon in tests")
	}
	oldOut := deployOut
	deployOut = io.Discard
	t.Cleanup(func() { splitPlanFn, deployOut = old, oldOut })
	return dir, profile.Default()
}

// The workers are IPs on loopback: nothing is resolved over ssh, nothing is started (dry runs).
var testFlags = []string{"--workers", "127.0.0.2", "--ts", "20,4", "--via-hub-node", "127.0.0.2", "--hub", "https://hub.example",
	"--spec", "draft-dflash", "--draft-model", "/m/draft.gguf", "--draft-max", "2", "--ctx", "8192", "--cache-type-k", "q8_0",
	"--io-timeout", "120", "--exit-on-fail", "--force", "--master-env", "GGML_X=1", "--worker-env", "127.0.0.2:CUDA_VISIBLE_DEVICES=0",
	"--master-arg", "--temp 0", "--worker-cpu-experts", "127.0.0.2=21-23"}

func TestProfileCommands(t *testing.T) {
	dir, store := profileEnv(t)
	model := tinyGGUF(t, dir, "llama", 24)
	if err := cmdProfile(append([]string{"save", "hub", model}, testFlags...)); err != nil {
		t.Fatal(err)
	}
	p, err := store.Load("hub")
	if err != nil || p.Model != model || p.Layout.TS != "20,4" || p.Layout.Mode != profile.LayoutFixed || p.Layout.WorkerCPUExperts["127.0.0.2"] != "21-23" ||
		p.Spec.Type != "draft-dflash" || p.Spec.DraftMax != 2 || p.Ctx != 8192 || p.Env.Master["GGML_X"] != "1" ||
		p.Env.Workers["127.0.0.2"]["CUDA_VISIBLE_DEVICES"] != "0" || strings.Join(p.ExtraMasterArgs, " ") != "--temp 0" || !p.ExitOnFail {
		t.Fatalf("%v %+v", err, p)
	}
	if p.Port != 0 || p.Np != 0 {
		t.Errorf("flags that were not given stay out of the profile: %+v", p)
	}
	if err := cmdProfile([]string{"save", "plain", model}); err != nil {
		t.Fatal(err)
	}
	if q, _ := store.Load("plain"); q.Layout.Mode != profile.LayoutAuto {
		t.Errorf("a new profile without a split is auto: %+v", q.Layout)
	}
	if err := cmdProfile([]string{"list"}); err != nil {
		t.Fatal(err)
	}
	if err := cmdProfile([]string{"show", "hub"}); err != nil {
		t.Fatal(err)
	}
	if err := cmdProfile([]string{"edit", "hub", "--set", "ctx=16384", "--set", "layout.ts=19,5"}); err != nil {
		t.Fatal(err)
	}
	if q, _ := store.Load("hub"); q.Ctx != 16384 || q.Layout.TS != "19,5" {
		t.Errorf("%+v", q)
	}
	if err := cmdProfile([]string{"edit", "hub", "--set", "env.master.BAD=a;b"}); err == nil {
		t.Error("an unsafe env value must be refused")
	}
	out := filepath.Join(dir, "hub-export.yaml")
	if err := cmdProfile([]string{"export", "hub", "-o", out}); err != nil {
		t.Fatal(err)
	}
	if err := cmdProfile([]string{"import", out, "--name", "hub2"}); err != nil {
		t.Fatal(err)
	}
	if err := cmdProfile([]string{"rm", "hub"}); err != nil {
		t.Fatal(err)
	}
	list, _, _ := store.List()
	if len(list) != 2 || list[0].Name != "hub2" || list[1].Name != "plain" {
		t.Fatalf("%v", list)
	}
	if err := cmdProfile([]string{"save", "../x", model}); err == nil {
		t.Error("bad name refused")
	}
}

func TestDeployProfileOverridePrecedence(t *testing.T) {
	dir, store := profileEnv(t)
	model := tinyGGUF(t, dir, "llama", 24)
	if err := cmdProfile(append([]string{"save", "hub", model}, testFlags...)); err != nil {
		t.Fatal(err)
	}
	req, err := parseDeploy([]string{"--profile", "hub", "--ctx", "2048", "--spec", "none", "--master-env", "GGML_Y=2"}, store)
	if err != nil {
		t.Fatal(err)
	}
	o := req.opts
	if o.Ctx != 2048 || o.SpecType != "none" || o.TensorSplit != "20,4" || o.ModelPath != model || o.DraftMax != 2 || o.IOTimeoutS != 120 ||
		o.MasterEnvExtra["GGML_X"] != "1" || o.MasterEnvExtra["GGML_Y"] != "2" || req.mode != profile.LayoutFixed {
		t.Fatalf("explicit flags override, the rest comes from the profile: %+v", o)
	}
	// a positional model overrides the profile's, --layout auto drops the pinned split
	req, err = parseDeploy([]string{"/other.gguf", "--profile", "hub", "--layout", "auto"}, store)
	if err != nil || req.model != "/other.gguf" || req.mode != profile.LayoutAuto || req.opts.TensorSplit != "" || len(req.opts.WorkerCPUExperts) != 0 {
		t.Fatalf("%v %+v", err, req)
	}
	// an explicit --ts on an auto profile makes it fixed
	if err := cmdProfile([]string{"save", "auto", model, "--workers", "127.0.0.2"}); err != nil {
		t.Fatal(err)
	}
	if req, _ := parseDeploy([]string{"--profile", "auto", "--ts", "12,12"}, store); req.mode != profile.LayoutFixed {
		t.Errorf("--ts pins the split: %s", req.mode)
	}
	if _, err := parseDeploy([]string{"--profile", "missing"}, store); err == nil {
		t.Error("a missing profile is an error")
	}
}

// The plan of `fleet deploy --profile X --dry-run` is the plan of the same flags.
func TestDryRunFromProfileEqualsFlags(t *testing.T) {
	dir, store := profileEnv(t)
	model := tinyGGUF(t, dir, "llama", 24)
	if err := cmdProfile(append([]string{"save", "hub", model}, testFlags...)); err != nil {
		t.Fatal(err)
	}
	a, err := parseDeploy(append(append([]string{model}, testFlags...), "--dry-run"), store)
	if err != nil {
		t.Fatal(err)
	}
	b, err := parseDeploy([]string{"--profile", "hub", "--dry-run"}, store)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(a.opts, b.opts) {
		t.Fatalf("options differ:\nflags   %+v\nprofile %+v", a.opts, b.opts)
	}
	cfg := config.Default()
	ra, err := deploy.PlanDeployment(cfg, a.opts)
	if err != nil {
		t.Fatal(err)
	}
	rb, _ := deploy.PlanDeployment(cfg, b.opts)
	if ra.MasterCmd != rb.MasterCmd || ra.MasterEnv != rb.MasterEnv || !reflect.DeepEqual(ra.WorkerCmds, rb.WorkerCmds) || !reflect.DeepEqual(ra.StreamCmds, rb.StreamCmds) {
		t.Fatalf("plans differ:\n%s\n%s", ra.MasterCmd, rb.MasterCmd)
	}
	if !strings.Contains(ra.MasterCmd, "-ts 20,4,0") || !strings.Contains(ra.MasterCmd, "--temp 0") || !strings.Contains(ra.MasterEnv, "GGML_X=1") {
		t.Errorf("the profile's layout and extras reach the plan: %s | %s", ra.MasterEnv, ra.MasterCmd)
	}
}

func TestDeployRecordsRunsAndSavesProfile(t *testing.T) {
	dir, store := profileEnv(t)
	model := tinyGGUF(t, dir, "llama", 24)
	var got deploy.DeployOptions
	old := executeDeployment
	executeDeployment = func(_ context.Context, cfg *config.Config, o deploy.DeployOptions) (*deploy.DeployResult, error) {
		got = o
		res, err := deploy.PlanDeployment(cfg, o)
		if err == nil && !o.DryRun {
			res.Status = "running"
		}
		return res, err
	}
	t.Cleanup(func() { executeDeployment = old })

	// a dry run saves the profile but records no run
	if err := cmdDeploy(append([]string{model, "--save-profile", "x", "--dry-run"}, testFlags...)); err != nil {
		t.Fatal(err)
	}
	p, err := store.Load("x")
	if err != nil || len(p.Runs) != 0 || p.Layout.TS != "20,4" {
		t.Fatalf("%v %+v", err, p)
	}
	// a real deploy of the profile records the concrete layout
	if err := cmdDeploy([]string{"--profile", "x"}); err != nil {
		t.Fatal(err)
	}
	p, _ = store.Load("x")
	if len(p.Runs) != 1 || got.TensorSplit != "20,4" {
		t.Fatalf("%+v", p.Runs)
	}
	r := p.Runs[0]
	if r.Layout.TS != "20,4" || r.Layout.Workers[0] != "127.0.0.2" || r.Layout.CPUExperts["127.0.0.2"] != "21-23" || r.Layout.SpecType != "draft-dflash" || r.Choice != "fixed" || r.URL == "" || r.StatsPort == 0 {
		t.Fatalf("%+v", r)
	}
	if err := cmdProfile([]string{"measure", "x", "--tps", "33.3"}); err != nil {
		t.Fatal(err)
	}
	p, _ = store.Load("x")
	if l := p.MeasuredLine(); !strings.Contains(l, "33.3 t/s") {
		t.Error(l)
	}
}

// fakeDaemon answers /healthz and /fleet/plan with r; the requests are kept.
func fakeDaemon(t *testing.T, r plan.Result) (*client.Client, *[]server.PlanRequest) {
	t.Helper()
	var reqs []server.PlanRequest
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, q *http.Request) {
		switch q.URL.Path {
		case "/healthz":
			w.Write([]byte("ok"))
		case "/fleet/plan":
			var pr server.PlanRequest
			json.NewDecoder(q.Body).Decode(&pr)
			reqs = append(reqs, pr)
			json.NewEncoder(w).Encode(r)
		default:
			http.NotFound(w, q)
		}
	}))
	t.Cleanup(ts.Close)
	return client.New(strings.TrimPrefix(ts.URL, "http://")), &reqs
}

func TestAutoLayoutFromPlanAndMeasurements(t *testing.T) {
	dir, store := profileEnv(t)
	model := tinyGGUF(t, dir, "llama", 48)
	// the planner's candidates (fixtures of Ornith-122B over the hub: GPU-only vs CPU experts on the worker)
	gpu := plan.Candidate{Kind: "split", Master: "127.0.0.1", TPS: 21, TokenMS: 47.6, Parts: []plan.Part{{Node: "127.0.0.1", Layers: 42, Output: true}, {Node: "127.0.0.2", Layers: 6, First: 42}}}
	cpx := plan.Candidate{Kind: "split", Master: "127.0.0.1", TPS: 25, TokenMS: 40, Parts: []plan.Part{{Node: "127.0.0.1", Layers: 34, Output: true}, {Node: "127.0.0.2", Layers: 14, First: 34, CPUExperts: 7}}}
	cl, reqs := fakeDaemon(t, plan.Result{Candidates: []plan.Candidate{cpx, gpu}})
	oldC, oldE := deployClient, executeDeployment
	deployClient = func() (*client.Client, error) { return cl, nil }
	var got deploy.DeployOptions
	executeDeployment = func(_ context.Context, cfg *config.Config, o deploy.DeployOptions) (*deploy.DeployResult, error) {
		got = o
		res, err := deploy.PlanDeployment(cfg, o)
		if err == nil {
			res.Status = "running"
		}
		return res, err
	}
	t.Cleanup(func() { deployClient, executeDeployment = oldC, oldE })

	if err := cmdProfile([]string{"save", "o", model, "--master", "127.0.0.1", "--workers", "127.0.0.2", "--via-hub", "--hub", "https://hub.example", "--hub-rtt-ms", "25", "--spec", "none", "--cache-type-k", "q8_0", "--cache-type-v", "q4_0", "--cpu-moe", "--moe-cache-mib", "3072"}); err != nil {
		t.Fatal(err)
	}
	if err := cmdDeploy([]string{"--profile", "o"}); err != nil {
		t.Fatal(err)
	}
	pr := (*reqs)[0]
	if pr.RTT["127.0.0.1>127.0.0.2"] != 25 || len(pr.Nodes) != 2 || pr.Master != "127.0.0.1" || pr.CacheTypeK != "q8_0" || pr.CacheTypeV != "q4_0" || !pr.CPUMoE || pr.MoECacheMiB != 3072 {
		t.Errorf("the plan request: hub round trip, cache types, MoE options and the profile's machines: %+v", pr)
	}
	if got.Candidate == nil || got.Candidate.Parts[1].Layers != 6 {
		t.Fatalf("nothing measured: the GPU-only candidate: %+v", got.Candidate)
	}
	// explore deploys the untested CPU-expert layout
	if err := cmdProfile([]string{"measure", "o", "--tps", "23.5"}); err != nil {
		t.Fatal(err)
	}
	if err := cmdDeploy([]string{"--profile", "o", "--explore"}); err != nil {
		t.Fatal(err)
	}
	if got.Candidate == nil || got.Candidate.Parts[1].CPUExperts != 7 || got.WorkerCPUExperts != nil {
		t.Fatalf("explore: %+v", got.Candidate)
	}
	if err := cmdProfile([]string{"measure", "o", "--tps", "14.4"}); err != nil {
		t.Fatal(err)
	}
	// both measured: the best measured layout is deployed again, exactly
	if err := cmdDeploy([]string{"--profile", "o"}); err != nil {
		t.Fatal(err)
	}
	p, _ := store.Load("o")
	last := p.Runs[len(p.Runs)-1]
	if got.Candidate == nil || got.Candidate.Parts[1].Layers != 6 || got.Candidate.Parts[1].CPUExperts != 0 || !strings.HasPrefix(last.Choice, "auto, measured") {
		t.Fatalf("best measured: %+v %s", got.Candidate, last.Choice)
	}
}

func TestBenchEndpoint(t *testing.T) {
	n := 0
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, q *http.Request) {
		switch q.URL.Path {
		case "/v1/chat/completions":
			var body map[string]any
			json.NewDecoder(q.Body).Decode(&body)
			if body["temperature"].(float64) != 0 || body["max_tokens"].(float64) != 128 {
				t.Errorf("greedy, 128 tokens: %v", body)
			}
			n++
			fmt.Fprintf(w, `{"usage":{"completion_tokens":128},"timings":{"predicted_per_second":%d}}`, 20+n%3)
		case "/ring/trace":
			w.Write([]byte(`{"token_p50_ms":42.5,"local_ms":22,"ring_ms":20.5}`))
		}
	}))
	defer ts.Close()
	port := ts.Listener.Addr().(*net.TCPAddr).Port
	m, err := benchEndpoint(context.Background(), ts.URL+"/v1", port, 3, 128, io.Discard)
	if err != nil {
		t.Fatal(err)
	}
	if n != 9 || len(m.PerPrompt) != 3 || m.TPS != 21 || m.Trace == nil || m.Trace.TokenMS != 42.5 || m.At.After(time.Now()) {
		t.Fatalf("%d %+v", n, m)
	}
}

// The run's registered URL is the master's LAN address; from outside the LAN the bench goes through the path
// reachEndpoint finds (an ssh tunnel to the run's master) instead of failing with "connection refused".
func TestProfileBenchUsesTheReachablePath(t *testing.T) {
	_, store := profileEnv(t)
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, q *http.Request) {
		if q.URL.Path == "/v1/chat/completions" {
			fmt.Fprint(w, `{"usage":{"completion_tokens":128},"timings":{"predicted_per_second":30}}`)
		}
	}))
	defer ts.Close()
	p := &profile.Profile{Name: "d", Model: "/m/x.gguf"}
	p.AddRun(profile.Run{RunID: "run-1", URL: "http://10.0.1.13:18090/v1", StatsPort: 50155, Layout: profile.RunLayout{Master: "dino"}})
	if err := store.Save(p); err != nil {
		t.Fatal(err)
	}
	old := reachEndpoint
	var gotNode, gotBase string
	closed := false
	reachEndpoint = func(_ context.Context, node, base string, stats int) (*deploy.Reach, error) {
		gotNode, gotBase = node, base
		return &deploy.Reach{URL: ts.URL + "/v1", Note: "tunnel", Close: func() { closed = true }}, nil
	}
	t.Cleanup(func() { reachEndpoint = old })
	if err := cmdProfileBench(store, []string{"d", "--reps", "1", "--tokens", "128"}); err != nil {
		t.Fatal(err)
	}
	if gotNode != "dino" || gotBase != "http://10.0.1.13:18090/v1" || !closed {
		t.Fatalf("%q %q closed=%v", gotNode, gotBase, closed)
	}
	q, _ := store.Load("d")
	if m := q.Runs[0].Measured; m == nil || m.TPS != 30 {
		t.Fatalf("%+v", q.Runs[0].Measured)
	}
}

func TestProfileForceDeploysOversizedMmapModel(t *testing.T) {
	dir, store := profileEnv(t)
	model := tinyGGUF(t, dir, "llama", 24)

	// Stub daemon planner returning 0 candidates ("does not fit") + executeDeployment enforcing Force on oversized mmap model.
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/fleet/plan" {
			_ = json.NewEncoder(w).Encode(plan.Result{Model: "oversized.gguf", Verdict: "does not fit in RAM+VRAM"})
			return
		}
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	oldCl, oldMeta, oldExec := deployClient, readModelMeta, executeDeployment
	deployClient = func() (*client.Client, error) { return client.New(strings.TrimPrefix(srv.URL, "http://")), nil }
	readModelMeta = func(_, _ string) (*plan.Model, error) {
		return &plan.Model{Name: "oversized.gguf", Arch: "llama", Layers: 24}, nil
	}
	var lastOpts deploy.DeployOptions
	executeDeployment = func(_ context.Context, cfg *config.Config, o deploy.DeployOptions) (*deploy.DeployResult, error) {
		lastOpts = o
		if !o.Force {
			return nil, fmt.Errorf("not enough memory: oversized mmap model requires --force")
		}
		res, err := deploy.PlanDeployment(cfg, o)
		if err == nil {
			res.Status = "running"
		}
		return res, err
	}
	t.Cleanup(func() { deployClient, readModelMeta, executeDeployment = oldCl, oldMeta, oldExec })

	// 1) Profile saved with --force (default layout.mode: auto) deploys oversized mmap model without CLI --force.
	if err := cmdProfile([]string{"save", "oversized-mmap", model, "--ctx", "65536", "--cpu-moe", "--moe-cache-mib", "3072", "--force"}); err != nil {
		t.Fatalf("profile save --force: %v", err)
	}
	saved, err := store.Load("oversized-mmap")
	if err != nil || !saved.Force || saved.EffectiveMode() != profile.LayoutAuto {
		t.Fatalf("saved profile: %v %+v", err, saved)
	}
	if err := cmdDeploy([]string{"--profile", "oversized-mmap"}); err != nil {
		t.Fatalf("deploy --profile oversized-mmap should succeed via profile force: true, got: %v", err)
	}
	if !lastOpts.Force || !lastOpts.CPUMoE || lastOpts.MoECacheMiB != 3072 {
		t.Fatalf("expected Force=true, CPUMoE=true, MoECacheMiB=3072, got: %+v", lastOpts)
	}

	// 2) Explicit --force=false on CLI overrides profile force: true and triggers refusal.
	if err := cmdDeploy([]string{"--profile", "oversized-mmap", "--force=false"}); err == nil {
		t.Fatal("expected refusal when --force=false overrides profile force: true")
	}

	// 3) Fixed layout profile loaded from YAML with force: true also deploys without CLI --force.
	yamlText := fmt.Sprintf("name: fixed-mmap\nmodel: %s\nctx: 32768\nlayout:\n  mode: fixed\nmoe:\n  cpu: true\n  cache_mib: 3072\nforce: true\n", model)
	yamlFile := filepath.Join(dir, "fixed-mmap.yaml")
	if err := os.WriteFile(yamlFile, []byte(yamlText), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := store.Import(yamlFile, "", true); err != nil {
		t.Fatalf("import fixed-mmap: %v", err)
	}
	if err := cmdDeploy([]string{"--profile", "fixed-mmap"}); err != nil {
		t.Fatalf("deploy --profile fixed-mmap failed: %v", err)
	}
	if !lastOpts.Force {
		t.Fatal("expected Force=true from imported YAML profile")
	}
}

func TestRunningRunsForProfile(t *testing.T) {
	// Create a profile with a few runs
	p := &profile.Profile{Name: "test"}
	p.AddRun(profile.Run{RunID: "run-1", At: time.Now()})
	p.AddRun(profile.Run{RunID: "run-2", At: time.Now()})
	p.AddRun(profile.Run{RunID: "run-3", At: time.Now()})

	// Create some runs from the server
	now := time.Now()
	running1 := server.Run{ID: "run-1", At: now, Master: "node1", Parts: []server.RunPart{{Node: "node1", First: 0, Layers: 1}}}
	running2 := server.Run{ID: "run-2", At: now, Master: "node1", Parts: []server.RunPart{{Node: "node1", First: 0, Layers: 1}}}
	ended := server.Run{ID: "run-3", At: now, Ended: now, Master: "node1", Parts: []server.RunPart{{Node: "node1", First: 0, Layers: 1}}}
	other := server.Run{ID: "run-other", At: now, Master: "node1", Parts: []server.RunPart{{Node: "node1", First: 0, Layers: 1}}}

	allRuns := []server.Run{running1, running2, ended, other}

	running := RunningRunsForProfile(p, allRuns)
	if len(running) != 2 {
		t.Fatalf("expected 2 running runs, got %d: %v", len(running), running)
	}
	// run-1 and run-2 should be running, run-3 (ended) and run-other (not in profile) should not
	runSet := make(map[string]bool)
	for _, r := range running {
		runSet[r] = true
	}
	if !runSet["run-1"] {
		t.Error("run-1 should be running")
	}
	if !runSet["run-2"] {
		t.Error("run-2 should be running")
	}
	if runSet["run-3"] {
		t.Error("run-3 should not be running (ended)")
	}
	if runSet["run-other"] {
		t.Error("run-other should not be running (not in profile)")
	}
}

func TestRunningRunsForProfile_Table(t *testing.T) {
	now := time.Now()
	tests := []struct {
		name      string
		profile   *profile.Profile
		runs      []server.Run
		wantCount int
	}{
		{
			name:      "running",
			profile:   &profile.Profile{Name: "test", Runs: []profile.Run{{RunID: "run-1", At: now}}},
			runs:      []server.Run{{ID: "run-1", At: now, Master: "node1", Parts: []server.RunPart{{Node: "node1", First: 0, Layers: 1}}}},
			wantCount: 1,
		},
		{
			name:      "ended",
			profile:   &profile.Profile{Name: "test", Runs: []profile.Run{{RunID: "run-1", At: now}}},
			runs:      []server.Run{{ID: "run-1", At: now, Ended: now, Master: "node1", Parts: []server.RunPart{{Node: "node1", First: 0, Layers: 1}}}},
			wantCount: 0,
		},
		{
			name:      "other profile",
			profile:   &profile.Profile{Name: "test", Runs: []profile.Run{{RunID: "run-1", At: now}}},
			runs:      []server.Run{{ID: "run-2", At: now, Master: "node1", Parts: []server.RunPart{{Node: "node1", First: 0, Layers: 1}}}},
			wantCount: 0,
		},
		{
			name:      "no runs",
			profile:   &profile.Profile{Name: "test"},
			runs:      []server.Run{{ID: "run-1", At: now, Master: "node1", Parts: []server.RunPart{{Node: "node1", First: 0, Layers: 1}}}},
			wantCount: 0,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := RunningRunsForProfile(tt.profile, tt.runs)
			if len(got) != tt.wantCount {
				t.Errorf("RunningRunsForProfile() = %v, want %d runs", got, tt.wantCount)
			}
		})
	}
}

func TestProfileDown_ChainRun(t *testing.T) {
	now := time.Now()
	chainProfile := &profile.Profile{
		Name:   "chain-prof",
		Engine: "chain",
		Model:  "/models/toy.gguf",
		Runs: []profile.Run{
			{RunID: "chain-run-1", At: now},
		},
	}
	runs := []server.Run{
		{ID: "chain-run-1", Engine: "chain", At: now, Master: "node1"},
		{ID: "ring-run-2", Engine: "ring", At: now, Master: "node1"},
	}
	running := RunningRunsForProfile(chainProfile, runs)
	if len(running) != 1 || running[0] != "chain-run-1" {
		t.Fatalf("expected running chain run 'chain-run-1', got: %v", running)
	}
}

func TestProfileAutostartCmd(t *testing.T) {
	dir := isolate(t)
	oldS, oldL := setup.SystemctlUser, setup.LoginctlLinger
	setup.SystemctlUser = func(args ...string) ([]byte, error) { return nil, nil }
	setup.LoginctlLinger = func() (string, error) { return "yes", nil }
	t.Cleanup(func() { setup.SystemctlUser, setup.LoginctlLinger = oldS, oldL })

	store := profileStore()
	m := filepath.Join(dir, "tiny.gguf")
	if err := os.WriteFile(m, []byte("GGUF fake"), 0644); err != nil {
		t.Fatal(err)
	}
	if err := store.Save(&profile.Profile{Name: "myprofile", Model: m}); err != nil {
		t.Fatal(err)
	}

	// invalid args
	if err := cmdProfile([]string{"autostart"}); err == nil || !strings.Contains(err.Error(), "usage: fleet profile autostart") {
		t.Fatalf("expected usage error, got %v", err)
	}
	if err := cmdProfile([]string{"autostart", "myprofile", "maybe"}); err == nil {
		t.Fatal("expected error on invalid on|off mode")
	}
	if err := cmdProfile([]string{"autostart", "nonexistent", "on"}); err == nil {
		t.Fatal("expected error for nonexistent profile")
	}

	// autostart on
	out := runOK(t, func() error { return cmdProfile([]string{"autostart", "myprofile", "on"}) })
	if !strings.Contains(out, "written:") || !strings.Contains(out, "fleet-profile-myprofile.service") {
		t.Fatalf("unexpected autostart on output:\n%s", out)
	}
	if !setup.ProfileAutostartEnabled("myprofile") {
		t.Fatal("expected profile autostart unit to be enabled")
	}

	// autostart off
	outOff := runOK(t, func() error { return cmdProfile([]string{"autostart", "myprofile", "off"}) })
	if !strings.Contains(outOff, "removed:") || !strings.Contains(outOff, "fleet-profile-myprofile.service") {
		t.Fatalf("unexpected autostart off output:\n%s", outOff)
	}
	if setup.ProfileAutostartEnabled("myprofile") {
		t.Fatal("expected profile autostart unit to be removed")
	}
}
