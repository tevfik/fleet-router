// Package mcpsrv is the stdio MCP server for Claude Code (and any MCP client). It is a client of the daemon.
package mcpsrv

import (
	"bufio"
	"encoding/json"
	"fmt"
	"os"
	"sort"
	"strings"
	"time"

	"github.com/tevfik/fleet-router/internal/board"
	"github.com/tevfik/fleet-router/internal/client"
	"github.com/tevfik/fleet-router/internal/server"
)

const maxOut = 6000

func clip(s string) string {
	if len(s) <= maxOut {
		return s
	}
	h := maxOut / 4
	return s[:h] + fmt.Sprintf("\n… [%d characters clipped] …\n", len(s)-maxOut) + s[len(s)-(maxOut-h):]
}

type obj = map[string]any

func tool(name, desc string, props obj, required ...string) obj {
	if required == nil {
		required = []string{} // JSON `[]`, not `null` — Claude Code rejects a null "required" and drops every tool
	}
	return obj{"name": name, "description": desc,
		"inputSchema": obj{"type": "object", "properties": props, "required": required, "additionalProperties": false}}
}

var str = obj{"type": "string"}

var tools = []obj{
	tool("fleet_status", "All endpoint health plus which endpoint each role currently routes to. Start fleet work with this.", obj{}),
	tool("fleet_ask", "SYNCHRONOUS one-shot question to a role/endpoint (no tools, fast). For short analysis, summaries, decisions. Falls back automatically if the role's endpoint is down.",
		obj{"role": str, "prompt": str, "system": str, "max_tokens": obj{"type": "integer", "description": "answer budget; thinking models need room for reasoning (e.g. 4000)"}, "timeout_s": obj{"type": "integer"}}, "role", "prompt"),
	tool("task_start", "Starts a background task → job_id. mode=opencode (default): `opencode run --agent fleet-<agent>` (with file/command tools); mode=chat: no tools. Start each task separately for parallel fan-out.",
		obj{"agent": str, "prompt": str, "mode": obj{"type": "string", "enum": []string{"opencode", "chat"}}, "system": str, "model": str, "cwd": str, "max_tokens": obj{"type": "integer", "description": "chat mode: answer budget; thinking models need room for reasoning (e.g. 4000)"}, "timeout_s": obj{"type": "integer"}}, "agent", "prompt"),
	tool("task_result", "Job result; if wait_s>0 waits until it finishes. Output is clipped.", obj{"job_id": str, "wait_s": obj{"type": "integer"}}, "job_id"),
	tool("task_list", "Lists jobs.", obj{}),
	tool("task_cancel", "Cancels a job.", obj{"job_id": str}, "job_id"),
	tool("fleet_role_set", "Binds a role to an endpoint (applied hot: sessions stay open, NO reload needed).",
		obj{"role": str, "preferred": str, "fallback": obj{"type": "array", "items": str}}, "role", "preferred"),
	tool("fleet_discover", "Discovers OpenAI-compatible endpoints on this machine + SSH hosts. apply=true registers new models under the machine's name. Never SSHes into itself.",
		obj{"ports": str, "ssh": obj{"type": "array", "items": str}, "apply": obj{"type": "boolean"}, "include_embed": obj{"type": "boolean"}}),
	tool("fleet_leaderboard", "Tokens/second ranking of chat endpoints. bench=true first measures the healthy ones (slow).", obj{"bench": obj{"type": "boolean"}}),
}

type Server struct {
	cl   *client.Client
	jobs *Jobs
}

func Run(listen string) error {
	cl := client.New(listen)
	s := &Server{cl: cl, jobs: newJobs(cl)}
	sc := bufio.NewScanner(os.Stdin)
	sc.Buffer(make([]byte, 1<<20), 16<<20)
	out := json.NewEncoder(os.Stdout)
	for sc.Scan() {
		var msg struct {
			ID     json.RawMessage `json:"id"`
			Method string          `json:"method"`
			Params struct {
				Name string `json:"name"`
				Args obj    `json:"arguments"`
			} `json:"params"`
		}
		if json.Unmarshal(sc.Bytes(), &msg) != nil || msg.Method == "" || len(msg.ID) == 0 {
			continue // notifications / unparseable lines get no reply
		}
		resp := obj{"jsonrpc": "2.0", "id": msg.ID}
		switch msg.Method {
		case "initialize":
			resp["result"] = obj{"protocolVersion": "2024-11-05", "capabilities": obj{"tools": obj{}},
				"serverInfo": obj{"name": "fleet", "version": "1.0.0"}}
		case "ping":
			resp["result"] = obj{}
		case "tools/list":
			resp["result"] = obj{"tools": tools}
		case "tools/call":
			text, err := s.call(msg.Params.Name, msg.Params.Args)
			if err != nil {
				text = "error: " + err.Error()
			}
			resp["result"] = obj{"content": []obj{{"type": "text", "text": text}}, "isError": err != nil}
		default:
			resp["error"] = obj{"code": -32601, "message": "method not found: " + msg.Method}
		}
		out.Encode(resp)
	}
	return sc.Err()
}

func sArg(a obj, k string) string { v, _ := a[k].(string); return v }
func iArg(a obj, k string, d int) int {
	if f, ok := a[k].(float64); ok {
		return int(f)
	}
	return d
}
func bArg(a obj, k string) bool { v, _ := a[k].(bool); return v }
func lArg(a obj, k string) (l []string) {
	if xs, ok := a[k].([]any); ok {
		for _, x := range xs {
			if s, ok := x.(string); ok {
				l = append(l, s)
			}
		}
	}
	return
}

// checkArgs enforces the tool's schema: required arguments present, no unknown ones (a typo like
// "role" for "agent" would otherwise start a job that can only fail).
func checkArgs(name string, a obj) error {
	for _, t := range tools {
		if t["name"] != name {
			continue
		}
		sch := t["inputSchema"].(obj)
		props := sch["properties"].(obj)
		for _, r := range sch["required"].([]string) {
			if v, ok := a[r]; !ok || v == "" {
				return fmt.Errorf("missing required argument %q for %s", r, name)
			}
		}
		for k := range a {
			if _, ok := props[k]; !ok {
				var known []string
				for p := range props {
					known = append(known, p)
				}
				sort.Strings(known)
				return fmt.Errorf("unknown argument %q for %s (known: %s)", k, name, strings.Join(known, ", "))
			}
		}
		return nil
	}
	return fmt.Errorf("unknown tool %q", name)
}

func (s *Server) call(name string, a obj) (string, error) {
	if err := checkArgs(name, a); err != nil {
		return "", err
	}
	switch name {
	case "fleet_status":
		return s.status()
	case "fleet_ask":
		out, ep, err := s.cl.Chat(sArg(a, "role"), sArg(a, "system"), sArg(a, "prompt"), iArg(a, "max_tokens", 0), time.Duration(iArg(a, "timeout_s", 300))*time.Second)
		if err != nil {
			return "", err
		}
		return fmt.Sprintf("[%s → %s]\n%s", sArg(a, "role"), ep, clip(strings.TrimSpace(out))), nil
	case "task_start":
		mode := sArg(a, "mode")
		if mode == "" {
			mode = "opencode"
		}
		cwd := sArg(a, "cwd")
		if cwd == "" {
			cwd, _ = os.Getwd()
		}
		id := s.jobs.Start(sArg(a, "agent"), mode, sArg(a, "prompt"), sArg(a, "system"), sArg(a, "model"), cwd, iArg(a, "max_tokens", 0), time.Duration(iArg(a, "timeout_s", 900))*time.Second)
		return "job_id=" + id, nil
	case "task_result":
		return s.jobs.Result(sArg(a, "job_id"), time.Duration(iArg(a, "wait_s", 0))*time.Second, clip), nil
	case "task_list":
		return s.jobs.List(), nil
	case "task_cancel":
		return s.jobs.Cancel(sArg(a, "job_id")), nil
	case "fleet_leaderboard":
		if bArg(a, "bench") {
			if err := s.cl.Bench(nil); err != nil {
				return "", err
			}
			for i := 0; i < 600; i++ { // up to ~10 minutes
				time.Sleep(time.Second)
				if st, err := s.cl.Status(); err == nil && !st.Bench.Running {
					break
				}
			}
		}
		st, err := s.cl.Status()
		if err != nil {
			return "", err
		}
		return clip(leaderboardText(st.Endpoints)), nil
	case "fleet_role_set":
		if err := s.cl.SetRole(sArg(a, "role"), sArg(a, "preferred"), lArg(a, "fallback")); err != nil {
			return "", err
		}
		return "✓ role updated (applied hot)", nil
	case "fleet_discover":
		r, err := s.cl.Discover(sArg(a, "ports"), lArg(a, "ssh"), bArg(a, "include_embed"), bArg(a, "apply"))
		if err != nil {
			return "", err
		}
		var sb strings.Builder
		for _, n := range r.Notes {
			sb.WriteString(n + "\n")
		}
		fmt.Fprintf(&sb, "found: %d · already registered: %d · to add/added: %d · skipped: %d\n", len(r.Found), len(r.Plan.Known), len(r.Plan.Add), len(r.Plan.Skip))
		for _, e := range r.Plan.Add {
			fmt.Fprintf(&sb, "  + %s  (%s)\n", e.Name, e.Model)
		}
		if !bArg(a, "apply") && len(r.Plan.Add) > 0 {
			sb.WriteString("set apply=true to register\n")
		}
		return clip(sb.String()), nil
	}
	return "", fmt.Errorf("unknown tool: %s", name)
}

func (s *Server) status() (string, error) {
	st, err := s.cl.Status()
	if err != nil {
		return "", err
	}
	var sb strings.Builder
	roles := make([]string, 0, len(st.Roles))
	for r := range st.Roles {
		roles = append(roles, r)
	}
	sort.Strings(roles)
	sb.WriteString("ROLLER\n")
	for _, r := range roles {
		x := st.Roles[r]
		fmt.Fprintf(&sb, "  %-10s → %-28s (preferred %s, fallback %v)\n", r, x.Active, x.Preferred, x.Fallback)
	}
	sb.WriteString("ENDPOINT'LER\n")
	for _, e := range st.Endpoints {
		mark := "✓"
		if !e.Known {
			mark = "?"
		} else if !e.OK {
			mark = "✗"
		}
		extra := ""
		if e.Err != "" {
			extra = " — " + e.Err
		}
		fmt.Fprintf(&sb, "  %s %-34s %-10s %-26s %dms%s\n", mark, e.Name, e.Node, e.Model, e.LatencyMS, extra)
	}
	return clip(sb.String()), nil
}

func leaderboardText(eps []server.EpStatus) string {
	var sb strings.Builder
	rows := board.Rows(eps)
	for _, r := range rows {
		if r.TPS > 0 {
			fmt.Fprintf(&sb, "%2d. %-30s %7.1f tok/s  %s\n", r.Rank, r.Name, r.TPS, r.Node)
		}
	}
	if sb.Len() == 0 {
		return "nothing measured yet — call with bench=true"
	}
	return sb.String()
}
