package deploy

import (
	"context"
	"os/exec"
	"sync"
	"syscall"
	"time"
)

// Processes the deploy starts (model server, ring workers, stream connects) must outlive the fleet daemon that
// started them. Under systemd the daemon's service has KillMode=control-group, so a restart (fleet update, systemctl
// restart) kills everything in its cgroup, and setsid does NOT help: it leaves the cgroup unchanged. Only a process
// in its own transient scope (systemd-run --user --scope) leaves the service's cgroup. Without a user systemd the
// fallback is setsid (own session, so no SIGHUP/terminal signal either).

var (
	scopeOnce sync.Once
	scopeOK   bool
)

// systemdScopeAvailable: systemd-run can put a process into its own transient scope of the user manager here.
func systemdScopeAvailable() bool {
	scopeOnce.Do(func() {
		if _, err := exec.LookPath("systemd-run"); err != nil {
			return
		}
		// bounded: a wedged user bus made the first deploy hang here
		ctx, cancel := context.WithTimeout(context.Background(), 8*time.Second)
		defer cancel()
		scopeOK = exec.CommandContext(ctx, "systemd-run", "--user", "--scope", "--collect", "--quiet", "true").Run() == nil
	})
	return scopeOK
}

// detachedCommand returns the program and arguments to start bin so that it is not part of this process's service
// cgroup, and the process attributes to start it with. systemd-run in scope mode execs the command itself, so the
// pid of the started process is the pid of the command.
func detachedCommand(bin string, args []string) (string, []string, *syscall.SysProcAttr) {
	if systemdScopeAvailable() {
		full := append([]string{"--user", "--scope", "--collect", "--quiet", "--", bin}, args...)
		return "systemd-run", full, &syscall.SysProcAttr{Setsid: true}
	}
	return bin, args, &syscall.SysProcAttr{Setsid: true}
}

// detachShell sets $FLEET_DETACH in a sh script: a command prefix that does the same as detachedCommand ("" when
// neither systemd-run nor setsid exists). Use it as: nohup $FLEET_DETACH cmd args &
const detachShell = `FLEET_DETACH=""
if command -v systemd-run >/dev/null 2>&1 && { if command -v timeout >/dev/null 2>&1; then timeout 8 systemd-run --user --scope --collect --quiet true; else systemd-run --user --scope --collect --quiet true; fi; } >/dev/null 2>&1; then FLEET_DETACH="systemd-run --user --scope --collect --quiet --"
elif command -v setsid >/dev/null 2>&1; then FLEET_DETACH="setsid"; fi
`
