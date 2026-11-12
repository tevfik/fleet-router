// Package ggufmeta reads what a GGUF file says about itself in its header (the key/value metadata) and derives what
// the model can do from that, never from its name: the architecture, size, context length, quantization, experts,
// whether it has a chat template (and whether that template knows tools or a thinking mode), whether it is an
// embedding model (pooling) or a vision projector (arch clip). Only the header is read, a few hundred KB at most.
package ggufmeta

import (
	"bufio"
	"encoding/binary"
	"fmt"
	"io"
	"math"
	"os"
	"strconv"
	"strings"
)

// Meta is the header of one GGUF file, in the terms fleet needs.
type Meta struct {
	Arch      string `json:"arch"`
	Name      string `json:"name,omitempty"`
	SizeLabel string `json:"size_label,omitempty"` // "8B", "35B-A3B" as the model's own metadata says
	Layers    int    `json:"layers,omitempty"`
	Ctx       int    `json:"ctx,omitempty"` // the context length the model was trained for
	Experts   int    `json:"experts,omitempty"`
	Used      int    `json:"experts_used,omitempty"`
	Quant     string `json:"quant,omitempty"` // general.file_type: Q4_K_M, …
	Pooling   int    `json:"pooling,omitempty"`
	Causal    *bool  `json:"causal,omitempty"` // <arch>.attention.causal; false for encoders
	// Chat template facts (tokenizer.chat_template): present, and which features its text uses.
	Template      bool  `json:"template,omitempty"`
	TemplateTool  bool  `json:"template_tools,omitempty"`
	TemplateThink bool  `json:"template_thinking,omitempty"`
	Shards        int   `json:"shards,omitempty"`        // split.count, when the file is one shard of a split model
	ShardNo       int   `json:"shard_no,omitempty"`      // split.no: which one (0-based)
	TargetLayers  []int `json:"target_layers,omitempty"` // dflash.target_layers
}

var ftype = map[int]string{0: "F32", 1: "F16", 2: "Q4_0", 3: "Q4_1", 7: "Q8_0", 8: "Q5_0", 9: "Q5_1", 10: "Q2_K", 11: "Q3_K_S", 12: "Q3_K_M",
	13: "Q3_K_L", 14: "Q4_K_S", 15: "Q4_K_M", 16: "Q5_K_S", 17: "Q5_K_M", 18: "Q6_K", 19: "IQ2_XXS", 20: "IQ2_XS", 21: "Q2_K_S", 22: "IQ3_XS",
	23: "IQ3_XXS", 24: "IQ1_S", 25: "IQ4_NL", 26: "IQ3_S", 27: "IQ3_M", 28: "IQ2_S", 29: "IQ2_M", 30: "IQ4_XS", 31: "IQ1_M", 32: "BF16",
	36: "TQ1_0", 37: "TQ2_0", 38: "MXFP4_MOE"}

// Kind: "mmproj" for a vision projector, "embed" for an embedding model, "chat" for a model with a chat template,
// "base" for a language model without one (completion only).
func (m *Meta) Kind() string {
	switch {
	case m.Arch == "clip":
		return "mmproj"
	case m.Pooling > 0 || (m.Causal != nil && !*m.Causal && !m.Template):
		return "embed"
	case m.Template:
		return "chat"
	}
	return "base"
}

// Caps are the capabilities the header proves or strongly implies. They are metadata facts, not a measurement:
// `fleet probe` is what shows what the loaded model really does (tools and thinking need the server's own template
// handling to work too, and vision needs the projector next to the model).
func (m *Meta) Caps() []string {
	switch m.Kind() {
	case "mmproj":
		return nil
	case "embed":
		return []string{"embed"}
	}
	caps := []string{"chat"}
	if m.TemplateTool {
		caps = append(caps, "tools")
	}
	if m.TemplateThink {
		caps = append(caps, "thinking")
	}
	return caps
}

// Read parses the header of the GGUF at path.
func Read(path string) (*Meta, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	return Parse(bufio.NewReaderSize(f, 1<<20))
}

type reader struct {
	r   *bufio.Reader
	err error
}

func (r *reader) u32() uint32 {
	var b [4]byte
	r.full(b[:])
	return binary.LittleEndian.Uint32(b[:])
}
func (r *reader) u64() uint64 {
	var b [8]byte
	r.full(b[:])
	return binary.LittleEndian.Uint64(b[:])
}
func (r *reader) full(b []byte) {
	if r.err == nil {
		_, r.err = io.ReadFull(r.r, b)
	}
}
func (r *reader) skip(n uint64) {
	for n > 0 && r.err == nil {
		c := int(min(n, 1<<30))
		var d int
		d, r.err = r.r.Discard(c)
		n -= uint64(d)
	}
}

// str reads a string, keeping at most max bytes of it (the rest is skipped).
func (r *reader) str(max int) string {
	n := r.u64()
	if r.err != nil {
		return ""
	}
	keep := int(min(n, uint64(max)))
	b := make([]byte, keep)
	r.full(b)
	r.skip(n - uint64(keep))
	return string(b)
}

var sizes = map[uint32]uint64{0: 1, 1: 1, 2: 2, 3: 2, 4: 4, 5: 4, 6: 4, 7: 1, 10: 8, 11: 8, 12: 8}

// value reads one value of type t; numbers come back as float64 (exact up to 2^53), strings as themselves.
func (r *reader) value(t uint32, keepStr int) any {
	switch t {
	case 8:
		return r.str(keepStr)
	case 9:
		et := r.u32()
		n := r.u64()
		if sz, ok := sizes[et]; ok {
			r.skip(sz * n)
			return nil
		}
		for i := uint64(0); i < n && r.err == nil; i++ {
			r.value(et, 0)
		}
		return nil
	case 4, 5, 2, 3, 0, 1, 7:
		var b [4]byte
		sz := sizes[t]
		r.full(b[:sz])
		var v uint32
		for i := int(sz) - 1; i >= 0; i-- {
			v = v<<8 | uint32(b[i])
		}
		return float64(v)
	case 10, 11:
		return float64(r.u64())
	case 6:
		return float64(math.Float32frombits(r.u32()))
	case 12:
		return math.Float64frombits(r.u64())
	}
	r.err = fmt.Errorf("unknown value type %d", t)
	return nil
}

// Parse reads the metadata of a GGUF from r.
func Parse(br *bufio.Reader) (*Meta, error) {
	r := &reader{r: br}
	var magic [4]byte
	r.full(magic[:])
	if r.err != nil || string(magic[:]) != "GGUF" {
		return nil, fmt.Errorf("not a GGUF file")
	}
	ver := r.u32()
	if ver < 2 || ver > 3 {
		return nil, fmt.Errorf("GGUF version %d is not supported", ver)
	}
	r.u64() // tensors
	n := r.u64()
	if n > 1<<20 {
		return nil, fmt.Errorf("implausible metadata count %d", n)
	}
	kv := map[string]any{}
	for i := uint64(0); i < n && r.err == nil; i++ {
		key := r.str(256)
		t := r.u32()
		if key == "dflash.target_layers" && t == 9 {
			et := r.u32()
			count := r.u64()
			var layers []int
			for j := uint64(0); j < count && r.err == nil; j++ {
				v := r.value(et, 0)
				if f, ok := v.(float64); ok {
					layers = append(layers, int(f))
				}
			}
			kv[key] = layers
			continue
		}
		keep := 0
		if key == "general.architecture" || key == "general.name" || key == "general.size_label" {
			keep = 512
		}
		if key == "tokenizer.chat_template" {
			keep = 256 << 10
		}
		kv[key] = r.value(t, keep)
	}
	if r.err != nil {
		return nil, r.err
	}
	m := &Meta{}
	if tl, ok := kv["dflash.target_layers"].([]int); ok {
		m.TargetLayers = tl
	}
	str := func(k string) string { s, _ := kv[k].(string); return s }
	num := func(k string) int {
		v, ok := kv[k].(float64)
		if !ok {
			return 0
		}
		return int(v)
	}
	m.Arch, m.Name, m.SizeLabel = str("general.architecture"), str("general.name"), str("general.size_label")
	a := m.Arch
	m.Layers, m.Ctx, m.Experts, m.Used = num(a+".block_count"), num(a+".context_length"), num(a+".expert_count"), num(a+".expert_used_count")
	m.Pooling = num(a + ".pooling_type")
	if v, ok := kv[a+".attention.causal"].(float64); ok {
		c := v != 0
		m.Causal = &c
	}
	if q, ok := kv["general.file_type"].(float64); ok {
		m.Quant = ftype[int(q)]
	}
	m.Shards, m.ShardNo = num("split.count"), num("split.no")
	if t := str("tokenizer.chat_template"); t != "" {
		m.Template = true
		low := strings.ToLower(t)
		m.TemplateTool = strings.Contains(low, "tools") || strings.Contains(low, "tool_call")
		m.TemplateThink = strings.Contains(low, "<think>") || strings.Contains(low, "enable_thinking") || strings.Contains(low, "reasoning_content")
	}
	return m, nil
}

// TargetLayersCSV returns dflash.target_layers as a comma-separated string (e.g. "14,28,42"),
// or empty string if not present.
func (m *Meta) TargetLayersCSV() string {
	if len(m.TargetLayers) == 0 {
		return ""
	}
	var b strings.Builder
	for i, l := range m.TargetLayers {
		if i > 0 {
			b.WriteByte(',')
		}
		b.WriteString(strconv.Itoa(l))
	}
	return b.String()
}
