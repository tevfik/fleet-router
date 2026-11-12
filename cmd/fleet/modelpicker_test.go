package main

import (
	"strings"
	"testing"
	"time"

	"github.com/tevfik/fleet-router/internal/server"
)

func TestFormatRunsRemoved(t *testing.T) {
	if got := formatRunsRemoved(nil, false); !strings.Contains(got, "nothing to remove") {
		t.Fatal(got)
	}
	r := []server.Run{{ID: "r1", Model: "m", Ended: time.Now()}}
	if got := formatRunsRemoved(r, true); !strings.Contains(got, "would remove 1") || !strings.Contains(got, "r1") {
		t.Fatal(got)
	}
	if got := formatRunsRemoved(r, false); !strings.Contains(got, "removed 1") {
		t.Fatal(got)
	}
}
