package deploy

import (
	"fmt"
	"os"
	"strconv"
	"strings"
	"syscall"
	"time"
)

// The memory guard of a loading stage. The check before the deploy reads the memory free at that moment; a load can
// still run a machine out (another program grows meanwhile, an estimate was off, unified memory where the GPU's
// allocations come out of RAM). Running out does not make a load fail cleanly: the kernel pages and stalls until the
// machine stops answering (dino needed a reset at the desk). While a stage loads, its machine is watched every second;
// three bad seconds in a row stop the stage and fail the deploy:
//   - MemAvailable below max(1 GiB, 2% of MemTotal), or
//   - memory pressure (PSI "full" avg10: every task stalled on memory) at or above 40%.

const (
	guardFloorKiB   = 1 << 20 // 1 GiB
	guardFloorShare = 50      // 1/50 = 2% of MemTotal
	guardStallPct   = 40
	guardRun        = 3
)

// memSample is one reading of a machine's memory.
type memSample struct {
	availKiB, totalKiB int64
	stallPct           float64 // -1: no PSI
	ok                 bool
}

type memGuard struct{ low, hot int }

func newMemGuard() *memGuard { return &memGuard{} }

// check takes one reading; it returns why the stage must stop ("" while it may go on).
func (g *memGuard) check(m memSample) string {
	if !m.ok {
		return ""
	}
	floor := max(int64(guardFloorKiB), m.totalKiB/guardFloorShare)
	if m.availKiB < floor {
		g.low++
	} else {
		g.low = 0
	}
	if m.stallPct >= guardStallPct {
		g.hot++
	} else {
		g.hot = 0
	}
	if g.low >= guardRun || g.hot >= guardRun {
		stall := "not reported"
		if m.stallPct >= 0 {
			stall = fmt.Sprintf("%.0f%%", m.stallPct)
		}
		return fmt.Sprintf("the machine ran out of memory (MemAvailable %d MiB of %d MiB, memory stall %s)", m.availKiB/1024, m.totalKiB/1024, stall)
	}
	return ""
}

// readLocalMem reads this machine's /proc/meminfo and /proc/pressure/memory.
var readLocalMem = func() memSample {
	b, err := os.ReadFile("/proc/meminfo")
	if err != nil {
		return memSample{}
	}
	m := memSample{stallPct: -1}
	for _, ln := range strings.Split(string(b), "\n") {
		f := strings.Fields(ln)
		if len(f) < 2 {
			continue
		}
		n, _ := strconv.ParseInt(f[1], 10, 64)
		switch f[0] {
		case "MemAvailable:":
			m.availKiB, m.ok = n, true
		case "MemTotal:":
			m.totalKiB = n
		}
	}
	if p, err := os.ReadFile("/proc/pressure/memory"); err == nil {
		for _, ln := range strings.Split(string(p), "\n") {
			if !strings.HasPrefix(ln, "full ") {
				continue
			}
			for _, kv := range strings.Fields(ln) {
				if v, ok := strings.CutPrefix(kv, "avg10="); ok {
					m.stallPct, _ = strconv.ParseFloat(v, 64)
				}
			}
		}
	}
	return m
}

// killPID stops a stage process: TERM, then KILL if it is still there after two seconds.
var killPID = func(pid int) {
	if pid <= 0 {
		return
	}
	_ = syscall.Kill(pid, syscall.SIGTERM)
	for i := 0; i < 20; i++ {
		time.Sleep(100 * time.Millisecond)
		if syscall.Kill(pid, 0) != nil {
			return
		}
	}
	_ = syscall.Kill(pid, syscall.SIGKILL)
}

// memGuardScript is one pass of the guard inside a sh loop on the stage's machine (the readiness loop runs it every
// second): FLEET_OOM <why> and the stage stopped when memory ran out three seconds in a row; FLEET_EXITED when the
// stage process is gone (a load that failed: the log tail follows).
func memGuardScript(pid int) string {
	return fmt.Sprintf(`if ! kill -0 %[1]d 2>/dev/null; then echo FLEET_EXITED; exit 0; fi
  fleet_avail=$(awk '/^MemAvailable:/ {print $2}' /proc/meminfo 2>/dev/null); fleet_total=$(awk '/^MemTotal:/ {print $2}' /proc/meminfo 2>/dev/null)
  fleet_floor=$(( ${fleet_total:-0} / %[3]d )); [ "$fleet_floor" -lt %[2]d ] && fleet_floor=%[2]d
  fleet_stall=$(awk '/^full/ {for (i = 1; i <= NF; i++) if ($i ~ /^avg10=/) {split($i, a, "="); print int(a[2])}}' /proc/pressure/memory 2>/dev/null)
  if [ -n "$fleet_avail" ] && [ "$fleet_avail" -lt "$fleet_floor" ]; then fleet_low=$(( ${fleet_low:-0} + 1 )); else fleet_low=0; fi
  if [ "${fleet_stall:-0}" -ge %[4]d ]; then fleet_hot=$(( ${fleet_hot:-0} + 1 )); else fleet_hot=0; fi
  if [ "$fleet_low" -ge %[5]d ] || [ "$fleet_hot" -ge %[5]d ]; then
    kill -TERM %[1]d 2>/dev/null; sleep 2; kill -KILL %[1]d 2>/dev/null
    echo "FLEET_OOM the machine ran out of memory (MemAvailable $(( ${fleet_avail:-0} / 1024 )) MiB of $(( ${fleet_total:-0} / 1024 )) MiB, memory stall ${fleet_stall:-?}%%)"; exit 0
  fi`, pid, guardFloorKiB, guardFloorShare, guardStallPct, guardRun)
}
