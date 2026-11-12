package server

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os/exec"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/tevfik/fleet-router/internal/config"
	"github.com/tevfik/fleet-router/internal/deploy"
)

// Stopping what a deploy started. `fleet run end` closes a run's record; `fleet run stop` (and `fleet endpoint stop`)
// also stop the processes: the master first (nobody sends the workers requests any more), then the hub streams on
// the master's machine, then the workers. Only processes of fleet's own kind that belong to the current user are
// stopped (the /proc/PID/comm check of stopOnPortScript, repeated here for the master names); whatever else holds
// the port is reported and left alone. Nothing is killed by name machine-wide and nothing is killed with fuser -k.

// stopMasterNames / stopWorkerNames: the process names (/proc/PID/comm) fleet will stop on a master / worker port.
var (
	stopMasterNames = []string{"llama-server", "fleet", "fleet-engine", "fleet-engine-st", "fleet-engine-stage"}
	stopWorkerNames = []string{"ring-worker", "fleet", "fleet-engine", "fleet-engine-st", "fleet-engine-stage"}
)

// StopStep is one process group of the stop: the master, a hub stream, a relay tunnel or a worker.
type StopStep struct {
	Role   string `json:"role"` // master | stream | relay | worker
	Node   string `json:"node"`
	Port   int    `json:"port,omitempty"`
	Result string `json:"result"` // stopped | nothing listening | would stop … | left alone … | error …
	OK     bool   `json:"ok"`
}

// StopReport says what a stop did (or, with DryRun, would do).
type StopReport struct {
	Run      string     `json:"run,omitempty"`
	Endpoint string     `json:"endpoint"`
	InConfig bool       `json:"in_config,omitempty"`
	DryRun   bool       `json:"dry_run,omitempty"`
	InUse    int        `json:"in_use,omitempty"`  // requests running through the endpoint right now
	Refused  string     `json:"refused,omitempty"` // why nothing was done (requests running, no port known, …)
	Steps    []StopStep `json:"steps,omitempty"`
	Ended    bool       `json:"ended,omitempty"`    // the run record was closed
	Removed  bool       `json:"removed,omitempty"`  // the endpoint was removed from the configuration
	Roles    []string   `json:"roles,omitempty"`    // what the removal changed in the roles that named the endpoint
	Warnings []string   `json:"warnings,omitempty"` // things the caller should know (workers without an address, …)
}

// Failed: some step could not stop what it found, or the stop was refused.
func (r StopReport) Failed() bool {
	if r.Refused != "" {
		return true
	}
	for _, s := range r.Steps {
		if !s.OK {
			return true
		}
	}
	return false
}

// StopOptions: DryRun only reports; Force stops although requests are running; Remove also deletes the endpoint
// from the configuration afterwards (only when every step went well).
type StopOptions struct {
	DryRun bool `json:"dry_run"`
	Force  bool `json:"force"`
	Remove bool `json:"remove_endpoint"`
	// MendRoles: with Remove, also take the endpoint out of the roles that name it (else the removal is refused)
	MendRoles bool `json:"mend_roles"`
}

// stopPort stops what listens on port of node when it is one of names (replaceable for tests). Its result is
// "stopped", "none" (nothing listened) or "found" (dry run), plus the lines of processes left alone.
var stopPort = func(ctx context.Context, node string, local bool, port int, names []string, dry bool) (state string, held []string, err error) {
	script := stopPortScript(port, names, dry)
	var out []byte
	if local {
		out, err = exec.CommandContext(ctx, "sh", "-c", script).Output()
	} else {
		cmd := exec.CommandContext(ctx, "ssh", "-o", "BatchMode=yes", "-o", "ConnectTimeout=5", "--", node, "sh -s")
		cmd.Stdin = strings.NewReader(script)
		out, err = cmd.Output()
	}
	state, held = parseStopOutput(string(out))
	if state == "" && err == nil {
		err = fmt.Errorf("no answer from %s", node)
	}
	return state, held, err
}

func stopPortScript(port int, names []string, dry bool) string {
	if len(names) == 1 && names[0] == deploy.RelayClientName {
		return deploy.RelayClientScript(port, dry)
	}
	kill := "kill $pids 2>/dev/null\n  i=0\n  while [ $i -lt 20 ]; do alive=\"\"; for p in $pids; do kill -0 $p 2>/dev/null && alive=\"$alive $p\"; done; [ -n \"$alive\" ] || break; sleep 0.25; i=$((i+1)); done\n  [ -z \"$alive\" ] || { kill -9 $alive 2>/dev/null; sleep 0.3; }\n  echo FLEET_STOPPED"
	if dry {
		kill = "echo FLEET_FOUND"
	}
	return fmt.Sprintf(`command -v fuser >/dev/null 2>&1 || { echo FLEET_NOFUSER; exit 0; }
pids=""
for p in $(fuser -n tcp %d 2>/dev/null); do
  p=${p%%%%[!0-9]*}
  [ -n "$p" ] || continue
  c=$(cat /proc/$p/comm 2>/dev/null)
  case "$c" in
    %s)
      if [ -O /proc/$p ]; then pids="$pids $p"; else echo "FLEET_BUSY $p $c (another user's)"; fi ;;
    *) echo "FLEET_BUSY $p $c" ;;
  esac
done
if [ -n "$pids" ]; then
  %s
else
  echo FLEET_NONE
fi
`, port, strings.Join(names, "|"), kill)
}

func parseStopOutput(out string) (state string, held []string) {
	for _, ln := range strings.Split(out, "\n") {
		ln = strings.TrimSpace(ln)
		switch {
		case ln == "FLEET_STOPPED":
			state = "stopped"
		case ln == "FLEET_FOUND":
			state = "found"
		case ln == "FLEET_NONE" && state == "":
			state = "none"
		case ln == "FLEET_NOFUSER":
			state = "nofuser"
		case strings.HasPrefix(ln, "FLEET_BUSY "):
			held = append(held, strings.TrimPrefix(ln, "FLEET_BUSY "))
		}
	}
	return state, held
}

// portOf is the TCP port a base URL / host:port names ("" -> 0).
func portOf(raw string) int {
	if raw == "" {
		return 0
	}
	if u, err := url.Parse(raw); err == nil && u.Host != "" {
		n, _ := strconv.Atoi(u.Port())
		return n
	}
	_, p, err := net.SplitHostPort(raw)
	if err != nil {
		return 0
	}
	n, _ := strconv.Atoi(p)
	return n
}

// masterPortOf is the port of an endpoint's own server on the master's machine.
func masterPortOf(ep *config.Endpoint) int {
	if ep.URL != "" {
		return portOf(ep.URL)
	}
	return portOf(ep.Remote)
}

// stopOne runs one step and turns its answer into a StopStep.
func (s *Server) stopOne(ctx context.Context, role, node string, local bool, port int, names []string, dry bool) StopStep {
	st := StopStep{Role: role, Node: node, Port: port}
	if local {
		st.Node = "this machine"
		if node != "" {
			st.Node = node
		}
	}
	state, held, err := stopPort(ctx, node, local, port, names, dry)
	var notes []string
	if len(held) > 0 {
		notes = append(notes, "left alone, not fleet's: "+strings.Join(held, ", "))
	}
	switch {
	case err != nil:
		st.Result = "error: " + err.Error()
	case state == "nofuser":
		st.Result = "error: fuser is not installed there, so the port's owner cannot be found"
	case state == "stopped":
		st.Result, st.OK = "stopped", true
	case state == "found":
		st.Result, st.OK = "would stop", true
	default:
		st.Result, st.OK = "nothing listening", true
	}
	if len(notes) > 0 {
		st.Result += "; " + strings.Join(notes, "; ")
		if state == "none" || state == "" {
			st.OK = false
		}
	}
	return st
}

// StopEndpoint stops the processes behind an endpoint: master, then its hub streams, then its workers.
func (s *Server) StopEndpoint(ctx context.Context, ep config.Endpoint, opt StopOptions) StopReport {
	rep := StopReport{Endpoint: ep.Name, DryRun: opt.DryRun, InConfig: s.Config().Endpoint(ep.Name) != nil}
	if ep.Peer != "" {
		rep.Refused = "this endpoint belongs to the fleet " + ep.Peer + ": stop it there"
		return rep
	}
	rep.InUse, _, _ = s.lim("ep:" + ep.Name).stats()
	if rep.InUse > 0 && !opt.Force && !opt.DryRun {
		rep.Refused = fmt.Sprintf("%d request(s) are running through %s: they would be cut (wait, or use --force)", rep.InUse, ep.Name)
		return rep
	}
	if rep.InUse > 0 {
		rep.Warnings = append(rep.Warnings, fmt.Sprintf("%d request(s) are running through %s", rep.InUse, ep.Name))
	}
	cfg := s.Config()
	guard := map[int]bool{4711: true, ep.FleetPort: true}
	if _, p, err := net.SplitHostPort(cfg.Listen); err == nil {
		n, _ := strconv.Atoi(p)
		guard[n] = true
	}
	mport := masterPortOf(&ep)
	if mport <= 0 {
		rep.Refused = "the endpoint's address has no port: nothing to stop"
		return rep
	}
	if guard[mport] {
		rep.Refused = fmt.Sprintf("port %d is the fleet daemon's, not a model server's: not touching it", mport)
		return rep
	}
	master := ep.SSH
	if master == "" {
		master = ep.Node
	}
	local := master == "" || s.isSelfNode(master)
	if ep.SSH != "" && s.isSelfNode(ep.SSH) {
		local = true
	}
	step := func(st StopStep) { rep.Steps = append(rep.Steps, st) }
	step(s.stopOne(ctx, "master", master, local, mport, stopMasterNames, opt.DryRun))
	if ep.Group != nil {
		var nodes []string
		for n := range ep.Group.Streams {
			nodes = append(nodes, n)
		}
		sort.Strings(nodes)
		for _, n := range nodes {
			p := portOf(ep.Group.Streams[n])
			if p <= 0 {
				step(StopStep{Role: "stream", Node: n, Result: "error: bad stream address " + ep.Group.Streams[n]})
				continue
			}
			step(s.stopOne(ctx, "stream", master, local, p, []string{"fleet"}, opt.DryRun))
			rep.Steps[len(rep.Steps)-1].Node = master + " (to " + n + ")"
		}
		// the tunnels of a cross-site chain (--relay): closed right after the master, before the stages they join
		for _, rel := range ep.Group.Relays {
			for _, leg := range deploy.RelayLegs(rel) {
				node, lcl := leg.Node, s.isSelfNode(leg.Node)
				st := s.stopOne(ctx, "relay", node, lcl, leg.Port, leg.Names, opt.DryRun)
				st.Node += fmt.Sprintf(" (%s -> %s)", rel.From, rel.To)
				step(st)
			}
		}
		withAddr := map[string]bool{}
		var wn []string
		for n := range ep.Group.Addrs {
			wn = append(wn, n)
		}
		sort.Strings(wn)
		for _, n := range wn {
			withAddr[n] = true
			p := portOf(ep.Group.Addrs[n])
			if p <= 0 {
				continue
			}
			if guard[p] {
				step(StopStep{Role: "worker", Node: n, Port: p, Result: "left alone: that is the fleet daemon's port"})
				rep.Steps[len(rep.Steps)-1].OK = false
				continue
			}
			step(s.stopOne(ctx, "worker", n, s.isSelfNode(n), p, stopWorkerNames, opt.DryRun))
		}
		// a chain's other stages: a second stage on a machine that already has one in Addrs (same key there)
		stopped := map[string]bool{}
		for _, n := range wn {
			stopped[n+"|"+strconv.Itoa(portOf(ep.Group.Addrs[n]))] = true
		}
		for _, sa := range ep.Group.Stages {
			p := portOf(sa.Addr)
			if p <= 0 || stopped[sa.Node+"|"+strconv.Itoa(p)] {
				continue
			}
			stopped[sa.Node+"|"+strconv.Itoa(p)] = true
			if guard[p] {
				step(StopStep{Role: "stage", Node: sa.Node, Port: p, Result: "left alone: that is the fleet daemon's port"})
				rep.Steps[len(rep.Steps)-1].OK = false
				continue
			}
			step(s.stopOne(ctx, "stage", sa.Node, s.isSelfNode(sa.Node), p, stopWorkerNames, opt.DryRun))
		}
		for _, w := range ep.Group.Workers {
			if !withAddr[w] {
				rep.Warnings = append(rep.Warnings, fmt.Sprintf("worker %s has no address in the group, so its process was not looked for (stop it there)", w))
			}
		}
	}
	// Stopped on purpose: the deploy's record says so, or the watchdog would bring the master back.
	if !opt.DryRun && len(rep.Steps) > 0 && rep.Steps[0].OK {
		deploy.MarkEndpointStopped(ep.Name, "stopped by fleet run stop/end")
	}
	return rep
}

// stopAndBook stops an endpoint and keeps the books: the run record (when there is one) is closed with an event, the
// endpoint is removed from the configuration on request. A failed or refused stop changes nothing.
func (s *Server) stopAndBook(ctx context.Context, runID string, ep config.Endpoint, opt StopOptions) StopReport {
	rep := s.StopEndpoint(ctx, ep, opt)
	rep.Run = runID
	if opt.DryRun || rep.Refused != "" {
		return rep
	}
	var parts []string
	for _, st := range rep.Steps {
		parts = append(parts, fmt.Sprintf("%s %s: %s", st.Role, st.Node, strings.SplitN(st.Result, ";", 2)[0]))
	}
	if runID != "" {
		s.runs.update(runID, func(x *Run) {
			if x.Active() && !rep.Failed() {
				x.Ended = time.Now()
				rep.Ended = true
			}
			what := "stopped (" + strings.Join(parts, ", ") + ")"
			if rep.Failed() {
				what = "stop incomplete (" + strings.Join(parts, ", ") + ")"
			}
			x.event(what)
		})
	}
	if opt.Remove && !rep.Failed() && rep.InConfig {
		cfg := s.Config()
		if used := rolesUsing(cfg, []string{ep.Name}); len(used) > 0 && !opt.MendRoles {
			rep.Warnings = append(rep.Warnings, fmt.Sprintf("stopped, but the endpoint stays in the configuration: role(s) %s name it (remove it from them, or use --mend-roles)", strings.Join(used, ", ")))
			return rep
		}
		rep.Roles = cfg.DropEndpoints(ep.Name)
		if err := s.SetConfig(cfg); err != nil {
			rep.Warnings = append(rep.Warnings, "the endpoint could not be removed: "+err.Error())
		} else {
			rep.Removed = true
		}
	}
	return rep
}

func (s *Server) stopHandlers(mux *http.ServeMux, adm func(http.HandlerFunc) http.HandlerFunc) {
	mux.HandleFunc("POST /fleet/runs/stop", adm(func(w http.ResponseWriter, r *http.Request) {
		var in struct {
			StopOptions
			ID    string `json:"id"`
			Local bool   `json:"local"` // set on a forwarded call
		}
		if err := json.NewDecoder(io.LimitReader(r.Body, 1<<20)).Decode(&in); err != nil || in.ID == "" {
			httpErr(w, 400, "id is required")
			return
		}
		run, ok := s.runs.get(in.ID)
		if !ok && !in.Local { // planned by another fleet: it knows the processes
			in.Local = true
			if s.forwardRun(w, r.Context(), "/fleet/runs/stop", in, 2*time.Minute) {
				return
			}
		}
		if !ok {
			httpErr(w, 404, "no such run here (only the fleet that planned a run can stop it): "+in.ID)
			return
		}
		var ep *config.Endpoint
		if run.Endpoint != "" {
			ep = s.Config().Endpoint(run.Endpoint)
		}
		if ep == nil {
			if rec, err := deploy.ReadRecord(run.ID); err == nil && rec != nil { // the full id: in.ID may be a prefix
				groupAddrs := make(map[string]string)
				for node, port := range rec.WorkerPorts {
					groupAddrs[node] = net.JoinHostPort(deploy.ResolveNodeIP(node), strconv.Itoa(port))
				}
				ep = &config.Endpoint{
					Name: rec.EndpointName,
					Node: rec.MasterNode,
					URL:  rec.URL,
					Group: &config.Group{
						Engine:  rec.Engine,
						Workers: rec.WorkerNodes,
						Addrs:   groupAddrs,
						Relays:  rec.Relays,
					},
				}
			}
		}
		if ep == nil {
			if run.Endpoint == "" {
				httpErr(w, 409, "the run is not bound to an endpoint, so fleet does not know which processes are its: fleet run bind "+run.ID+" <endpoint> first")
			} else {
				httpErr(w, 409, "the run's endpoint "+run.Endpoint+" is no longer in the configuration: nothing known to stop (fleet run end "+run.ID+" closes the record)")
			}
			return
		}
		ctx, cancel := context.WithTimeout(r.Context(), 2*time.Minute)
		defer cancel()
		writeJSON(w, s.stopAndBook(ctx, run.ID, *ep, in.StopOptions))
	}))
	mux.HandleFunc("POST /fleet/endpoints/stop", adm(func(w http.ResponseWriter, r *http.Request) {
		var in struct {
			StopOptions
			Name string `json:"name"`
		}
		if err := json.NewDecoder(io.LimitReader(r.Body, 1<<20)).Decode(&in); err != nil || in.Name == "" {
			httpErr(w, 400, "name is required")
			return
		}
		ep := s.Config().Endpoint(in.Name)
		if ep == nil {
			httpErr(w, 404, "no such endpoint: "+in.Name)
			return
		}
		runID := ""
		for _, x := range s.runs.recent(maxRuns) {
			if x.Active() && x.Endpoint == ep.Name {
				runID = x.ID // recent is newest first: the run serving the endpoint now
				break
			}
		}
		ctx, cancel := context.WithTimeout(r.Context(), 2*time.Minute)
		defer cancel()
		writeJSON(w, s.stopAndBook(ctx, runID, *ep, in.StopOptions))
	}))
}
