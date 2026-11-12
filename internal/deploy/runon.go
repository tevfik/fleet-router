package deploy

import (
	"context"
	"fmt"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"github.com/tevfik/fleet-router/internal/config"
	"github.com/tevfik/fleet-router/internal/ringengine"
)

// RunOnReady is how long RunOnNode waits for the started server to answer /health before it returns with state
// "starting" (a large model keeps loading afterwards; nothing is stopped).
var RunOnReady = 90 * time.Second

// RunOnNode starts a model on a single machine (local or remote). The engine is resolved the way the master of a ring
// is (masterEngineDirs on a remote machine, ringengine here): a machine without one is an error saying where it
// looked, not a process that silently never starts. The server's output goes to ~/.cache/fleet/runs/<id>.log on that
// machine, and the call returns once /health answers (or after RunOnReady, state "starting", with the log's last line).
func RunOnNode(ctx context.Context, cfg *config.Config, node, modelPath string, port, ctxLen, ngl int, dryRun bool) (*DeployResult, error) {
	if port <= 0 {
		port = 18092
	}
	if ctxLen <= 0 {
		ctxLen = 4096
	}
	if ngl <= 0 {
		ngl = 99
	}
	if strings.ContainsAny(modelPath, " \t\n'\"$;&|<>`\\") {
		return nil, fmt.Errorf("model path %q has characters that cannot be passed to a remote shell safely", modelPath)
	}

	isLocal := isLocalNode(node)
	slug := endpointSlug(modelPath, "")
	epName := fmt.Sprintf("%s-%s", node, slug)
	host := "127.0.0.1"
	if !isLocal {
		host = ResolveNodeIP(node)
	}
	url := fmt.Sprintf("http://%s:%d/v1", host, port)

	cmdStr := fmt.Sprintf("fleet ring serve -m %s -ngl %d -c %d --port %d", modelPath, ngl, ctxLen, port)
	if !isLocal {
		cmdStr += " --host 0.0.0.0" // the URL above is the machine's address: the server must listen on it
	}
	res := &DeployResult{
		RunID:        fmt.Sprintf("single-%s-%d", node, time.Now().UnixMilli()),
		EndpointName: epName,
		ModelSHA256:  knownModelSHA(modelPath),
		URL:          url,
		Strategy:     "single-node",
		MasterNode:   node,
		MasterCmd:    cmdStr,
		Devices:      "CUDA0",
		TensorSplit:  "all",
		Status:       "planned",
	}
	if !isLocal {
		res.MasterEngine = describeRemoteMaster(node)
	}
	if dryRun {
		res.Status = "dry-run"
		return res, nil
	}

	parts := strings.Fields(cmdStr)
	var pid int
	var exited <-chan error
	logPath := filepath.Join(runDir(), res.RunID+".log")
	if isLocal {
		bin, args := resolveLocalMaster(parts)
		if bin == "fleet" { // no engine binary beside this machine's builds: this fleet itself, if it has the engine
			switch exe, err := fleetBinary(); {
			case err == nil && ringengine.Available():
				bin = exe
			default:
				p, lerr := exec.LookPath("fleet")
				if lerr != nil {
					return nil, fmt.Errorf("no engine on this machine for %s: no llama-server / fleet-engine found (looked in the llama-ring build directories and $PATH) and this fleet has no engine built in", node)
				}
				bin = p
			}
		}
		var err error
		pid, exited, err = startLocalProc(ctx, bin, localArgs(args), os.Environ(), logPath)
		if err != nil {
			return nil, fmt.Errorf("failed to start local model: %w", err)
		}
	} else {
		sctx, cancel := context.WithTimeout(ctx, 60*time.Second)
		out, err := sshScript(sctx, node, remoteMasterScript("", strings.TrimPrefix(strings.Join(parts[1:], " "), "ring serve "), res.RunID))
		cancel()
		if err != nil {
			return nil, fmt.Errorf("failed to start model on remote %s: %w", node, err)
		}
		text := string(out)
		if strings.Contains(text, "FLEET_NOBIN") {
			return nil, noMasterEngineError(node)
		}
		var status string
		var tail []string
		pid, status, tail = parseLaunchOutput(text)
		if status == "FLEET_DIED" {
			return nil, fmt.Errorf("the server on %s (pid %d) exited right after it started; ~/.cache/fleet/runs/%s.log:\n%s", node, pid, res.RunID, strings.Join(tail, "\n"))
		}
	}
	res.MasterPID = pid
	res.Status = StatusStarting
	res.StartedAt = time.Now().UTC()
	res.saveState()

	// wait for /health, noticing a server that died
	client := &http.Client{Timeout: time.Second}
	start := time.Now()
	for time.Since(start) < RunOnReady {
		if ctx.Err() != nil {
			break
		}
		if isLocal {
			if resp, err := client.Get(fmt.Sprintf("http://127.0.0.1:%d/health", port)); err == nil {
				_ = resp.Body.Close()
				if resp.StatusCode == 200 {
					res.Status = StatusRunning
					break
				}
			}
			select {
			case werr := <-exited:
				res.Status, res.Error = StatusFailed, "the server exited"
				res.saveState()
				return nil, fmt.Errorf("the server (pid %d) exited before it was healthy (%v); %s:\n%s", pid, werr, logPath, tailFile(logPath, 20))
			default:
			}
		} else {
			if remoteHealthy(ctx, node, port) {
				res.Status = StatusRunning
				break
			}
			if pid > 0 && time.Since(start) > 10*time.Second && remoteDead(ctx, node, pid) {
				res.Status, res.Error = StatusFailed, "the server exited"
				res.saveState()
				return nil, fmt.Errorf("the server on %s (pid %d) exited before it was healthy; ~/.cache/fleet/runs/%s.log:\n%s", node, pid, res.RunID, remoteTail(ctx, node, res.RunID, 20))
			}
		}
		select {
		case <-ctx.Done():
		case <-time.After(time.Second):
		}
	}
	if res.Status != StatusRunning {
		last := lastLine(tailFile(logPath, 1))
		if !isLocal {
			last = lastLine(remoteTail(ctx, node, res.RunID, 1))
		}
		res.Warnings = append(res.Warnings, fmt.Sprintf("the server (pid %d) is not healthy yet after %s: it may still be loading the model (log %s on %s; last line: %q). Nothing was stopped.", pid, time.Since(start).Round(time.Second), "~/.cache/fleet/runs/"+res.RunID+".log", node, last))
	}
	res.saveState()
	return res, nil
}
