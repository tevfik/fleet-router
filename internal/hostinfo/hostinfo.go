// Package hostinfo reads what the machine a fleet runs on is doing: CPU load and temperature, RAM and swap,
// GPUs (load, VRAM, temperature, power), disks (space, read/write rate, NVMe temperature) and load average.
// The ideas are strata's (serve/telemetry.py); without cgo NVML is reached through nvidia-smi, AMD cards
// through the amdgpu driver's sysfs files, the rest through /proc and /sys. Anything that cannot be read is
// -1 (or absent); nothing here fails.
//
// The sampler runs only while someone looks: it starts on the first Snapshot and stops after idleStop
// without one, so a fleet nobody watches pays nothing.
package hostinfo

import (
	"bufio"
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"sort"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"
)

const (
	Every    = 2 * time.Second
	Keep     = 60 // samples of history (two minutes)
	idleStop = 5 * time.Minute
)

// GPU is one card. Unknown values are -1.
type GPU struct {
	Index      int     `json:"index"`
	Name       string  `json:"name"`
	Util       float64 `json:"util"`      // %
	MemUsed    int64   `json:"mem_used"`  // bytes (-1: unknown — unified memory reports none)
	MemTotal   int64   `json:"mem_total"` // bytes
	Temp       float64 `json:"temp"`      // °C
	Power      float64 `json:"power"`     // W
	PowerLimit float64 `json:"power_limit"`
}

// Temp is one temperature sensor.
type Temp struct {
	Name string  `json:"name"` // "cpu", "nvme0", …
	C    float64 `json:"c"`
}

// Disk is one mounted filesystem.
type Disk struct {
	Mount string `json:"mount"`
	Dev   string `json:"dev"`
	Used  int64  `json:"used"`
	Total int64  `json:"total"`
}

// Sample is one reading.
type Sample struct {
	At        time.Time `json:"at"`
	CPU       float64   `json:"cpu"` // % of all cores
	Load1     float64   `json:"load1"`
	RAMUsed   int64     `json:"ram_used"`
	RAMTotal  int64     `json:"ram_total"`
	SwapUsed  int64     `json:"swap_used"`
	SwapTotal int64     `json:"swap_total"`
	Temps     []Temp    `json:"temps,omitempty"`
	GPUs      []GPU     `json:"gpus,omitempty"`
	DiskRead  float64   `json:"disk_read_mb"`  // MB/s, all disks
	DiskWrite float64   `json:"disk_write_mb"` // MB/s
	NetRx     float64   `json:"net_rx_mb"`     // MB/s, all non-loopback interfaces
	NetTx     float64   `json:"net_tx_mb"`     // MB/s
	Disks     []Disk    `json:"disks,omitempty"`
}

// Static does not change while the machine runs.
type Static struct {
	Host    string `json:"host"`
	OS      string `json:"os"`
	Kernel  string `json:"kernel,omitempty"`
	CPUName string `json:"cpu_name,omitempty"`
	Threads int    `json:"threads"`
	Cores   int    `json:"cores,omitempty"`
	Machine string `json:"machine,omitempty"` // the board / product (DMI or device tree), e.g. "ASUSTeK GX10"
}

// Snapshot is the current reading, the static facts and the history of the main series (oldest first).
type Snapshot struct {
	Static  Static               `json:"static"`
	Now     Sample               `json:"now"`
	Uptime  int64                `json:"uptime_s,omitempty"`
	History map[string][]float64 `json:"history"` // cpu, ram (%), gpu (%), vram (%), cpu_temp, gpu_temp, disk_read, disk_write, net_rx, net_tx
	Notes   []string             `json:"notes,omitempty"`
}

// Sampler keeps the readings of this machine.
type Sampler struct {
	mu        sync.Mutex
	running   bool
	lastAsk   time.Time
	now       Sample
	hist      map[string][]float64
	static    Static
	notes     []string
	prevCPU   [2]uint64 // idle, total
	prevDisk  [2]uint64 // sectors read, written
	prevNet   [2]uint64 // rx, tx bytes
	prevNetOK bool
	prevAt    time.Time
	noSMI     time.Time // nvidia-smi missing or failing: retried after a minute
}

func New() *Sampler { return &Sampler{hist: map[string][]float64{}, static: readStatic()} }

// Snapshot returns the current state, starting the sampler (and taking a first reading) if needed.
func (s *Sampler) Snapshot() Snapshot {
	s.mu.Lock()
	s.lastAsk = time.Now()
	start := !s.running
	s.running = s.running || start
	s.mu.Unlock()
	if start {
		s.sample() // primes the rates, so the first answer already has a CPU % and disk rates
		time.Sleep(250 * time.Millisecond)
		s.sample()
		go s.loop()
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	h := make(map[string][]float64, len(s.hist))
	for k, v := range s.hist {
		h[k] = append([]float64{}, v...)
	}
	return Snapshot{Static: s.static, Now: s.now, Uptime: uptime(), History: h, Notes: append([]string{}, s.notes...)}
}

func (s *Sampler) loop() {
	t := time.NewTicker(Every)
	defer t.Stop()
	for range t.C {
		s.mu.Lock()
		idle := time.Since(s.lastAsk) > idleStop
		if idle {
			s.running = false
			s.hist = map[string][]float64{} // a gap would make the history lie
		}
		s.mu.Unlock()
		if idle {
			return
		}
		s.sample()
	}
}

func (s *Sampler) push(k string, v float64) {
	h := append(s.hist[k], v)
	if len(h) > Keep {
		h = h[len(h)-Keep:]
	}
	s.hist[k] = h
}

func (s *Sampler) sample() {
	now := time.Now()
	x := Sample{At: now, CPU: -1, Load1: loadavg()}
	idle, total := cpuTimes()
	x.RAMUsed, x.RAMTotal, x.SwapUsed, x.SwapTotal = meminfo()
	x.Temps = temps()
	rd, wr := diskSectors()
	rx, tx, netOK := netBytes()
	x.Disks = disks()
	gpus, note := s.gpus()
	x.GPUs = gpus

	s.mu.Lock()
	defer s.mu.Unlock()
	if s.prevCPU[1] > 0 && total > s.prevCPU[1] {
		x.CPU = 100 * (1 - float64(idle-s.prevCPU[0])/float64(total-s.prevCPU[1]))
	}
	x.DiskRead, x.DiskWrite = -1, -1
	x.NetRx, x.NetTx = -1, -1
	if dt := now.Sub(s.prevAt).Seconds(); !s.prevAt.IsZero() && dt > 0 {
		if rd >= s.prevDisk[0] && wr >= s.prevDisk[1] {
			x.DiskRead = float64(rd-s.prevDisk[0]) * 512 / dt / (1 << 20)
			x.DiskWrite = float64(wr-s.prevDisk[1]) * 512 / dt / (1 << 20)
		}
		if s.prevNetOK && netOK && rx >= s.prevNet[0] && tx >= s.prevNet[1] {
			x.NetRx = float64(rx-s.prevNet[0]) / dt / (1 << 20)
			x.NetTx = float64(tx-s.prevNet[1]) / dt / (1 << 20)
		}
	}
	s.prevCPU, s.prevDisk, s.prevNet, s.prevNetOK, s.prevAt = [2]uint64{idle, total}, [2]uint64{rd, wr}, [2]uint64{rx, tx}, netOK, now
	s.now = x
	s.notes = nil
	if note != "" {
		s.notes = append(s.notes, note)
	}
	if x.CPU >= 0 {
		s.push("cpu", round1(x.CPU))
	}
	if x.RAMTotal > 0 {
		s.push("ram", round1(100*float64(x.RAMUsed)/float64(x.RAMTotal)))
	}
	if x.DiskRead >= 0 {
		s.push("disk_read", round1(x.DiskRead))
		s.push("disk_write", round1(x.DiskWrite))
	}
	if x.NetRx >= 0 {
		s.push("net_rx", round1(x.NetRx))
		s.push("net_tx", round1(x.NetTx))
	}
	for _, t := range x.Temps {
		if t.Name == "cpu" {
			s.push("cpu_temp", t.C)
		}
	}
	if len(gpus) > 0 { // several cards: mean load, hottest card, total memory (as strata does)
		var util, n, temp float64 = 0, 0, -1
		var used, tot int64
		for _, g := range gpus {
			if g.Util >= 0 {
				util, n = util+g.Util, n+1
			}
			temp = max(temp, g.Temp)
			if g.MemUsed >= 0 && g.MemTotal > 0 {
				used, tot = used+g.MemUsed, tot+g.MemTotal
			}
		}
		if n > 0 {
			s.push("gpu", round1(util/n))
		}
		if temp >= 0 {
			s.push("gpu_temp", temp)
		}
		if tot > 0 {
			s.push("vram", round1(100*float64(used)/float64(tot)))
		}
	}
}

func round1(v float64) float64 { return float64(int(v*10+0.5)) / 10 }

// ---- readers ----

func readFile(p string) string {
	b, err := os.ReadFile(p)
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(b))
}

func readInt(p string) (int64, bool) {
	v, err := strconv.ParseInt(readFile(p), 10, 64)
	return v, err == nil
}

func readStatic() Static {
	h, _ := os.Hostname()
	st := Static{Host: strings.Split(h, ".")[0], OS: runtime.GOOS + "/" + runtime.GOARCH, Threads: runtime.NumCPU(),
		Kernel: readFile("/proc/sys/kernel/osrelease")}
	if f, err := os.Open("/etc/os-release"); err == nil {
		sc := bufio.NewScanner(f)
		for sc.Scan() {
			if v, ok := strings.CutPrefix(sc.Text(), "PRETTY_NAME="); ok {
				st.OS = strings.Trim(v, `"`)
			}
		}
		f.Close()
	}
	st.CPUName, st.Cores = parseCPUInfo(readFile("/proc/cpuinfo"))
	st.Machine = machineName(readFile("/sys/class/dmi/id/sys_vendor"), readFile("/sys/class/dmi/id/product_name"),
		strings.Trim(readFile("/proc/device-tree/model"), "\x00"))
	return st
}

// armParts names the cores of ARM's own designs (CPU implementer 0x41) by their "CPU part" number.
var armParts = map[string]string{
	"0xd03": "Cortex-A53", "0xd05": "Cortex-A55", "0xd07": "Cortex-A57", "0xd08": "Cortex-A72", "0xd09": "Cortex-A73",
	"0xd0a": "Cortex-A75", "0xd0b": "Cortex-A76", "0xd0c": "Neoverse-N1", "0xd0d": "Cortex-A77", "0xd40": "Neoverse-V1",
	"0xd41": "Cortex-A78", "0xd44": "Cortex-X1", "0xd46": "Cortex-A510", "0xd47": "Cortex-A710", "0xd48": "Cortex-X2",
	"0xd49": "Neoverse-N2", "0xd4b": "Cortex-A78C", "0xd4d": "Cortex-A715", "0xd4e": "Cortex-X3", "0xd4f": "Neoverse-V2",
	"0xd80": "Cortex-A520", "0xd81": "Cortex-A720", "0xd82": "Cortex-X4", "0xd84": "Neoverse-V3", "0xd85": "Cortex-X925",
	"0xd87": "Cortex-A725", "0xd8e": "Neoverse-N3",
}

// parseCPUInfo reads the CPU's name and physical core count from /proc/cpuinfo. x86 names itself ("model
// name"); ARM lists part numbers, which are named here and counted per kind ("10× Cortex-X925 + 10× Cortex-A725").
func parseCPUInfo(text string) (name string, cores int) {
	coreIDs := map[string]bool{}
	parts := map[string]int{}
	var order []string
	var phys, impl string
	for _, line := range strings.Split(text, "\n") {
		k, v, ok := strings.Cut(line, ":")
		if !ok {
			continue
		}
		k, v = strings.TrimSpace(k), strings.TrimSpace(v)
		switch k {
		case "model name", "Model", "Hardware":
			if name == "" {
				name = v
			}
		case "physical id":
			phys = v
		case "core id":
			coreIDs[phys+"/"+v] = true
		case "CPU implementer":
			impl = v
		case "CPU part":
			p := v
			if n, ok := armParts[strings.ToLower(v)]; ok && impl == "0x41" {
				p = n
			} else {
				p = "ARM part " + v
			}
			if parts[p] == 0 {
				order = append(order, p)
			}
			parts[p]++
		}
	}
	cores = len(coreIDs)
	if name == "" && len(parts) > 0 {
		var names []string
		for _, p := range order {
			names = append(names, fmt.Sprintf("%d× %s", parts[p], p))
		}
		name = strings.Join(names, " + ")
	}
	return name, cores
}

// machineName is the board or product name: DMI vendor + product, else the device tree's model.
func machineName(vendor, product, dtModel string) string {
	junk := func(s string) bool {
		l := strings.ToLower(s)
		return s == "" || strings.Contains(l, "to be filled") || strings.Contains(l, "default string") || strings.Contains(l, "system product name")
	}
	if !junk(product) {
		v := strings.Fields(vendor)
		if len(v) > 0 && !junk(vendor) && !strings.HasPrefix(strings.ToLower(product), strings.ToLower(v[0])) {
			return strings.TrimSuffix(strings.TrimSuffix(v[0], ","), ".") + " " + product
		}
		return product
	}
	return dtModel
}

func uptime() int64 {
	f := strings.Fields(readFile("/proc/uptime"))
	if len(f) == 0 {
		return 0
	}
	v, _ := strconv.ParseFloat(f[0], 64)
	return int64(v)
}

func loadavg() float64 {
	f := strings.Fields(readFile("/proc/loadavg"))
	if len(f) == 0 {
		return -1
	}
	v, err := strconv.ParseFloat(f[0], 64)
	if err != nil {
		return -1
	}
	return v
}

func cpuTimes() (idle, total uint64) {
	line, _, _ := strings.Cut(readFile("/proc/stat"), "\n")
	f := strings.Fields(line)
	if len(f) < 5 || f[0] != "cpu" {
		return 0, 0
	}
	for i, x := range f[1:] {
		v, _ := strconv.ParseUint(x, 10, 64)
		if i == 7 || i == 8 { // guest time is already counted in user
			continue
		}
		total += v
		if i == 3 || i == 4 { // idle + iowait
			idle += v
		}
	}
	return idle, total
}

func meminfo() (used, total, swapUsed, swapTotal int64) {
	m := map[string]int64{}
	for _, line := range strings.Split(readFile("/proc/meminfo"), "\n") {
		k, v, ok := strings.Cut(line, ":")
		if !ok {
			continue
		}
		f := strings.Fields(v)
		if len(f) > 0 {
			n, _ := strconv.ParseInt(f[0], 10, 64)
			m[k] = n * 1024
		}
	}
	if m["MemTotal"] == 0 {
		return -1, 0, -1, 0
	}
	return m["MemTotal"] - m["MemAvailable"], m["MemTotal"], m["SwapTotal"] - m["SwapFree"], m["SwapTotal"]
}

// temps: the CPU package (coretemp / k10temp / zenpower / cpu_thermal …) and every NVMe drive.
func temps() []Temp {
	var cpu float64 = -1
	var out []Temp
	dirs, _ := filepath.Glob("/sys/class/hwmon/hwmon*")
	sort.Strings(dirs)
	nvme := 0
	for _, d := range dirs {
		name := readFile(filepath.Join(d, "name"))
		v, ok := readInt(filepath.Join(d, "temp1_input"))
		if !ok || v <= 0 {
			continue
		}
		c := float64(v) / 1000
		switch name {
		case "coretemp", "k10temp", "zenpower", "cpu_thermal", "soc_thermal":
			cpu = max(cpu, c)
		case "nvme":
			out = append(out, Temp{Name: "nvme" + strconv.Itoa(nvme), C: c})
			nvme++
		}
	}
	if cpu < 0 { // no CPU hwmon: the thermal zones (x86_pkg_temp, cpu-thermal …)
		zones, _ := filepath.Glob("/sys/class/thermal/thermal_zone*")
		for _, z := range zones {
			t := strings.ToLower(readFile(filepath.Join(z, "type")))
			if strings.Contains(t, "pkg") || strings.Contains(t, "cpu") || strings.Contains(t, "soc") {
				if v, ok := readInt(filepath.Join(z, "temp")); ok && v > 0 {
					cpu = max(cpu, float64(v)/1000)
				}
			}
		}
	}
	if cpu >= 0 {
		return append([]Temp{{Name: "cpu", C: cpu}}, out...)
	}
	// no CPU sensor at all (e.g. ARM boards such as the DGX Spark): the ACPI thermal zones are the
	// closest thing — shown as "board", the hottest zone
	board := -1.0
	zones, _ := filepath.Glob("/sys/class/thermal/thermal_zone*")
	for _, z := range zones {
		if strings.Contains(strings.ToLower(readFile(filepath.Join(z, "type"))), "acpitz") {
			if v, ok := readInt(filepath.Join(z, "temp")); ok && v > 0 {
				board = max(board, float64(v)/1000)
			}
		}
	}
	if board >= 0 {
		out = append([]Temp{{Name: "board", C: board}}, out...)
	}
	return out
}

// diskSectors adds up the sectors read and written by whole disks (not partitions, loop or ram devices).
func diskSectors() (rd, wr uint64) {
	for _, line := range strings.Split(readFile("/proc/diskstats"), "\n") {
		f := strings.Fields(line)
		if len(f) < 10 {
			continue
		}
		dev := f[2]
		if strings.HasPrefix(dev, "loop") || strings.HasPrefix(dev, "ram") || strings.HasPrefix(dev, "zram") || strings.HasPrefix(dev, "dm-") {
			continue
		}
		if _, err := os.Stat("/sys/block/" + dev); err != nil { // a partition: counted by its disk
			continue
		}
		r, _ := strconv.ParseUint(f[5], 10, 64)
		w, _ := strconv.ParseUint(f[9], 10, 64)
		rd, wr = rd+r, wr+w
	}
	return rd, wr
}

// netBytes adds up receive and transmit bytes across all non-loopback network interfaces.
func netBytes() (rx, tx uint64, ok bool) {
	c := readFile("/proc/net/dev")
	if c == "" {
		return 0, 0, false
	}
	for _, line := range strings.Split(c, "\n") {
		idx := strings.IndexByte(line, ':')
		if idx < 0 {
			continue
		}
		iface := strings.TrimSpace(line[:idx])
		if iface == "" || iface == "lo" || strings.HasPrefix(iface, "lo:") {
			continue
		}
		fields := strings.Fields(line[idx+1:])
		if len(fields) < 9 {
			continue
		}
		r, errR := strconv.ParseUint(fields[0], 10, 64)
		t, errT := strconv.ParseUint(fields[8], 10, 64)
		if errR == nil && errT == nil {
			rx += r
			tx += t
			ok = true
		}
	}
	return rx, tx, ok
}

var realFS = map[string]bool{"ext4": true, "ext3": true, "xfs": true, "btrfs": true, "zfs": true, "f2fs": true, "bcachefs": true, "ntfs3": true, "ntfs": true}

// disks lists the real filesystems (one per device), largest first, at most six.
func disks() []Disk {
	f, err := os.Open("/proc/self/mounts")
	if err != nil {
		return nil
	}
	defer f.Close()
	seen := map[string]bool{}
	var out []Disk
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		x := strings.Fields(sc.Text())
		if len(x) < 3 || !realFS[x[2]] || seen[x[0]] || strings.HasPrefix(x[1], "/snap") || strings.HasPrefix(x[1], "/boot") {
			continue
		}
		var st syscall.Statfs_t
		if syscall.Statfs(x[1], &st) != nil || st.Blocks == 0 {
			continue
		}
		seen[x[0]] = true
		bs := int64(st.Bsize)
		total := int64(st.Blocks) * bs
		if total < 1<<30 {
			continue
		}
		out = append(out, Disk{Mount: x[1], Dev: filepath.Base(x[0]), Total: total, Used: total - int64(st.Bfree)*bs})
	}
	sort.SliceStable(out, func(i, j int) bool { return out[i].Total > out[j].Total })
	if len(out) > 6 {
		out = out[:6]
	}
	return out
}

// gpus: NVIDIA cards through nvidia-smi, AMD cards through sysfs.
func (s *Sampler) gpus() ([]GPU, string) {
	var out []GPU
	note := ""
	if time.Now().After(s.noSMI) {
		g, err := nvidiaSMI()
		switch {
		case err == nil:
			out = append(out, g...)
		case !os.IsNotExist(err) && !strings.Contains(err.Error(), "executable file not found"):
			note = "nvidia-smi: " + err.Error()
			s.noSMI = time.Now().Add(time.Minute)
		default:
			s.noSMI = time.Now().Add(time.Minute)
		}
	}
	return append(out, amdGPUs(len(out))...), note
}

func num(s string) float64 {
	v, err := strconv.ParseFloat(strings.TrimSpace(s), 64)
	if err != nil {
		return -1 // "[N/A]", "[Not Supported]"
	}
	return v
}

func nvidiaSMI() ([]GPU, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	b, err := exec.CommandContext(ctx, "nvidia-smi", "--query-gpu=index,name,utilization.gpu,memory.used,memory.total,temperature.gpu,power.draw,power.limit",
		"--format=csv,noheader,nounits").Output()
	if err != nil {
		return nil, err
	}
	return parseSMI(string(b)), nil
}

func parseSMI(out string) []GPU {
	var gs []GPU
	for _, line := range strings.Split(strings.TrimSpace(out), "\n") {
		f := strings.Split(line, ",")
		if len(f) < 8 {
			continue
		}
		mib := func(s string) int64 {
			if v := num(s); v >= 0 {
				return int64(v) << 20
			}
			return -1
		}
		gs = append(gs, GPU{Index: int(num(f[0])), Name: strings.TrimSpace(f[1]), Util: num(f[2]), MemUsed: mib(f[3]), MemTotal: mib(f[4]),
			Temp: num(f[5]), Power: num(f[6]), PowerLimit: num(f[7])})
	}
	return gs
}

func amdGPUs(first int) []GPU {
	cards, _ := filepath.Glob("/sys/class/drm/card[0-9]*/device")
	sort.Strings(cards)
	var out []GPU
	for _, d := range cards {
		if readFile(filepath.Join(d, "vendor")) != "0x1002" {
			continue
		}
		busy, ok := readInt(filepath.Join(d, "gpu_busy_percent"))
		if !ok {
			continue
		}
		g := GPU{Index: first + len(out), Name: "AMD " + readFile(filepath.Join(d, "product_name")), Util: float64(busy), MemUsed: -1, MemTotal: -1, Temp: -1, Power: -1, PowerLimit: -1}
		if g.Name == "AMD " {
			g.Name = "AMD GPU " + readFile(filepath.Join(d, "device"))
		}
		if v, ok := readInt(filepath.Join(d, "mem_info_vram_used")); ok {
			g.MemUsed = v
		}
		if v, ok := readInt(filepath.Join(d, "mem_info_vram_total")); ok {
			g.MemTotal = v
		}
		if hw, _ := filepath.Glob(filepath.Join(d, "hwmon", "hwmon*")); len(hw) > 0 {
			if v, ok := readInt(filepath.Join(hw[0], "temp1_input")); ok {
				g.Temp = float64(v) / 1000
			}
			for _, p := range []string{"power1_average", "power1_input"} {
				if v, ok := readInt(filepath.Join(hw[0], p)); ok {
					g.Power = float64(v) / 1e6
					break
				}
			}
			if v, ok := readInt(filepath.Join(hw[0], "power1_cap")); ok {
				g.PowerLimit = float64(v) / 1e6
			}
		}
		out = append(out, g)
	}
	return out
}
