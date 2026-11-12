package plan

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/tevfik/fleet-router/internal/hostinfo"
)

// descr: a small stage-pipe model: 4 layers (3 recurrent, the last one attention), alike in size.
func descr() Descriptor {
	d := Descriptor{Version: 1, Name: "toy-hybrid", Architecture: "toy-dense", Runtime: RuntimeStagePipe,
		LayerCount: 4, HiddenSize: 5120, VocabSize: 1000, ContextMax: 4096,
		EmbeddingAt: "first", EmbeddingBytes: 1 << 20, HeadAt: "last", HeadBytes: 2 << 20}
	for i := 0; i < 4; i++ {
		l := DescriptorLayer{Index: i, File: "layer.elf", Bytes: 100 << 20, ResidentBytes: 120 << 20, State: StateRecurrent, FixedStateBytes: 512 << 10}
		if i == 3 {
			l.State, l.FixedStateBytes, l.KVBytesPerToken = StateAttentionKV, 0, 32768
		}
		d.Layers = append(d.Layers, l)
	}
	return d
}

func enc(t *testing.T, d Descriptor) string {
	b, err := json.Marshal(d)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

func TestDescriptorToModel(t *testing.T) {
	d := descr()
	d.Layers[1].ReadBytes = 25 << 20 // a mixture-of-experts layer reads a part
	m, err := ParseDescriptor(strings.NewReader(enc(t, d)))
	if err != nil {
		t.Fatal(err)
	}
	mod, err := m.Model()
	if err != nil {
		t.Fatal(err)
	}
	if mod.Layers != 4 || mod.Runtime != RuntimeStagePipe || mod.Embd != 5120 || mod.Vocab != 1000 || mod.CtxMax != 4096 {
		t.Fatalf("%+v", mod)
	}
	if mod.HiddenBytes != 10240 { // 5120 values × 2 bytes (the default width) [PDF: 10 KB per token]
		t.Fatalf("hidden bytes %d", mod.HiddenBytes)
	}
	if mod.LayerBytes[0] != 120<<20 || mod.LayerRead[0] != 100<<20 || mod.LayerRead[1] != 25<<20 {
		t.Fatalf("resident %v read %v", mod.LayerBytes, mod.LayerRead)
	}
	if mod.OutputBytes != 2<<20 || mod.EmbdBytes != 1<<20 || mod.Weights() != 4*(120<<20)+3<<20 {
		t.Fatalf("weights %d", mod.Weights())
	}
	if mod.FileBytes != 4*(100<<20)+3<<20 {
		t.Fatalf("file bytes %d", mod.FileBytes)
	}
	if mod.KVPerTokenLayer != 32768/4 {
		t.Fatalf("kv per token and layer %d", mod.KVPerTokenLayer)
	}
}

func TestDescriptorWidthOfBoundaryValue(t *testing.T) {
	d := descr()
	d.BoundaryValueBytes = 4
	mod, err := d.Model()
	if err != nil || mod.HiddenBytes != 5120*4 {
		t.Fatalf("%v %+v", err, mod)
	}
}

func TestDescriptorRejects(t *testing.T) {
	cases := map[string]func(d *Descriptor){
		"version":                      func(d *Descriptor) { d.Version = 2 },
		"name":                         func(d *Descriptor) { d.Name = "" },
		"runtime_kind":                 func(d *Descriptor) { d.Runtime = "skip" },
		"layer_count":                  func(d *Descriptor) { d.LayerCount = 5 },
		"layer_count must be positive": func(d *Descriptor) { d.LayerCount = 0; d.Layers = nil },
		"listed":                       func(d *Descriptor) { d.Layers = d.Layers[:3] },
		"index":                        func(d *Descriptor) { d.Layers[2].Index = 3 },
		"hidden_size":                  func(d *Descriptor) { d.HiddenSize = 0 },
		"vocab_size":                   func(d *Descriptor) { d.VocabSize = -1 },
		"experts_active":               func(d *Descriptor) { d.Experts, d.ExpertsActive = 4, 8 },
		"embedding_at":                 func(d *Descriptor) { d.EmbeddingAt = "last" },
		"head_at":                      func(d *Descriptor) { d.HeadAt = "first" },
		"bytes must be":                func(d *Descriptor) { d.Layers[0].Bytes = 0 },
		"exceeds bytes":                func(d *Descriptor) { d.Layers[0].ReadBytes = 200 << 20 },
		"attention-kv layer":           func(d *Descriptor) { d.Layers[3].KVBytesPerToken = 0 },
		"recurrent layer":              func(d *Descriptor) { d.Layers[0].FixedStateBytes = 0 },
		"stateless":                    func(d *Descriptor) { d.Layers[0].State, d.Layers[0].FixedStateBytes = StateNone, 1 },
		"state \"mamba\"":              func(d *Descriptor) { d.Layers[0].State = "mamba" },
		"negative":                     func(d *Descriptor) { d.Layers[0].ResidentBytes = -1 },
	}
	for want, mutate := range cases {
		d := descr()
		mutate(&d)
		_, err := ParseDescriptor(strings.NewReader(enc(t, d)))
		if err == nil || !strings.Contains(err.Error(), want) {
			t.Errorf("want an error with %q, got %v", want, err)
		}
	}
	if _, err := ParseDescriptor(strings.NewReader(`{"version":1,"layer_cuont":4}`)); err == nil || !strings.Contains(err.Error(), "unknown field") {
		t.Errorf("an unknown field must be refused: %v", err)
	}
	if _, err := ParseDescriptor(strings.NewReader(`{`)); err == nil {
		t.Error("broken JSON")
	}
}

func TestLoadModelByPathKind(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, DescriptorFile), []byte(enc(t, descr())), 0o644); err != nil {
		t.Fatal(err)
	}
	for _, p := range []string{dir, filepath.Join(dir, DescriptorFile)} { // the directory, and the file
		m, err := LoadModel(p)
		if err != nil || m.Name != "toy-hybrid" {
			t.Fatalf("%s: %v %+v", p, err, m)
		}
	}
	gg := filepath.Join(dir, "tiny.gguf")
	if err := os.WriteFile(gg, llamaFile(t), 0o644); err != nil {
		t.Fatal(err)
	}
	if m, err := LoadModel(gg); err != nil || m.Arch != "llama" || m.Runtime != "" {
		t.Fatalf("a GGUF still loads as before: %v %+v", err, m)
	}
	if _, err := LoadModel(filepath.Join(dir, "missing.json")); err == nil {
		t.Error("a missing descriptor is an error")
	}
}

func TestPlanFromDescriptor(t *testing.T) {
	m, err := (func() (*Model, error) { d := descr(); return d.Model() })()
	if err != nil {
		t.Fatal(err)
	}
	b := &hostinfo.Bench{FastWhere: "NPU"}
	nodes := []Node{
		{Name: "a", CapGB: 0.3, BW: 34, Eff: 1, Bench: b}, // holds two layers of 120 MiB (+ KV) and the embedding
		{Name: "b", CapGB: 0.3, BW: 34, Eff: 1, Bench: b},
	}
	r := Plan(m, nodes, Options{Ctx: 128})
	if len(r.Candidates) == 0 || r.Candidates[0].Kind != "split" {
		t.Fatalf("it fits no single node but two: %+v %s", r.Candidates, r.Verdict)
	}
}

func TestStagePipeNeverPutsTheHeadOnTheFirstStage(t *testing.T) {
	d := descr()
	m, _ := d.Model()
	b := &hostinfo.Bench{FastWhere: "NPU"}
	r := Plan(m, []Node{{Name: "a", CapGB: 0.3, BW: 34, Eff: 1, Bench: b}, {Name: "b", CapGB: 0.3, BW: 34, Eff: 1, Bench: b}}, Options{Ctx: 128})
	for _, c := range r.Candidates {
		if c.Kind == "split" && c.Parts[0].Output {
			t.Fatalf("the head is on the first stage: %+v", c.Parts)
		}
	}
	m.Runtime = ""
	r = Plan(m, []Node{{Name: "a", CapGB: 0.3, BW: 34, Eff: 1, Bench: b}, {Name: "b", CapGB: 0.3, BW: 34, Eff: 1, Bench: b}}, Options{Ctx: 128})
	on := false
	for _, c := range r.Candidates {
		on = on || (c.Kind == "split" && c.Parts[0].Output)
	}
	if !on {
		t.Fatal("the ring still tries the head on the master")
	}
}
