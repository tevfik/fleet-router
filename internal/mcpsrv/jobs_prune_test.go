package mcpsrv

import (
	"fmt"
	"testing"
	"time"
)

func TestFinishedJobsArePruned(t *testing.T) {
	j := newJobs(nil)
	base := time.Now().Add(-time.Hour)
	for i := 0; i < maxFinishedJobs+25; i++ {
		id := fmt.Sprintf("j%d", i)
		j.m[id] = &job{ID: id, State: "done", Started: base.Add(time.Duration(i) * time.Second)}
		j.done[id] = make(chan struct{})
	}
	j.m["live"] = &job{ID: "live", State: "running", Started: base}
	j.mu.Lock()
	j.pruneLocked()
	j.mu.Unlock()
	if len(j.m) != maxFinishedJobs+1 {
		t.Fatalf("%d jobs kept", len(j.m))
	}
	if _, ok := j.m["j0"]; ok {
		t.Fatal("the oldest finished job must go")
	}
	if _, ok := j.m["live"]; !ok {
		t.Fatal("a running job is never pruned")
	}
}
