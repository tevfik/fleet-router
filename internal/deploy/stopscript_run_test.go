package deploy

import (
	"context"
	"net"
	"os/exec"
	"strconv"
	"strings"
	"testing"
	"time"
)

func openPort(t *testing.T) int {
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer l.Close()
	return l.Addr().(*net.TCPAddr).Port
}

// The stop script really runs: a process named in names that listens on the port is stopped; one that is not named
// is reported FLEET_BUSY and left alone; a free port is silent.
func TestStopOnPortScriptRuns(t *testing.T) {
	for _, noFuser := range []bool{false, true} {
		t.Run(map[bool]string{false: "fuser", true: "ss-fallback"}[noFuser], func(t *testing.T) { stopScriptCase(t, noFuser) })
	}
}

func stopScriptCase(t *testing.T, noFuser bool) {
	if _, err := exec.LookPath("python3"); err != nil {
		t.Skip("no python3")
	}
	if _, err := exec.LookPath("ss"); err != nil {
		t.Skip("no ss")
	}
	port := openPort(t)
	run := func(names []string) string {
		script := stopOnPortScript(port, names) + "fleet_stop_port\n"
		if noFuser { // a node without psmisc: the owners must come from ss
			script = strings.Replace(script, "command -v fuser >/dev/null 2>&1", "false", 1)
		}
		out, _ := localScript(context.Background(), script)
		return strings.TrimSpace(string(out))
	}
	if out := run([]string{"python3"}); out != "" {
		t.Fatalf("a free port must be silent: %q", out)
	}
	srv := exec.Command("python3", "-m", "http.server", strconv.Itoa(port), "--bind", "127.0.0.1")
	if err := srv.Start(); err != nil {
		t.Fatal(err)
	}
	done := make(chan struct{})
	go func() { _ = srv.Wait(); close(done) }() // reap it at once: a zombie still answers kill -0
	t.Cleanup(func() { _ = srv.Process.Kill() })
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if c, err := net.DialTimeout("tcp", "127.0.0.1:"+strconv.Itoa(port), 200*time.Millisecond); err == nil {
			c.Close()
			break
		}
		time.Sleep(50 * time.Millisecond)
	}
	if out := run([]string{"someone-else"}); !strings.Contains(out, "FLEET_BUSY") {
		t.Fatalf("a process that is not fleet's must be reported, not killed: %q", out)
	}
	if out := run([]string{"python3"}); strings.Contains(out, "FLEET_BUSY") {
		t.Fatalf("fleet's own process must be stopped: %q", out)
	}
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("the listener was not stopped")
	}
}
