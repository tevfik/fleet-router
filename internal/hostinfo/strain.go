package hostinfo

import (
	"context"
	"fmt"
	"os/exec"
	"strconv"
	"strings"
	"time"
)

// Strain says whether the machine is struggling right now, in a way that slows its models down. It is
// cheap (no sampler): the kernel's pressure stall information (PSI, /proc/pressure) is already averaged over
// 10 seconds, and nvidia-smi says whether a GPU is slowing itself down for heat.
//
// Swap use is deliberately not a sign: a machine with plenty of memory (a DGX Spark's 128 GB of unified
// memory) may keep cold pages in swap for days and be perfectly fast. What hurts is tasks waiting for
// memory — PSI "full" counts the time every task was stalled on it — and that is what is measured.
type Strain struct {
	At       time.Time `json:"at"`
	MemFull  float64   `json:"mem_full"` // % of the last 10 s all tasks were stalled on memory (-1: no PSI)
	MemSome  float64   `json:"mem_some"` // % some task was
	IOFull   float64   `json:"io_full"`
	CPUSome  float64   `json:"cpu_some"`
	GPUTemp  float64   `json:"gpu_temp"` // hottest card (-1: none)
	Throttle []string  `json:"throttle,omitempty"`
	DiskMax  float64   `json:"disk_max"` // fullest filesystem, %
	DiskAt   string    `json:"disk_at,omitempty"`
	// Reasons make routing prefer other machines while they last; Warnings are only shown.
	Reasons  []string `json:"reasons,omitempty"`
	Warnings []string `json:"warnings,omitempty"`
}

// Thresholds of Strain. A memory stall of 10% means requests on this machine wait a tenth of the time.
const (
	MemFullLimit = 10.0
	GPUTempLimit = 90.0
	IOFullLimit  = 30.0
	CPUSomeLimit = 80.0
	DiskLimit    = 90.0
)

// parsePSI reads "some avg10=…" / "full avg10=…" from a /proc/pressure file (-1 when absent).
func parsePSI(text string) (some, full float64) {
	some, full = -1, -1
	for _, line := range strings.Split(text, "\n") {
		f := strings.Fields(line)
		if len(f) < 2 {
			continue
		}
		for _, kv := range f[1:] {
			if v, ok := strings.CutPrefix(kv, "avg10="); ok {
				x, err := strconv.ParseFloat(v, 64)
				if err != nil {
					continue
				}
				switch f[0] {
				case "some":
					some = x
				case "full":
					full = x
				}
			}
		}
	}
	return some, full
}

// thermalThrottle asks nvidia-smi which cards are slowing down for heat (newer drivers name the field
// clocks_event_reasons, older ones clocks_throttle_reasons; a driver that knows neither reports nothing).
func thermalThrottle() []string {
	for _, f := range []string{"clocks_event_reasons", "clocks_throttle_reasons"} {
		ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
		b, err := exec.CommandContext(ctx, "nvidia-smi", "--query-gpu=index,"+f+".hw_thermal_slowdown,"+f+".sw_thermal_slowdown",
			"--format=csv,noheader").Output()
		cancel()
		if err != nil {
			continue
		}
		return parseThrottle(string(b))
	}
	return nil
}

func parseThrottle(out string) []string {
	var hot []string
	for _, line := range strings.Split(strings.TrimSpace(out), "\n") {
		f := strings.Split(line, ",")
		if len(f) < 3 {
			continue
		}
		for i, kind := range []string{"hardware", "software"} {
			if strings.TrimSpace(f[1+i]) == "Active" {
				hot = append(hot, fmt.Sprintf("GPU %s: %s thermal slowdown", strings.TrimSpace(f[0]), kind))
			}
		}
	}
	return hot
}

// ReadStrain takes one reading of this machine.
func ReadStrain() Strain {
	st := Strain{At: time.Now(), GPUTemp: -1}
	st.MemSome, st.MemFull = parsePSI(readFile("/proc/pressure/memory"))
	_, st.IOFull = parsePSI(readFile("/proc/pressure/io"))
	st.CPUSome, _ = parsePSI(readFile("/proc/pressure/cpu"))
	for _, g := range ReadGPUs() {
		st.GPUTemp = max(st.GPUTemp, g.Temp)
	}
	st.Throttle = thermalThrottle()
	for _, d := range disks() {
		if p := 100 * float64(d.Used) / float64(d.Total); p > st.DiskMax {
			st.DiskMax, st.DiskAt = p, d.Mount
		}
	}
	st.judge()
	return st
}

// judge turns the figures into reasons (routing avoids the machine) and warnings (shown only).
func (st *Strain) judge() {
	st.Reasons, st.Warnings = nil, nil
	if st.MemFull >= MemFullLimit {
		st.Reasons = append(st.Reasons, fmt.Sprintf("memory pressure: every task stalled %.0f%% of the last 10 s", st.MemFull))
	}
	st.Reasons = append(st.Reasons, st.Throttle...)
	if st.GPUTemp >= GPUTempLimit && len(st.Throttle) == 0 {
		st.Reasons = append(st.Reasons, fmt.Sprintf("GPU at %.0f°C", st.GPUTemp))
	}
	if st.IOFull >= IOFullLimit {
		st.Warnings = append(st.Warnings, fmt.Sprintf("disk I/O pressure: stalled %.0f%% of the last 10 s (loading models is slow)", st.IOFull))
	}
	if st.CPUSome >= CPUSomeLimit {
		st.Warnings = append(st.Warnings, fmt.Sprintf("CPU pressure: tasks waited for a CPU %.0f%% of the last 10 s", st.CPUSome))
	}
	if st.DiskMax >= DiskLimit {
		st.Warnings = append(st.Warnings, fmt.Sprintf("disk %s is %.0f%% full", st.DiskAt, st.DiskMax))
	}
}

// Strained reports whether routing should prefer other machines.
func (st Strain) Strained() bool { return len(st.Reasons) > 0 }
