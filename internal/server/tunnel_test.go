package server

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// A fake ssh that listens on every -L local port (via bash /dev/tcp is not enough, so python) and logs one line per start.
func TestTunnelsShareOneSSHPerHost(t *testing.T) {
	dir := t.TempDir()
	log := filepath.Join(dir, "log")
	script := `#!/bin/sh
echo "$@" >> ` + log + `
exec python3 -c '
import socket,sys,time
a=sys.argv[1:]; ss=[]
for i,x in enumerate(a):
    if x=="-L":
        p=int(a[i+1].split(":")[1]); s=socket.socket(); s.setsockopt(socket.SOL_SOCKET,socket.SO_REUSEADDR,1); s.bind(("127.0.0.1",p)); s.listen(5); ss.append(s)
time.sleep(60)' "$@"
`
	if err := os.WriteFile(filepath.Join(dir, "ssh"), []byte(script), 0o755); err != nil {
		t.Skip(err)
	}
	t.Setenv("PATH", dir+":"+os.Getenv("PATH"))
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	tn := newTunnels()
	tn.Ensure(ctx, "dino", "127.0.0.1:11434")
	tn.Ensure(ctx, "dino", "127.0.0.1:8000")
	tn.Ensure(ctx, "other", "127.0.0.1:11434")
	deadline := time.Now().Add(15 * time.Second)
	for time.Now().Before(deadline) &&
		(tn.Port("dino", "127.0.0.1:11434") == 0 || tn.Port("dino", "127.0.0.1:8000") == 0 || tn.Port("other", "127.0.0.1:11434") == 0) {
		time.Sleep(100 * time.Millisecond)
	}
	for _, k := range [][2]string{{"dino", "127.0.0.1:11434"}, {"dino", "127.0.0.1:8000"}, {"other", "127.0.0.1:11434"}} {
		if tn.Port(k[0], k[1]) == 0 {
			t.Fatalf("tunnel %v not up: %s", k, tn.Err(k[0], k[1]))
		}
	}
	b, _ := os.ReadFile(log)
	// the last start per host must carry every -L of that host
	var lastDgx string
	for _, l := range strings.Split(strings.TrimSpace(string(b)), "\n") {
		if strings.HasSuffix(l, " dino") {
			lastDgx = l
		}
	}
	if strings.Count(lastDgx, "-L") != 2 {
		t.Fatalf("dino should have one connection with two -L, got %q", lastDgx)
	}
}

func fakeSSH(t *testing.T) (logPath string) {
	dir := t.TempDir()
	logPath = filepath.Join(dir, "log")
	script := `#!/bin/sh
echo "$@" >> ` + logPath + `
exec python3 -c '
import socket,sys,time
a=sys.argv[1:]; ss=[]
for i,x in enumerate(a):
    if x=="-L":
        p=int(a[i+1].split(":")[1]); s=socket.socket(); s.setsockopt(socket.SOL_SOCKET,socket.SO_REUSEADDR,1); s.bind(("127.0.0.1",p)); s.listen(5); ss.append(s)
time.sleep(120)' "$@"
`
	if err := os.WriteFile(filepath.Join(dir, "ssh"), []byte(script), 0o755); err != nil {
		t.Skip(err)
	}
	t.Setenv("PATH", dir+":"+os.Getenv("PATH"))
	return logPath
}

func starts(logPath string) int {
	b, _ := os.ReadFile(logPath)
	return strings.Count(string(b), "\n")
}

func waitFor(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(15 * time.Second)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(50 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for %s", what)
}

// An unused connection is closed, real traffic reopens it, and a request in flight keeps it open.
func TestIdleTunnelClosesAndReopensOnUse(t *testing.T) {
	logPath := fakeSSH(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	tn := newTunnels()
	tn.tick = 50 * time.Millisecond
	tn.SetIdle(400 * time.Millisecond)
	tn.Ensure(ctx, "dino", "127.0.0.1:11434")
	waitFor(t, "tunnel up", func() bool { return tn.Port("dino", "127.0.0.1:11434") > 0 })

	waitFor(t, "idle close", func() bool { return tn.IsIdle("dino") })
	if tn.Port("dino", "127.0.0.1:11434") != 0 {
		t.Fatal("a closed tunnel must report no port")
	}
	if tn.Err("dino", "127.0.0.1:11434") == "" || !strings.Contains(tn.Err("dino", "127.0.0.1:11434"), "idle") {
		t.Fatalf("the reason should say idle: %q", tn.Err("dino", "127.0.0.1:11434"))
	}
	if starts(logPath) != 1 {
		t.Fatalf("ssh started %d times", starts(logPath))
	}
	time.Sleep(300 * time.Millisecond)
	if starts(logPath) != 1 {
		t.Fatal("an idle tunnel must not reconnect by itself")
	}

	port, release := tn.Use(ctx, "dino", "127.0.0.1:11434")
	if port == 0 || tn.IsIdle("dino") {
		t.Fatalf("Use must reopen the tunnel (port %d)", port)
	}
	if starts(logPath) != 2 {
		t.Fatalf("expected a second ssh start, got %d", starts(logPath))
	}
	time.Sleep(900 * time.Millisecond) // longer than the idle time, but a request is in flight
	if tn.IsIdle("dino") || tn.Port("dino", "127.0.0.1:11434") == 0 {
		t.Fatal("a tunnel in use must stay open")
	}
	release()
	waitFor(t, "idle close after release", func() bool { return tn.IsIdle("dino") })
}

func TestIdleDisabledKeepsTunnelsOpen(t *testing.T) {
	fakeSSH(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	tn := newTunnels()
	tn.tick = 20 * time.Millisecond
	tn.SetIdle(0)
	tn.Ensure(ctx, "dino", "127.0.0.1:11434")
	waitFor(t, "tunnel up", func() bool { return tn.Port("dino", "127.0.0.1:11434") > 0 })
	time.Sleep(400 * time.Millisecond)
	if tn.IsIdle("dino") || tn.Port("dino", "127.0.0.1:11434") == 0 {
		t.Fatal("with idle = 0 the tunnel must never close")
	}
}

// fakeSSHScript installs an ssh that runs body (sh) before it binds its -L ports, or instead of it when bind is false.
func fakeSSHScript(t *testing.T, body string, bind bool) {
	dir := t.TempDir()
	script := "#!/bin/sh\n" + body + "\n"
	if bind {
		script += `exec python3 -c '
import socket,sys,time
a=sys.argv[1:]; ss=[]
for i,x in enumerate(a):
    if x=="-L":
        p=int(a[i+1].split(":")[1]); s=socket.socket(); s.setsockopt(socket.SOL_SOCKET,socket.SO_REUSEADDR,1); s.bind(("127.0.0.1",p)); s.listen(5); ss.append(s)
time.sleep(120)' "$@"
`
	}
	if err := os.WriteFile(filepath.Join(dir, "ssh"), []byte(script), 0o755); err != nil {
		t.Skip(err)
	}
	t.Setenv("PATH", dir+":"+os.Getenv("PATH"))
}

// The first request right after the daemon starts waits for the tunnel that is still connecting, instead of
// failing with "tunnel down" (seen end to end: a fresh daemon answered 502 for its first ssh request).
func TestUseWaitsForATunnelThatIsStillConnecting(t *testing.T) {
	fakeSSHScript(t, "sleep 1", true)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	tn := newTunnels()
	tn.Ensure(ctx, "box", "127.0.0.1:11434")
	port, release := tn.Use(ctx, "box", "127.0.0.1:11434")
	defer release()
	if port == 0 {
		t.Fatalf("Use gave up on a tunnel that was still connecting: %s", tn.Err("box", "127.0.0.1:11434"))
	}
}

// A connection that fails does not hold requests for the full wait: failover to the next model stays fast.
func TestUseDoesNotWaitForAFailedTunnel(t *testing.T) {
	fakeSSHScript(t, "exit 255", false)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	tn := newTunnels()
	tn.Ensure(ctx, "box", "127.0.0.1:11434")
	start := time.Now()
	port, release := tn.Use(ctx, "box", "127.0.0.1:11434")
	release()
	if port != 0 {
		t.Fatal("a failed tunnel reported a port")
	}
	if d := time.Since(start); d > 3*time.Second {
		t.Fatalf("Use held the request %s for a tunnel that failed", d)
	}
	// once it failed (backoff), the next request does not wait at all
	start = time.Now()
	if p, rel := tn.Use(ctx, "box", "127.0.0.1:11434"); p != 0 || time.Since(start) > time.Second {
		t.Fatalf("second Use: port %d after %s", p, time.Since(start))
	} else {
		rel()
	}
}
