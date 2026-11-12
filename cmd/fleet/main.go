// fleet — a model fleet in one binary: router daemon + menu (TUI) + MCP + CLI.
package main

import (
	"fmt"
	"os"
	"strings"

	"github.com/tevfik/fleet-router/internal/client"
	"github.com/tevfik/fleet-router/internal/config"
	"github.com/tevfik/fleet-router/internal/feature"
	"github.com/tevfik/fleet-router/internal/mcpsrv"
	"github.com/tevfik/fleet-router/internal/ringengine"
	"github.com/tevfik/fleet-router/internal/server"
	"github.com/tevfik/fleet-router/internal/update"
)

var version = "dev"

const usage = `fleet — a model fleet in one binary

  fleet                      menu (TUI); starts an embedded daemon if none is running
  fleet serve                router daemon: OpenAI-compatible /v1 + SSH tunnels + health + admin API
  fleet mcp                  MCP server for Claude Code (stdio)
  fleet status               endpoint health + roles
  fleet route <role|model> [--in N] [--out N]   where a request would go and what each candidate costs
  fleet vision [serve|describe|status] [--model mmproj.gguf] [--port N] [--cpu]   modular multimodal vision microservice (decoupled zero-VRAM image encoder)
  fleet apikey [create <name> [--qos tier] [--role R] | list | rm <key>]   manage API keys & client QoS
  fleet wallet [address|balance|status]   cryptographic identity and token credit balance
  fleet share [on|off|status] [--price N] [--vram M]       share idle GPU compute to earn network token credits
  fleet endpoint [list|check|add <name> <url>|stop <name>|rm <name…>]   the registered model servers; stop/rm --stop stop the processes behind one; run-on --register records a run-on server
  fleet leave [--back]       this machine leaves the line (plans skip it, groups are told, its models come last) / rejoins
  fleet join [hub-url] [--token T] [--stream-port P]... [--add-stream-port P]...   join a hub over an outbound tunnel; --stream-port lets the hub open raw TCP streams to local port P (replaces the list; --add-stream-port adds to it) (--leave disconnects)
  fleet stream connect <node> <port> [--listen addr] [--hub URL] [--key K]   a local listener whose connections reach <port> on a joined node through the hub (e.g. a ring worker)
  fleet log [-n 30] [-f]     recent router requests (who answered, failover, speed)
  fleet peers [list|lan|pair|add|rm|sync] other fleets: their models show up here as node/model
  fleet usage [--by client|role|model|endpoint|day] [--since 7d]   who used what, how much
  fleet top                  tokens/second leaderboard
  fleet bench [endpoint…]    measure tokens/s (default: every healthy chat endpoint)
  fleet nodes [add <host…>|rm <host…>|rtt|node|bench|suggest] the machines: ping, CPU, RAM, GPU, temperatures, disks, AI score (read by the fleet on each); bench: measure this machine again
  fleet probe [--apply] [--show] [endpoint…]   what this machine's models really do (chat, tools, thinking, vision, embed) — loads each model
  fleet discover [--ssh a,b] [--ports …] [--embed] [--apply]
  fleet role set <role> <endpoint> [fallback…] | test <role> | rm <role> | list | opt <role> priority=N max_parallel=N overflow=true strategy=least-busy | init [--apply] | slots <endpoint> <N> | fleetport <endpoint> <port> | match <role> tag:fast … | tag <endpoint> fast …
  fleet ask <role> <text>    one-shot question through the router
  fleet hosts [add|rm <ssh-host…>] [ports <list>] [ignore <machine:port…>]   discovery targets
  fleet opencode             opencode provider JSON
  fleet import <old fleet.yaml> [--self <machine>] [--write]
  fleet setup claude|opencode|skills|service|install|all [--system]   (--system: /usr/local/bin + /etc/systemd/system, run with sudo; claude/opencode: registration + guidance; skills: refresh guidance only)
  fleet doctor
  fleet config history | undo   previous configurations (every change keeps the one it replaced) / go one step back
  fleet key set <key> | show | rm   manage the hub API key (~/.config/fleet/.env and config)
  fleet eval [--role R | --all | endpoint…] [--show]   quality score per model (a fixed suite checked by code, no judge model)
  fleet update [--check] [--force] [--rollback] [--all] [--source]   install the newest release from the git repository (the daemon also does it itself); --all: also on every SSH machine; --source: where this build looks
  fleet version

Configuration: ` + "`fleet config-path`" + ` (override with FLEET_CONFIG).
`

// usageRunner lists the runner side's commands (feature.Runner): shown only where fleet-engine is.
const usageRunner = `
Runner (fleet-engine):
  fleet store [list|add <file|dir>…|pull <ref>|fill <ref> --blocks 0-3,7|rm <ref>|info <ref>|prune|abort [--discard]|status] [--node N] [--from M]   model files: which machine has what (hashed once), moving them between machines, worker caches
  fleet plan <model.gguf> [--ctx N] [--np N] [--master node] [--rtt a>b=ms,…] [--link-gbps X] [--fetch]   where a model would run best: one machine or split over several (starts nothing)
  fleet deploy [<model.gguf>] [--profile NAME [--explore]] [--save-profile NAME] [--layout auto|fixed] [--master M (default: this machine)] [--workers W (default: none)] [--ts T] [--spec S] [--via-hub | --via-hub-node N1,N2 | --no-hub] [--dry-run] …   deploy a model across machines (fleet deploy with no arguments lists every flag)
  fleet profile save|list|show|rm|edit|export|import|bench|measure …   saved deploy configurations and their measurements (docs/profiles.md)
  fleet watchdog [--interval 30s] [--once] [--max-restarts N]   redeploy a ring whose engine lost a worker (llama-server /health stays ok then)
  fleet shards <model.gguf> [--out fleet-shards.json] [--no-hash] [--need first-last]   which bytes of a GGUF belong to which block (for a worker that holds only some blocks)
  fleet run-on <node> <model.gguf> [--port N] [--ngl N] [--register]   run model on a specific device
  fleet run [list|show <id>|bind <id> <endpoint>|stop <id>|end <id>|rm <id>|prune [--stale] [--dry-run]]   distributed runs: where each block runs, plan vs measured, events (every fleet sees them); stop = stop the processes (master, streams, workers) and close the record, end = close the record (and stop in the background; --record-only keeps them running)
  fleet config node list | set <name> [addr=IP] [mem_gib=N] [weight_gbps=N] [expert_gbps=N] | rm <name>   what deploy knows about a machine (nothing is built in)
  fleet engine [status|source|install [--variant V] [--force] [--all]]   the llama-ring engine: what this machine has and which build suits it; source: where packages come from; install fetches it from the package registry (every machine with --all)
  fleet ring worker|serve|version [engine options]   Protocol 1 (ring): embedded/standalone ggml-ring worker or llama-server
  fleet chain serve|run|version [stage options]      Protocol 2 (chain / FSP1): pipeline stage server or chain driver
`

// runnerCmds are the commands of the runner side: without fleet-engine they are not offered.
var runnerCmds = map[string]bool{
	"store": true, "plan": true, "deploy": true, "profile": true, "profiles": true, "shards": true, "watchdog": true,
	"run-on": true, "run": true, "runs": true, "ring": true, "chain": true, "stage": true,
}

// helpText is the usage for this machine: the runner's commands only where fleet-engine is.
func helpText() string {
	if feature.Runner() {
		return usage + usageRunner
	}
	return usage
}

func main() {
	server.Version = version // the version this binary reports everywhere (menu, status, peers)
	server.Engine = ringengine.Version()
	args := os.Args[1:]
	// `fleet-engine-stage` (the link `fleet engine install` makes next to fleet) is fleet's own stage mode: `fleet chain …`
	args = stageInvocation(os.Args[0], args)
	cmd := "tui"
	if len(args) > 0 {
		cmd, args = args[0], args[1:]
	}
	// `fleet engine` is how fleet-engine gets onto a machine: it exists where the engine is, or where this build (or
	// FLEET_ENGINE_REPO) knows a source for it. Anywhere else it is an unknown command, like any other.
	if cmd == "engine" && !feature.Runner() && update.EngineSource() == "" {
		fmt.Fprintf(os.Stderr, "unknown command: %s\n\n%s", cmd, helpText())
		os.Exit(2)
	}
	if runnerCmds[cmd] && !feature.Runner() {
		fmt.Fprintf(os.Stderr, "fleet %s: %s\n", cmd, feature.RunnerHint)
		os.Exit(2)
	}
	var err error
	switch cmd {
	case "serve":
		err = cmdServe()
	case "tui", "menu":
		err = cmdTUI()
	case "mcp":
		err = mcpsrv.Run(listen())
	case "status":
		err = cmdStatus()
	case "discover":
		err = cmdDiscover(args)
	case "role":
		err = cmdRole(args)
	case "ask":
		err = cmdAsk(args)
	case "hosts":
		err = cmdHosts(args)
	case "top":
		err = cmdTop()
	case "log":
		err = cmdLog(args)
	case "usage":
		err = cmdUsage(args)
	case "peers":
		err = cmdPeers(args)
	case "bench":
		err = cmdBench(args)
	case "nodes", "node":
		err = cmdNodes(args)
	case "route":
		err = cmdRoute(args)
	case "plan":
		err = cmdPlan(args)
	case "deploy":
		err = cmdDeploy(args)
	case "profile", "profiles":
		err = cmdProfile(args)
	case "shards":
		err = cmdShards(args)
	case "watchdog":
		err = cmdWatchdog(args)
	case "vision":
		err = cmdVision(args)
	case "run-on":
		err = cmdRunOn(args)
	case "endpoint", "endpoints", "ep":
		err = cmdEndpoint(args)
	case "apikey", "apikeys":
		err = cmdAPIKey(args)
	case "store":
		err = cmdStore(args)
	case "run", "runs":
		err = cmdRun(args)
	case "leave":
		err = cmdLeave(args)
	case "join":
		err = cmdJoin(args)
	case "stream":
		err = cmdStream(args)
	case "wallet":
		err = cmdWallet(args)
	case "share":
		err = cmdShare(args)
	case "economy":
		err = cmdEconomy(args)
	case "ring":
		err = cmdRing(args)
	case "chain", "stage":
		err = cmdChain(args)
	case "engine":
		err = cmdEngine(args)
	case "probe":
		err = cmdProbe(args)
	case "opencode":
		err = cmdOpenCode()
	case "import":
		err = cmdImport(args)
	case "setup":
		err = cmdSetup(args)
	case "doctor":
		err = cmdDoctor()
	case "config-path":
		fmt.Println(config.Path())
	case "config":
		err = cmdConfig(args)
	case "key":
		err = cmdKey(args)
	case "update":
		err = cmdUpdate(args)
	case "eval":
		err = cmdEval(args)
	case "version", "--version":
		fmt.Println("fleet", version)
	case "help", "-h", "--help":
		fmt.Print(helpText())
	default:
		fmt.Fprintf(os.Stderr, "unknown command: %s\n\n%s", cmd, helpText())
		os.Exit(2)
	}
	if err != nil {
		fmt.Fprintln(os.Stderr, "error:", err)
		os.Exit(1)
	}
}

func listen() string {
	if l := os.Getenv("FLEET_LISTEN"); l != "" {
		return l
	}
	if c, err := config.Load(config.Path()); err == nil && c.Listen != "" {
		return c.Listen
	}
	return config.DefaultListen
}

func mustClient() (*client.Client, error) {
	cl := client.New(listen())
	if !cl.Alive() {
		return nil, fmt.Errorf("the daemon is not running (%s) — run `fleet serve` or `fleet setup service`", cl.Base)
	}
	return cl, nil
}

// flagsFirst: Go's flag package stops at the first positional argument; to also accept "file --write"
// it moves the flags to the front. valueFlags: names of flags that take a value.
func flagsFirst(args []string, valueFlags ...string) []string {
	var flags, pos []string
	for i := 0; i < len(args); i++ {
		a := args[i]
		if strings.HasPrefix(a, "-") && a != "-" { // a lone "-" is a positional (stdin)
			flags = append(flags, a)
			name := strings.TrimLeft(a, "-")
			for _, v := range valueFlags {
				if name == v && i+1 < len(args) {
					i++
					flags = append(flags, args[i])
				}
			}
		} else {
			pos = append(pos, a)
		}
	}
	return append(flags, pos...)
}

func trunc(s string, n int) string {
	if r := []rune(s); len(r) > n {
		return string(r[:n-1]) + "…"
	}
	return s
}

// clientIfAlive is the daemon's client when it runs (nil otherwise).
func clientIfAlive() *client.Client {
	if cl := client.New(listen()); cl.Alive() {
		return cl
	}
	return nil
}
