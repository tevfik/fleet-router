package plan

import (
	"bytes"
	"encoding/binary"
	"math"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/tevfik/fleet-router/internal/hostinfo"
)

// ---------------------------------------------------------------- a GGUF writer for the tests

type gw struct{ bytes.Buffer }

func (w *gw) u32(v uint32) { binary.Write(w, binary.LittleEndian, v) }
func (w *gw) u64(v uint64) { binary.Write(w, binary.LittleEndian, v) }
func (w *gw) str(s string) { w.u64(uint64(len(s))); w.WriteString(s) }

type kv struct {
	k string
	t uint32
	v any
}

type ten struct {
	name string
	size int64
}

// gguf builds a file: the header, then the tensors' data (zeros), each size a multiple of 32.
func gguf(t *testing.T, kvs []kv, ts []ten) (data []byte, total int64) {
	var w gw
	w.WriteString("GGUF")
	w.u32(3)
	w.u64(uint64(len(ts)))
	w.u64(uint64(len(kvs)))
	for _, e := range kvs {
		w.str(e.k)
		w.u32(e.t)
		switch e.t {
		case 4:
			w.u32(e.v.(uint32))
		case 6:
			w.u32(math.Float32bits(e.v.(float32)))
		case 8:
			w.str(e.v.(string))
		case 9: // array of u32, or of strings
			switch a := e.v.(type) {
			case []uint32:
				w.u32(4)
				w.u64(uint64(len(a)))
				for _, x := range a {
					w.u32(x)
				}
			case []string:
				w.u32(8)
				w.u64(uint64(len(a)))
				for _, x := range a {
					w.str(x)
				}
			}
		default:
			t.Fatalf("test writer: type %d", e.t)
		}
	}
	var off uint64
	for _, x := range ts {
		w.str(x.name)
		w.u32(2)
		w.u64(8)
		if x.name == "output.weight" { // [embedding, vocabulary]
			w.u64(testVocab)
		} else {
			w.u64(8)
		}
		w.u32(0)
		w.u64(off)
		off += uint64(x.size)
	}
	for w.Len()%32 != 0 {
		w.WriteByte(0)
	}
	w.Write(make([]byte, off))
	return w.Bytes(), int64(w.Len())
}

const mb = 1 << 20

// the vocabulary the test files declare in the output tensor's shape
const testVocab = 32000

// llama: 4 blocks of 100 MB (+1 MB norms), embedding 40 MB, output 40 MB; GQA 8 kv heads of 128.
func llamaFile(t *testing.T, extra ...kv) []byte {
	kvs := []kv{
		{"general.architecture", 8, "llama"}, {"general.name", 8, "tiny-llama"},
		{"llama.block_count", 4, uint32(4)}, {"llama.embedding_length", 4, uint32(4096)},
		{"llama.attention.head_count", 4, uint32(32)}, {"llama.attention.head_count_kv", 4, uint32(8)},
		{"llama.context_length", 4, uint32(8192)}, {"tokenizer.ggml.tokens", 9, []string{"a", "bb", "ccc"}},
		{"general.file_type", 4, uint32(15)}, {"some.float", 6, float32(1.5)},
	}
	kvs = append(kvs, extra...)
	ts := []ten{{"token_embd.weight", 40 * mb}}
	for i := 0; i < 4; i++ {
		p := "blk." + string(rune('0'+i)) + "."
		ts = append(ts, ten{p + "attn_q.weight", 40 * mb}, ten{p + "ffn_down.weight", 60 * mb}, ten{p + "attn_norm.weight", mb})
	}
	ts = append(ts, ten{"output_norm.weight", mb}, ten{"output.weight", 40 * mb})
	d, _ := gguf(t, kvs, ts)
	return d
}

func TestParseGGUF_Dense(t *testing.T) {
	d := llamaFile(t)
	m, err := ParseGGUF(bytes.NewReader(d), int64(len(d)))
	if err != nil {
		t.Fatal(err)
	}
	if m.Name != "tiny-llama" || m.Arch != "llama" || m.Layers != 4 || m.Embd != 4096 || m.CtxMax != 8192 {
		t.Fatalf("%+v", m)
	}
	for i, b := range m.LayerBytes {
		if b != 101*mb || m.LayerRead[i] != b {
			t.Fatalf("block %d: %d bytes, reads %d (a dense block is read whole)", i, b, m.LayerRead[i])
		}
	}
	if m.OutputBytes != 40*mb || m.EmbdBytes != 40*mb || m.OtherBytes != 81*mb {
		t.Fatalf("other %d output %d embd %d", m.OtherBytes, m.OutputBytes, m.EmbdBytes)
	}
	if m.KVPerTokenLayer != 2*8*128*2 || m.HiddenBytes != 4096*4 {
		t.Fatalf("kv %d hidden %d", m.KVPerTokenLayer, m.HiddenBytes)
	}
	if got, want := m.Weights(), int64(4*101+81)*mb; got != want {
		t.Fatalf("weights %d, want %d", got, want)
	}
}

func TestParseGGUF_TiedOutputUsesEmbedding(t *testing.T) {
	ts := []ten{{"token_embd.weight", 40 * mb}, {"blk.0.attn_q.weight", 10 * mb}}
	d, _ := gguf(t, []kv{{"general.architecture", 8, "llama"}, {"llama.block_count", 4, uint32(1)}, {"llama.embedding_length", 4, uint32(64)}}, ts)
	m, err := ParseGGUF(bytes.NewReader(d), int64(len(d)))
	if err != nil {
		t.Fatal(err)
	}
	if m.OutputBytes != 40*mb {
		t.Fatalf("tied output = the embedding table: %d", m.OutputBytes)
	}
}

func TestParseGGUF_MixtureOfExpertsReadsFewExperts(t *testing.T) {
	kvs := []kv{{"general.architecture", 8, "qwen3moe"}, {"qwen3moe.block_count", 4, uint32(2)}, {"qwen3moe.embedding_length", 4, uint32(2048)},
		{"qwen3moe.attention.head_count", 4, uint32(16)}, {"qwen3moe.expert_count", 4, uint32(128)}, {"qwen3moe.expert_used_count", 4, uint32(8)}}
	var ts []ten
	for _, p := range []string{"blk.0.", "blk.1."} {
		ts = append(ts, ten{p + "attn_q.weight", 8 * mb}, ten{p + "ffn_gate_exps.weight", 128 * mb}, ten{p + "ffn_gate_shexp.weight", 4 * mb})
	}
	d, _ := gguf(t, kvs, ts)
	m, err := ParseGGUF(bytes.NewReader(d), int64(len(d)))
	if err != nil {
		t.Fatal(err)
	}
	if m.LayerBytes[0] != 140*mb {
		t.Fatalf("whole block %d", m.LayerBytes[0])
	}
	if want := int64(8+128*8/128+4) * mb; m.LayerRead[0] != want { // attention + 8 of 128 experts + the shared expert
		t.Fatalf("a token reads %d bytes of the block, want %d", m.LayerRead[0], want)
	}
	if m.KVPerTokenLayer != 2*16*128*2 {
		t.Fatalf("no head_count_kv → the head count: %d", m.KVPerTokenLayer)
	}
}

func TestParseGGUF_PerLayerHeadArray(t *testing.T) {
	d := llamaFile(t)
	// a model that stores head_count_kv per block: the largest counts
	d2, _ := gguf(t, []kv{{"general.architecture", 8, "gemma"}, {"gemma.block_count", 4, uint32(2)}, {"gemma.embedding_length", 4, uint32(256)},
		{"gemma.attention.head_count", 4, uint32(8)}, {"gemma.attention.head_count_kv", 9, []uint32{1, 4}}, {"gemma.attention.key_length", 4, uint32(64)}},
		[]ten{{"blk.0.a.weight", mb}, {"blk.1.a.weight", mb}})
	m, err := ParseGGUF(bytes.NewReader(d2), int64(len(d2)))
	if err != nil {
		t.Fatal(err)
	}
	if m.KVPerTokenLayer != 2*4*64*2 {
		t.Fatalf("kv %d", m.KVPerTokenLayer)
	}
	_ = d
}

func TestParseGGUF_Errors(t *testing.T) {
	d := llamaFile(t)
	for name, in := range map[string][]byte{
		"empty": nil, "not gguf": []byte("GGML1234567890123456"), "cut": d[:200],
	} {
		if _, err := ParseGGUF(bytes.NewReader(in), int64(len(in))); err == nil {
			t.Errorf("%s: want an error", name)
		}
	}
	bad := append([]byte{}, d...)
	binary.LittleEndian.PutUint32(bad[4:], 9)
	if _, err := ParseGGUF(bytes.NewReader(bad), int64(len(bad))); err == nil || !strings.Contains(err.Error(), "version") {
		t.Errorf("version: %v", err)
	}
	noArch, _ := gguf(t, []kv{{"x", 4, uint32(1)}}, nil)
	if _, err := ParseGGUF(bytes.NewReader(noArch), int64(len(noArch))); err == nil {
		t.Error("no architecture")
	}
}

func TestReadGGUF_File(t *testing.T) {
	p := filepath.Join(t.TempDir(), "unnamed-model.gguf")
	d, _ := gguf(t, []kv{{"general.architecture", 8, "llama"}, {"llama.block_count", 4, uint32(1)}, {"llama.embedding_length", 4, uint32(64)}}, []ten{{"blk.0.a.weight", mb}})
	os.WriteFile(p, d, 0o644)
	m, err := ReadGGUF(p)
	if err != nil || m.Name != "unnamed-model" || m.FileBytes != int64(len(d)) {
		t.Fatalf("%+v %v", m, err)
	}
	if _, err := ReadGGUF(filepath.Join(t.TempDir(), "none.gguf")); err == nil {
		t.Fatal("a missing file is an error")
	}
}

// ---------------------------------------------------------------- the cost model

// The figures measured on llama-ring over WAN (qwen4exp, victor 4 / dino 40 / papa 4 blocks):
// 29.7 + (2.5 + 9.7 + 0.4) + (3.2 + 10.3 + 0.4) = 56.2 ms predicted, 60.9 ms measured.
func TestTokenMS_MatchesTheWANMeasurement(t *testing.T) {
	got := TokenMS(29.7, []WorkerTerm{{2.5, 9.7, 0.4}, {3.2, 10.3, 0.4}})
	if math.Abs(got-56.2) > 0.01 {
		t.Fatalf("T = %.1f ms", got)
	}
	if measured := 60.9; math.Abs(got-measured)/measured > 0.1 {
		t.Fatalf("%.1f ms is more than 10%% from the measured %.1f", got, measured)
	}
	if TokenMS(20, nil) != 20 {
		t.Fatal("no workers: the master's own time")
	}
}

// ---------------------------------------------------------------- planning

func bench(fastGB, bw float64, where string, ramGB float64) *hostinfo.Bench {
	return &hostinfo.Bench{FastMem: int64(fastGB * gib), FastBW: bw, FastWhere: where, RAMTotal: int64(ramGB * gib), RAMBW: 40}
}

func node(name string, capGB, bw, rtt float64, b *hostinfo.Bench) Node {
	return Node{Name: name, CapGB: capGB, BW: bw, RTTms: rtt, LinkGBps: 1.2, Bench: b}
}

// big builds a model of n blocks of layerGB each (dense), embedding and output 1 GB each.
func big(n int, layerGB float64) *Model {
	m := &Model{Name: "big", Arch: "llama", Layers: n, Embd: 4096, KVPerTokenLayer: 4096, HiddenBytes: 16384,
		OutputBytes: 1 << 30, EmbdBytes: 1 << 30, OtherBytes: 2 << 30}
	for i := 0; i < n; i++ {
		b := int64(layerGB * gib)
		m.LayerBytes, m.LayerRead = append(m.LayerBytes, b), append(m.LayerRead, b)
	}
	return m
}

func TestPlan_FitsOneMachine_OneMachineWins(t *testing.T) {
	m := big(40, 0.5) // ~22 GB
	nodes := []Node{
		node("dino", 100, 273, 0, bench(96, 273, "GPU (unified memory)", 128)),
		node("p1", 14, 300, 10, bench(14, 300, "GPU", 62)), // a WAN round trip: the ring measured 27 tok/s alone vs 15.6 over 3
		node("vec", 14, 250, 10, bench(14, 250, "GPU", 32)),
	}
	r := Plan(m, nodes, Options{Ctx: 4096, Np: 1})
	best := r.Candidates[0]
	if best.Kind != "single" || best.Master != "dino" {
		t.Fatalf("a model that fits dino runs on dino alone: %+v\n%s", best, r.Verdict)
	}
	if !strings.Contains(r.Verdict, "dino alone") {
		t.Fatal(r.Verdict)
	}
	for _, c := range r.Candidates {
		if c.Kind == "split" && c.TPS > best.TPS {
			t.Fatalf("a split beat the single machine: %+v", c)
		}
	}
}

func TestPlan_TooBigForAnyOne_SplitsAndFillsFastestFirst(t *testing.T) {
	m := big(60, 1) // ~62 GB, KV tiny
	nodes := []Node{
		node("a", 40, 500, 0, bench(40, 500, "GPU", 16)),
		node("b", 30, 250, 2, bench(30, 250, "GPU", 16)),
		node("c", 8, 900, 2, bench(8, 900, "GPU", 16)),
	}
	r := Plan(m, nodes, Options{Ctx: 2048, Np: 1, Master: "a"})
	if len(r.Candidates) == 0 {
		t.Fatal(r.Verdict)
	}
	b := r.Candidates[0]
	if b.Kind != "split" || b.Master != "a" {
		t.Fatalf("%+v", b)
	}
	layers := map[string]int{}
	total := 0
	for _, p := range b.Parts {
		layers[p.Node], total = p.Layers, total+p.Layers
	}
	if total != 60 {
		t.Fatalf("every block is placed once: %v", layers)
	}
	if layers["c"] == 0 || layers["c"] > 7 {
		t.Fatalf("the fastest machine takes what its 8 GB holds (one block each, minus the output): %v", layers)
	}
	if !strings.Contains(r.Verdict, "fits no single machine") {
		t.Fatal(r.Verdict)
	}
	for _, p := range b.Parts {
		if p.GB > map[string]float64{"a": 40, "b": 30, "c": 8}[p.Node]+1e-6 {
			t.Fatalf("%s holds %.1f GB, more than it has", p.Node, p.GB)
		}
	}
}

func TestPlan_RoundTripsDecideBetweenWorkers(t *testing.T) {
	m := big(40, 1)
	mk := func(rttB float64) Candidate {
		nodes := []Node{
			node("m", 25, 400, 0, bench(25, 400, "GPU", 16)),
			node("near", 25, 400, 1, bench(25, 400, "GPU", 16)),
			node("far", 25, 400, rttB, bench(25, 400, "GPU", 16)),
		}
		return Plan(m, nodes, Options{Ctx: 1024, Master: "m"}).Candidates[0]
	}
	if c := mk(2); len(c.Parts) != 2 {
		t.Fatalf("two machines hold it: a third round trip is not worth it: %+v", c.Parts)
	}
	// the cheaper link wins between two equal workers
	nodes := []Node{
		node("m", 25, 400, 0, bench(25, 400, "GPU", 16)),
		node("slowlink", 25, 400, 30, bench(25, 400, "GPU", 16)),
		node("fastlink", 25, 400, 1, bench(25, 400, "GPU", 16)),
	}
	c := Plan(m, nodes, Options{Ctx: 1024, Master: "m"}).Candidates[0]
	if c.Parts[1].Node != "fastlink" {
		t.Fatalf("%+v", c.Parts)
	}
}

func TestPlan_DoesNotFit(t *testing.T) {
	m := big(80, 2) // ~160 GB
	r := Plan(m, []Node{node("a", 20, 300, 0, bench(20, 300, "GPU", 16)), node("b", 20, 300, 2, bench(20, 300, "GPU", 16))}, Options{})
	if len(r.Candidates) != 0 || !strings.HasPrefix(r.Verdict, "does not fit") {
		t.Fatalf("%+v", r)
	}
}

func TestPlan_NoBenchmarkDataYet(t *testing.T) {
	m := big(10, 0.5)
	r := Plan(m, []Node{{Name: "a"}}, Options{})
	if len(r.Candidates) != 0 || !strings.HasPrefix(r.Verdict, "no benchmark data yet") {
		t.Fatalf("expected verdict starting with 'no benchmark data yet', got: %s", r.Verdict)
	}
}

func TestPlan_KVCacheCountsPerSlot(t *testing.T) {
	m := big(40, 0.5)
	m.KVPerTokenLayer = 256 << 10 // 256 KB per token per block: 10 MB per token
	r1 := Plan(m, nil, Options{Ctx: 1000, Np: 1})
	r4 := Plan(m, nil, Options{Ctx: 1000, Np: 4})
	if math.Abs(r4.KVGB/r1.KVGB-4) > 1e-9 {
		t.Fatalf("four slots hold four caches: %.1f vs %.1f GB", r4.KVGB, r1.KVGB)
	}
	n := []Node{node("a", 28, 300, 0, bench(30, 300, "GPU", 16)), node("b", 28, 300, 2, bench(30, 300, "GPU", 16))}
	if r := Plan(m, n, Options{Ctx: 100, Np: 1, Master: "a"}); len(r.Candidates) == 0 || r.Candidates[0].Kind != "single" {
		t.Fatalf("a short context fits one machine: %s", r.Verdict)
	}
	if r := Plan(m, n, Options{Ctx: 2000, Np: 4, Master: "a"}); len(r.Candidates) != 0 {
		t.Fatalf("a long context in four slots fits nowhere: %s", r.Verdict)
	}
}

func TestPlan_MasterOptionAndSkipped(t *testing.T) {
	m := big(10, 0.5)
	nodes := []Node{
		node("a", 30, 300, 0, bench(30, 300, "GPU", 16)),
		node("b", 30, 300, 2, bench(30, 300, "GPU", 16)),
		{Name: "nodata"},
	}
	r := Plan(m, nodes, Options{Master: "b"})
	for _, c := range r.Candidates {
		if c.Master != "b" {
			t.Fatalf("the master is fixed: %+v", c)
		}
	}
	if len(r.Skipped) != 1 || !strings.HasPrefix(r.Skipped[0], "nodata") {
		t.Fatalf("%v", r.Skipped)
	}
}

func TestPlan_OutputProjectionRidesTheLastMachine(t *testing.T) {
	m := big(30, 1)
	m.OutputBytes = 8 << 30
	nodes := []Node{node("m", 25, 400, 0, bench(25, 400, "GPU", 16)), node("w", 25, 400, 2, bench(25, 400, "GPU", 16))}
	c := Plan(m, nodes, Options{Ctx: 256, Master: "m"}).Candidates[0]
	var out *Part
	for i := range c.Parts {
		if c.Parts[i].Output {
			out = &c.Parts[i]
		}
	}
	if c.Kind != "split" || out == nil || out.Node != "w" {
		t.Fatalf("%+v", c)
	}
	if out.GB < 8 {
		t.Fatalf("the worker holds the 8 GB output projection: %.1f", out.GB)
	}
}

func TestPlan_LoadTime(t *testing.T) {
	m := big(40, 1)
	nodes := []Node{node("m", 25, 400, 0, bench(25, 400, "GPU", 16)), node("w", 25, 400, 2, bench(25, 400, "GPU", 16))}
	nodes[1].LinkGBps = 0.1 // a 1 Gbit link: the workers' share crawls
	c := Plan(m, nodes, Options{Ctx: 256, Master: "m"}).Candidates[0]
	if c.Kind != "split" || c.LoadS < 100 {
		t.Fatalf("shipping ~20 GB over 0.1 GB/s takes minutes: %+v", c)
	}
}

func TestPlan_RoundTripBetweenTwoMachinesIsBoundedBySumOrMeasured(t *testing.T) {
	m := big(40, 1)
	mk := func(rtt map[string]float64) float64 {
		nodes := []Node{
			node("fleet", 25, 400, 0, bench(25, 400, "GPU", 16)),
			node("a", 25, 400, 4, bench(25, 400, "GPU", 16)),
			node("b", 25, 400, 6, bench(25, 400, "GPU", 16)),
		}
		c := Plan(m, nodes, Options{Ctx: 256, Master: "a", RTT: rtt}).Candidates[0]
		for _, p := range c.Parts {
			if p.Node == "b" {
				return p.RTTms
			}
		}
		for _, p := range c.Parts {
			if p.Node == "fleet" {
				return p.RTTms
			}
		}
		t.Fatalf("no worker: %+v", c)
		return 0
	}
	if got := mk(nil); got != 4 && got != 10 { // a→fleet 4+0, or a→b 4+6
		t.Fatalf("sum of the two machines' figures: %v", got)
	}
	if got := mk(map[string]float64{"a>b": 1, "fleet>a": 1}); got != 1 {
		t.Fatalf("a measured figure replaces the sum (either order): %v", got)
	}
	if got := mk(map[string]float64{"b>a": 0.5, "a>fleet": 0.5}); got != 0.5 {
		t.Fatalf("%v", got)
	}
}

func TestFromHost(t *testing.T) {
	gpu := &hostinfo.Bench{FastMem: 16 << 30, FastBW: 576, FastWhere: "GPU", RAMTotal: 64 << 30}
	n := FromHost("p1", gpu, hostinfo.Sample{GPUs: []hostinfo.GPU{{MemUsed: 4 << 30}}})
	if math.Abs(n.CapGB-(12-overheadGB)) > 1e-9 || n.BW != 576 {
		t.Fatalf("16 GB card with 4 GB in use: %+v", n)
	}
	uni := &hostinfo.Bench{FastMem: 96 << 30, FastBW: 273, FastWhere: "GPU (unified memory)", RAMTotal: 128 << 30}
	n = FromHost("dino", uni, hostinfo.Sample{RAMTotal: 128 << 30, RAMUsed: 50 << 30, GPUs: []hostinfo.GPU{{MemUsed: -1}}})
	if math.Abs(n.CapGB-(78-overheadGB)) > 1e-9 {
		t.Fatalf("unified memory: what the system has left (128−50), not the card's report: %+v", n)
	}
	if n := FromHost("x", nil, hostinfo.Sample{}); n.CapGB != 0 || n.BW != 0 {
		t.Fatalf("no benchmark, no figures: %+v", n)
	}
}

// ---------------------------------------------------------------- what a machine already holds

func twoNodes() []Node {
	return []Node{node("m", 25, 400, 0, bench(25, 400, "GPU", 16)), node("w", 25, 400, 2, bench(25, 400, "GPU", 16))}
}

func split(t *testing.T, nodes []Node, o Options) Candidate {
	t.Helper()
	o.Master = "m"
	if o.Ctx == 0 {
		o.Ctx = 256
	}
	for _, c := range Plan(big(40, 1), nodes, o).Candidates {
		if c.Kind == "split" {
			return c
		}
	}
	t.Fatal("no split")
	return Candidate{}
}

func TestPlan_Transfers_UnknownMeansEverythingTravels(t *testing.T) {
	r := Plan(big(40, 1), twoNodes(), Options{Ctx: 256, Master: "m"})
	if r.Known {
		t.Fatal("not known")
	}
	c := split(t, twoNodes(), Options{})
	var w Part
	for _, p := range c.Parts {
		if p.Node == "w" {
			w = p
		}
	}
	if w.FetchGB < 10 || c.LoadS <= 0 {
		t.Fatalf("the worker's weights are assumed to travel: %+v", w)
	}
}

func TestPlan_Transfers_WorkerCacheAndFileCutTheMove(t *testing.T) {
	cold := split(t, twoNodes(), Options{Known: true})
	var coldW Part
	for _, p := range cold.Parts {
		if p.Node == "w" {
			coldW = p
		}
	}
	if coldW.FetchGB < 10 || cold.FetchGB < coldW.FetchGB {
		t.Fatalf("a worker with nothing receives its blocks: %+v", cold)
	}
	// the master has the file, the worker's cache holds half of what it needs
	n := twoNodes()
	n[0].HasFile = true
	n[1].CachedGB = coldW.FetchGB / 2
	half := split(t, n, Options{Known: true})
	for _, p := range half.Parts {
		if p.Node == "w" && (p.FetchGB < coldW.FetchGB/2-0.01 || p.FetchGB > coldW.FetchGB/2+0.01) {
			t.Fatalf("half is cached: %+v", p)
		}
	}
	if half.LoadS >= cold.LoadS {
		t.Fatalf("less to ship, a shorter first load: %.0f vs %.0f s", half.LoadS, cold.LoadS)
	}
	// a worker with the whole file, or a cache that covers everything: nothing moves
	n[1].HasFile = true
	if full := split(t, n, Options{Known: true}); full.FetchGB != 0 {
		t.Fatalf("nothing to move: %+v", full)
	}
	n[1].HasFile, n[1].CachedGB = false, 1000
	if full := split(t, n, Options{Known: true}); full.FetchGB != 0 {
		t.Fatalf("a cache holding more than needed: %+v", full)
	}
}

func TestPlan_Transfers_MasterWithoutTheFileMustFetchIt(t *testing.T) {
	n := twoNodes() // nobody has the file
	c := split(t, n, Options{Known: true})
	if !strings.Contains(strings.Join(c.Notes, " "), "master m does not have the file") {
		t.Fatalf("%v", c.Notes)
	}
	n[0].HasFile = true
	c2 := split(t, n, Options{Known: true})
	if c2.FetchGB >= c.FetchGB || strings.Contains(strings.Join(c2.Notes, " "), "does not have the file") {
		t.Fatalf("with the file on the master only the workers' share moves: %.1f vs %.1f", c2.FetchGB, c.FetchGB)
	}
}

func TestPlan_Transfers_SingleMachineWithoutTheFile(t *testing.T) {
	m := big(10, 1)
	n := []Node{node("a", 30, 300, 0, bench(30, 300, "GPU", 16))}
	r := Plan(m, n, Options{Ctx: 256, Known: true})
	c := r.Candidates[0]
	if c.Kind != "single" || c.FetchGB < 10 || !strings.Contains(strings.Join(c.Notes, " "), "does not have the file") {
		t.Fatalf("%+v", c)
	}
	n[0].HasFile = true
	if c := Plan(m, n, Options{Ctx: 256, Known: true}).Candidates[0]; c.FetchGB != 0 {
		t.Fatalf("%+v", c)
	}
}

func TestPlan_KVBlocksAndBoundaryOverrides(t *testing.T) {
	m := big(40, 0.5)
	m.KVPerTokenLayer = 1 << 20
	all := Plan(m, nil, Options{Ctx: 1000})
	few := Plan(m, nil, Options{Ctx: 1000, KVBlocks: 10})
	if math.Abs(few.KVGB/all.KVGB-0.25) > 1e-9 {
		t.Fatalf("10 of 40 blocks keep a KV cache: %.2f vs %.2f GB", few.KVGB, all.KVGB)
	}
	if Plan(m, nil, Options{Ctx: 1000, KVBlocks: 99}).KVGB != all.KVGB {
		t.Fatal("more blocks than there are is all of them")
	}
	big4 := big(40, 1)
	nodes := twoNodes()
	base := split(t, nodes, Options{})
	wide := split(t, nodes, Options{BoundaryBytes: 80 << 20}) // a boundary state of 80 MB per token (absurd, visible)
	var sb, sw float64
	for _, p := range base.Parts {
		sb += p.SendMS
	}
	for _, p := range wide.Parts {
		sw += p.SendMS
	}
	if sw <= sb*100 {
		t.Fatalf("the boundary size is what is sent: %.3f vs %.3f ms", sb, sw)
	}
	_ = big4
}

func TestPlan_BlocksFollowThePartsInOrder(t *testing.T) {
	c := split(t, twoNodes(), Options{})
	next := 0
	for _, p := range c.Parts {
		if p.First != next {
			t.Fatalf("blocks are consecutive in the order of the parts: %+v", c.Parts)
		}
		next += p.Layers
	}
	if next != 40 {
		t.Fatalf("all 40 blocks: %d", next)
	}
	if got := (Part{First: 4, Layers: 3}).Blocks(); got != "4-6" {
		t.Fatal(got)
	}
	if got := (Part{First: 9, Layers: 1}).Blocks(); got != "9" {
		t.Fatal(got)
	}
	if got := (Part{First: 9}).Blocks(); got != "" {
		t.Fatal(got)
	}
	last := c.Parts[len(c.Parts)-1]
	if !last.Output {
		t.Fatalf("the output layer is on the last machine: %+v", c.Parts)
	}
}

func TestPlan_RingOverheadIsAddedOnlyWhenThereAreWorkers(t *testing.T) {
	m := big(60, 1)
	nodes := []Node{
		node("a", 40, 500, 0, bench(40, 500, "GPU", 16)),
		node("b", 30, 250, 2, bench(30, 250, "GPU", 16)),
		node("c", 8, 900, 2, bench(8, 900, "GPU", 16)),
	}
	base := Plan(m, nodes, Options{Ctx: 2048, Np: 1, Master: "a"}).Candidates[0]
	with := Plan(m, nodes, Options{Ctx: 2048, Np: 1, Master: "a", RingMS: 8}).Candidates[0]
	if with.RingMS != 8 || math.Abs(with.TokenMS-base.TokenMS-8) > 1e-9 || with.TPS >= base.TPS {
		t.Fatalf("base %.2f ms, with the ring overhead %.2f ms (%+v)", base.TokenMS, with.TokenMS, with.RingMS)
	}
	one := Plan(big(10, 1), nodes[:1], Options{Ctx: 2048, Np: 1, RingMS: 8}).Candidates[0]
	if one.RingMS != 0 {
		t.Fatalf("a model on one machine has no ring: %+v", one)
	}
}

func TestParseGGUF_ReadsTheVocabularyFromTheOutputTensor(t *testing.T) {
	m, err := ParseGGUF(bytes.NewReader(llamaFile(t)), 0)
	if err != nil {
		t.Fatal(err)
	}
	if m.Vocab != testVocab {
		t.Fatalf("vocab %d, want %d", m.Vocab, testVocab)
	}
}

// On a slow link the logits (vocabulary × 4 bytes) of an output projection on the worker cost more than the whole
// hidden state: the plan keeps the output on the master.
func TestPlan_SlowLinkKeepsTheOutputProjectionOnTheMaster(t *testing.T) {
	m := big(30, 1)
	m.Vocab = 250000 // ~1 MB of logits per token
	slow := func(n Node) Node { n.LinkGBps = 0.006; return n }
	nodes := []Node{slow(node("m", 25, 400, 0, bench(25, 400, "GPU", 16))), slow(node("w", 25, 400, 2, bench(25, 400, "GPU", 16)))}
	c := Plan(m, nodes, Options{Ctx: 256, Master: "m"}).Candidates[0]
	if c.Kind != "split" || !c.Parts[0].Output {
		t.Fatalf("the master should hold the output projection: %+v", c.Parts)
	}
	for _, p := range c.Parts[1:] {
		if p.Output {
			t.Fatalf("a worker holds it: %+v", c.Parts)
		}
		if p.SendMS > 10 { // only the hidden state (16 KB each way) crosses
			t.Fatalf("send %.1f ms: the logits must not cross", p.SendMS)
		}
	}
	// the same plan with the output on the worker is slower: 1 MB at 6 MB/s
	var onWorker *Candidate
	r := Plan(m, nodes, Options{Ctx: 256, Master: "m", Top: 50})
	for i := range r.Candidates {
		if r.Candidates[i].Kind == "split" && !r.Candidates[i].Parts[0].Output {
			onWorker = &r.Candidates[i]
		}
	}
	if onWorker == nil || onWorker.TokenMS < c.TokenMS+100 {
		t.Fatalf("output on the worker should cost >100 ms more: %+v vs %.1f", onWorker, c.TokenMS)
	}
}

func TestPlan_WarnsWhenASplitArchitectureWasNeverGated(t *testing.T) {
	nodes := []Node{
		node("a", 40, 500, 0, bench(40, 500, "GPU", 16)),
		node("b", 30, 250, 2, bench(30, 250, "GPU", 16)),
	}
	m := big(60, 1)
	m.Arch = "no-such-arch"
	r := Plan(m, nodes, Options{Ctx: 2048, Np: 1, Master: "a"})
	if len(r.Warnings) != 1 || !strings.Contains(r.Warnings[0], "has not passed") {
		t.Fatalf("%v (%s)", r.Warnings, r.Verdict)
	}
	m2 := big(10, 1) // fits one machine: no ring, no warning
	m2.Arch = "no-such-arch"
	if r := Plan(m2, nodes, Options{Ctx: 2048, Np: 1, Master: "a"}); r.Candidates[0].Kind == "single" && len(r.Warnings) != 0 {
		t.Fatalf("%v", r.Warnings)
	}
}

func TestFromHostNPU(t *testing.T) {
	b := &hostinfo.Bench{RAMTotal: 16 << 30, RAMBW: 12, FastWhere: "NPU", FastMem: 8 << 30, FastBW: 34}
	n := FromHost("npu-board", b, hostinfo.Sample{RAMTotal: 16 << 30, RAMUsed: 10 << 30})
	if n.CapGB != 8 || n.BW != 34 || n.Eff != 1 {
		t.Fatalf("the pool is the limit, not the system RAM in use: %+v", n)
	}
}

func TestPlan_KVFactorScalesTheCacheEstimate(t *testing.T) {
	m := big(40, 0.5)
	m.KVPerTokenLayer = 1 << 20
	base := Plan(m, nil, Options{Ctx: 1000}).KVGB
	if got := Plan(m, nil, Options{Ctx: 1000, KVFactor: 0}).KVGB; got != base {
		t.Fatalf("zero means f16: %.3f vs %.3f", got, base)
	}
	if got := Plan(m, nil, Options{Ctx: 1000, KVFactor: 0.5}).KVGB; math.Abs(got/base-0.5) > 1e-9 {
		t.Fatalf("half-size cache: %.3f vs %.3f", got, base)
	}
	if got := Plan(m, nil, Options{Ctx: 1000, KVFactor: 0.5, KVBlocks: 10}).KVGB; math.Abs(got/base-0.125) > 1e-9 {
		t.Fatalf("a quarter of the blocks at half size: %.3f vs %.3f", got, base)
	}
}

func TestParseGGUF_HybridKVBlocksAndPlannerCacheType(t *testing.T) {
	// 8 blocks: every 4th block is full attention (.attn_k.), the other 6 are recurrent (.ssm_in.)
	kvs := []kv{
		{"general.architecture", 8, "qwen35moe"}, {"general.name", 8, "qwen-hybrid"},
		{"qwen35moe.block_count", 4, uint32(8)}, {"qwen35moe.embedding_length", 4, uint32(2048)},
		{"qwen35moe.attention.head_count", 4, uint32(16)}, {"qwen35moe.attention.head_count_kv", 4, uint32(4)},
		{"qwen35moe.full_attention_interval", 4, uint32(4)},
	}
	var ts []ten
	for i := 0; i < 8; i++ {
		p := "blk." + string(rune('0'+i)) + "."
		if i%4 == 3 {
			ts = append(ts, ten{p + "attn_k.weight", 4 * mb})
		} else {
			ts = append(ts, ten{p + "ssm_in.weight", 4 * mb})
		}
	}
	d, _ := gguf(t, kvs, ts)
	m, err := ParseGGUF(bytes.NewReader(d), int64(len(d)))
	if err != nil {
		t.Fatal(err)
	}
	if !m.Recurrent || m.KVBlocks != 2 {
		t.Fatalf("want Recurrent=true and KVBlocks=2, got Recurrent=%v KVBlocks=%d", m.Recurrent, m.KVBlocks)
	}
	// Planner automatically uses m.KVBlocks (2/8 = 0.25) and CacheTypeK/V (q4_0) without manual KVBlocks/KVFactor
	fullF16 := m.KVGB(4096, 1)
	res := Plan(m, nil, Options{Ctx: 4096, CacheTypeK: "q4_0", CacheTypeV: "q4_0"})
	want := fullF16 * 0.25 * KVCacheFactor("q4_0", "q4_0")
	if math.Abs(res.KVGB-want) > 1e-9 {
		t.Fatalf("KVGB %.6f, want %.6f (full f16 %.6f)", res.KVGB, want, fullF16)
	}
}

func TestPlan_SingleNodeCPUMoEVRAMBudget(t *testing.T) {
	// MoE model: 2.5 GiB dense weights + 40 GiB expert weights; 12 GiB GPU (10.5 GiB after overhead) + 32 GiB RAM
	m := &Model{
		Name: "qwen-moe", Arch: "qwen35moe", Layers: 4, Embd: 2048,
		Experts: 64, ExpertsUsed: 4, KVPerTokenLayer: 2 * 4 * 128 * 2,
		OtherBytes:  512 * mb,
		LayerBytes:  []int64{10512 * mb, 10512 * mb, 10512 * mb, 10512 * mb},
		LayerRead:   []int64{1152 * mb, 1152 * mb, 1152 * mb, 1152 * mb},
		ExpertBytes: []int64{10 * (1 << 30), 10 * (1 << 30), 10 * (1 << 30), 10 * (1 << 30)},
	}
	n := node("victor", 10.5, 500, 0, bench(12, 500, "GPU", 32))
	// With --cpu-moe and --moe-cache-mib 3072: VRAM need = ~2.5 dense + KV + 3.0 cache + 0.5 compute buffer = ~6.0 GiB <= 10.5 GiB
	r := Plan(m, []Node{n}, Options{Ctx: 4096, CPUMoE: true, MoECacheMiB: 3072})
	if len(r.Candidates) != 1 || r.Candidates[0].Where != "GPU+RAM" {
		t.Fatalf("expected GPU+RAM candidate with --cpu-moe, got %+v", r.Candidates)
	}
	// With an oversized --moe-cache-mib (9216 MiB = 9 GiB): VRAM need > 10.5 GiB -> rejected
	r2 := Plan(m, []Node{n}, Options{Ctx: 4096, CPUMoE: true, MoECacheMiB: 9216})
	if len(r2.Candidates) != 0 {
		t.Fatalf("expected candidate rejection when VRAM budget exceeds GPU memory, got %+v", r2.Candidates)
	}
}
