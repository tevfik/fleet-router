package server

import (
	"context"
	"errors"
	"fmt"
	"log"
	"strings"
	"time"

	"github.com/tevfik/fleet-router/internal/config"
	"github.com/tevfik/fleet-router/internal/tunnel"
)

// startTunnelClient opens the outbound tunnel to a hub (a package var: tests replace it).
var startTunnelClient = tunnel.StartClientOpts

// joinState is what the daemon knows about getting into the hub while there is no tunnel client yet (or it failed
// to start one): the auto-join loop's last error and when it tries next.
type joinState struct {
	Joining   bool // an auto-join loop is retrying
	LastError string
	LastAt    time.Time
	NextRetry time.Time
	Attempts  int
	Rejected  bool // the hub answered 401/403 or refused the node: the same token/key will not work
}

func joinRefused(err error) bool {
	var rej *tunnel.RejectedError
	var se *tunnel.StatusError
	return errors.As(err, &rej) || (errors.As(err, &se) && se.IsAuth())
}

func (s *Server) setJoinState(f func(*joinState)) {
	s.tmu.Lock()
	f(&s.joinState)
	s.tmu.Unlock()
}

// hubStatus is the body of GET /fleet/tunnel/status: the node's side (is its tunnel up, why not, when is the next try)
// and the hub's side (nodes joined, streams open).
func (s *Server) hubStatus() map[string]any {
	cfg := s.Config()
	s.tmu.Lock()
	cli, hubURL, js := s.tunnelClient, s.tunnelHubURL, s.joinState
	s.tmu.Unlock()
	out := map[string]any{"connected": false, "hub_url": hubURL, "auto_join": cfg.HubAutoJoin, "insecure": cfg.HubInsecure}
	if hubURL == "" && js.Joining {
		out["hub_url"] = cfg.HubURL
	}
	var st tunnel.Status
	if cli != nil {
		st = cli.Status()
		out["connected"] = st.Connected
	}
	if !st.Connected {
		// the client's own last failure wins; before it exists, the auto-join loop's
		switch {
		case st.LastError != "":
		case js.LastError != "":
			st = tunnel.Status{LastError: js.LastError, LastErrorAt: js.LastAt, NextRetry: js.NextRetry, Attempts: js.Attempts, Rejected: js.Rejected}
		}
	}
	out["status"] = st
	if s.hub != nil {
		nodes := s.hub.ConnectedNodes()
		for i := range nodes {
			nodes[i].AuthToken = "" // the join token authenticates the node (stream access, name takeover): never in a status reply
		}
		out["nodes"] = nodes
		if as := s.hub.ActiveStreams(); len(as) > 0 {
			out["active_streams"] = as
		}
	}
	return out
}

// Auto-join retry bounds (package vars: tests shorten them).
var (
	autoJoinFirstWait = 5 * time.Second
	autoJoinMaxWait   = time.Minute
)

func (s *Server) bumpJoinGen() int {
	s.tmu.Lock()
	defer s.tmu.Unlock()
	s.joinGen++
	return s.joinGen
}

// joinHub replaces the current hub connection (if any) by one to hubURL. gen is the join generation the caller
// took with bumpJoinGen: a join or leave that came later wins, an older attempt is dropped.
func (s *Server) joinHub(gen int, hubURL, token string, streamPorts []int, insecure bool) error {
	cfg := s.Config()
	var myModels []string
	for _, ep := range cfg.Endpoints {
		if ep.Node == cfg.Name || ep.Node == "" {
			myModels = append(myModels, ep.Model)
		}
	}
	reg := tunnel.NodeRegistration{
		NodeName:     cfg.Name,
		Models:       myModels,
		JoinedAt:     time.Now(),
		AuthToken:    token,
		StreamPorts:  streamPorts,
		FleetVersion: Version, EngineVersion: Engine,
	}
	listen := cfg.Listen
	if listen == "" {
		listen = "127.0.0.1:4711"
	}
	localAddr := "http://" + listen
	if strings.HasPrefix(listen, "0.0.0.0:") {
		localAddr = "http://127.0.0.1" + strings.TrimPrefix(listen, "0.0.0.0")
	}
	s.tmu.Lock()
	if gen != s.joinGen {
		s.tmu.Unlock()
		return fmt.Errorf("superseded by a later join or leave")
	}
	old := s.tunnelClient
	s.tunnelClient, s.tunnelHubURL = nil, ""
	s.tmu.Unlock()
	if old != nil {
		old.Close()
	}
	key, _ := config.ResolveHubAPIKey(cfg)
	cli, err := startTunnelClient(hubURL, localAddr, reg, tunnel.Options{APIKey: key, Insecure: insecure})
	if err != nil {
		return err
	}
	s.setJoinState(func(j *joinState) { *j = joinState{} })
	s.tmu.Lock()
	defer s.tmu.Unlock()
	if gen != s.joinGen { // a leave or another join came while we connected
		cli.Close()
		return fmt.Errorf("superseded by a later join or leave")
	}
	s.tunnelClient, s.tunnelHubURL = cli, hubURL
	return nil
}

func (s *Server) leaveHub() {
	s.tmu.Lock()
	s.joinGen++
	old := s.tunnelClient
	s.tunnelClient, s.tunnelHubURL = nil, ""
	s.joinState = joinState{}
	s.tmu.Unlock()
	if old != nil {
		old.Close()
	}
}

// rememberJoin saves a successful join so that the daemon joins again at its next start.
func (s *Server) rememberJoin(hubURL, token string, streamPorts []int, insecure bool) error {
	cfg := s.Config()
	cfg.HubURL, cfg.HubToken, cfg.HubAutoJoin, cfg.HubInsecure = hubURL, token, true, insecure
	cfg.HubStreamPorts = append([]int(nil), streamPorts...)
	return s.SetConfig(cfg)
}

// mergePorts returns have followed by the ports of add it does not contain yet (order kept, no duplicates).
func mergePorts(have, add []int) []int {
	out := append([]int(nil), have...)
	for _, p := range add {
		dup := false
		for _, q := range out {
			if q == p {
				dup = true
				break
			}
		}
		if !dup {
			out = append(out, p)
		}
	}
	return out
}

func (s *Server) forgetJoin() error {
	cfg := s.Config()
	if !cfg.HubAutoJoin {
		return nil
	}
	cfg.HubAutoJoin = false
	return s.SetConfig(cfg)
}

// autoJoinLoop joins the remembered hub at start; while the hub cannot be reached it retries with a growing wait
// (5 s .. 1 min). Once connected, the tunnel client itself reconnects after drops. A join or leave meanwhile ends it.
func (s *Server) autoJoinLoop(ctx context.Context) {
	cfg := s.Config()
	if !cfg.HubAutoJoin || cfg.HubURL == "" {
		return
	}
	gen := s.bumpJoinGen()
	wait := autoJoinFirstWait
	attempts := 0
	s.setJoinState(func(j *joinState) { *j = joinState{Joining: true} })
	for {
		err := s.joinHub(gen, cfg.HubURL, cfg.HubToken, cfg.HubStreamPorts, cfg.HubInsecure)
		if err == nil {
			log.Printf("hub: joined %s again (hub_auto_join; stream ports %v)", cfg.HubURL, cfg.HubStreamPorts)
			return
		}
		s.tmu.Lock()
		stale := gen != s.joinGen
		s.tmu.Unlock()
		if stale {
			return
		}
		attempts++
		refused := joinRefused(err)
		s.setJoinState(func(j *joinState) {
			*j = joinState{Joining: true, LastError: err.Error(), LastAt: time.Now(), NextRetry: time.Now().Add(wait), Attempts: attempts, Rejected: refused}
		})
		if refused {
			log.Printf("hub: %s refused this node (%v); retrying in %s. The token or API key will not be accepted until it is changed: fleet key set / fleet join --token", cfg.HubURL, err, wait)
		} else {
			log.Printf("hub: cannot join %s yet (%v); retrying in %s", cfg.HubURL, err, wait)
		}
		select {
		case <-ctx.Done():
			return
		case <-time.After(wait):
		}
		if wait *= 2; wait > autoJoinMaxWait {
			wait = autoJoinMaxWait
		}
	}
}
