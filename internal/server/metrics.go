package server

import (
	"fmt"
	"net/http"
	"strings"
)

// metrics serves Prometheus text format: per-endpoint health, speed, traffic and queue figures.
func (s *Server) metrics(w http.ResponseWriter, r *http.Request) {
	if !s.adminAllowed(r) {
		httpErr(w, 403, "metrics are available from loopback or with the admin token")
		return
	}
	st := s.Status()
	var b strings.Builder
	type metric struct{ name, help, typ string }
	ms := []metric{
		{"fleet_endpoint_up", "1 when the endpoint passed its last health check", "gauge"},
		{"fleet_endpoint_latency_ms", "health-check latency in milliseconds", "gauge"},
		{"fleet_endpoint_tokens_per_second", "decode speed (live average, else benchmark)", "gauge"},
		{"fleet_endpoint_ttft_ms", "time to first token in milliseconds", "gauge"},
		{"fleet_endpoint_prefill_tokens_per_second", "prompt processing speed", "gauge"},
		{"fleet_endpoint_in_use", "requests currently holding a slot", "gauge"},
		{"fleet_endpoint_waiting", "requests queued for a slot", "gauge"},
		{"fleet_endpoint_requests_total", "requests answered since start", "counter"},
		{"fleet_endpoint_errors_total", "failed attempts (connection errors, 5xx, 429) since start", "counter"},
		{"fleet_endpoint_prompt_tokens_total", "prompt tokens sent since start", "counter"},
		{"fleet_endpoint_completion_tokens_total", "completion tokens produced since start", "counter"},
	}
	val := func(m string, e EpStatus) float64 {
		switch m {
		case "fleet_endpoint_up":
			if e.OK {
				return 1
			}
			return 0
		case "fleet_endpoint_latency_ms":
			return float64(e.LatencyMS)
		case "fleet_endpoint_tokens_per_second":
			return e.TPS
		case "fleet_endpoint_ttft_ms":
			return float64(e.TTFTMS)
		case "fleet_endpoint_prefill_tokens_per_second":
			return e.PrefillTPS
		case "fleet_endpoint_in_use":
			return float64(e.InUse)
		case "fleet_endpoint_waiting":
			return float64(e.Waiting)
		case "fleet_endpoint_requests_total":
			return float64(e.Requests)
		case "fleet_endpoint_errors_total":
			return float64(e.Errors)
		case "fleet_endpoint_prompt_tokens_total":
			return float64(e.TokensIn)
		}
		return float64(e.TokensOut)
	}
	for _, m := range ms {
		fmt.Fprintf(&b, "# HELP %s %s\n# TYPE %s %s\n", m.name, m.help, m.name, m.typ)
		for _, e := range st.Endpoints {
			fmt.Fprintf(&b, "%s{endpoint=%q,node=%q} %g\n", m.name, e.Name, e.Node, val(m.name, e))
		}
	}
	s.hostMetrics(&b, st)
	w.Header().Set("Content-Type", "text/plain; version=0.0.4")
	w.Write([]byte(b.String()))
}

// hostMetrics adds this machine's readings (scraping keeps the sampler running), its AI score, and every
// node's ping and strain.
func (s *Server) hostMetrics(b *strings.Builder, st Status) {
	g := func(name, help string) { fmt.Fprintf(b, "# HELP %s %s\n# TYPE %s gauge\n", name, help, name) }
	snap := s.host.Snapshot()
	n := snap.Now
	g("fleet_host_cpu_percent", "CPU use of this machine, % of all cores")
	fmt.Fprintf(b, "fleet_host_cpu_percent %g\n", n.CPU)
	g("fleet_host_ram_used_bytes", "RAM in use (total − available)")
	fmt.Fprintf(b, "fleet_host_ram_used_bytes %d\n", n.RAMUsed)
	g("fleet_host_ram_total_bytes", "RAM size")
	fmt.Fprintf(b, "fleet_host_ram_total_bytes %d\n", n.RAMTotal)
	g("fleet_host_temperature_celsius", "temperature sensors (cpu, board, nvme…)")
	for _, t := range n.Temps {
		fmt.Fprintf(b, "fleet_host_temperature_celsius{sensor=%q} %g\n", t.Name, t.C)
	}
	g("fleet_host_gpu_percent", "GPU load")
	for _, x := range n.GPUs {
		fmt.Fprintf(b, "fleet_host_gpu_percent{gpu=\"%d\",name=%q} %g\n", x.Index, x.Name, x.Util)
	}
	g("fleet_host_gpu_temperature_celsius", "GPU temperature")
	for _, x := range n.GPUs {
		fmt.Fprintf(b, "fleet_host_gpu_temperature_celsius{gpu=\"%d\",name=%q} %g\n", x.Index, x.Name, x.Temp)
	}
	g("fleet_host_gpu_memory_used_bytes", "VRAM in use (-1: unified memory)")
	for _, x := range n.GPUs {
		fmt.Fprintf(b, "fleet_host_gpu_memory_used_bytes{gpu=\"%d\",name=%q} %d\n", x.Index, x.Name, x.MemUsed)
	}
	g("fleet_host_disk_used_ratio", "filesystem use, 0–1")
	for _, d := range n.Disks {
		fmt.Fprintf(b, "fleet_host_disk_used_ratio{mount=%q} %g\n", d.Mount, float64(d.Used)/float64(d.Total))
	}
	if bench, _ := s.HWBench(); bench != nil {
		g("fleet_host_ai_score", "machine benchmark: est. tok/s of an 8B Q4 model × √(largest model that fits / 8B)")
		fmt.Fprintf(b, "fleet_host_ai_score %d\n", bench.Score)
	}
	g("fleet_node_ping_ms", "fastest health check of the node's models")
	for _, nd := range st.Nodes {
		fmt.Fprintf(b, "fleet_node_ping_ms{node=%q} %d\n", nd.Node, nd.PingMS)
	}
	strains := map[string]NodeStrain{}
	for _, nd := range st.Nodes {
		if ns, ok := s.NodeStrainOf(nd.Node); ok {
			strains[nd.Node] = ns
		}
	}
	g("fleet_node_strained", "1 while the node is struggling (memory stalls, GPU thermal slowdown); routing prefers others")
	for _, nd := range st.Nodes {
		if ns, ok := strains[nd.Node]; ok {
			v := 0
			if ns.Strained() {
				v = 1
			}
			fmt.Fprintf(b, "fleet_node_strained{node=%q} %d\n", nd.Node, v)
		}
	}
	g("fleet_node_memory_stall_percent", "PSI memory full avg10 of the node (-1: not reported)")
	for _, nd := range st.Nodes {
		if ns, ok := strains[nd.Node]; ok {
			fmt.Fprintf(b, "fleet_node_memory_stall_percent{node=%q} %g\n", nd.Node, ns.MemFull)
		}
	}
}
