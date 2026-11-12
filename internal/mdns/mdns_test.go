package mdns

import (
	"context"
	"net"
	"os"
	"testing"
	"time"

	"golang.org/x/net/dns/dnsmessage"
)

func TestPackParse(t *testing.T) {
	info := Info{Instance: "victor (ab12cd)", Host: "victor", Port: 4711, TXT: map[string]string{"id": "ab12cd34", "scope": "loopback"}}
	n := New(func() Info { return info })
	b, err := n.response(info, 0, ttl, true).Pack()
	if err != nil {
		t.Fatal(err)
	}
	var m dnsmessage.Message
	if err := m.Unpack(b); err != nil {
		t.Fatal(err)
	}
	browser := New(nil)
	browser.record(&m, &net.UDPAddr{IP: net.IPv4(10, 0, 1, 7), Port: 5353})
	es := browser.Entries()
	if len(es) != 1 {
		t.Fatalf("want 1 entry, got %+v", es)
	}
	e := es[0]
	if e.Instance != "victor (ab12cd)" || e.Host != "victor.local" || e.Port != 4711 || e.TXT["id"] != "ab12cd34" || e.Addrs[0] != "10.0.1.7" {
		t.Fatalf("wrong entry: %+v", e)
	}
	// goodbye: TTL 0 removes it
	b, _ = n.response(info, 0, 0, true).Pack()
	m.Unpack(b)
	browser.record(&m, &net.UDPAddr{IP: net.IPv4(10, 0, 1, 7), Port: 5353})
	if es := browser.Entries(); len(es) != 0 {
		t.Fatalf("goodbye ignored: %+v", es)
	}
}

func TestLabel(t *testing.T) {
	if got := label("a.b.c"); got != "a-b-c" {
		t.Fatal(got)
	}
	long := ""
	for len(long) < 80 {
		long += "x"
	}
	if len(label(long)) != 63 {
		t.Fatal("label not cut to 63 bytes")
	}
}

// TestLive runs two nodes on this machine: the browse-only one must hear the other over multicast.
func TestLive(t *testing.T) {
	if testing.Short() || len(interfaces()) == 0 {
		t.Skip("no multicast interface")
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	a := New(func() Info {
		return Info{Instance: "fleet-test-a", Host: "fleet-test-a", Port: 4799, TXT: map[string]string{"id": "test-a"}}
	})
	b := New(nil)
	errs := make(chan error, 2)
	go func() { errs <- a.Run(ctx) }()
	go func() { errs <- b.Run(ctx) }()
	deadline := time.After(5 * time.Second)
	for {
		for _, e := range b.Entries() {
			if e.TXT["id"] == "test-a" && e.Port == 4799 {
				return
			}
		}
		select {
		case err := <-errs:
			t.Skipf("cannot open the mDNS socket here: %v", err)
		case <-deadline:
			if os.Getenv("CI") != "" { // CI containers often do not loop multicast back
				t.Skipf("multicast not delivered here (CI)")
			}
			t.Fatalf("not heard; entries: %+v", b.Entries())
		case <-time.After(100 * time.Millisecond):
		}
	}
}
