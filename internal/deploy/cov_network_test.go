package deploy

import (
	"context"
	"errors"
	"net"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/tevfik/fleet-router/internal/config"
)

func TestCovNodeAddr(t *testing.T) {
	// 1. parseSSHConfigAddr
	casesSSH := []struct {
		name string
		out  string
		want string
	}{
		{
			name: "direct non-loopback IP",
			out:  "HostName 10.0.1.50\nUser test\nProxyJump none\n",
			want: "10.0.1.50",
		},
		{
			name: "proxyjump set",
			out:  "HostName 10.0.1.50\nProxyJump jump.example.com\n",
			want: "",
		},
		{
			name: "proxycommand set",
			out:  "HostName 10.0.1.50\nProxyCommand ssh -W %h:%p bastion\n",
			want: "",
		},
		{
			name: "loopback IP ignored",
			out:  "HostName 127.0.0.1\n",
			want: "",
		},
		{
			name: "unspecified IP ignored",
			out:  "HostName 0.0.0.0\n",
			want: "",
		},
		{
			name: "hostname not an IP",
			out:  "HostName mynode.local\n",
			want: "",
		},
		{
			name: "malformed lines skipped",
			out:  "singleword\n  \nHostName 10.0.0.1\n",
			want: "10.0.0.1",
		},
	}
	for _, tc := range casesSSH {
		t.Run("parseSSHConfigAddr/"+tc.name, func(t *testing.T) {
			if got := parseSSHConfigAddr(tc.out); got != tc.want {
				t.Errorf("parseSSHConfigAddr() = %q; want %q", got, tc.want)
			}
		})
	}

	// 2. defaultSSHConfigAddr checks
	if got := defaultSSHConfigAddr(""); got != "" {
		t.Errorf("defaultSSHConfigAddr(\"\") = %q, want empty", got)
	}
	if got := defaultSSHConfigAddr("-flag"); got != "" {
		t.Errorf("defaultSSHConfigAddr(\"-flag\") = %q, want empty", got)
	}
	if got := defaultSSHConfigAddr("bad name; rm -rf"); got != "" {
		t.Errorf("defaultSSHConfigAddr with shell metachars = %q, want empty", got)
	}
	// cache hit
	sshAddrCache.Store("cached-node", "10.20.30.40")
	if got := defaultSSHConfigAddr("cached-node"); got != "10.20.30.40" {
		t.Errorf("defaultSSHConfigAddr cached = %q, want 10.20.30.40", got)
	}
	// live invocation for node not in cache
	_ = defaultSSHConfigAddr("node-not-in-cache")

	// 3. NodeAddrFromConfig
	if got := NodeAddrFromConfig(nil, "node1"); got != "" {
		t.Errorf("NodeAddrFromConfig(nil) = %q, want empty", got)
	}
	if got := NodeAddrFromConfig(&config.Config{}, ""); got != "" {
		t.Errorf("NodeAddrFromConfig(empty node) = %q, want empty", got)
	}

	cfg := &config.Config{
		Nodes: map[string]config.NodeSpec{
			"loopback-node": {Addr: "127.0.0.1"},
			"valid-node":    {Addr: "10.0.1.100"},
		},
		Endpoints: []config.Endpoint{
			{Name: "ep1", Node: "ep-node", URL: "http://10.0.1.200:8080"},
			{Name: "ep2", Node: "ep-loopback", URL: "http://127.0.0.1:8080"},
			{Name: "ep3", Node: "ep-ssh", URL: "http://10.0.1.201:8080", SSH: "user@host"},
			{Name: "ep4", Node: "ep-peer", URL: "http://10.0.1.202:8080", Peer: "other-peer"},
			{Name: "ep5", Node: "ep-nourl", URL: ""},
			{Name: "ep6", Node: "ep-badurl", URL: "://bad-url"},
		},
	}
	if got := NodeAddrFromConfig(cfg, "loopback-node"); got != "" {
		t.Errorf("NodeAddrFromConfig(loopback-node) = %q, want empty", got)
	}
	if got := NodeAddrFromConfig(cfg, "valid-node"); got != "10.0.1.100" {
		t.Errorf("NodeAddrFromConfig(valid-node) = %q, want 10.0.1.100", got)
	}
	if got := NodeAddrFromConfig(cfg, "ep-node"); got != "10.0.1.200" {
		t.Errorf("NodeAddrFromConfig(ep-node) = %q, want 10.0.1.200", got)
	}
	if got := NodeAddrFromConfig(cfg, "ep-loopback"); got != "" {
		t.Errorf("NodeAddrFromConfig(ep-loopback) = %q, want empty", got)
	}
	if got := NodeAddrFromConfig(cfg, "ep-ssh"); got != "" {
		t.Errorf("NodeAddrFromConfig(ep-ssh) = %q, want empty", got)
	}
	if got := NodeAddrFromConfig(cfg, "ep-badurl"); got != "" {
		t.Errorf("NodeAddrFromConfig(ep-badurl) = %q, want empty", got)
	}

	// 4. ownAddr
	oldIfs := interfaceIPs
	oldOut := outboundIP
	t.Cleanup(func() {
		interfaceIPs = oldIfs
		outboundIP = oldOut
	})

	// Non-IP, loopback, unspecified
	if addr, fixed := ownAddr("not-an-ip"); addr != "not-an-ip" || fixed {
		t.Errorf("ownAddr(not-an-ip) = %q, %v", addr, fixed)
	}
	if addr, fixed := ownAddr("127.0.0.1"); addr != "127.0.0.1" || fixed {
		t.Errorf("ownAddr(127.0.0.1) = %q, %v", addr, fixed)
	}
	if addr, fixed := ownAddr("0.0.0.0"); addr != "0.0.0.0" || fixed {
		t.Errorf("ownAddr(0.0.0.0) = %q, %v", addr, fixed)
	}

	// Matching interface IP
	interfaceIPs = func() []net.IP {
		return []net.IP{net.ParseIP("10.0.1.10")}
	}
	if addr, fixed := ownAddr("10.0.1.10"); addr != "10.0.1.10" || fixed {
		t.Errorf("ownAddr matching iface = %q, %v", addr, fixed)
	}

	// Not on interface -> outboundIP used
	outboundIP = func() net.IP {
		return net.ParseIP("10.0.0.5")
	}
	if addr, fixed := ownAddr("10.0.1.99"); addr != "10.0.0.5" || !fixed {
		t.Errorf("ownAddr outbound fallback = %q, %v", addr, fixed)
	}

	// Outbound nil or loopback -> returns configured unchanged
	outboundIP = func() net.IP { return nil }
	if addr, fixed := ownAddr("10.0.1.99"); addr != "10.0.1.99" || fixed {
		t.Errorf("ownAddr outbound nil = %q, %v", addr, fixed)
	}
	outboundIP = func() net.IP { return net.ParseIP("127.0.0.1") }
	if addr, fixed := ownAddr("10.0.1.99"); addr != "10.0.1.99" || fixed {
		t.Errorf("ownAddr outbound loopback = %q, %v", addr, fixed)
	}

	// 5. ResolveNodeIP
	oldCfgNode := configNodeAddr
	oldSSHAddr := sshConfigAddr
	t.Cleanup(func() {
		configNodeAddr = oldCfgNode
		sshConfigAddr = oldSSHAddr
	})

	// Direct IP
	if got := ResolveNodeIP("1.2.3.4"); got != "1.2.3.4" {
		t.Errorf("ResolveNodeIP(IP) = %q, want 1.2.3.4", got)
	}

	// Via configNodeAddr
	configNodeAddr = func(n string) string {
		if n == "cfg-node" {
			return "10.1.1.1"
		}
		return ""
	}
	sshConfigAddr = func(n string) string {
		if n == "ssh-node" {
			return "10.2.2.2"
		}
		return ""
	}
	if got := ResolveNodeIP("cfg-node"); got != "10.1.1.1" {
		t.Errorf("ResolveNodeIP(cfg-node) = %q, want 10.1.1.1", got)
	}
	if got := ResolveNodeIP("ssh-node"); got != "10.2.2.2" {
		t.Errorf("ResolveNodeIP(ssh-node) = %q, want 10.2.2.2", got)
	}
	if got := ResolveNodeIP("unknown-node"); got != "unknown-node" {
		t.Errorf("ResolveNodeIP(unknown-node) = %q, want unknown-node", got)
	}

	// defaultConfigNodeAddr execution
	_ = defaultConfigNodeAddr("nonexistent")
}

func TestCovReachEndpoint(t *testing.T) {
	ctx := context.Background()

	oldDial := dialTCP
	oldFree := freePort
	oldTunnel := startTunnel
	oldThis := isThisMachine
	oldLocalHost := localHostname
	t.Cleanup(func() {
		dialTCP = oldDial
		freePort = oldFree
		startTunnel = oldTunnel
		isThisMachine = oldThis
		localHostname = oldLocalHost
	})

	// Invalid URL
	if _, err := ReachEndpoint(ctx, "node1", "://bad-url", 0); err == nil {
		t.Errorf("ReachEndpoint expected error on bad URL")
	}
	if _, err := ReachEndpoint(ctx, "node1", "http://", 0); err == nil {
		t.Errorf("ReachEndpoint expected error on empty host")
	}

	// Direct loopback IP or localhost
	r, err := ReachEndpoint(ctx, "node1", "http://127.0.0.1:8080/v1", 0)
	if err != nil || !r.Direct || r.URL != "http://127.0.0.1:8080/v1" {
		t.Fatalf("ReachEndpoint loopback expected direct, got: %+v, %v", r, err)
	}
	r.Close()

	r, err = ReachEndpoint(ctx, "node1", "http://localhost:8080/v1", 0)
	if err != nil || !r.Direct {
		t.Fatalf("ReachEndpoint localhost expected direct, got: %+v, %v", r, err)
	}
	r.Close()

	// Direct reachable via dialTCP
	dialTCP = func(_ context.Context, _ string, _ time.Duration) bool { return true }
	r, err = ReachEndpoint(ctx, "node1", "http://10.0.1.50/v1", 9000)
	if err != nil || !r.Direct || r.StatsPort != 9000 {
		t.Fatalf("ReachEndpoint dialTCP=true expected direct, got: %+v, %v", r, err)
	}
	r.Close()

	// Direct dial fails, unreachable and node is local or empty -> error
	dialTCP = func(_ context.Context, _ string, _ time.Duration) bool { return false }
	if _, err := ReachEndpoint(ctx, "", "http://10.0.1.50:8080/v1", 0); err == nil {
		t.Errorf("ReachEndpoint expected error when node is empty and not direct")
	}

	isThisMachine = func(n string) bool { return true }
	if _, err := ReachEndpoint(ctx, "localnode", "http://10.0.1.50:8080/v1", 0); err == nil {
		t.Errorf("ReachEndpoint expected error when node is local and not direct")
	}

	// Unreachable and node is remote -> needs tunnel
	isThisMachine = func(n string) bool { return false }
	localHostname = func() string { return "myhost" }

	// freePort error on local port
	freePort = func() (int, error) { return 0, errors.New("no free port") }
	if _, err := ReachEndpoint(ctx, "remote-node", "http://10.0.1.50:8080/v1", 0); err == nil {
		t.Errorf("ReachEndpoint expected freePort error")
	}

	// freePort error on stats port
	portSeq := 0
	freePort = func() (int, error) {
		portSeq++
		if portSeq == 1 {
			return 20001, nil
		}
		return 0, errors.New("no free stats port")
	}
	if _, err := ReachEndpoint(ctx, "remote-node", "http://10.0.1.50:8080/v1", 9000); err == nil {
		t.Errorf("ReachEndpoint expected freePort stats port error")
	}

	// startTunnel error
	portSeq = 0
	freePort = func() (int, error) {
		portSeq++
		return 20000 + portSeq, nil
	}
	startTunnel = func(ctx context.Context, node string, forwards [][2]int) (func(), error) {
		return nil, errors.New("tunnel failed")
	}
	if _, err := ReachEndpoint(ctx, "remote-node", "https://10.0.1.50/v1", 9000); err == nil {
		t.Errorf("ReachEndpoint expected startTunnel error")
	}

	// startTunnel success (test https default port 443 + stats port colliding with lp)
	tunnelClosed := false
	freePort = func() (int, error) {
		return 20050, nil // returns same port to hit sp == lp branch
	}
	startTunnel = func(ctx context.Context, node string, forwards [][2]int) (func(), error) {
		if len(forwards) != 2 {
			t.Errorf("expected 2 forwards, got %d", len(forwards))
		}
		return func() { tunnelClosed = true }, nil
	}

	r, err = ReachEndpoint(ctx, "remote-node", "https://10.0.1.50/v1", 9000)
	if err != nil {
		t.Fatalf("ReachEndpoint tunnel success failed: %v", err)
	}
	if r.Direct {
		t.Errorf("expected tunnel (Direct=false), got Direct=true")
	}
	if !strings.Contains(r.URL, "127.0.0.1:20050") {
		t.Errorf("expected forwarded URL with 127.0.0.1:20050, got %s", r.URL)
	}
	if r.StatsPort != 20051 {
		t.Errorf("expected StatsPort incremented to 20051, got %d", r.StatsPort)
	}
	r.Close()
	if !tunnelClosed {
		t.Errorf("r.Close() did not call tunnel stop")
	}
}

func TestCovRemoteWorkerManager(t *testing.T) {
	m := NewRemoteWorkerManager()
	if m == nil {
		t.Fatal("NewRemoteWorkerManager returned nil")
	}

	// 1. IsTCPPortOpen
	// Start a real TCP listener on loopback
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("net.Listen failed: %v", err)
	}
	defer l.Close()
	addr := l.Addr().String()

	if !m.IsTCPPortOpen(addr, 500*time.Millisecond) {
		t.Errorf("IsTCPPortOpen on open listener returned false")
	}
	// closed port
	l.Close()
	if m.IsTCPPortOpen(addr, 100*time.Millisecond) {
		t.Errorf("IsTCPPortOpen on closed port returned true")
	}

	// 2. IsHostOnline for local
	if !m.IsHostOnline("localhost") {
		t.Errorf("IsHostOnline(localhost) = false, want true")
	}
	if !m.IsHostOnline("127.0.0.1") {
		t.Errorf("IsHostOnline(127.0.0.1) = false, want true")
	}

	// 3. ResolveLANAddress
	// IP address branch
	l2, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("net.Listen failed: %v", err)
	}
	defer l2.Close()
	portStr := strconv.Itoa(l2.Addr().(*net.TCPAddr).Port)

	resAddr, ok := m.ResolveLANAddress("127.0.0.1", portStr)
	if !ok || resAddr != "127.0.0.1:"+portStr {
		t.Errorf("ResolveLANAddress(127.0.0.1, open) = %q, %v", resAddr, ok)
	}

	l2.Close()
	resAddr, ok = m.ResolveLANAddress("127.0.0.1", portStr)
	if ok || resAddr != "127.0.0.1:"+portStr {
		t.Errorf("ResolveLANAddress(127.0.0.1, closed) = %q, %v", resAddr, ok)
	}

	// Hostname branch with ResolveNodeIP mock
	l3, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("net.Listen failed: %v", err)
	}
	defer l3.Close()
	portStr3 := strconv.Itoa(l3.Addr().(*net.TCPAddr).Port)

	oldCfgNode := configNodeAddr
	t.Cleanup(func() { configNodeAddr = oldCfgNode })
	configNodeAddr = func(node string) string {
		if node == "my-lan-node" {
			return "127.0.0.1"
		}
		return ""
	}

	resAddr, ok = m.ResolveLANAddress("my-lan-node", portStr3)
	if !ok || resAddr != "127.0.0.1:"+portStr3 {
		t.Errorf("ResolveLANAddress(hostname candidate, open) = %q, %v", resAddr, ok)
	}

	resAddr, ok = m.ResolveLANAddress("unresolvable-node-12345", "59999")
	if ok || resAddr != "" {
		t.Errorf("ResolveLANAddress(unresolvable) = %q, %v; want \"\", false", resAddr, ok)
	}

	// 4. MeasureRTT
	// Fallback when port 22 not open
	rtt := m.MeasureRTT("127.0.0.1")
	if rtt <= 0 {
		t.Errorf("MeasureRTT returned invalid duration: %v", rtt)
	}

	// 5. readyTimeout
	if d := m.readyTimeout(); d != DefaultWorkerReady {
		t.Errorf("readyTimeout default = %v, want %v", d, DefaultWorkerReady)
	}
	m.ReadyTimeout = 10 * time.Second
	if d := m.readyTimeout(); d != 10*time.Second {
		t.Errorf("readyTimeout customized = %v, want 10s", d)
	}
}
