package deploy

import (
	"context"
	"fmt"
	"net"
	"strconv"
	"strings"
	"time"

	"github.com/tevfik/fleet-router/internal/config"
)

// Cross-site chains (E12). Stages on machines that cannot reach each other (another site, a LAN address that is not
// routed) are joined by ssh tunnels through the machine that deploys, which reaches all of them over ssh:
//
//	From 127.0.0.1:Port  --(ssh -R, From -> Via)-->  Via 127.0.0.1:Port  --(ssh -L, Via -> To)-->  To 127.0.0.1:ToPort
//
// The tunnels are ssh processes on Via (and their listeners on From), found again by their port: the run's group
// records the relays, and stopping the run closes them the way it stops stages (see relayStopScripts).

// relayOpener is a StageRunner that can open and close the tunnels of a relayed link (the dry runner prints them).
type relayOpener interface {
	OpenRelay(rel StageRelay) error
	CloseRelay(rel StageRelay)
}

// relayProcNames: the processes that carry a tunnel (comm of the ssh client, of the sshd session holding the -R listener).
var relayProcNames = []string{"ssh", "sshd", "sshd-session"}

// ExitOnForwardFailure=no: a host's own ~/.ssh/config may carry a RemoteForward/LocalForward that fails (taken by another
// session) and would kill this tunnel too; the open tunnel is verified by its port instead (E12: a host forwarding a fixed port).
// relaySSHArgs is the ssh command line of one tunnel leg. spec is the -L/-R argument, node the machine ssh connects to.
func relaySSHArgs(flag, spec, node string) []string {
	return []string{"-N", "-o", "BatchMode=yes", "-o", "ExitOnForwardFailure=no", "-o", "ConnectTimeout=8",
		"-o", "ServerAliveInterval=15", "-o", "ServerAliveCountMax=3", flag, spec, "--", node}
}

// relayLegs says which tunnel legs a relay needs: the -L at Via (unless To is Via), the -R at From (unless From is Via).
// backPort is where the -R leg's traffic goes on Via: the tunnel port when a -L carries it on, else To's stage port itself.
func relayLegs(rel StageRelay) (listenVia, remoteFrom bool, backPort int) {
	listenVia = rel.To != rel.Via
	remoteFrom = rel.From != rel.Via
	backPort = rel.Port
	if !listenVia {
		backPort = rel.ToPort
	}
	return
}

// relayLegArgs: the ssh arguments of the -L leg (nil when there is none) and of the -R leg (nil when there is none).
func relayLegArgs(rel StageRelay) (l, r []string) {
	listenVia, remoteFrom, back := relayLegs(rel)
	if listenVia {
		l = relaySSHArgs("-L", fmt.Sprintf("127.0.0.1:%d:127.0.0.1:%d", rel.Port, rel.ToPort), rel.To)
	}
	if remoteFrom {
		r = relaySSHArgs("-R", fmt.Sprintf("127.0.0.1:%d:127.0.0.1:%d", rel.Port, back), rel.From)
	}
	return
}

// OpenRelay starts the legs of rel and waits until the tunnel port answers on both ends. A stale tunnel on the port
// (an earlier deploy) is closed first; a port held by something else is an error.
func (r *LiveStageRunner) OpenRelay(rel StageRelay) error {
	if rel.Hub != "" {
		if err := r.closeRelay(rel); err != nil {
			return fmt.Errorf("relay %s -> %s: %w", rel.From, rel.To, err)
		}
		if err := r.openHubRelay(rel); err != nil {
			r.CloseRelay(rel)
			return err
		}
		return nil
	}
	if err := r.closeRelay(rel); err != nil {
		return fmt.Errorf("relay %s -> %s: %w", rel.From, rel.To, err)
	}
	lArgs, rArgs := relayLegArgs(rel)
	ctx := context.Background()
	var dones []<-chan error
	var logs []string
	for _, leg := range []struct {
		name string
		args []string
	}{{"L", lArgs}, {"R", rArgs}} {
		if leg.args == nil {
			continue
		}
		log := runLogPath(fmt.Sprintf("relay-%d-%s.log", rel.Port, leg.name))
		_, done, err := startLocalProc(ctx, "ssh", leg.args, nil, log)
		if err != nil {
			r.CloseRelay(rel)
			return fmt.Errorf("relay %s -> %s: start ssh: %w", rel.From, rel.To, err)
		}
		dones = append(dones, done)
		logs = append(logs, log)
	}
	fail := func(why string) error {
		r.CloseRelay(rel)
		return fmt.Errorf("relay %s -> %s (port %d): %s (logs: %s)", rel.From, rel.To, rel.Port, why, strings.Join(logs, ", "))
	}
	exited := func() bool {
		for _, d := range dones {
			select {
			case <-d:
				return true
			default:
			}
		}
		return false
	}
	addr := net.JoinHostPort("127.0.0.1", strconv.Itoa(rel.Port))
	deadline := time.Now().Add(12 * time.Second)
	for {
		if exited() {
			return fail("an ssh tunnel exited at once")
		}
		// the listener at Via (or From when it is this machine) answers, the one on the far end of the -R leg too
		if rel.To == rel.Via || localPortOpen(addr) {
			break
		}
		if time.Now().After(deadline) {
			return fail("the tunnel port did not open on this machine")
		}
		time.Sleep(readyPoll)
	}
	if rel.From != rel.Via {
		s := StageLaunch{Node: rel.From, Port: rel.Port, Ready: Readiness{Kind: "tcp", Addr: addr}}
		if err := remoteWaitReady(s, 12*time.Second); err != nil {
			return fail("the tunnel port did not open on " + rel.From + ": " + err.Error())
		}
	}
	return nil
}

// CloseRelay stops the legs of rel (errors are not the caller's to handle: a stop that finds nothing is fine).
func (r *LiveStageRunner) CloseRelay(rel StageRelay) { _ = r.closeRelay(rel) }

// closeRelay stops the listener of the -L leg at Via and of the -R leg at From; a port held by a process that is not
// a tunnel is reported (busyError) and left alone.
func (r *LiveStageRunner) closeRelay(rel StageRelay) error {
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	var first error
	for _, leg := range RelayLegs(rel.Relay) {
		script := stopOnPortScript(leg.Port, leg.Names) + "fleet_stop_port\n"
		if leg.Names[0] == RelayClientName {
			script = RelayClientScript(leg.Port, false)
		}
		var out []byte
		where := "this machine"
		if leg.Local || defaultIsThisMachine(leg.Node) {
			out, _ = localScript(ctx, script)
		} else {
			out, _ = sshScript(ctx, leg.Node, script)
			where = leg.Node
		}
		if err := busyError(where, leg.Port, string(out)); err != nil && first == nil {
			first = err
		}
	}
	return first
}

func (d *DryRunner) OpenRelay(rel StageRelay) error {
	if rel.Hub != "" {
		d.Lines = append(d.Lines, hubRelayLine(rel))
		return nil
	}
	l, r := relayLegArgs(rel)
	if l != nil {
		d.Lines = append(d.Lines, "[this machine] ssh "+strings.Join(l, " "))
	}
	if r != nil {
		d.Lines = append(d.Lines, "[this machine] ssh "+strings.Join(r, " "))
	}
	return nil
}

func (d *DryRunner) CloseRelay(StageRelay) {}

// RelayLeg is one place a relay's tunnel port is listened on.
type RelayLeg struct {
	Node  string
	Local bool // Node is Via, the machine that deployed (the caller decides whether that is it: see Via)
	Port  int
	Names []string // the processes that may be stopped there
}

// RelayLegs lists where the tunnel port of rel is listened on: the -L leg at Via (unless To is Via) and the -R leg's
// listener at From (unless From is Via). Local marks the Via leg.
func RelayLegs(rel config.Relay) []RelayLeg {
	var out []RelayLeg
	if rel.Hub != "" {
		// the stream connect on the dialing stage's machine is the only process
		return []RelayLeg{{Node: rel.From, Port: rel.Port, Names: streamProcNames}}
	}
	if rel.To != rel.Via {
		out = append(out, RelayLeg{Node: rel.Via, Local: true, Port: rel.Port, Names: []string{"ssh"}})
	}
	if rel.From != rel.Via {
		// the client first: it carries the -R listener on From, which then goes away with it
		out = append([]RelayLeg{{Node: rel.Via, Local: true, Port: rel.Port, Names: []string{RelayClientName}}}, out...)
		out = append(out, RelayLeg{Node: rel.From, Port: rel.Port, Names: relayProcNames})
	}
	return out
}

// relayNode names node the way Via does when it is Via itself, so a relay's ends compare with ==.
func relayNode(via, node string) string {
	if defaultIsThisMachine(node) {
		return via
	}
	return node
}

func relayRecords(rs []StageRelay) []config.Relay {
	var out []config.Relay
	for _, r := range rs {
		out = append(out, r.Relay)
	}
	return out
}

// RelayClientName is the sentinel in RelayLeg.Names for the -R leg's ssh client on Via: it listens on nothing here (its
// listener is sshd's on the stage machine, often another user's process that cannot be stopped from there), so it is
// found by its command line, "ssh ... -R 127.0.0.1:PORT:..." of the current user, and stopped; the remote listener goes with it.
const RelayClientName = "ssh -R client"

// RelayClientScript stops (or with dry only finds) the ssh -R client of tunnel port on this machine and prints
// FLEET_STOPPED / FLEET_FOUND / FLEET_NONE, like the stop-by-port scripts.
func RelayClientScript(port int, dry bool) string {
	act := `kill $pids 2>/dev/null; i=0; while [ $i -lt 20 ]; do alive=""; for p in $pids; do kill -0 $p 2>/dev/null && alive="$alive $p"; done; [ -n "$alive" ] || break; sleep 0.25; i=$((i+1)); done; [ -z "$alive" ] || kill -9 $alive 2>/dev/null; echo FLEET_STOPPED`
	if dry {
		act = "echo FLEET_FOUND"
	}
	return fmt.Sprintf(`pids=""
for d in /proc/[0-9]*; do
  p=${d#/proc/}
  [ "$(cat $d/comm 2>/dev/null)" = ssh ] || continue
  [ -O $d ] || continue
  case " $(tr '\0' ' ' < $d/cmdline 2>/dev/null)" in *" -R 127.0.0.1:%d:"*) pids="$pids $p" ;; esac
done
if [ -n "$pids" ]; then %s; else echo FLEET_NONE; fi
`, port, act)
}
