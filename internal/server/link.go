package server

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"os/exec"
	"strings"
	"time"
)

// What line a file transfer would use. A model file is gigabytes: over the LAN it is minutes, over the internet (or
// a hub tunnel) it is a long time and somebody's data allowance. The machines that receive a file can tell which one
// it is from the address of the route to the machine that has it; `fleet store pull` asks first and wants a yes
// for a big file on a line that is not the LAN.

// LinkInfo describes the line a pull of sha would take on this machine.
type LinkInfo struct {
	Node      string `json:"node"`  // the machine that receives
	From      string `json:"from"`  // the machine that has the file
	Route     string `json:"route"` // how it is reached ("lan 10.0.1.5", "ssh dino", …)
	Kind      string `json:"kind"`  // lan | wan | vpn | peer | unknown
	LimitMbps int    `json:"limit_mbps,omitempty"`
}

// privateKind: lan for private / loopback / link-local addresses, vpn for the shared 100.64/10 range, wan otherwise.
func privateKind(host string) string {
	var ips []net.IP
	if ip := net.ParseIP(host); ip != nil {
		ips = append(ips, ip)
	} else {
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()
		addrs, err := net.DefaultResolver.LookupIPAddr(ctx, host)
		if err != nil {
			return "unknown"
		}
		for _, a := range addrs {
			ips = append(ips, a.IP)
		}
	}
	if len(ips) == 0 {
		return "unknown"
	}
	for _, ip := range ips {
		if ip.IsLoopback() || ip.IsPrivate() || ip.IsLinkLocalUnicast() {
			continue
		}
		if ip4 := ip.To4(); ip4 != nil && ip4[0] == 100 && ip4[1]&0xc0 == 64 {
			return "vpn"
		}
		return "wan"
	}
	return "lan"
}

// sshHostName is the host an ssh alias really connects to (ssh -G), or the alias itself.
var sshHostName = func(alias string) string {
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	out, err := exec.CommandContext(ctx, "ssh", "-G", "--", alias).Output()
	if err != nil {
		return alias
	}
	for _, ln := range strings.Split(string(out), "\n") {
		if h, ok := strings.CutPrefix(ln, "hostname "); ok {
			return strings.TrimSpace(h)
		}
	}
	return alias
}

// routeKind classifies one route to a machine.
func routeKind(r nodeRoute) string {
	switch {
	case strings.HasPrefix(r.source, "lan "):
		return "lan"
	case r.peer != nil:
		return "peer"
	case r.ssh != "":
		return privateKind(sshHostName(r.ssh))
	case r.base != "":
		h := strings.TrimPrefix(strings.TrimPrefix(r.base, "http://"), "https://")
		if host, _, err := net.SplitHostPort(h); err == nil {
			h = host
		}
		return privateKind(h)
	}
	return "unknown"
}

// LinkFor says which line the pull of sha onto this machine would use (the first route that is tried).
func (s *Server) LinkFor(ctx context.Context, from, sha string) (LinkInfo, error) {
	li := LinkInfo{Node: s.Config().Name}
	if m := s.Config().PullMbps; m > 0 {
		li.LimitMbps = m
	}
	var holders []string
	for _, x := range s.StoreAll(ctx) {
		if x.Store == nil || s.isSelfNode(x.Node) {
			continue
		}
		for _, f := range x.Store.Files {
			if f.SHA256 == sha && f.Complete && (from == "" || strings.EqualFold(x.Node, from)) {
				holders = append(holders, x.Node)
			}
		}
	}
	if len(holders) == 0 {
		return li, fmt.Errorf("no other machine has that file")
	}
	li.From = holders[0]
	routes := s.nodeRoutes(li.From)
	if len(routes) == 0 {
		li.Kind = "unknown"
		return li, nil
	}
	li.Route, li.Kind = routes[0].source, routeKind(routes[0])
	return li, nil
}

func (s *Server) linkHandlers(mux *http.ServeMux, adm func(http.HandlerFunc) http.HandlerFunc) {
	mux.HandleFunc("POST /fleet/store/link", adm(func(w http.ResponseWriter, r *http.Request) {
		var in struct {
			Node string `json:"node"`
			From string `json:"from"`
			SHA  string `json:"sha256"`
		}
		if err := json.NewDecoder(io.LimitReader(r.Body, 1<<20)).Decode(&in); err != nil || in.SHA == "" {
			httpErr(w, 400, "sha256 is required")
			return
		}
		if in.Node != "" && !s.isSelfNode(in.Node) { // the line is the receiving machine's
			code, out, err := s.forwardStore(r.Context(), in.Node, "/fleet/store/link", map[string]string{"from": in.From, "sha256": in.SHA})
			if err != nil {
				httpErr(w, 502, err.Error())
				return
			}
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(code)
			w.Write(out)
			return
		}
		li, err := s.LinkFor(r.Context(), in.From, in.SHA)
		if err != nil {
			httpErr(w, 404, err.Error())
			return
		}
		if in.Node != "" {
			li.Node = in.Node
		}
		writeJSON(w, li)
	}))
}
