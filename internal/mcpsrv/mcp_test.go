package mcpsrv

import (
	"encoding/json"
	"testing"
)

func TestCheckArgs(t *testing.T) {
	cases := []struct {
		tool string
		args obj
		ok   bool
	}{
		{"task_start", obj{"agent": "scout", "prompt": "x"}, true},
		{"task_start", obj{"role": "scout", "prompt": "x"}, false}, // wrong name: unknown + missing agent
		{"task_start", obj{"agent": "scout"}, false},
		{"task_result", obj{"job_id": "1", "wait_s": 5}, true},
		{"task_result", obj{"job_id": "1", "wait_seconds": 5}, false},
		{"fleet_status", obj{}, true},
		{"nope", obj{}, false},
	}
	for _, c := range cases {
		if err := checkArgs(c.tool, c.args); (err == nil) != c.ok {
			t.Errorf("%s %v: err=%v, want ok=%v", c.tool, c.args, err, c.ok)
		}
	}
}

// Claude Code validates tools/list strictly: "required" must be an array (a null drops all of fleet's tools).
func TestToolSchemasAreStrictlyValid(t *testing.T) {
	b, err := json.Marshal(tools)
	if err != nil {
		t.Fatal(err)
	}
	var back []map[string]any
	if err := json.Unmarshal(b, &back); err != nil {
		t.Fatal(err)
	}
	for _, tl := range back {
		sch, _ := tl["inputSchema"].(map[string]any)
		if _, ok := sch["required"].([]any); !ok {
			t.Errorf("tool %v: inputSchema.required is %v, want an array", tl["name"], sch["required"])
		}
	}
}
