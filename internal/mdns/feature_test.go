package mdns

import (
	"fmt"
	"net"
	"strings"
	"testing"
	"time"

	"golang.org/x/net/dns/dnsmessage"
	"golang.org/x/net/ipv4"
)

// These tests drive handle() and record() directly. Replies are captured on loopback: the node's
// socket is a plain 127.0.0.1 UDP socket (never port 5353) and the asker is another one, so only the
// unicast ("legacy", source port ≠ 5353) path can be observed; multicast and QU-to-5353 replies are not
// sent anywhere in these tests (pc nil → send is a no-op).

var testInfo = Info{Instance: "Victor Box", Host: "victor", Port: 4711, TXT: map[string]string{"id": "ab12", "v": "1.2"}}

func loopIndex(t *testing.T) int {
	t.Helper()
	ifs, _ := net.Interfaces()
	for _, i := range ifs {
		if i.Flags&net.FlagLoopback != 0 && i.Flags&net.FlagUp != 0 {
			return i.Index
		}
	}
	t.Skip("no loopback interface")
	return 0
}

// rig wires n to a loopback socket and returns an asker socket to receive unicast replies.
func rig(t *testing.T, n *Node) *net.UDPConn {
	t.Helper()
	c, err := net.ListenUDP("udp4", &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1)})
	if err != nil {
		t.Skipf("udp: %v", err)
	}
	t.Cleanup(func() { c.Close() })
	n.pc = ipv4.NewPacketConn(c)
	a, err := net.ListenUDP("udp4", &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1)})
	if err != nil {
		t.Skipf("udp: %v", err)
	}
	t.Cleanup(func() { a.Close() })
	return a
}

// ask sends questions to n (as if from asker) and returns the unicast reply, or nil after a short wait.
func ask(t *testing.T, n *Node, asker *net.UDPConn, id uint16, qs ...dnsmessage.Question) *dnsmessage.Message {
	t.Helper()
	b, err := (&dnsmessage.Message{Header: dnsmessage.Header{ID: id}, Questions: qs}).Pack()
	if err != nil {
		t.Fatal(err)
	}
	n.handle(b, asker.LocalAddr().(*net.UDPAddr), loopIndex(t))
	asker.SetReadDeadline(time.Now().Add(150 * time.Millisecond))
	buf := make([]byte, 9000)
	k, _, err := asker.ReadFromUDP(buf)
	if err != nil {
		return nil
	}
	var m dnsmessage.Message
	if err := m.Unpack(buf[:k]); err != nil {
		t.Fatalf("reply does not parse: %v", err)
	}
	return &m
}

func q(name string, typ dnsmessage.Type) dnsmessage.Question {
	return dnsmessage.Question{Name: dnsmessage.MustNewName(name), Type: typ, Class: dnsmessage.ClassINET}
}

func types(rs []dnsmessage.Resource) map[dnsmessage.Type]int {
	out := map[dnsmessage.Type]int{}
	for _, r := range rs {
		out[r.Header.Type]++
	}
	return out
}

func TestFeature_Handle_LegacyPTRQuery(t *testing.T) {
	n := New(func() Info { return testInfo })
	asker := rig(t, n)
	m := ask(t, n, asker, 0x4242, q(Service, dnsmessage.TypePTR))
	if m == nil {
		t.Fatal("no unicast reply to a legacy PTR query")
	}
	if !m.Response || !m.Authoritative || m.ID != 0x4242 {
		t.Errorf("header: %+v", m.Header)
	}
	if len(m.Questions) != 1 || m.Questions[0].Name.String() != Service {
		t.Errorf("question must be echoed: %+v", m.Questions)
	}
	at := types(m.Answers)
	if at[dnsmessage.TypePTR] != 1 || at[dnsmessage.TypeSRV] != 1 || at[dnsmessage.TypeTXT] != 1 {
		t.Errorf("answers: %v", at)
	}
	if types(m.Additionals)[dnsmessage.TypeA] == 0 {
		t.Error("A record for the host expected (loopback interface)")
	}
	for _, rs := range [][]dnsmessage.Resource{m.Answers, m.Additionals} {
		for _, r := range rs {
			if r.Header.Class&(1<<15) != 0 {
				t.Errorf("legacy reply carries the cache-flush bit: %v", r.Header)
			}
			if r.Header.TTL > 10 {
				t.Errorf("legacy reply TTL %d > 10", r.Header.TTL)
			}
		}
	}
	for _, r := range m.Answers {
		switch b := r.Body.(type) {
		case *dnsmessage.PTRResource:
			if b.PTR.String() != "Victor Box."+Service {
				t.Errorf("PTR: %s", b.PTR)
			}
		case *dnsmessage.SRVResource:
			if b.Port != 4711 || b.Target.String() != "victor.local." {
				t.Errorf("SRV: %+v", b)
			}
		case *dnsmessage.TXTResource:
			if strings.Join(b.TXT, ",") != "id=ab12,v=1.2" {
				t.Errorf("TXT (sorted): %v", b.TXT)
			}
		}
	}
	// The reply is understood by a browser.
	br := New(nil)
	br.record(m, &net.UDPAddr{IP: net.IPv4(10, 0, 0, 9), Port: 5353})
	if es := br.Entries(); len(es) != 1 || es[0].Instance != "Victor Box" || es[0].TXT["v"] != "1.2" {
		t.Errorf("browser: %+v", es)
	}
}

// The asker port is not 5353, so this goes the legacy path too; it checks a QU-class question is still
// recognised (class bit masked). The QU→unicast-to-5353 branch itself needs port 5353 and is not observed.
func TestFeature_Handle_QUBitIsUnicast(t *testing.T) {
	n := New(func() Info { return testInfo })
	asker := rig(t, n)
	qu := q(Service, dnsmessage.TypePTR)
	qu.Class |= 1 << 15
	if m := ask(t, n, asker, 1, qu); m == nil || types(m.Answers)[dnsmessage.TypePTR] != 1 {
		t.Fatalf("QU question must be answered (directly): %+v", m)
	}
}

func TestFeature_Handle_InstanceAndANY(t *testing.T) {
	n := New(func() Info { return testInfo })
	asker := rig(t, n)
	// SRV for the instance (case-insensitive) → SRV+TXT, no PTR.
	m := ask(t, n, asker, 2, q("victor box."+Service, dnsmessage.TypeSRV))
	if m == nil {
		t.Fatal("no reply to an instance SRV query")
	}
	if at := types(m.Answers); at[dnsmessage.TypePTR] != 0 || at[dnsmessage.TypeSRV] != 1 || at[dnsmessage.TypeTXT] != 1 {
		t.Errorf("instance answer: %v", at)
	}
	// ANY on the service → PTR included.
	if m := ask(t, n, asker, 3, q(Service, dnsmessage.TypeALL)); m == nil || types(m.Answers)[dnsmessage.TypePTR] != 1 {
		t.Errorf("ANY: %+v", m)
	}
}

func TestFeature_Handle_HostA(t *testing.T) {
	n := New(func() Info { return testInfo })
	asker := rig(t, n)
	m := ask(t, n, asker, 4, q("VICTOR.local.", dnsmessage.TypeA))
	if m == nil {
		t.Fatal("no reply to a host A query")
	}
	if len(m.Additionals) != 0 {
		t.Errorf("A records belong in the answer section: %+v", m.Additionals)
	}
	found := false
	for _, r := range m.Answers {
		if r.Header.Type != dnsmessage.TypeA {
			t.Errorf("only A records expected: %v", r.Header)
		}
		if a, ok := r.Body.(*dnsmessage.AResource); ok && net.IP(a.A[:]).Equal(net.IPv4(127, 0, 0, 1)) {
			found = true
		}
	}
	if !found {
		t.Errorf("loopback address missing: %+v", m.Answers)
	}
}

func TestFeature_Handle_ServicesEnumeration(t *testing.T) {
	n := New(func() Info { return testInfo })
	asker := rig(t, n)
	m := ask(t, n, asker, 5, q(servicesDN, dnsmessage.TypePTR))
	if m == nil || len(m.Answers) != 1 {
		t.Fatalf("enumeration reply: %+v", m)
	}
	r := m.Answers[0]
	p, ok := r.Body.(*dnsmessage.PTRResource)
	if !ok || r.Header.Name.String() != servicesDN || p.PTR.String() != Service {
		t.Errorf("enumeration answer: %+v", r)
	}
	// Combined with a service question: one reply with both.
	m = ask(t, n, asker, 6, q(Service, dnsmessage.TypePTR), q(servicesDN, dnsmessage.TypePTR))
	if m == nil || types(m.Answers)[dnsmessage.TypePTR] != 2 {
		t.Errorf("combined: %+v", m)
	}
}

func TestFeature_Handle_NothingToSay(t *testing.T) {
	browse := New(nil)
	asker := rig(t, browse)
	if m := ask(t, browse, asker, 7, q(Service, dnsmessage.TypePTR)); m != nil {
		t.Errorf("a browse-only node answered: %+v", m)
	}
	n := New(func() Info { return testInfo })
	asker = rig(t, n)
	for _, qq := range []dnsmessage.Question{
		q("_http._tcp.local.", dnsmessage.TypePTR),
		q("other."+Service, dnsmessage.TypeSRV),
		q("victor.local.", dnsmessage.TypeAAAA),
		q(Service, dnsmessage.TypeSRV),
	} {
		if m := ask(t, n, asker, 8, qq); m != nil {
			t.Errorf("answered an unrelated question %v: %+v", qq, m)
		}
	}
	n.handle([]byte("garbage"), asker.LocalAddr().(*net.UDPAddr), 0) // must not panic
}

func TestFeature_Handle_ResponseIsRecorded(t *testing.T) {
	src := New(func() Info { return testInfo })
	b, _ := src.response(testInfo, 0, ttl, true).Pack()
	n := New(nil) // pc nil: never sends
	n.handle(b, &net.UDPAddr{IP: net.IPv4(10, 0, 0, 5), Port: 5353}, 0)
	if es := n.Entries(); len(es) != 1 || es[0].Addrs[0] != "10.0.0.5" {
		t.Errorf("a response handled is recorded: %+v", es)
	}
}

// msg builds a response from resources.
func msg(rs ...dnsmessage.Resource) *dnsmessage.Message {
	return &dnsmessage.Message{Header: dnsmessage.Header{Response: true}, Answers: rs}
}

func rr(name string, ttl uint32, body dnsmessage.ResourceBody) dnsmessage.Resource {
	return dnsmessage.Resource{Header: dnsmessage.ResourceHeader{Name: dnsmessage.MustNewName(name), Class: dnsmessage.ClassINET, TTL: ttl}, Body: body}
}

func TestFeature_Record_MergeAndCase(t *testing.T) {
	n := New(nil)
	src := &net.UDPAddr{IP: net.IPv4(10, 0, 0, 1), Port: 5353}
	inst := "My Fleet." + Service

	// A bare PTR waits for the SRV.
	n.record(msg(rr(Service, 120, &dnsmessage.PTRResource{PTR: dnsmessage.MustNewName(inst)})), src)
	if len(n.Entries()) != 0 {
		t.Fatal("bare PTR created an entry")
	}
	// SRV (lower-cased name) + A records: the PTR in the same message keeps the original case.
	n.record(&dnsmessage.Message{Header: dnsmessage.Header{Response: true},
		Answers: []dnsmessage.Resource{
			rr(Service, 120, &dnsmessage.PTRResource{PTR: dnsmessage.MustNewName(inst)}),
			rr("my fleet."+Service, 120, &dnsmessage.SRVResource{Target: dnsmessage.MustNewName("box.local."), Port: 4711}),
		},
		Additionals: []dnsmessage.Resource{
			rr("BOX.local.", 120, &dnsmessage.AResource{A: [4]byte{10, 0, 0, 1}}), // the source again: not doubled
			rr("box.local.", 120, &dnsmessage.AResource{A: [4]byte{10, 0, 1, 2}}),
		}}, src)
	es := n.Entries()
	if len(es) != 1 {
		t.Fatalf("entries: %+v", es)
	}
	e := es[0]
	if e.Instance != "My Fleet" || e.Host != "box.local" || e.Port != 4711 {
		t.Errorf("entry: %+v", e)
	}
	if strings.Join(e.Addrs, ",") != "10.0.0.1,10.0.1.2" {
		t.Errorf("addrs (source first, no duplicate): %v", e.Addrs)
	}
	if e.TXT != nil {
		t.Errorf("no TXT yet: %v", e.TXT)
	}
	// A later TXT alone updates TXT (keys lower-cased, empty keys dropped) and keeps host/port.
	n.record(msg(rr("MY FLEET."+Service, 120, &dnsmessage.TXTResource{TXT: []string{"ID=x", "=bad", "flag"}})), src)
	e = n.Entries()[0]
	if e.Host != "box.local" || e.Port != 4711 || e.TXT["id"] != "x" || len(e.TXT) != 2 {
		t.Errorf("after TXT: %+v", e)
	}
	if _, ok := e.TXT["flag"]; !ok {
		t.Errorf("a key without value is kept: %v", e.TXT)
	}
	if e.Instance != "My Fleet" {
		t.Errorf("instance case lost on update: %q", e.Instance)
	}
	// Records of other services are ignored.
	n.record(msg(rr("x._http._tcp.local.", 120, &dnsmessage.SRVResource{Target: dnsmessage.MustNewName("h.local."), Port: 80})), src)
	if len(n.Entries()) != 1 {
		t.Error("foreign service recorded")
	}
	// Goodbye via PTR TTL 0.
	n.record(msg(rr(Service, 0, &dnsmessage.PTRResource{PTR: dnsmessage.MustNewName(inst)})), src)
	if len(n.Entries()) != 0 {
		t.Error("goodbye PTR did not remove the entry")
	}
}

func TestFeature_Record_Expiry(t *testing.T) {
	n := New(nil)
	src := &net.UDPAddr{IP: net.IPv4(10, 0, 0, 2), Port: 5353}
	n.record(msg(rr("a."+Service, 120, &dnsmessage.SRVResource{Target: dnsmessage.MustNewName("a.local."), Port: 1}),
		rr("b."+Service, 120, &dnsmessage.SRVResource{Target: dnsmessage.MustNewName("b.local."), Port: 2})), src)
	if es := n.Entries(); len(es) != 2 || es[0].Instance != "a" || es[1].Instance != "b" {
		t.Fatalf("sorted entries: %+v", es)
	}
	n.mu.Lock()
	n.entries["a"].expires = time.Now().Add(-time.Second)
	n.mu.Unlock()
	if es := n.Entries(); len(es) != 1 || es[0].Instance != "b" {
		t.Errorf("expired entry kept: %+v", es)
	}
	n.mu.Lock()
	_, still := n.entries["a"]
	exp := n.entries["b"].expires
	n.mu.Unlock()
	if still {
		t.Error("expired entry not pruned")
	}
	if d := time.Until(exp); d < 110*time.Second || d > 121*time.Second {
		t.Errorf("expiry follows the TTL: %v", d)
	}
	// Entries returns copies.
	es := n.Entries()
	es[0].Addrs[0] = "changed"
	if n.Entries()[0].Addrs[0] == "changed" {
		t.Error("Entries leaks internal slices")
	}
}

func TestFeature_Info_SameAndAnnouncement(t *testing.T) {
	a := testInfo
	b := Info{Instance: a.Instance, Host: a.Host, Port: a.Port, TXT: map[string]string{"id": "ab12", "v": "1.2"}}
	if !sameInfo(a, b) {
		t.Error("equal infos differ")
	}
	b.TXT["v"] = "1.3"
	if sameInfo(a, b) {
		t.Error("TXT change not seen")
	}
	if sameInfo(a, Info{Instance: a.Instance, Host: a.Host, Port: 1, TXT: a.TXT}) {
		t.Error("port change not seen")
	}
	browse := New(nil)
	if browse.announcement(0) != nil {
		t.Error("browse-only node has an announcement")
	}
	browse.announce() // records last, sends nothing
	browse.query()    // pc nil: no-op
	n := New(func() Info { return testInfo })
	n.announce() // pc nil: only remembers what it announced
	if !sameInfo(n.lastInfo(), testInfo) {
		t.Error("announce did not remember the info")
	}
	m := n.announcement(0)
	if m == nil {
		t.Fatal("no goodbye")
	}
	for _, r := range m.Answers {
		if r.Header.TTL != 0 {
			t.Errorf("goodbye TTL %d", r.Header.TTL)
		}
	}
	if txt := txtStrings(nil); len(txt) != 1 || txt[0] != "" {
		t.Errorf("empty TXT must be one empty string: %q", txt)
	}
}

// Any host on the LAN can announce endless instance names: the cache of a browser is bounded.
func TestFeature_Record_CacheIsBounded(t *testing.T) {
	n := New(func() Info { return testInfo })
	asker := rig(t, n)
	m := ask(t, n, asker, 0x4242, q(Service, dnsmessage.TypePTR))
	if m == nil {
		t.Fatal("no reply to record")
	}
	br := New(nil)
	for i := 0; i < maxEntries; i++ {
		br.entries[fmt.Sprintf("fake-%d", i)] = &Entry{Instance: "x", expires: time.Now().Add(time.Hour)}
	}
	br.record(m, &net.UDPAddr{IP: net.IPv4(10, 0, 0, 9), Port: 5353})
	if len(br.entries) != maxEntries {
		t.Fatalf("a full cache must not grow: %d", len(br.entries))
	}
}
