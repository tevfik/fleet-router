// Package mdns is a small multicast DNS / DNS-SD (RFC 6762, 6763) responder and browser for one service
// type — enough for fleets on the same network to find each other, and visible to the usual tools
// (`avahi-browse -r _fleet._tcp`, `dns-sd -B _fleet._tcp`).
//
// One socket does both jobs: it answers questions about this fleet and listens to every answer on the
// network, so fleets learn about each other from their own queries, from other fleets' queries and from
// the announcements a fleet sends when it starts (and the goodbye it sends when it stops). It shares port
// 5353 with avahi / mDNSResponder (SO_REUSEADDR).
package mdns

import (
	"context"
	"log"
	"net"
	"sort"
	"strings"
	"sync"
	"time"

	"golang.org/x/net/dns/dnsmessage"
	"golang.org/x/net/ipv4"
)

const (
	// Service is the DNS-SD service type fleets announce.
	Service    = "_fleet._tcp.local."
	servicesDN = "_services._dns-sd._udp.local."
	maxEntries = 512  // cached instances
	maxTTL     = 4500 // seconds a received record may live (RFC 6762 recommends 75 min at most)
	ttl        = 120  // seconds; queries every queryEvery keep live entries fresh
	queryEvery = 60 * time.Second
)

var group = &net.UDPAddr{IP: net.IPv4(224, 0, 0, 251), Port: 5353}

// Info is what this node announces. An empty Instance announces nothing (browse only).
type Info struct {
	Instance string            // shown name, unique on the network (one DNS label)
	Host     string            // host label, without ".local."
	Port     int               //
	TXT      map[string]string // key=value pairs
}

// Entry is one service instance seen on the network.
type Entry struct {
	Instance string            `json:"instance"`
	Host     string            `json:"host"` // e.g. "victor.local"
	Port     int               `json:"port"`
	TXT      map[string]string `json:"txt"`
	Addrs    []string          `json:"addrs"` // IPv4 addresses: the packet's source first, then the A records
	Seen     time.Time         `json:"seen"`
	expires  time.Time
}

// Node answers for one service instance and collects the instances others announce.
type Node struct {
	info func() Info

	mu      sync.Mutex
	entries map[string]*Entry // by lower-case instance name
	pc      *ipv4.PacketConn
	last    Info // what was announced last (re-announced when it changes)
}

// New returns a node; info is called whenever the current announcement is needed (nil: browse only).
func New(info func() Info) *Node {
	if info == nil {
		info = func() Info { return Info{} }
	}
	return &Node{info: info, entries: map[string]*Entry{}}
}

// Entries lists the live instances, sorted by name.
func (n *Node) Entries() []Entry {
	n.mu.Lock()
	defer n.mu.Unlock()
	now := time.Now()
	var out []Entry
	for k, e := range n.entries {
		if now.After(e.expires) {
			delete(n.entries, k)
			continue
		}
		c := *e
		c.Addrs = append([]string{}, e.Addrs...)
		out = append(out, c)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Instance < out[j].Instance })
	return out
}

// Run serves until ctx ends: it announces (twice), queries now and every minute, answers questions and
// records answers. On the way out it says goodbye, so the others drop this instance at once.
func (n *Node) Run(ctx context.Context) error {
	c, err := net.ListenMulticastUDP("udp4", nil, group)
	if err != nil {
		return err
	}
	pc := ipv4.NewPacketConn(c)
	pc.SetControlMessage(ipv4.FlagInterface, true)
	pc.SetMulticastTTL(255)
	pc.SetMulticastLoopback(true) // other fleets on this machine (and tests) hear it too
	n.pc = pc
	n.join()
	go func() {
		<-ctx.Done()
		n.send(n.announcement(0), nil) // goodbye
		c.Close()
	}()
	go n.tick(ctx)
	buf := make([]byte, 9000)
	for {
		k, cm, src, err := pc.ReadFrom(buf)
		if err != nil {
			if ctx.Err() != nil {
				return nil
			}
			return err
		}
		var ifi int
		if cm != nil {
			ifi = cm.IfIndex
		}
		ua, _ := src.(*net.UDPAddr)
		if ua == nil {
			continue
		}
		n.handle(buf[:k], ua, ifi)
	}
}

func (n *Node) tick(ctx context.Context) {
	n.announce()
	time.Sleep(time.Second)
	n.announce()
	n.query()
	t := time.NewTicker(queryEvery)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			n.join() // interfaces come and go (Wi-Fi, VPN)
			if cur := n.info(); !sameInfo(cur, n.lastInfo()) {
				n.announce()
			}
			n.query()
		}
	}
}

func (n *Node) lastInfo() Info { n.mu.Lock(); defer n.mu.Unlock(); return n.last }

func sameInfo(a, b Info) bool {
	if a.Instance != b.Instance || a.Host != b.Host || a.Port != b.Port || len(a.TXT) != len(b.TXT) {
		return false
	}
	for k, v := range a.TXT {
		if b.TXT[k] != v {
			return false
		}
	}
	return true
}

// interfaces are the ones multicast can leave through (loopback excluded).
func interfaces() []net.Interface {
	ifs, _ := net.Interfaces()
	var out []net.Interface
	for _, i := range ifs {
		if i.Flags&net.FlagUp != 0 && i.Flags&net.FlagMulticast != 0 && i.Flags&net.FlagLoopback == 0 {
			out = append(out, i)
		}
	}
	return out
}

func (n *Node) join() {
	for _, i := range interfaces() {
		i := i
		n.pc.JoinGroup(&i, group) // "already joined" is fine
	}
}

// send writes m to the multicast group on every interface (ifi 0) or on one, or unicast to dst.
func (n *Node) send(m *dnsmessage.Message, dst *net.UDPAddr, ifi ...int) {
	if m == nil || n.pc == nil {
		return
	}
	b, err := m.Pack()
	if err != nil {
		log.Printf("mdns: %v", err)
		return
	}
	if dst != nil {
		n.pc.WriteTo(b, nil, dst)
		return
	}
	targets := ifi
	if len(targets) == 0 || targets[0] == 0 {
		targets = nil
		for _, i := range interfaces() {
			targets = append(targets, i.Index)
		}
	}
	for _, idx := range targets {
		n.pc.WriteTo(b, &ipv4.ControlMessage{IfIndex: idx}, group)
	}
}

func (n *Node) announce() {
	info := n.info()
	n.mu.Lock()
	n.last = info
	n.mu.Unlock()
	if info.Instance == "" {
		return
	}
	for _, i := range interfaces() {
		if m := n.response(info, i.Index, ttl, true); m != nil {
			n.send(m, nil, i.Index)
		}
	}
}

// announcement is the full record set for all interfaces (used for the goodbye, TTL 0).
func (n *Node) announcement(t uint32) *dnsmessage.Message {
	info := n.lastInfo()
	if info.Instance == "" {
		return nil
	}
	return n.response(info, 0, t, true)
}

func (n *Node) query() {
	m := &dnsmessage.Message{Questions: []dnsmessage.Question{{Name: mustName(Service), Type: dnsmessage.TypePTR, Class: dnsmessage.ClassINET}}}
	n.send(m, nil)
}

func mustName(s string) dnsmessage.Name { return dnsmessage.MustNewName(s) }

func (i Info) fqdn() string { return label(i.Instance) + "." + Service }
func (i Info) host() string { return label(i.Host) + ".local." }

// label makes s usable as one DNS label: no dots, at most 63 bytes.
func label(s string) string {
	s = strings.ReplaceAll(s, ".", "-")
	for len(s) > 63 {
		s = s[:len(s)-1]
	}
	return s
}

// ipv4s lists the IPv4 addresses of interface idx (0: every multicast interface).
func ipv4s(idx int) []net.IP {
	var ifs []net.Interface
	if idx == 0 {
		ifs = interfaces()
	} else if i, err := net.InterfaceByIndex(idx); err == nil {
		ifs = []net.Interface{*i}
	}
	var out []net.IP
	for _, i := range ifs {
		as, _ := i.Addrs()
		for _, a := range as {
			if ipn, ok := a.(*net.IPNet); ok {
				if v4 := ipn.IP.To4(); v4 != nil && !v4.IsLinkLocalUnicast() {
					out = append(out, v4)
				}
			}
		}
	}
	return out
}

func txtStrings(m map[string]string) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	var out []string
	for _, k := range keys {
		out = append(out, k+"="+m[k])
	}
	if len(out) == 0 {
		out = []string{""}
	}
	return out
}

// response builds PTR + SRV + TXT + A for info; with withPTR false only the instance and host records.
func (n *Node) response(info Info, ifi int, t uint32, withPTR bool) *dnsmessage.Message {
	inst, host := mustName(info.fqdn()), mustName(info.host())
	hdr := func(name dnsmessage.Name, typ dnsmessage.Type, flush bool) dnsmessage.ResourceHeader {
		class := dnsmessage.ClassINET
		if flush {
			class |= 1 << 15 // cache-flush: these records are ours alone
		}
		return dnsmessage.ResourceHeader{Name: name, Type: typ, Class: class, TTL: t}
	}
	m := &dnsmessage.Message{Header: dnsmessage.Header{Response: true, Authoritative: true}}
	if withPTR {
		m.Answers = append(m.Answers, dnsmessage.Resource{Header: hdr(mustName(Service), dnsmessage.TypePTR, false), Body: &dnsmessage.PTRResource{PTR: inst}})
	}
	m.Answers = append(m.Answers,
		dnsmessage.Resource{Header: hdr(inst, dnsmessage.TypeSRV, true), Body: &dnsmessage.SRVResource{Target: host, Port: uint16(info.Port)}},
		dnsmessage.Resource{Header: hdr(inst, dnsmessage.TypeTXT, true), Body: &dnsmessage.TXTResource{TXT: txtStrings(info.TXT)}})
	for _, ip := range ipv4s(ifi) {
		var a [4]byte
		copy(a[:], ip)
		m.Additionals = append(m.Additionals, dnsmessage.Resource{Header: hdr(host, dnsmessage.TypeA, true), Body: &dnsmessage.AResource{A: a}})
	}
	return m
}

func (n *Node) handle(b []byte, src *net.UDPAddr, ifi int) {
	var m dnsmessage.Message
	if err := m.Unpack(b); err != nil {
		return
	}
	if m.Response {
		n.record(&m, src)
		return
	}
	info := n.info()
	if info.Instance == "" {
		return
	}
	inst, host := strings.ToLower(info.fqdn()), strings.ToLower(info.host())
	var want struct{ ptr, svc, inst, host, unicast bool }
	for _, q := range m.Questions {
		name := strings.ToLower(q.Name.String())
		all := q.Type == dnsmessage.TypeALL
		switch {
		case name == strings.ToLower(Service) && (q.Type == dnsmessage.TypePTR || all):
			want.ptr = true
		case name == servicesDN && (q.Type == dnsmessage.TypePTR || all):
			want.svc = true
		case name == inst && (q.Type == dnsmessage.TypeSRV || q.Type == dnsmessage.TypeTXT || all):
			want.inst = true
		case name == host && (q.Type == dnsmessage.TypeA || all):
			want.host = true
		default:
			continue
		}
		want.unicast = want.unicast || q.Class&(1<<15) != 0 // QU: the asker wants a unicast answer
	}
	var r *dnsmessage.Message
	switch {
	case want.ptr || want.inst:
		r = n.response(info, ifi, ttl, want.ptr)
	case want.host:
		r = n.response(info, ifi, ttl, false)
		r.Answers, r.Additionals = r.Additionals, nil
	}
	if want.svc {
		if r == nil {
			r = &dnsmessage.Message{Header: dnsmessage.Header{Response: true, Authoritative: true}}
		}
		r.Answers = append(r.Answers, dnsmessage.Resource{
			Header: dnsmessage.ResourceHeader{Name: mustName(servicesDN), Type: dnsmessage.TypePTR, Class: dnsmessage.ClassINET, TTL: ttl},
			Body:   &dnsmessage.PTRResource{PTR: mustName(Service)}})
	}
	if r == nil {
		return
	}
	switch {
	case src.Port != 5353: // a one-shot ("legacy") resolver: answer it directly, echoing id and question
		r.ID, r.Questions = m.ID, m.Questions
		for _, rs := range [][]dnsmessage.Resource{r.Answers, r.Additionals} {
			for i := range rs { // RFC 6762 §6.7: no cache-flush bit, short TTL
				rs[i].Header.Class &^= 1 << 15
				rs[i].Header.TTL = min(rs[i].Header.TTL, 10)
			}
		}
		n.send(r, src)
	case want.unicast:
		n.send(r, src)
	default:
		n.send(r, nil, ifi)
	}
}

// record takes in what an answer says about instances of Service.
func (n *Node) record(m *dnsmessage.Message, src *net.UDPAddr) {
	type part struct {
		host  string
		port  int
		txt   map[string]string
		ttl   uint32
		gotSR bool
	}
	insts := map[string]*part{}
	get := func(name string) *part {
		p := insts[name]
		if p == nil {
			p = &part{}
			insts[name] = p
		}
		return p
	}
	hosts := map[string][]string{}
	svc := strings.ToLower(Service)
	for _, rr := range append(append([]dnsmessage.Resource{}, m.Answers...), m.Additionals...) {
		name := strings.ToLower(rr.Header.Name.String())
		switch body := rr.Body.(type) {
		case *dnsmessage.PTRResource:
			if name == svc {
				p := get(strings.ToLower(body.PTR.String()))
				p.ttl, p.gotSR = rr.Header.TTL, true
			}
		case *dnsmessage.SRVResource:
			if strings.HasSuffix(name, "."+svc) {
				p := get(name)
				p.host, p.port, p.ttl, p.gotSR = strings.TrimSuffix(body.Target.String(), "."), int(body.Port), rr.Header.TTL, true
			}
		case *dnsmessage.TXTResource:
			if strings.HasSuffix(name, "."+svc) {
				p := get(name)
				p.txt = map[string]string{}
				for _, s := range body.TXT {
					if k, v, _ := strings.Cut(s, "="); k != "" {
						p.txt[strings.ToLower(k)] = v
					}
				}
			}
		case *dnsmessage.AResource:
			hosts[strings.TrimSuffix(name, ".")] = append(hosts[strings.TrimSuffix(name, ".")], net.IP(body.A[:]).String())
		}
	}
	if len(insts) == 0 {
		return
	}
	now := time.Now()
	n.mu.Lock()
	defer n.mu.Unlock()
	for name, p := range insts {
		key := strings.TrimSuffix(name, "."+svc)
		if p.gotSR && p.ttl == 0 { // goodbye
			delete(n.entries, key)
			continue
		}
		e := n.entries[key]
		if e == nil {
			if len(n.entries) >= maxEntries { // any host on the LAN can invent instance names: the cache is bounded
				continue
			}
			if p.host == "" { // a bare PTR: wait for the SRV (the responder sends it along)
				continue
			}
			e = &Entry{Instance: instanceName(m, name)}
			n.entries[key] = e
		}
		if p.host != "" {
			e.Host, e.Port = p.host, p.port
		}
		if p.txt != nil {
			e.TXT = p.txt
		}
		addrs := []string{src.IP.String()}
		for _, a := range hosts[strings.ToLower(e.Host)] {
			if a != addrs[0] {
				addrs = append(addrs, a)
			}
		}
		e.Addrs, e.Seen = addrs, now
		t := p.ttl
		if t == 0 {
			t = ttl
		}
		t = min(t, maxTTL) // a record can claim 2^32 s
		e.expires = now.Add(time.Duration(t) * time.Second)
	}
}

// instanceName recovers the instance label with its original case.
func instanceName(m *dnsmessage.Message, lower string) string {
	for _, rr := range append(append([]dnsmessage.Resource{}, m.Answers...), m.Additionals...) {
		if s := rr.Header.Name.String(); strings.ToLower(s) == lower {
			return strings.TrimSuffix(s, "."+Service)
		}
		if p, ok := rr.Body.(*dnsmessage.PTRResource); ok && strings.ToLower(p.PTR.String()) == lower {
			return strings.TrimSuffix(p.PTR.String(), "."+Service)
		}
	}
	return strings.TrimSuffix(lower, "."+strings.ToLower(Service))
}
