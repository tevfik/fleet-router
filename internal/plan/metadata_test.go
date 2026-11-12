package plan

import (
	"bytes"
	"math"
	"testing"
)

// The model facts the deploy planner uses come from the GGUF schema, whatever the file or model is called.
func TestParseGGUF_RecurrentAndNextNFromMetadata(t *testing.T) {
	base := []kv{{"general.architecture", 8, "anyarch"}, {"anyarch.block_count", 4, uint32(2)}, {"anyarch.embedding_length", 4, uint32(64)}}
	ts := []ten{{"token_embd.weight", mb}, {"blk.0.attn_q.weight", mb}, {"blk.1.attn_q.weight", mb}}
	parse := func(kvs []kv, ts []ten) *Model {
		d, _ := gguf(t, kvs, ts)
		m, err := ParseGGUF(bytes.NewReader(d), int64(len(d)))
		if err != nil {
			t.Fatal(err)
		}
		return m
	}
	if m := parse(base, ts); m.Recurrent || m.HasMTP {
		t.Fatalf("plain attention model: %+v", m)
	}
	for _, k := range []string{"anyarch.ssm.conv_kernel", "anyarch.shortconv.l_cache", "anyarch.full_attention_interval", "anyarch.wkv.head_size"} {
		if m := parse(append(append([]kv{}, base...), kv{k, 4, uint32(4)}), ts); !m.Recurrent {
			t.Errorf("%s must mark the model recurrent", k)
		}
	}
	if m := parse(append(append([]kv{}, base...), kv{"otherarch.ssm.conv_kernel", 4, uint32(4)}), ts); m.Recurrent {
		t.Error("a key of another architecture must not count")
	}
	if m := parse(base, append(append([]ten{}, ts...), ten{"blk.1.ssm_conv1d.weight", mb})); !m.Recurrent {
		t.Error("an ssm tensor marks the model recurrent")
	}
	m := parse(base, append(append([]ten{}, ts...), ten{"blk.2.nextn.eh_proj.weight", mb}, ten{"blk.2.nextn.enorm.weight", mb}))
	if !m.HasMTP || m.MTPLayers != 1 {
		t.Errorf("nextn tensors: HasMTP %v MTPLayers %d", m.HasMTP, m.MTPLayers)
	}
}

func TestRoughTokenMS(t *testing.T) {
	m := &Model{Layers: 4, LayerRead: []int64{1 << 30, 1 << 30, 1 << 30, 1 << 30}, HiddenBytes: 4096 * 4}
	ms, ok := RoughTokenMS(m, []Stage{{Layers: 4, WeightGBps: 100}}, Options{})
	if !ok || math.Abs(ms-(DefaultFixedMS+40)) > 0.01 {
		t.Fatalf("one machine: %v %v", ms, ok)
	}
	split, ok := RoughTokenMS(m, []Stage{{Layers: 2, WeightGBps: 100}, {Layers: 2, WeightGBps: 100}}, Options{})
	if !ok || split <= ms {
		t.Fatalf("a worker adds its round trip: %v vs %v", split, ms)
	}
	if _, ok := RoughTokenMS(m, []Stage{{Layers: 2, WeightGBps: 100}, {Layers: 2}}, Options{}); ok {
		t.Fatal("an unknown rate gives no estimate")
	}
}
