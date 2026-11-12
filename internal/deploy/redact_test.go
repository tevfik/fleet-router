package deploy

import (
	"encoding/json"
	"strings"
	"testing"
)

func TestResultRedactedAndHubKeyNeverSerialized(t *testing.T) {
	r := &DeployResult{RunID: "r", Options: &DeployOptions{
		HubKey:         "flt_live_SECRET",
		MasterEnvExtra: map[string]string{"TOKEN": "s3cret"},
		WorkerEnvExtra: map[string]map[string]string{"dino": {"T": "w3cret"}},
	}}
	b, _ := json.Marshal(r)
	if strings.Contains(string(b), "flt_live_SECRET") {
		t.Fatalf("hub key serialized: %s", b)
	}
	b, _ = json.Marshal(r.Redacted())
	if strings.Contains(string(b), "s3cret") || strings.Contains(string(b), "w3cret") {
		t.Fatalf("env values printed: %s", b)
	}
	if r.Options.MasterEnvExtra["TOKEN"] != "s3cret" {
		t.Fatal("the original must stay intact (the record needs it)")
	}
}

func TestRunIDsAreUniqueWithinASecondAndSortAfterOldOnes(t *testing.T) {
	seen := map[string]bool{}
	var last string
	for i := 0; i < 2000; i++ {
		id := newRunID()
		if seen[id] {
			t.Fatalf("duplicate run id %s", id)
		}
		seen[id] = true
		if id <= last {
			t.Fatalf("ids must grow: %s after %s", id, last)
		}
		last = id
	}
	if !(last+".json" > "run-1793730017.json") {
		t.Fatal("a new id must sort after an old seconds-based one")
	}
}

func TestLibPathWithHasNoEmptyEntry(t *testing.T) {
	if got := libPathWith("/a", ""); got != "/a" {
		t.Fatal(got)
	}
	if got := libPathWith("/a", "/b:/c"); got != "/a:/b:/c" {
		t.Fatal(got)
	}
}

// What ValidateExtras accepts for a worker must reach the worker (it used to be dropped silently when it held = or +).
func TestWorkerEnvFilterAgreesWithValidation(t *testing.T) {
	o := DeployOptions{WorkerEnvExtra: map[string]map[string]string{"dino": {"K": "a+b=c"}}}
	if err := ValidateExtras(o); err != nil {
		t.Fatal(err)
	}
	if got := filterWorkerEnv([]string{"K=a+b=c"}); len(got) != 1 {
		t.Fatalf("dropped: %v", got)
	}
	if got := filterWorkerEnv([]string{"K=a;id"}); len(got) != 0 {
		t.Fatalf("unsafe must go: %v", got)
	}
}
