package server

import (
	"context"
	"fmt"
	"net"
	"os/exec"
	"sync"
	"time"
)

// Tunnels keeps SSH -L tunnels alive (re-establishes them when they drop).
type Tunnels struct {
	mu     sync.RWMutex
	ports  map[string]int        // "ssh|remote" → local port
	errs   map[string]string     // last error
	groups map[string]*hostGroup // ssh host → its single connection
	idle   time.Duration         // close a connection after this long without use (0 = never)
	tick   time.Duration         // how often idleness is checked (tests shorten it)
}

func newTunnels() *Tunnels {
	return &Tunnels{ports: map[string]int{}, errs: map[string]string{}, groups: map[string]*hostGroup{}}
}

func tkey(ssh, remote string) string { return ssh + "|" + remote }

func (t *Tunnels) Port(ssh, remote string) int {
	t.mu.RLock()
	defer t.mu.RUnlock()
	return t.ports[tkey(ssh, remote)]
}

func (t *Tunnels) Err(ssh, remote string) string {
	t.mu.RLock()
	defer t.mu.RUnlock()
	return t.errs[tkey(ssh, remote)]
}

// LocalPorts returns the local ports of open tunnels (so discovery does not mistake our own tunnels for models).
func (t *Tunnels) LocalPorts() map[int]string {
	t.mu.RLock()
	defer t.mu.RUnlock()
	m := map[int]string{}
	for k, p := range t.ports {
		if p > 0 { // 0 = tunnel not open (starting, or dropped)
			m[p] = k
		}
	}
	return m
}

func freePort() (int, error) {
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return 0, err
	}
	defer l.Close()
	return l.Addr().(*net.TCPAddr).Port, nil
}

// hostGroup is the one ssh process of a host: every tunnel to that host is a -L of the same connection,
// so scanning N ports of a machine costs one login, not N.
type hostGroup struct {
	remotes []string
	changed chan struct{} // a remote was added → restart the process with the new -L list
	// idle handling: the connection is closed when no request used it for Tunnels.idle, and reopened on demand
	lastUse time.Time
	active  int           // requests using the tunnels right now
	closed  bool          // closed because it was idle (not because it dropped)
	wake    chan struct{} // someone needs the tunnel again
	// connecting: an ssh process is starting and its forwards are not up yet (first start, reopen after idle,
	// restart for a new forward). A request then waits for it; after a drop (backoff) it does not.
	connecting bool
}

// Ensure makes sure there is a tunnel to remote through ssh; the ssh process dies when ctx is cancelled.
func (t *Tunnels) Ensure(ctx context.Context, ssh, remote string) {
	k := tkey(ssh, remote)
	t.mu.Lock()
	if _, ok := t.ports[k]; ok {
		t.mu.Unlock()
		return
	}
	t.ports[k] = 0
	g := t.groups[ssh]
	if g == nil {
		g = &hostGroup{changed: make(chan struct{}, 1), wake: make(chan struct{}, 1), lastUse: time.Now(), connecting: true}
		t.groups[ssh] = g
		go t.run(ctx, ssh, g)
	}
	g.remotes = append(g.remotes, remote)
	t.mu.Unlock()
	select {
	case g.changed <- struct{}{}:
	default:
	}
}

// SetIdle sets how long a tunnel connection may stay unused before it is closed (0 = never).
func (t *Tunnels) SetIdle(d time.Duration) {
	t.mu.Lock()
	t.idle = d
	t.mu.Unlock()
}

// IsIdle reports whether the connection to ssh was closed for being unused (it opens again on Use).
func (t *Tunnels) IsIdle(ssh string) bool {
	t.mu.RLock()
	defer t.mu.RUnlock()
	g := t.groups[ssh]
	return g != nil && g.closed
}

// Use is what real traffic calls before it talks through a tunnel: it marks the connection as in use (so it
// is not closed under the request), reopens a connection that was closed for being idle and waits briefly
// for it, and returns the local port (0 if it did not come up). release must be called when the request is
// over; the idle clock starts then. Health checks do not call Use — they must not keep a tunnel alive.
func (t *Tunnels) Use(ctx context.Context, ssh, remote string) (port int, release func()) {
	t.mu.Lock()
	g := t.groups[ssh]
	if g == nil {
		t.mu.Unlock()
		return 0, func() {}
	}
	g.active++
	g.lastUse = time.Now()
	wasClosed := g.closed
	if wasClosed {
		g.connecting = true // the wake below reopens it
	}
	connecting := g.connecting
	t.mu.Unlock()
	var once sync.Once
	release = func() {
		once.Do(func() {
			t.mu.Lock()
			g.active--
			g.lastUse = time.Now()
			t.mu.Unlock()
		})
	}
	if wasClosed {
		select {
		case g.wake <- struct{}{}:
		default:
		}
	}
	if p := t.Port(ssh, remote); p > 0 || !connecting {
		return p, release
	}
	deadline := time.After(15 * time.Second) // login + forward
	for {
		select {
		case <-ctx.Done():
			return 0, release
		case <-deadline:
			return t.Port(ssh, remote), release
		case <-time.After(100 * time.Millisecond):
			if p := t.Port(ssh, remote); p > 0 {
				return p, release
			}
			t.mu.RLock()
			still := g.connecting
			t.mu.RUnlock()
			if !still { // the attempt is over and the forward did not come up: do not hold the request
				return 0, release
			}
		}
	}
}

func (t *Tunnels) setConnecting(g *hostGroup, on bool) {
	t.mu.Lock()
	g.connecting = on
	t.mu.Unlock()
}

// shouldClose: unused for longer than the idle time and nothing in flight.
func (t *Tunnels) shouldClose(g *hostGroup) bool {
	t.mu.RLock()
	defer t.mu.RUnlock()
	return t.idle > 0 && g.active == 0 && time.Since(g.lastUse) > t.idle
}

func (t *Tunnels) run(ctx context.Context, ssh string, g *hostGroup) {
	backoff := time.Second
	for ctx.Err() == nil {
		t.mu.RLock()
		closed := g.closed
		t.mu.RUnlock()
		if closed { // idle: no connection until someone needs it (or a tunnel is added)
			select {
			case <-ctx.Done():
				return
			case <-g.wake:
			case <-g.changed:
			}
			t.mu.Lock()
			g.closed, g.lastUse = false, time.Now()
			t.mu.Unlock()
			backoff = time.Second
		}
		t.mu.RLock()
		remotes := append([]string(nil), g.remotes...)
		t.mu.RUnlock()
		select { // a pending "changed" is covered by the snapshot just taken
		case <-g.changed:
		default:
		}
		args := []string{"-N", "-T",
			// not ExitOnForwardFailure: forwards from the user's ssh config (RemoteForward …) may fail
			// harmlessly (already bound on the other side); our own -L are verified below instead
			"-o", "BatchMode=yes",
			"-o", "ServerAliveInterval=15", "-o", "ServerAliveCountMax=3", "-o", "ConnectTimeout=8"}
		ports := make([]int, len(remotes))
		for i, r := range remotes {
			p, err := freePort()
			if err != nil {
				t.set(tkey(ssh, r), 0, err.Error())
				continue
			}
			ports[i] = p
			args = append(args, "-L", fmt.Sprintf("127.0.0.1:%d:%s", p, r))
		}
		cmd := exec.CommandContext(ctx, "ssh", append(args, "--", ssh)...)
		start := time.Now()
		restart := false
		t.setConnecting(g, true)
		if err := cmd.Start(); err != nil {
			for _, r := range remotes {
				t.set(tkey(ssh, r), 0, err.Error())
			}
			t.setConnecting(g, false)
		} else {
			done := make(chan error, 1)
			exited := make(chan struct{})
			go func() { err := cmd.Wait(); close(exited); done <- err }()
			up := false
			for i, r := range remotes { // the first forward waits for the login, the rest are up with it
				if ports[i] == 0 {
					continue
				}
				if waitPort(ports[i], 8*time.Second, exited) {
					t.set(tkey(ssh, r), ports[i], "")
					up = true
				} else {
					t.set(tkey(ssh, r), 0, "the forward did not come up")
				}
			}
			t.setConnecting(g, false)
			if !up {
				cmd.Process.Kill()
			}
			var err error
			closing := false
			every := t.tick
			if every <= 0 {
				every = 10 * time.Second
			}
			tick := time.NewTicker(every)
		wait:
			for {
				select {
				case err = <-done:
					break wait
				case <-g.changed: // new tunnel to this host: restart the connection with it included
					restart = true
					cmd.Process.Kill()
					err = <-done
					break wait
				case <-tick.C:
					if t.shouldClose(g) {
						closing = true
						cmd.Process.Kill()
						err = <-done
						break wait
					}
				}
			}
			tick.Stop()
			if closing {
				t.mu.Lock()
				g.closed = true
				t.mu.Unlock()
				for _, r := range remotes {
					t.set(tkey(ssh, r), 0, "idle: the tunnel was closed because nothing used it — it opens on the next request")
				}
				continue
			}
			for _, r := range remotes {
				t.set(tkey(ssh, r), 0, fmt.Sprintf("tunnel dropped: %v", err))
			}
		}
		if restart {
			continue
		}
		if time.Since(start) > 30*time.Second {
			backoff = time.Second
		} else if backoff < 30*time.Second {
			backoff *= 2
		}
		select {
		case <-ctx.Done():
		case <-g.changed: // a new tunnel does not wait for the back-off
		case <-time.After(backoff):
		}
	}
}

func (t *Tunnels) set(k string, port int, err string) {
	t.mu.Lock()
	t.ports[k], t.errs[k] = port, err
	t.mu.Unlock()
}

// waitPort reports whether the local port starts accepting connections within d.
// waitPort waits up to d for the local port to accept, and gives up at once when stop closes (ssh exited).
func waitPort(port int, d time.Duration, stop <-chan struct{}) bool {
	end := time.Now().Add(d)
	for time.Now().Before(end) {
		if c, err := net.DialTimeout("tcp", fmt.Sprintf("127.0.0.1:%d", port), 300*time.Millisecond); err == nil {
			c.Close()
			return true
		}
		select {
		case <-stop:
			return false
		case <-time.After(200 * time.Millisecond):
		}
	}
	return false
}
