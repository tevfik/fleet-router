package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"log"
	"net"
	"os"
	"os/signal"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/tevfik/fleet-router/internal/config"
	"github.com/tevfik/fleet-router/internal/tunnel"
)

// portList is a repeatable --stream-port flag (also "50160,50161").
type portList []int

func (p *portList) String() string {
	s := make([]string, len(*p))
	for i, v := range *p {
		s[i] = strconv.Itoa(v)
	}
	return strings.Join(s, ",")
}

func (p *portList) Set(v string) error {
	for _, f := range strings.Split(v, ",") {
		f = strings.TrimSpace(f)
		if f == "" {
			continue
		}
		n, err := strconv.Atoi(f)
		if err != nil || n <= 0 || n > 65535 {
			return fmt.Errorf("bad port %q", f)
		}
		*p = append(*p, n)
	}
	return nil
}

const streamUsage = `usage: fleet stream ls   (the stream connects running on this machine)
       fleet stream connect <node> <remote-port> [--listen 127.0.0.1:P] [--hub URL] [--key K] [--insecure]

Opens a local TCP listener; every connection to it becomes a raw TCP stream through the hub to
127.0.0.1:<remote-port> on <node>, a node joined with fleet join --stream-port <remote-port>.
  --listen    local address (default 127.0.0.1:<remote-port>)
  --hub       hub URL (default: hub_url of the config, else the build's default hub, if any)
  --key       API key for the hub (default: $FLEET_API_KEY, else the key of api_key_env)
  --insecure  do not verify the hub's TLS certificate`

// cmdStream runs the master side of raw TCP streams through a hub.
func cmdStream(args []string) error {
	if len(args) > 0 && args[0] == "ls" {
		return cmdStreamLs()
	}
	if len(args) == 0 || args[0] != "connect" {
		return fmt.Errorf("%s", streamUsage)
	}
	fs := flag.NewFlagSet("stream connect", flag.ExitOnError)
	listenAddr := fs.String("listen", "", "local listen address (default 127.0.0.1:<remote-port>)")
	hub := fs.String("hub", "", "hub URL")
	key := fs.String("key", "", "API key for the hub")
	insecure := fs.Bool("insecure", false, "do not verify the hub's TLS certificate")
	fs.Usage = func() { fmt.Fprintln(os.Stderr, streamUsage) }
	fs.Parse(flagsFirst(args[1:], "listen", "hub", "key"))
	if fs.NArg() != 2 {
		return fmt.Errorf("%s", streamUsage)
	}
	node := fs.Arg(0)
	port, err := strconv.Atoi(fs.Arg(1))
	if err != nil || port <= 0 || port > 65535 {
		return fmt.Errorf("bad remote port %q", fs.Arg(1))
	}
	cfg, _ := config.Load(config.Path())
	hubURL := *hub
	if hubURL == "" && cfg != nil {
		hubURL = cfg.HubURL
	}
	if hubURL, err = config.HubOrDefault(hubURL); err != nil {
		return err
	}
	apiKey := *key
	if apiKey == "" {
		apiKey, _ = config.ResolveHubAPIKey(cfg) // $FLEET_API_KEY > .env > hub_api_key > api_key_env
	}
	if apiKey == "" {
		return fmt.Errorf("no API key for the hub: --key, FLEET_API_KEY (fleet key set) or api_key_env")
	}
	if cfg != nil && cfg.HubInsecure {
		*insecure = true
	}
	addr := *listenAddr
	if addr == "" {
		addr = net.JoinHostPort("127.0.0.1", strconv.Itoa(port))
	}
	ln, err := net.Listen("tcp", addr)
	if err != nil {
		return listenError(addr, err)
	}
	pf, perr := writeStreamPID(streamInfo{PID: os.Getpid(), Node: node, Port: port, Listen: ln.Addr().String(), Hub: hubURL})
	if perr != nil {
		log.Printf("stream: no pid file (%v): `fleet stream ls` will not list this one", perr)
	} else {
		defer os.Remove(pf)
	}
	sc := &tunnel.StreamConnector{HubURL: hubURL, Node: node, Port: port, Key: apiKey, Insecure: *insecure, Logf: log.Printf}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	fmt.Printf("✓ %s → %s:%d through %s (Ctrl-C stops; pid %d)\n", ln.Addr(), node, port, hubURL, os.Getpid())
	if *insecure {
		fmt.Println("  ! the hub's TLS certificate is NOT verified")
	}
	log.Printf("stream: serving %s → %s:%d via %s; a dropped connection (hub restart) is redialed on the next connection, new ones retry while the hub is down", ln.Addr(), node, port, hubURL)
	return sc.Serve(ctx, ln)
}

// streamInfo is the pid file of one running `fleet stream connect`.
type streamInfo struct {
	PID     int       `json:"pid"`
	Node    string    `json:"node"`
	Port    int       `json:"port"`
	Listen  string    `json:"listen"`
	Hub     string    `json:"hub"`
	Started time.Time `json:"started"`
}

// streamDir keeps one JSON file per running stream connect, named after its listen port.
func streamDir() string { return filepath.Join(filepath.Dir(config.StatsPath()), "streams") }

func writeStreamPID(si streamInfo) (string, error) {
	si.Started = time.Now()
	_, port, _ := net.SplitHostPort(si.Listen)
	if err := os.MkdirAll(streamDir(), 0o700); err != nil {
		return "", err
	}
	b, _ := json.Marshal(si)
	p := filepath.Join(streamDir(), port+".json")
	return p, os.WriteFile(p, b, 0o600)
}

func pidAlive(pid int) bool {
	if pid <= 0 {
		return false
	}
	p, err := os.FindProcess(pid)
	return err == nil && p.Signal(syscall.Signal(0)) == nil
}

// runningStreams lists the live stream connects; files of dead processes are removed.
func runningStreams() []streamInfo {
	ents, _ := os.ReadDir(streamDir())
	var out []streamInfo
	for _, e := range ents {
		p := filepath.Join(streamDir(), e.Name())
		b, err := os.ReadFile(p)
		var si streamInfo
		if err != nil || json.Unmarshal(b, &si) != nil || !pidAlive(si.PID) {
			_ = os.Remove(p)
			continue
		}
		out = append(out, si)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Listen < out[j].Listen })
	return out
}

func cmdStreamLs() error {
	ss := runningStreams()
	if len(ss) == 0 {
		fmt.Println("no stream connects running on this machine (fleet stream connect <node> <port>)")
		return nil
	}
	for _, s := range ss {
		fmt.Printf("  pid %-7d %-22s → %s:%d via %s (since %s)\n", s.PID, s.Listen, s.Node, s.Port, s.Hub, time.Since(s.Started).Round(time.Second))
	}
	return nil
}

// listenError explains a failed listen: when another stream connect of ours holds the port, which one.
func listenError(addr string, err error) error {
	if !errors.Is(err, syscall.EADDRINUSE) {
		return fmt.Errorf("listen on %s: %w (choose another with --listen)", addr, err)
	}
	_, port, _ := net.SplitHostPort(addr)
	for _, s := range runningStreams() {
		if _, p, _ := net.SplitHostPort(s.Listen); p == port {
			return fmt.Errorf("listen on %s: the port is already served by fleet stream connect pid %d (→ %s:%d via %s); stop it (kill %d) or choose another with --listen", addr, s.PID, s.Node, s.Port, s.Hub, s.PID)
		}
	}
	return fmt.Errorf("listen on %s: the port is in use by another program (ss -ltnp | grep :%s); choose another with --listen", addr, port)
}
