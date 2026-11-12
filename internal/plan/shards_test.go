package plan

import (
	"os"
	"path/filepath"
	"testing"
)

func TestShards_PartitionNeedAndVerify(t *testing.T) {
	p := filepath.Join(t.TempDir(), "m.gguf")
	d := llamaFile(t)
	if err := os.WriteFile(p, d, 0o644); err != nil {
		t.Fatal(err)
	}
	m, _ := ReadGGUF(p)
	s, err := BuildShards(p, true)
	if err != nil {
		t.Fatal(err)
	}
	if len(s.Blocks) != 4 || len(s.Globals) != 1 {
		t.Fatalf("%d blocks, %d globals", len(s.Blocks), len(s.Globals))
	}
	var sum int64
	for _, b := range s.Blocks {
		sum += b.Bytes
		if b.Sha256 == "" {
			t.Fatal("hash missing")
		}
	}
	sum += s.Globals[0].Bytes
	if sum != m.Weights() {
		t.Fatalf("the parts hold %d bytes, the model has %d weights", sum, m.Weights())
	}
	_, all := s.Need(0, 3, true)
	_, mid := s.Need(1, 2, false)
	if all != sum || mid != s.Blocks[1].Bytes+s.Blocks[2].Bytes {
		t.Fatalf("need: all %d (want %d), middle %d", all, sum, mid)
	}
	if err := s.Verify(p, 0, 3); err != nil {
		t.Fatal(err)
	}
	d[s.Blocks[2].Ranges[0].Offset+5] ^= 0xff
	os.WriteFile(p, d, 0o644)
	if err := s.Verify(p, 1, 1); err != nil {
		t.Fatalf("block 1 is intact: %v", err)
	}
	if err := s.Verify(p, 2, 2); err == nil {
		t.Fatal("a changed byte in block 2 must fail its hash")
	}
}

// A split GGUF keeps the metadata in the first file and lists only its own tensors in each file: the weights must be
// the sum over all files (a 138 GB model used to plan as "weights 0.0 GB").
func TestReadGGUF_ShardedSumsAllFiles(t *testing.T) {
	dir := t.TempDir()
	kvs := []kv{
		{"general.architecture", 8, "llama"}, {"general.name", 8, "split"},
		{"llama.block_count", 4, uint32(4)}, {"llama.embedding_length", 4, uint32(4096)},
		{"llama.attention.head_count", 4, uint32(32)}, {"llama.attention.head_count_kv", 4, uint32(8)},
	}
	d1, _ := gguf(t, kvs, []ten{{"token_embd.weight", 40 * mb}, {"blk.0.ffn_down.weight", 60 * mb}, {"blk.1.ffn_down.weight", 60 * mb}})
	d2, _ := gguf(t, []kv{{"split.no", 4, uint32(1)}}, []ten{{"blk.2.ffn_down.weight", 60 * mb}, {"blk.3.ffn_down.weight", 60 * mb}, {"output.weight", 40 * mb}})
	p1 := filepath.Join(dir, "Big-00001-of-00002.gguf")
	p2 := filepath.Join(dir, "Big-00002-of-00002.gguf")
	for p, d := range map[string][]byte{p1: d1, p2: d2} {
		if err := os.WriteFile(p, d, 0o644); err != nil {
			t.Fatal(err)
		}
	}
	for _, from := range []string{p1, p2} { // pointing at any shard reads the whole set
		m, err := ReadGGUF(from)
		if err != nil {
			t.Fatal(err)
		}
		if got, want := m.Weights(), int64(320*mb); got != want {
			t.Fatalf("%s: weights %d, want %d", from, got, want)
		}
		for b, w := range m.LayerBytes {
			if w != 60*mb {
				t.Fatalf("block %d: %d bytes", b, w)
			}
		}
		if m.OutputBytes != 40*mb || m.EmbdBytes != 40*mb {
			t.Fatalf("output %d embd %d", m.OutputBytes, m.EmbdBytes)
		}
		if m.FileBytes != int64(len(d1)+len(d2)) {
			t.Fatalf("file bytes %d", m.FileBytes)
		}
		if w := ShardWarnings(m); len(w) != 0 {
			t.Fatalf("warnings %v", w)
		}
	}
	// an Engram-style separate file next to the shards is reported, not counted
	if err := os.WriteFile(filepath.Join(dir, "Big-engram.gguf"), make([]byte, 64), 0o644); err != nil {
		t.Fatal(err)
	}
	m, _ := ReadGGUF(p1)
	if w := ShardWarnings(m); len(w) != 1 || m.Weights() != 320*mb {
		t.Fatalf("extra file: warnings %v weights %d", w, m.Weights())
	}
	// a missing shard: first one present -> warning; first one gone -> error
	os.Remove(p2)
	m, err := ReadGGUF(p1)
	if err != nil {
		t.Fatal(err)
	}
	if w := ShardWarnings(m); len(w) < 1 {
		t.Fatal("missing shard not reported")
	}
	os.Remove(p1)
	if err := os.WriteFile(p2, d2, 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := ReadGGUF(p2); err == nil {
		t.Fatal("no first shard: want an error")
	}
}

func TestShardNames(t *testing.T) {
	got := ShardNames("/m/DeepSeek-V4-00002-of-00003.gguf")
	want := []string{"/m/DeepSeek-V4-00001-of-00003.gguf", "/m/DeepSeek-V4-00002-of-00003.gguf", "/m/DeepSeek-V4-00003-of-00003.gguf"}
	if len(got) != 3 || got[0] != want[0] || got[1] != want[1] || got[2] != want[2] {
		t.Fatalf("%v", got)
	}
	if ShardNames("/m/plain.gguf") != nil {
		t.Fatal("a single file has no shards")
	}
}
