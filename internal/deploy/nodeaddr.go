package deploy

import (
	"bufio"
	"context"
	"fmt"
	"net"
	"net/url"
	"os/exec"
	"strings"
	"sync"
	"time"

	"github.com/tevfik/fleet-router/internal/config"
)

// Test seams for ResolveNodeIP: the fleet config's address for a node, and the IP ssh's own config gives it ("" = none).
var (
	configNodeAddr = defaultConfigNodeAddr
	sshConfigAddr  = defaultSSHConfigAddr
)

// ResolveNodeIP maps a machine name to the address another machine of the cluster dials it on. In order: the node given
// as an IP; the address fleet's config has for it (its nodes: entry, else an endpoint of that node reached by a direct
// URL); the IP `ssh -G` gives as its HostName (only without ProxyJump/ProxyCommand: a proxied host's HostName is an
// address on the far side); else the name unchanged. fleet has no built-in table of machines.
func ResolveNodeIP(node string) string {
	n := strings.TrimSpace(node)
	if net.ParseIP(n) != nil {
		return n
	}
	if a := configNodeAddr(n); a != "" {
		return a
	}
	if a := sshConfigAddr(n); a != "" {
		return a
	}
	return node
}

// NodeAddrFromConfig returns the IP of node from cfg: the addr of its nodes: entry (fleet config node set), else the
// address of an endpoint that runs on it and is reached by a direct URL (not through SSH or a peer fleet): the address
// this machine already uses for that node. Loopback is never returned.
func NodeAddrFromConfig(cfg *config.Config, node string) string {
	if cfg == nil || node == "" {
		return ""
	}
	if n, ok := cfg.Node(node); ok {
		if ip := net.ParseIP(n.Addr); ip != nil && !ip.IsLoopback() && !ip.IsUnspecified() {
			return ip.String()
		}
	}
	for _, e := range cfg.Endpoints {
		if !strings.EqualFold(e.Node, node) || e.URL == "" || e.SSH != "" || e.Peer != "" {
			continue
		}
		u, err := url.Parse(e.URL)
		if err != nil {
			continue
		}
		if ip := net.ParseIP(u.Hostname()); ip != nil && !ip.IsLoopback() && !ip.IsUnspecified() {
			return ip.String()
		}
	}
	return ""
}

var (
	cfgOnce   sync.Once
	cfgCached *config.Config
)

func defaultConfigNodeAddr(node string) string {
	cfgOnce.Do(func() {
		if c, err := config.Load(config.Path()); err == nil {
			cfgCached = c
		}
	})
	return NodeAddrFromConfig(cfgCached, node)
}

var sshAddrCache sync.Map // node -> string

// defaultSSHConfigAddr asks `ssh -G` (reads the ssh config, connects nowhere) for the node's HostName.
func defaultSSHConfigAddr(node string) string {
	if node == "" || strings.ContainsAny(node, " \t\n'\"$;&|<>`\\") || strings.HasPrefix(node, "-") {
		return ""
	}
	if v, ok := sshAddrCache.Load(node); ok {
		return v.(string)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	out, err := exec.CommandContext(ctx, "ssh", "-G", "--", node).Output()
	addr := ""
	if err == nil {
		addr = parseSSHConfigAddr(string(out))
	}
	sshAddrCache.Store(node, addr)
	return addr
}

// parseSSHConfigAddr picks the HostName from `ssh -G` output when it is a non-loopback IP and the host is reached
// directly (no ProxyJump / ProxyCommand).
func parseSSHConfigAddr(out string) string {
	host := ""
	sc := bufio.NewScanner(strings.NewReader(out))
	for sc.Scan() {
		k, v, ok := strings.Cut(strings.TrimSpace(sc.Text()), " ")
		if !ok {
			continue
		}
		v = strings.TrimSpace(v)
		switch strings.ToLower(k) {
		case "hostname":
			host = v
		case "proxyjump", "proxycommand":
			if v != "" && !strings.EqualFold(v, "none") {
				return ""
			}
		}
	}
	if ip := net.ParseIP(host); ip != nil && !ip.IsLoopback() && !ip.IsUnspecified() {
		return ip.String()
	}
	return ""
}

// Seams: the addresses on this machine's interfaces, and its address towards the outside (a laptop changes network:
// DHCP gives it another address and the nodes: addr of the config goes stale).
var (
	interfaceIPs = func() []net.IP {
		var out []net.IP
		addrs, _ := net.InterfaceAddrs()
		for _, a := range addrs {
			if ipn, ok := a.(*net.IPNet); ok {
				out = append(out, ipn.IP)
			}
		}
		return out
	}
	outboundIP = func() net.IP {
		c, err := net.Dial("udp", "192.0.2.1:9") // documentation address: no packet is sent, the OS only picks the route
		if err != nil {
			return nil
		}
		defer c.Close()
		if u, ok := c.LocalAddr().(*net.UDPAddr); ok {
			return u.IP
		}
		return nil
	}
)

// ownAddr corrects the address configured for THIS machine: when it is not on any interface (the machine moved to
// another network) the address of the route to the outside is used instead. fixed reports that it was replaced.
// A non-IP or loopback value is returned unchanged.
func ownAddr(configured string) (addr string, fixed bool) {
	ip := net.ParseIP(configured)
	if ip == nil || ip.IsLoopback() || ip.IsUnspecified() {
		return configured, false
	}
	for _, have := range interfaceIPs() {
		if have.Equal(ip) {
			return configured, false
		}
	}
	if out := outboundIP(); out != nil && !out.IsLoopback() && !out.IsUnspecified() {
		return out.String(), true
	}
	return configured, false
}

// ResolveStageHost is the address a chain stage of node is reached at: ResolveNodeIP, except that for this machine a
// configured address that is no longer on any interface (DHCP gave it another one) is replaced by the current one.
func ResolveStageHost(node string) string {
	ip := ResolveNodeIP(node)
	if isThisMachine(node) {
		if cur, fixed := ownAddr(ip); fixed {
			return cur
		}
	}
	return ip
}

// staleOwnAddr says, for a node that is this machine, that its configured address is not on any interface ("" when
// it is, or the node is another machine).
func staleOwnAddr(node string) string {
	if !isThisMachine(node) {
		return ""
	}
	ip := ResolveNodeIP(node)
	if cur, fixed := ownAddr(ip); fixed {
		return fmt.Sprintf("the address configured for %s (%s) is not on this machine any more: %s is used (fix it: fleet config node set %s addr=%s)", node, ip, cur, node, cur)
	}
	return ""
}
