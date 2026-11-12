package deploy

import (
	"context"
	"fmt"
	"net"
	"net/url"
	"os/exec"
	"strconv"
	"strings"
	"time"
)

// A deployed master registers the URL the other machines of the LAN use (http://10.0.1.13:18090/v1). The machine
// that ran the deploy, or runs `fleet profile bench`, may be outside that LAN (a laptop away from home): a plain
// connection is refused and everything that follows (a benchmark, a probe) wrongly says the ring is down. Reach finds
// a path that works: the address itself when it answers, else an ssh tunnel through the master's machine.

// Reach is how to talk to an endpoint from this machine.
type Reach struct {
	URL       string // base URL to use (…/v1): the endpoint's own when Direct, else a loopback port forwarded to it
	StatsPort int    // port of the stats server on the host of URL (0: none asked for)
	Direct    bool
	Note      string // what was done, "" when the endpoint was reachable as it is
	Close     func() // ends the tunnel (never nil)
}

// Test seams.
var (
	dialTCP = func(ctx context.Context, addr string, timeout time.Duration) bool {
		d := net.Dialer{Timeout: timeout}
		c, err := d.DialContext(ctx, "tcp", addr)
		if err != nil {
			return false
		}
		_ = c.Close()
		return true
	}
	freePort = func() (int, error) {
		l, err := net.Listen("tcp", "127.0.0.1:0")
		if err != nil {
			return 0, err
		}
		defer l.Close()
		return l.Addr().(*net.TCPAddr).Port, nil
	}
	// startTunnel runs `ssh -N -L 127.0.0.1:<local>:127.0.0.1:<remote> node` for every forward {local, remote} and
	// returns once the first local port accepts, with the function that ends it.
	startTunnel = defaultStartTunnel
)

func defaultStartTunnel(ctx context.Context, node string, forwards [][2]int) (func(), error) {
	args := []string{"-N", "-o", "BatchMode=yes", "-o", "ConnectTimeout=8", "-o", "ExitOnForwardFailure=yes", "-o", "ServerAliveInterval=15"}
	for _, f := range forwards {
		args = append(args, "-L", fmt.Sprintf("127.0.0.1:%d:127.0.0.1:%d", f[0], f[1]))
	}
	cctx, cancel := context.WithCancel(ctx)
	cmd := exec.CommandContext(cctx, "ssh", append(args, node)...)
	if err := cmd.Start(); err != nil {
		cancel()
		return nil, err
	}
	done := make(chan error, 1)
	go func() { done <- cmd.Wait() }()
	stop := func() { cancel(); <-done }
	deadline := time.Now().Add(12 * time.Second)
	for time.Now().Before(deadline) {
		select {
		case err := <-done:
			cancel()
			return nil, fmt.Errorf("ssh tunnel to %s ended at once: %v", node, err)
		default:
		}
		if dialTCP(ctx, fmt.Sprintf("127.0.0.1:%d", forwards[0][0]), 300*time.Millisecond) {
			return func() { cancel(); <-done }, nil
		}
		time.Sleep(200 * time.Millisecond)
	}
	stop()
	return nil, fmt.Errorf("ssh tunnel to %s did not come up", node)
}

// ReachEndpoint returns a way to talk to the endpoint at base (…/v1) whose master runs on node, and to its stats
// server on statsPort (0: none). Direct connection first; when that is refused or times out and node is a machine ssh
// reaches, a tunnel. The caller must call Close.
func ReachEndpoint(ctx context.Context, node, base string, statsPort int) (*Reach, error) {
	noop := func() {}
	u, err := url.Parse(strings.TrimSuffix(base, "/"))
	if err != nil || u.Hostname() == "" {
		return nil, fmt.Errorf("cannot read the endpoint URL %q", base)
	}
	port := 80
	if p := u.Port(); p != "" {
		port, _ = strconv.Atoi(p)
	} else if u.Scheme == "https" {
		port = 443
	}
	direct := &Reach{URL: base, StatsPort: statsPort, Direct: true, Close: noop}
	host := u.Hostname()
	if ip := net.ParseIP(host); (ip != nil && ip.IsLoopback()) || host == "localhost" || dialTCP(ctx, net.JoinHostPort(host, strconv.Itoa(port)), 3*time.Second) {
		return direct, nil
	}
	if node == "" || isLocalNode(node) {
		return nil, fmt.Errorf("%s does not answer from this machine (not on its LAN?) and the machine that runs it is not known: pass --via <master machine> to go through ssh", net.JoinHostPort(host, strconv.Itoa(port)))
	}
	lp, err := freePort()
	if err != nil {
		return nil, err
	}
	forwards := [][2]int{{lp, port}}
	sp := 0
	if statsPort > 0 {
		if sp, err = freePort(); err != nil {
			return nil, err
		}
		if sp == lp {
			sp++
		}
		forwards = append(forwards, [2]int{sp, statsPort})
	}
	stop, err := startTunnel(ctx, node, forwards)
	if err != nil {
		return nil, fmt.Errorf("%s does not answer from this machine and the ssh tunnel through %s failed: %w", net.JoinHostPort(host, strconv.Itoa(port)), node, err)
	}
	nu := *u
	nu.Host = net.JoinHostPort("127.0.0.1", strconv.Itoa(lp))
	return &Reach{URL: nu.String(), StatsPort: sp, Close: stop,
		Note: fmt.Sprintf("%s is not reachable from this machine (outside its LAN): going through an ssh tunnel via %s (127.0.0.1:%d)", net.JoinHostPort(host, strconv.Itoa(port)), node, lp)}, nil
}
