package deploy

import (
	"context"
	"fmt"
	"net"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"time"
)

// RemoteWorkerManager manages starting, health checking, and terminating ring workers
// on remote or local machines.
type RemoteWorkerManager struct {
	// ReadyTimeout is how long a started worker gets to listen on its port (0: DefaultWorkerReady).
	ReadyTimeout time.Duration
}

// NewRemoteWorkerManager creates a new RemoteWorkerManager.
func NewRemoteWorkerManager() *RemoteWorkerManager {
	return &RemoteWorkerManager{}
}

// IsTCPPortOpen checks if host:port is listening.
func (m *RemoteWorkerManager) IsTCPPortOpen(addr string, timeout time.Duration) bool {
	conn, err := net.DialTimeout("tcp", addr, timeout)
	if err != nil {
		return false
	}
	_ = conn.Close()
	return true
}

// IsHostOnline checks if a machine is responsive over SSH or local (timeout 1.5s).
func (m *RemoteWorkerManager) IsHostOnline(host string) bool {
	if isLocalNode(host) {
		return true
	}
	ctx, cancel := context.WithTimeout(context.Background(), 1500*time.Millisecond)
	defer cancel()
	cmd := exec.CommandContext(ctx, "ssh", "-o", "BatchMode=yes", "-o", "ConnectTimeout=1", "--", host, "echo ok")
	out, err := cmd.Output()
	return err == nil && strings.TrimSpace(string(out)) == "ok"
}

// ResolveLANAddress checks if the remote node is reachable directly on local LAN.
func (m *RemoteWorkerManager) ResolveLANAddress(nodeName, defaultPort string) (string, bool) {
	// If nodeName is an IP, check directly
	if ip := net.ParseIP(nodeName); ip != nil {
		addr := net.JoinHostPort(nodeName, defaultPort)
		if m.IsTCPPortOpen(addr, 200*time.Millisecond) {
			return addr, true
		}
		return addr, false
	}

	// Try common local LAN resolutions
	candidates := []string{
		nodeName,
		nodeName + ".local",
		nodeName + ".lan",
	}
	// The address fleet resolves for the node (config, ssh config, known table) comes first
	if ip := ResolveNodeIP(nodeName); ip != nodeName {
		candidates = append([]string{ip}, candidates...)
	}

	for _, c := range candidates {
		addr := net.JoinHostPort(c, defaultPort)
		if m.IsTCPPortOpen(addr, 300*time.Millisecond) {
			return addr, true
		}
	}
	return "", false
}

// MeasureRTT measures the round-trip latency to a target node.
func (m *RemoteWorkerManager) MeasureRTT(node string) time.Duration {
	host := ResolveNodeIP(node)
	t0 := time.Now()
	conn, err := net.DialTimeout("tcp", net.JoinHostPort(host, "22"), 500*time.Millisecond)
	if err == nil {
		rtt := time.Since(t0)
		_ = conn.Close()
		return rtt
	}
	return 45 * time.Millisecond
}

// StartWorker starts a ring-worker on a remote or local node.
// Returns the endpoint address ("host:port") and PID.
func (m *RemoteWorkerManager) StartWorker(ctx context.Context, node string, port int, isLocal bool) (string, int, error) {
	return m.StartWorkerEnv(ctx, node, port, isLocal, nil)
}

func (m *RemoteWorkerManager) readyTimeout() time.Duration {
	if m.ReadyTimeout > 0 {
		return m.ReadyTimeout
	}
	return DefaultWorkerReady
}

// StartWorkerEnv starts a ring-worker on node with extra KEY=VALUE environment (chain peer port, timeouts,
// --local-weights shard file) and waits until it listens on port. A previous worker on that port is stopped only
// when it is a ring-worker/fleet process of this user; anything else holding the port is an error. The worker that
// exits or does not listen within ReadyTimeout is an error carrying the tail of its log. The returned address is
// the node's address as other machines dial it (ResolveNodeIP), never loopback.
func (m *RemoteWorkerManager) StartWorkerEnv(ctx context.Context, node string, port int, isLocal bool, env []string) (string, int, error) {
	addr, pid, _, err := m.startWorker(ctx, node, port, isLocal, env, "", "")
	return addr, pid, err
}

// startWorker is StartWorkerEnv; with hub != "" the started worker's port is also declared as a hub stream port on
// its node (hubJoinScript, only when the node's fleet daemon answers). hubNote is "" when that worked, else what to
// do by hand (the deploy goes on: the port may already be declared through hub_stream_ports). devices is the
// worker's -d list ("": CUDA0).
func (m *RemoteWorkerManager) startWorker(ctx context.Context, node string, port int, isLocal bool, env []string, hub, devices string) (addr string, pid int, hubNote string, err error) {
	if port <= 0 {
		port = baseWorkerPort
	}
	if isLocal || node == "localhost" || node == "127.0.0.1" {
		addr, pid, err = m.startLocalWorker(ctx, node, port, env, devices)
		if err == nil && hub != "" {
			out, _ := localScript(ctx, hubJoinScript(hub, port))
			hubNote = hubJoinNote("this machine", hub, port, string(out))
		}
		return addr, pid, hubNote, err
	}

	readyS := int((m.readyTimeout() + time.Second - 1) / time.Second)
	sctx, cancel := context.WithTimeout(ctx, m.readyTimeout()+45*time.Second)
	defer cancel()
	out, err := sshScript(sctx, node, remoteWorkerScriptHub(port, env, readyS, hub, devices))
	text := string(out)
	if err != nil {
		return "", 0, "", fmt.Errorf("remote worker start on %s failed: %w\n%s", node, err, tailLines(text, 20))
	}
	if berr := busyError(node, port, text); berr != nil {
		return "", 0, "", berr
	}
	if strings.Contains(text, "FLEET_NOBIN") {
		return "", 0, "", fmt.Errorf("no ring-worker on %s (looked in %s, $PATH, fleet-engine)", node, strings.Join(workerCandidateDirs, ", "))
	}
	pid, status, logTail := parseLaunchOutput(text)
	logName := fmt.Sprintf("~/.cache/fleet/worker_%d.log", port)
	switch status {
	case "FLEET_READY":
		if hub != "" {
			hubNote = hubJoinNote(node, hub, port, text)
		}
		return net.JoinHostPort(ResolveNodeIP(node), strconv.Itoa(port)), pid, hubNote, nil
	case "FLEET_DIED":
		return "", pid, "", fmt.Errorf("worker on %s (pid %d) exited before listening on port %d; %s:\n%s", node, pid, port, logName, strings.Join(logTail, "\n"))
	case "FLEET_TIMEOUT":
		return "", pid, "", fmt.Errorf("worker on %s (pid %d) did not listen on port %d within %ds; %s:\n%s", node, pid, port, readyS, logName, strings.Join(logTail, "\n"))
	}
	return "", pid, "", fmt.Errorf("worker start on %s gave no readiness answer:\n%s", node, tailLines(text, 20))
}

func (m *RemoteWorkerManager) startLocalWorker(ctx context.Context, node string, port int, env []string, devices string) (string, int, error) {
	out, _ := localScript(ctx, stopOnPortScript(port, workerProcNames)+"fleet_stop_port\n")
	if berr := busyError("this machine", port, string(out)); berr != nil {
		return "", 0, berr
	}
	l, err := localWorkerLaunch(port, env, devices)
	if err != nil {
		return "", 0, err
	}
	logPath := workerLogPath(port)
	full := append(os.Environ(), l.EnvAdd(os.Getenv("LD_LIBRARY_PATH"))...)
	pid, done, err := startLocalProc(ctx, l.Bin, l.Args(), full, logPath)
	if err != nil {
		return "", 0, fmt.Errorf("local worker start failed (%s): %w", l.Bin, err)
	}
	logTail := func() string {
		b, _ := os.ReadFile(logPath)
		return tailLines(string(b), 20)
	}
	deadline := time.Now().Add(m.readyTimeout())
	local := net.JoinHostPort("127.0.0.1", strconv.Itoa(port))
	for {
		select {
		case werr := <-done:
			return "", pid, fmt.Errorf("local worker (pid %d, %s) exited before listening on port %d (%v); %s:\n%s", pid, l.Bin, port, werr, logPath, logTail())
		case <-ctx.Done():
			return "", pid, ctx.Err()
		default:
		}
		if localPortOpen(local) {
			host := node
			if host == "" || host == "localhost" || host == "127.0.0.1" {
				host, _ = os.Hostname()
			}
			// the configured address of this machine can be stale (a laptop on another network): use the current one
			ip, _ := ownAddr(ResolveNodeIP(host))
			return net.JoinHostPort(ip, strconv.Itoa(port)), pid, nil
		}
		if time.Now().After(deadline) {
			return "", pid, fmt.Errorf("local worker (pid %d) did not listen on port %d within %s; %s:\n%s", pid, port, m.readyTimeout(), logPath, logTail())
		}
		time.Sleep(readyPoll)
	}
}

// StopWorker stops the fleet worker listening on port of node (only a ring-worker/fleet process of this user).
func (m *RemoteWorkerManager) StopWorker(ctx context.Context, node string, port int, isLocal bool) error {
	return stopOnPort(ctx, node, isLocal, port, workerProcNames)
}

// stopOnPort stops what listens on port on node when it is one of names (see stopOnPortScript).
func stopOnPort(ctx context.Context, node string, isLocal bool, port int, names []string) error {
	script := stopOnPortScript(port, names) + "fleet_stop_port\n"
	var out []byte
	var err error
	where := node
	if isLocal {
		where = "this machine"
		out, err = localScript(ctx, script)
	} else {
		out, err = sshScript(ctx, node, script)
	}
	if berr := busyError(where, port, string(out)); berr != nil {
		return berr
	}
	return err
}
