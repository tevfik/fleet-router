package main

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
)

// serviceChildren lists the processes inside the cgroup of the running fleet.service (user or system) that a restart
// of the service would kill: everything but the daemon itself and its SSH tunnels (the daemon restarts those). These are
// model servers, ring workers and stream connects that an older fleet (before they were started in their own
// systemd scope) deployed from the daemon. Empty when there is no such service.
func serviceChildren() []string {
	for _, scope := range [][]string{{"--user"}, {}} {
		show := func(prop string) string {
			out, err := exec.Command("systemctl", append(append([]string{}, scope...), "show", "fleet.service", "-p", prop, "--value")...).Output()
			if err != nil {
				return ""
			}
			return strings.TrimSpace(string(out))
		}
		if exec.Command("systemctl", append(append([]string{}, scope...), "is-active", "--quiet", "fleet.service")...).Run() != nil {
			continue
		}
		return cgroupChildren(show("ControlGroup"), show("MainPID"), show("KillMode"))
	}
	return nil
}

// cgroupChildren reads /sys/fs/cgroup<cg>/cgroup.procs; killMode "process" means a restart kills only the main pid.
func cgroupChildren(cg, mainPID, killMode string) []string {
	if cg == "" || killMode == "process" {
		return nil
	}
	b, err := os.ReadFile(filepath.Join("/sys/fs/cgroup", cg, "cgroup.procs"))
	if err != nil {
		return nil
	}
	return filterChildren(strings.Fields(string(b)), mainPID, func(pid string) (string, string) {
		comm, _ := os.ReadFile("/proc/" + pid + "/comm")
		cmd, _ := os.ReadFile("/proc/" + pid + "/cmdline")
		return strings.TrimSpace(string(comm)), strings.TrimSpace(strings.ReplaceAll(string(cmd), "\x00", " "))
	})
}

func filterChildren(pids []string, mainPID string, info func(pid string) (comm, cmdline string)) []string {
	var out []string
	for _, p := range pids {
		if p == mainPID {
			continue
		}
		if _, err := strconv.Atoi(p); err != nil {
			continue
		}
		comm, cmd := info(p)
		if comm == "" || comm == "ssh" {
			continue // gone, or a tunnel of the daemon
		}
		if len(cmd) > 100 {
			cmd = cmd[:100] + "…"
		}
		out = append(out, fmt.Sprintf("pid %s: %s", p, cmd))
	}
	return out
}
