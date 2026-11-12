package deploy

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/tevfik/fleet-router/internal/config"
	"github.com/tevfik/fleet-router/internal/plan"
)

const blobHex = "0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"

func ollamaDir(t *testing.T) (blob string) {
	t.Helper()
	dir := t.TempDir()
	_ = os.MkdirAll(filepath.Join(dir, "blobs"), 0o755)
	mdir := filepath.Join(dir, "manifests", "registry.ollama.ai", "library", "gemma4")
	_ = os.MkdirAll(mdir, 0o755)
	blob = filepath.Join(dir, "blobs", "sha256-"+blobHex)
	_ = os.WriteFile(blob, []byte("GGUF"), 0o644)
	mf := `{"layers":[{"mediaType":"application/vnd.ollama.image.model","digest":"sha256:` + blobHex + `"}]}`
	_ = os.WriteFile(filepath.Join(mdir, "12b"), []byte(mf), 0o644)
	return blob
}

func TestEndpointSlugFromOllamaManifestAndGGUFName(t *testing.T) {
	blob := ollamaDir(t)
	if got := endpointSlug(blob, "Gemma 4 12B It"); got != "gemma4-12b" {
		t.Fatalf("the Ollama name wins: %s", got)
	}
	// a blob with no manifest: general.name; with neither: a short hash, never the 64-digit file name
	lone := filepath.Join(t.TempDir(), "sha256-"+blobHex)
	if got := endpointSlug(lone, "Gemma 4 12B It"); got != "gemma-4-12b-it" {
		t.Fatalf("general.name: %s", got)
	}
	if got := endpointSlug(lone, "sha256-"+blobHex); got != "sha256-0123456789ab" {
		t.Fatalf("short hash: %s", got)
	}
	if got := endpointSlug("/m/Qwen3-8B-Q4_K_M.gguf", "Qwen3 8B"); got != "qwen3-8b-q4-k-m-gguf" {
		t.Fatalf("a named file keeps its file name: %s", got)
	}
}

func TestPlanNamesEndpointAfterOllamaModelAndKeepsSHA(t *testing.T) {
	stubPlanner(t, "llama")
	blob := ollamaDir(t)
	old := readGGUF
	readGGUF = func(string) (*plan.Model, error) { return &plan.Model{Name: "Gemma 4", Arch: "llama", Layers: 40}, nil }
	t.Cleanup(func() { readGGUF = old })
	res := dry(t, DeployOptions{ModelPath: blob})
	if res.EndpointName != "distributed-gemma4-12b" {
		t.Fatalf("%s", res.EndpointName)
	}
	if res.ModelSHA256 != blobHex {
		t.Fatalf("an Ollama blob's name is its hash: %q", res.ModelSHA256)
	}
}

func TestKnownModelSHAUsesStoreRecordNeverHashes(t *testing.T) {
	old := storeSHAOf
	storeSHAOf = func(p string) string {
		if strings.HasSuffix(p, "known.gguf") {
			return "feed"
		}
		return ""
	}
	t.Cleanup(func() { storeSHAOf = old })
	if knownModelSHA("/m/known.gguf") != "feed" || knownModelSHA("/m/other.gguf") != "" {
		t.Fatal("the store's record, or nothing")
	}
}

func writeRecord(t *testing.T, id, ep, status string) {
	t.Helper()
	b, _ := json.Marshal(DeployResult{RunID: id, EndpointName: ep, Status: status, Options: &DeployOptions{ModelPath: id, ExitOnFail: true}})
	_ = os.WriteFile(filepath.Join(runDir(), id+".json"), b, 0o644)
}

// A master that was deliberately stopped (fleet run stop/end) is not redeployed by a watchdog that had seen it up.
func TestWatchdogDoesNotRedeployDeliberatelyStoppedRun(t *testing.T) {
	check := func(stop bool) []string {
		t.Setenv("XDG_CACHE_HOME", t.TempDir())
		writeRecord(t, "run-100", "ring-a", StatusRunning)
		if stop {
			if n := MarkEndpointStopped("ring-a", "test"); n != 1 {
				t.Fatalf("marked %d", n)
			}
			writeRecord(t, "run-050", "other", StatusRunning)
			if n := MarkEndpointStopped("ring-a", "test"); n != 0 {
				t.Fatalf("already stopped / other endpoint: %d", n)
			}
		}
		var got []string
		now := time.Now()
		w := testDog(nil, &got, &now)
		w.Supervised = func(ep config.Endpoint) bool { o, err := LastRunOptions(ep.Name); return err == nil && o.ExitOnFail }
		w.stateOf("ring-a").seenUp = true
		w.CheckOnce(context.Background())
		w.CheckOnce(context.Background())
		return got
	}
	if got := check(false); len(got) != 1 {
		t.Fatalf("control: a running supervised master that vanished is redeployed: %v", got)
	}
	if got := check(true); len(got) != 0 {
		t.Fatalf("a stopped run must stay stopped: %v", got)
	}
}

func TestOnStateIsToldEveryRecordWrite(t *testing.T) {
	t.Setenv("XDG_CACHE_HOME", t.TempDir())
	var seen []string
	OnState = func(r DeployResult) { seen = append(seen, r.RunID+":"+r.Status) }
	t.Cleanup(func() { OnState = nil })
	(&DeployResult{RunID: "run-9", Status: StatusStarting}).saveState()
	if len(seen) != 1 || seen[0] != "run-9:starting" {
		t.Fatalf("%v", seen)
	}
}

func TestPlanUsesGivenEndpointName(t *testing.T) {
	stubPlanner(t, "llama")
	blob := ollamaDir(t)
	old := readGGUF
	readGGUF = func(string) (*plan.Model, error) { return &plan.Model{Name: "Gemma 4", Arch: "llama", Layers: 40}, nil }
	t.Cleanup(func() { readGGUF = old })
	res := dry(t, DeployOptions{ModelPath: blob, EndpointName: "my-gemma"})
	if res.EndpointName != "my-gemma" {
		t.Fatalf("--name wins over the derived name: %s", res.EndpointName)
	}
}

// A deploy without workers must not get the throughput profile's np=8 (each slot would get ctx/8 tokens).
func TestPlanWithoutWorkersUsesFewSlots(t *testing.T) {
	stubPlanner(t, "llama")
	blob := ollamaDir(t)
	old := readGGUF
	readGGUF = func(string) (*plan.Model, error) { return &plan.Model{Name: "Gemma 4", Arch: "llama", Layers: 40}, nil }
	t.Cleanup(func() { readGGUF = old })
	res := dry(t, DeployOptions{ModelPath: blob})
	if res.Np >= 8 {
		t.Fatalf("np=%d: a single machine must not default to the throughput slot count", res.Np)
	}
}

func TestPlanRefusesPortCollisionsOnOneMachine(t *testing.T) {
	stubPlanner(t, "llama")
	blob := ollamaDir(t)
	old := readGGUF
	readGGUF = func(string) (*plan.Model, error) { return &plan.Model{Name: "G", Arch: "llama", Layers: 40}, nil }
	t.Cleanup(func() { readGGUF = old })
	_, err := PlanDeployment(config.Default(), DeployOptions{ModelPath: blob, Port: 50155, DryRun: true})
	if err == nil || !strings.Contains(err.Error(), "statistics port") {
		t.Fatalf("--port equal to the statistics port: %v", err)
	}
	_, err = PlanDeployment(config.Default(), DeployOptions{ModelPath: blob, MasterNode: "dino", WorkerNodes: []string{"dino"}, Port: 18091, WorkerPort: 18091, DryRun: true})
	if err == nil || !strings.Contains(err.Error(), "ring port") {
		t.Fatalf("a worker port equal to the master port on one machine: %v", err)
	}
}

// One machine, Auto strategy: -ub only sizes prompt batches, so the latency profile's 16 just made prefill ~4.5x slower.
func TestPlanWithoutWorkersUsesFullMicroBatch(t *testing.T) {
	stubPlanner(t, "llama")
	blob := ollamaDir(t)
	old := readGGUF
	readGGUF = func(string) (*plan.Model, error) { return &plan.Model{Name: "Gemma 4", Arch: "llama", Layers: 40}, nil }
	t.Cleanup(func() { readGGUF = old })
	res := dry(t, DeployOptions{ModelPath: blob})
	if !strings.Contains(res.MasterCmd, "-ub 512") {
		t.Fatalf("a single machine must not get -ub 16: %s", res.MasterCmd)
	}
}
