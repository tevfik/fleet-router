package deploy

import (
	"net"
	"strings"
	"testing"
)

func TestOwnAddrReplacesAStaleConfiguredAddress(t *testing.T) {
	oldI, oldO := interfaceIPs, outboundIP
	t.Cleanup(func() { interfaceIPs, outboundIP = oldI, oldO })
	interfaceIPs = func() []net.IP { return []net.IP{net.ParseIP("127.0.0.1"), net.ParseIP("10.0.1.17")} }
	outboundIP = func() net.IP { return net.ParseIP("10.0.1.17") }

	if a, fixed := ownAddr("10.0.1.119"); a != "10.0.1.17" || !fixed {
		t.Fatalf("stale address: %s %v", a, fixed)
	}
	if a, fixed := ownAddr("10.0.1.17"); a != "10.0.1.17" || fixed {
		t.Fatalf("an address on an interface stays: %s %v", a, fixed)
	}
	if a, fixed := ownAddr("papa"); a != "papa" || fixed {
		t.Fatalf("a name is left alone: %s %v", a, fixed)
	}
	if a, fixed := ownAddr("127.0.0.1"); a != "127.0.0.1" || fixed {
		t.Fatalf("loopback stays: %s %v", a, fixed)
	}
	outboundIP = func() net.IP { return nil }
	if a, fixed := ownAddr("10.0.1.119"); a != "10.0.1.119" || fixed {
		t.Fatalf("no route known: unchanged: %s %v", a, fixed)
	}
}

// A chain stage of this machine is addressed at its current address when the configured one went stale, and the
// deploy says so; another machine's address is not touched.
func TestResolveStageHostFixesThisMachineOnly(t *testing.T) {
	oldI, oldO, oldT := interfaceIPs, outboundIP, isThisMachine
	t.Cleanup(func() { interfaceIPs, outboundIP, isThisMachine = oldI, oldO, oldT })
	interfaceIPs = func() []net.IP { return []net.IP{net.ParseIP("127.0.0.1"), net.ParseIP("10.0.1.50")} }
	outboundIP = func() net.IP { return net.ParseIP("10.0.1.50") }
	isThisMachine = func(n string) bool { return n == "10.0.1.119" }
	if h := ResolveStageHost("10.0.1.119"); h != "10.0.1.50" {
		t.Fatalf("this machine: %s", h)
	}
	if w := staleOwnAddr("10.0.1.119"); !strings.Contains(w, "10.0.1.50") || !strings.Contains(w, "fleet config node set") {
		t.Fatalf("warning %q", w)
	}
	if h := ResolveStageHost("10.0.1.46"); h != "10.0.1.46" {
		t.Fatalf("another machine: %s", h)
	}
	if w := staleOwnAddr("10.0.1.46"); w != "" {
		t.Fatalf("another machine warned: %q", w)
	}
}

// The readiness of a stage of this machine is asked on loopback, same port.
func TestLoopbackReady(t *testing.T) {
	s := loopbackReady(StageLaunch{Ready: Readiness{Kind: "http", Addr: "10.0.1.119:52310", Path: "/health"}})
	if s.Ready.Addr != "127.0.0.1:52310" || s.Ready.Path != "/health" {
		t.Fatalf("%+v", s.Ready)
	}
}
