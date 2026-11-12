package mcpsrv

import (
	"context"
	"fmt"
	"os/exec"
	"sort"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/tevfik/fleet-router/internal/client"
)

type job struct {
	ID, Agent, Mode, Prompt, Endpoint string
	Started, Ended                    time.Time
	State                             string // running|done|failed|timeout|cancelled
	Out                               string
	cancel                            context.CancelFunc
}

type Jobs struct {
	mu   sync.Mutex
	m    map[string]*job
	seq  int
	cl   *client.Client
	done map[string]chan struct{}
}

func newJobs(cl *client.Client) *Jobs {
	return &Jobs{m: map[string]*job{}, done: map[string]chan struct{}{}, cl: cl}
}

// Start: mode "chat" → one-shot chat through the router (no tools, fast);
// mode "opencode" → `opencode run --agent` (with file/command tools).
func (j *Jobs) Start(agent, mode, prompt, system, model, cwd string, maxTokens int, timeout time.Duration) string {
	j.mu.Lock()
	j.seq++
	id := fmt.Sprintf("%s-%d", time.Now().Format("150405"), j.seq)
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	jb := &job{ID: id, Agent: agent, Mode: mode, Prompt: prompt, Started: time.Now(), State: "running", cancel: cancel}
	j.m[id] = jb
	j.pruneLocked()
	ch := make(chan struct{})
	j.done[id] = ch
	j.mu.Unlock()

	go func() {
		defer close(ch)
		defer cancel()
		var out, ep string
		var err error
		if mode == "chat" {
			out, ep, err = j.cl.ChatCtx(ctx, agent, system, prompt, maxTokens)
		} else {
			// opencode agents are installed as fleet-<role> (fleet setup opencode)
			name := agent
			if !strings.HasPrefix(name, "fleet-") {
				name = "fleet-" + name
			}
			args := []string{"run", "--agent", name}
			if model != "" {
				args = append(args, "--model", model)
			}
			if strings.HasPrefix(prompt, "-") {
				prompt = " " + prompt // never an option of opencode
			}
			cmd := exec.CommandContext(ctx, "opencode", append(args, prompt)...)
			cmd.Dir = cwd
			cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
			cmd.Cancel = func() error { return syscall.Kill(-cmd.Process.Pid, syscall.SIGTERM) } // including child processes
			var b []byte
			b, err = cmd.CombinedOutput()
			out = string(b)
		}
		j.mu.Lock()
		defer j.mu.Unlock()
		jb.Out, jb.Endpoint, jb.Ended = out, ep, time.Now()
		switch {
		case jb.State == "cancelled":
		case ctx.Err() == context.DeadlineExceeded:
			jb.State = "timeout"
		case err != nil:
			jb.State = "failed"
			jb.Out += "\n[error] " + err.Error()
		default:
			jb.State = "done"
		}
	}()
	return id
}

func (j *Jobs) Result(id string, wait time.Duration, clip func(string) string) string {
	j.mu.Lock()
	jb, ok := j.m[id]
	ch := j.done[id]
	j.mu.Unlock()
	if !ok {
		return "job not found: " + id
	}
	if wait > 0 {
		select {
		case <-ch:
		case <-time.After(wait):
		}
	}
	j.mu.Lock()
	defer j.mu.Unlock()
	age := time.Since(jb.Started).Round(time.Second)
	if jb.State == "running" {
		return fmt.Sprintf("[%s] %s · running · %s (not finished yet)", id, jb.Agent, age)
	}
	ep := ""
	if jb.Endpoint != "" {
		ep = " · " + jb.Endpoint
	}
	return fmt.Sprintf("[%s] %s · %s%s · %s\n%s", id, jb.Agent, jb.State, ep, jb.Ended.Sub(jb.Started).Round(time.Second), clip(strings.TrimSpace(jb.Out)))
}

func (j *Jobs) List() string {
	j.mu.Lock()
	defer j.mu.Unlock()
	var ids []string
	for id := range j.m {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	if len(ids) == 0 {
		return "no jobs"
	}
	var sb strings.Builder
	for _, id := range ids {
		jb := j.m[id]
		p := jb.Prompt
		if len(p) > 60 {
			p = p[:60]
		}
		fmt.Fprintf(&sb, "%s  %-10s %-9s %-8s %q\n", id, jb.Agent, jb.Mode, jb.State, p)
	}
	return sb.String()
}

func (j *Jobs) Cancel(id string) string {
	j.mu.Lock()
	jb, ok := j.m[id]
	if !ok {
		j.mu.Unlock()
		return "job not found: " + id
	}
	if jb.State != "running" {
		defer j.mu.Unlock()
		return "already finished: " + jb.State
	}
	jb.State = "cancelled"
	j.mu.Unlock()
	jb.cancel()
	return "cancelled: " + id
}

// maxFinishedJobs: finished jobs (with their whole output) kept for Result/List; older ones are dropped.
const maxFinishedJobs = 100

// pruneLocked drops the oldest finished jobs beyond maxFinishedJobs (the caller holds j.mu): the map used to grow
// for the life of the server.
func (j *Jobs) pruneLocked() {
	var fin []*job
	for _, jb := range j.m {
		if jb.State != "running" {
			fin = append(fin, jb)
		}
	}
	if len(fin) <= maxFinishedJobs {
		return
	}
	sort.Slice(fin, func(a, b int) bool { return fin[a].Started.Before(fin[b].Started) })
	for _, jb := range fin[:len(fin)-maxFinishedJobs] {
		delete(j.m, jb.ID)
		delete(j.done, jb.ID)
	}
}
