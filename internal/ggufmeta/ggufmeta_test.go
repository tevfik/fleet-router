package ggufmeta

import (
	"bufio"
	"bytes"
	"encoding/binary"
	"reflect"
	"testing"
)

type kv struct {
	k string
	t uint32
	v any
}

func build(kvs ...kv) *bufio.Reader {
	var b bytes.Buffer
	u32 := func(v uint32) { binary.Write(&b, binary.LittleEndian, v) }
	u64 := func(v uint64) { binary.Write(&b, binary.LittleEndian, v) }
	str := func(s string) { u64(uint64(len(s))); b.WriteString(s) }
	b.WriteString("GGUF")
	u32(3)
	u64(0)
	u64(uint64(len(kvs)))
	for _, e := range kvs {
		str(e.k)
		u32(e.t)
		switch e.t {
		case 8:
			str(e.v.(string))
		case 4:
			u32(uint32(e.v.(int)))
		case 7:
			if e.v.(bool) {
				b.WriteByte(1)
			} else {
				b.WriteByte(0)
			}
		case 9: // array
			switch items := e.v.(type) {
			case []string:
				u32(8)
				u64(uint64(len(items)))
				for _, s := range items {
					str(s)
				}
			case []int:
				u32(4) // uint32
				u64(uint64(len(items)))
				for _, num := range items {
					u32(uint32(num))
				}
			}
		}
	}
	return bufio.NewReader(&b)
}

func TestParseChatModel(t *testing.T) {
	m, err := Parse(build(
		kv{"general.architecture", 8, "qwen3moe"},
		kv{"tokenizer.ggml.tokens", 9, []string{"a", "b", "c"}}, // arrays are skipped, not kept
		kv{"general.name", 8, "Some Model"},
		kv{"qwen3moe.block_count", 4, 48},
		kv{"qwen3moe.context_length", 4, 40960},
		kv{"qwen3moe.expert_count", 4, 128},
		kv{"qwen3moe.expert_used_count", 4, 8},
		kv{"general.file_type", 4, 15},
		kv{"tokenizer.chat_template", 8, "{% if tools %}…{% endif %}<think>"},
	))
	if err != nil {
		t.Fatal(err)
	}
	if m.Arch != "qwen3moe" || m.Layers != 48 || m.Ctx != 40960 || m.Experts != 128 || m.Used != 8 || m.Quant != "Q4_K_M" || m.Name != "Some Model" {
		t.Fatalf("%+v", m)
	}
	if m.Kind() != "chat" || !reflect.DeepEqual(m.Caps(), []string{"chat", "tools", "thinking"}) {
		t.Fatalf("%s %v", m.Kind(), m.Caps())
	}
}

func TestKinds(t *testing.T) {
	emb, _ := Parse(build(kv{"general.architecture", 8, "bert"}, kv{"bert.pooling_type", 4, 1}, kv{"bert.attention.causal", 7, false}))
	if emb.Kind() != "embed" || !reflect.DeepEqual(emb.Caps(), []string{"embed"}) {
		t.Fatalf("%+v", emb)
	}
	proj, _ := Parse(build(kv{"general.architecture", 8, "clip"}))
	if proj.Kind() != "mmproj" || proj.Caps() != nil {
		t.Fatalf("%+v", proj)
	}
	base, _ := Parse(build(kv{"general.architecture", 8, "llama"}))
	if base.Kind() != "base" || !reflect.DeepEqual(base.Caps(), []string{"chat"}) {
		t.Fatalf("%+v", base)
	}
	if _, err := Parse(bufio.NewReader(bytes.NewReader([]byte("nope")))); err == nil {
		t.Fatal("not a GGUF")
	}
}

func TestParseDFlashTargetLayers(t *testing.T) {
	m, err := Parse(build(
		kv{"general.architecture", 8, "dflash"},
		kv{"dflash.target_layers", 9, []int{14, 28, 42}},
	))
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(m.TargetLayers, []int{14, 28, 42}) {
		t.Fatalf("TargetLayers = %v, want [14 28 42]", m.TargetLayers)
	}
	if got := m.TargetLayersCSV(); got != "14,28,42" {
		t.Fatalf("TargetLayersCSV() = %q, want %q", got, "14,28,42")
	}

	empty := &Meta{}
	if got := empty.TargetLayersCSV(); got != "" {
		t.Fatalf("empty TargetLayersCSV() = %q, want empty", got)
	}
}
