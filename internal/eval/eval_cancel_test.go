package eval

import (
	"context"
	"errors"
	"path/filepath"
	"testing"
)

// An item cancelled in flight is dropped, not recorded as a failure.
func TestCancelledItemIsNotScored(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	items := []Item{{ID: "a", Prompt: "p"}, {ID: "b", Prompt: "p"}}
	ask := func(c context.Context, _, _ string) (string, error) {
		cancel()
		return "", errors.New("context canceled")
	}
	res := Run(ctx, ask, "t", items, nil)
	if len(res.Items) != 0 {
		t.Fatalf("a cancelled run recorded %d item(s)", len(res.Items))
	}
}

func TestSaveLeavesNoTempFile(t *testing.T) {
	p := t.TempDir() + "/scores.json"
	rs := []Result{{Target: "t", Items: []ItemResult{{ID: "a", Pass: true, Passed: 1, Total: 1}}}}
	if err := Save(p, rs); err != nil {
		t.Fatal(err)
	}
	if m, _ := filepath.Glob(p + ".tmp-*"); len(m) != 0 {
		t.Fatalf("temp files: %v", m)
	}
	if Load(p)["t"].Score < 0 {
		t.Fatal("saved scores must load")
	}
}

func TestEnOnlyAllowsTwoSentencesNotThree(t *testing.T) {
	for _, it := range Items() {
		if it.ID != "en_only" {
			continue
		}
		two, three := "An API lets programs talk. It is an interface.", "An API lets programs talk. It is an interface. It is used a lot."
		var sentence func(string) bool
		for _, c := range it.Checks {
			if c.Name == "two sentences or fewer" {
				sentence = c.OK
			}
		}
		if sentence == nil || !sentence(two) || sentence(three) {
			t.Fatalf("two ok=%v three ok=%v", sentence != nil && sentence(two), sentence != nil && sentence(three))
		}
	}
}
