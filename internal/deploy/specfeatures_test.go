package deploy

import (
	"context"
	"errors"
	"strings"
	"testing"
)

func hybridRes() *DeployResult {
	return &DeployResult{Warnings: []string{"other", hybridSpecGeneric}, hybridSpec: true, MasterNode: "localhost", statsPort: 50155}
}

func withInfo(t *testing.T, body string, err error) {
	t.Helper()
	old := ringInfoFetcher
	ringInfoFetcher = func(context.Context, string, bool, int) ([]byte, error) { return []byte(body), err }
	t.Cleanup(func() { ringInfoFetcher = old })
}

func TestSpecFeatures_AllHaveStateSnapSaysNothing(t *testing.T) {
	withInfo(t, `{"worker_features":{"a:1":31,"b:2":15}}`, nil)
	r := hybridRes()
	checkSpecFeatures(context.Background(), r, true)
	if len(r.Warnings) != 1 || r.Warnings[0] != "other" {
		t.Fatalf("generic warning must go, nothing replaces it: %v", r.Warnings)
	}
}

func TestSpecFeatures_PreciseWarningNamesTheWorker(t *testing.T) {
	withInfo(t, `{"worker_features":{"a:1":31,"b:2":23,"c:3":0}}`, nil)
	r := hybridRes()
	checkSpecFeatures(context.Background(), r, true)
	if len(r.Warnings) != 2 || !strings.Contains(r.Warnings[1], "b:2, c:3") || strings.Contains(r.Warnings[1], "a:1") {
		t.Fatalf("%v", r.Warnings)
	}
}

func TestSpecFeatures_UnknownKeepsGenericWarning(t *testing.T) {
	for _, c := range []struct {
		body string
		err  error
	}{{`{"engine":"llama-ring"}`, nil}, {`{"worker_features":{}}`, nil}, {`garbage`, nil}, {``, errors.New("down")}} {
		withInfo(t, c.body, c.err)
		r := hybridRes()
		checkSpecFeatures(context.Background(), r, true)
		if len(r.Warnings) != 2 || r.Warnings[1] != hybridSpecGeneric {
			t.Fatalf("%q: %v", c.body, r.Warnings)
		}
	}
}

func TestSpecFeatures_NotHybridNeverFetches(t *testing.T) {
	old := ringInfoFetcher
	ringInfoFetcher = func(context.Context, string, bool, int) ([]byte, error) {
		t.Fatal("no fetch expected")
		return nil, nil
	}
	t.Cleanup(func() { ringInfoFetcher = old })
	checkSpecFeatures(context.Background(), &DeployResult{Warnings: []string{"x"}}, true)
}
