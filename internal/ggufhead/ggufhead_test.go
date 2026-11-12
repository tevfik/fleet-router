package ggufhead

import (
	"bytes"
	"encoding/binary"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

type kv struct {
	key string
	val any // string, uint32, []string (string array), []uint32 (u32 array)
}

type tensor struct {
	name string
	ne   []uint64
	typ  uint32
	data []byte
}

func le32(b *bytes.Buffer, v uint32) { binary.Write(b, binary.LittleEndian, v) }
func le64(b *bytes.Buffer, v uint64) { binary.Write(b, binary.LittleEndian, v) }
func gstr(b *bytes.Buffer, s string) { le64(b, uint64(len(s))); b.WriteString(s) }

// writeGGUF builds a GGUF v3 file; alignment 32 unless a general.alignment key says otherwise.
func writeGGUF(t *testing.T, path string, kvs []kv, ts []tensor) {
	t.Helper()
	al := uint64(32)
	for _, e := range kvs {
		if e.key == "general.alignment" {
			al = uint64(e.val.(uint32))
		}
	}
	var b bytes.Buffer
	le32(&b, ggufMagic)
	le32(&b, 3)
	le64(&b, uint64(len(ts)))
	le64(&b, uint64(len(kvs)))
	for _, e := range kvs {
		gstr(&b, e.key)
		switch v := e.val.(type) {
		case string:
			le32(&b, tString)
			gstr(&b, v)
		case uint32:
			le32(&b, tU32)
			le32(&b, v)
		case []string:
			le32(&b, tArray)
			le32(&b, tString)
			le64(&b, uint64(len(v)))
			for _, s := range v {
				gstr(&b, s)
			}
		case []uint32:
			le32(&b, tArray)
			le32(&b, tU32)
			le64(&b, uint64(len(v)))
			for _, x := range v {
				le32(&b, x)
			}
		default:
			t.Fatalf("kv type %T", v)
		}
	}
	var off uint64
	offs := make([]uint64, len(ts))
	for i, x := range ts {
		offs[i] = off
		off = alignUp(off+uint64(len(x.data)), al)
	}
	for i, x := range ts {
		gstr(&b, x.name)
		le32(&b, uint32(len(x.ne)))
		for _, n := range x.ne {
			le64(&b, n)
		}
		le32(&b, x.typ)
		le64(&b, offs[i])
	}
	b.Write(make([]byte, alignUp(uint64(b.Len()), al)-uint64(b.Len())))
	start := b.Len()
	for i, x := range ts {
		b.Write(make([]byte, start+int(offs[i])-b.Len()))
		b.Write(x.data)
	}
	if err := os.WriteFile(path, b.Bytes(), 0o644); err != nil {
		t.Fatal(err)
	}
}

func fill(n int, seed byte) []byte {
	b := make([]byte, n)
	for i := range b {
		b[i] = seed + byte(i*7)
	}
	return b
}

func draftKVs(vocab int) []kv {
	toks := make([]string, vocab)
	for i := range toks {
		toks[i] = "t"
	}
	return []kv{
		{"general.architecture", "dflash"},
		{"dflash.embedding_length", uint32(4)},
		{"dflash.target_layers", []uint32{1, 2}},
		{"tokenizer.ggml.tokens", toks},
	}
}

func draftTensors() []tensor {
	return []tensor{
		{"fc.weight", []uint64{8, 4}, 0, fill(8*4*4, 1)},
		{"output_norm.weight", []uint64{4}, 0, fill(16, 9)},
	}
}

// tensorBytes reads a tensor's first n bytes from a parsed file.
func tensorBytes(t *testing.T, g *file, name string, n int) []byte {
	t.Helper()
	ti, ok := g.find(name)
	if !ok {
		t.Fatalf("%s: no tensor %s", g.path, name)
	}
	raw, err := os.ReadFile(g.path)
	if err != nil {
		t.Fatal(err)
	}
	o := g.dataStart + int64(ti.offset)
	return raw[o : o+int64(n)]
}

func TestEmbedHeadSharded(t *testing.T) {
	dir := t.TempDir()
	draft := filepath.Join(dir, "draft.gguf")
	writeGGUF(t, draft, draftKVs(6), draftTensors())
	head := fill(4*6*2, 50) // F16 [4, 6]
	writeGGUF(t, filepath.Join(dir, "tgt-00001-of-00002.gguf"), []kv{{"general.architecture", "qwen4exp"}},
		[]tensor{{"token_embd.weight", []uint64{4, 6}, 1, fill(48, 3)}, {"blk.0.x", []uint64{5}, 0, fill(20, 4)}})
	writeGGUF(t, filepath.Join(dir, "tgt-00002-of-00002.gguf"), []kv{{"general.architecture", "qwen4exp"}},
		[]tensor{{"blk.1.x", []uint64{3}, 0, fill(12, 5)}, {"output.weight", []uint64{4, 6}, 1, head}})
	out := filepath.Join(dir, "out.gguf")

	res, err := EmbedHead(draft, filepath.Join(dir, "tgt-00002-of-00002.gguf"), out)
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Tensors) != 1 || res.Tensors[0] != "output.weight" {
		t.Fatalf("added %v", res.Tensors)
	}
	g, err := parse(out)
	if err != nil {
		t.Fatal(err)
	}
	if len(g.tensors) != 3 || g.strs["general.architecture"] != "dflash" || g.ints["dflash.embedding_length"] != 4 || g.arrLen["tokenizer.ggml.tokens"] != 6 {
		t.Fatalf("bad output header: %d tensors, %v %v %v", len(g.tensors), g.strs, g.ints, g.arrLen)
	}
	ow, _ := g.find("output.weight")
	if ow.typ != 1 || len(ow.ne) != 2 || ow.ne[0] != 4 || ow.ne[1] != 6 || ow.offset%32 != 0 {
		t.Fatalf("bad head info %+v", ow)
	}
	if !bytes.Equal(tensorBytes(t, g, "output.weight", len(head)), head) {
		t.Fatal("head bytes differ (must come from output.weight, not token_embd.weight)")
	}
	d, _ := parse(draft)
	for _, x := range draftTensors() {
		if !bytes.Equal(tensorBytes(t, g, x.name, len(x.data)), x.data) || !bytes.Equal(tensorBytes(t, d, x.name, len(x.data)), x.data) {
			t.Fatalf("draft tensor %s not preserved", x.name)
		}
	}
	if _, err := os.Stat(out + ".tmp"); !os.IsNotExist(err) {
		t.Fatal("temporary file left behind")
	}
}

func TestEmbedHeadTiedAndScale(t *testing.T) {
	dir := t.TempDir()
	draft := filepath.Join(dir, "draft.gguf")
	writeGGUF(t, draft, draftKVs(6), draftTensors())
	emb := fill(48, 70)
	tied := filepath.Join(dir, "tied.gguf")
	writeGGUF(t, tied, nil, []tensor{{"token_embd.weight", []uint64{4, 6}, 1, emb}})
	out := filepath.Join(dir, "o1.gguf")
	if _, err := EmbedHead(draft, tied, out); err != nil {
		t.Fatal(err)
	}
	g, _ := parse(out)
	if !bytes.Equal(tensorBytes(t, g, "output.weight", 48), emb) {
		t.Fatal("tied head must be the token embedding matrix")
	}

	scaled := filepath.Join(dir, "scaled.gguf")
	sc := fill(4, 90)
	writeGGUF(t, scaled, nil, []tensor{{"output.weight", []uint64{4, 6}, 1, fill(48, 80)}, {"output.scale", []uint64{1}, 0, sc}})
	out2 := filepath.Join(dir, "o2.gguf")
	res, err := EmbedHead(draft, scaled, out2)
	if err != nil {
		t.Fatal(err)
	}
	g2, _ := parse(out2)
	if len(res.Tensors) != 2 || len(g2.tensors) != 4 || !bytes.Equal(tensorBytes(t, g2, "output.scale", 4), sc) ||
		!bytes.Equal(tensorBytes(t, g2, "output.weight", 48), fill(48, 80)) {
		t.Fatalf("scale case: %v", res.Tensors)
	}
}

func TestEmbedHeadRejects(t *testing.T) {
	dir := t.TempDir()
	tgt := filepath.Join(dir, "tgt.gguf")
	writeGGUF(t, tgt, nil, []tensor{{"output.weight", []uint64{4, 6}, 1, fill(48, 1)}})
	mk := func(name string, kvs []kv, ts []tensor) string {
		p := filepath.Join(dir, name)
		writeGGUF(t, p, kvs, ts)
		return p
	}
	good := mk("good.gguf", draftKVs(6), draftTensors())
	cases := []struct {
		name, draft, target, out, want string
	}{
		{"not dflash", mk("llama.gguf", []kv{{"general.architecture", "llama"}}, nil), tgt, "out-a.gguf", "not a DFlash draft"},
		{"has head", mk("h.gguf", draftKVs(6), append(draftTensors(), tensor{"output.weight", []uint64{4, 6}, 1, fill(48, 2)})), tgt, "out-b.gguf", "already has output.weight"},
		{"d2t", mk("d.gguf", draftKVs(6), append(draftTensors(), tensor{"d2t", []uint64{6}, 0, fill(24, 2)})), tgt, "out-c.gguf", "reduced-vocabulary"},
		{"width", mk("w.gguf", append(draftKVs(6)[:1], kv{"dflash.embedding_length", uint32(8)}), draftTensors()), tgt, "out-d.gguf", "width"},
		{"vocab", mk("v.gguf", draftKVs(7), draftTensors()), tgt, "out-e.gguf", "vocabulary"},
		{"no head", good, mk("nohead.gguf", nil, []tensor{{"blk.0.x", []uint64{4}, 0, fill(16, 1)}}), "out-f.gguf", "neither"},
		{"missing shard", good, filepath.Join(dir, "m-00001-of-00002.gguf"), "out-g.gguf", "missing"},
		{"same out", good, tgt, "good.gguf", "new file"},
		{"not gguf", mk("x.gguf", nil, nil), tgt, "out-h.gguf", ""},
	}
	os.WriteFile(filepath.Join(dir, "m-00001-of-00002.gguf"), nil, 0o644)
	os.WriteFile(filepath.Join(dir, "x.gguf"), []byte("nope, not a gguf file at all"), 0o644)
	for _, c := range cases {
		_, err := EmbedHead(c.draft, c.target, filepath.Join(dir, c.out))
		if err == nil || !strings.Contains(err.Error(), c.want) {
			t.Errorf("%s: err = %v, want %q", c.name, err, c.want)
		}
		if c.out != "good.gguf" {
			if _, serr := os.Stat(filepath.Join(dir, c.out)); serr == nil {
				t.Errorf("%s: output written despite error", c.name)
			}
		}
	}
}

func TestShards(t *testing.T) {
	dir := t.TempDir()
	for i := 1; i <= 3; i++ {
		os.WriteFile(filepath.Join(dir, "m-0000"+string(rune('0'+i))+"-of-00003.gguf"), nil, 0o644)
	}
	got, err := Shards(filepath.Join(dir, "m-00002-of-00003.gguf"))
	if err != nil || len(got) != 3 || !strings.HasSuffix(got[0], "m-00001-of-00003.gguf") || !strings.HasSuffix(got[2], "m-00003-of-00003.gguf") {
		t.Fatalf("shards %v %v", got, err)
	}
	one, err := Shards("plain.gguf")
	if err != nil || len(one) != 1 || one[0] != "plain.gguf" {
		t.Fatalf("plain %v %v", one, err)
	}
}

func TestEmbedHeadAlignment(t *testing.T) {
	dir := t.TempDir()
	draft := filepath.Join(dir, "draft.gguf")
	writeGGUF(t, draft, append(draftKVs(6), kv{"general.alignment", uint32(256)}), draftTensors())
	tgt := filepath.Join(dir, "tgt.gguf")
	head := fill(48, 11)
	writeGGUF(t, tgt, []kv{{"general.alignment", uint32(128)}}, []tensor{{"blk.0.x", []uint64{3}, 0, fill(12, 2)}, {"output.weight", []uint64{4, 6}, 1, head}})
	out := filepath.Join(dir, "out.gguf")
	if _, err := EmbedHead(draft, tgt, out); err != nil {
		t.Fatal(err)
	}
	g, err := parse(out)
	if err != nil {
		t.Fatal(err)
	}
	ow, _ := g.find("output.weight")
	if g.alignment != 256 || g.dataStart%256 != 0 || ow.offset%256 != 0 {
		t.Fatalf("alignment %d, data %d, head offset %d", g.alignment, g.dataStart, ow.offset)
	}
	if !bytes.Equal(tensorBytes(t, g, "output.weight", 48), head) || !bytes.Equal(tensorBytes(t, g, "fc.weight", 128), draftTensors()[0].data) {
		t.Fatal("bytes differ with non-default alignment")
	}
}

func TestHasHead(t *testing.T) {
	dir := t.TempDir()
	draftWithoutHead := filepath.Join(dir, "draft-nohead.gguf")
	writeGGUF(t, draftWithoutHead, draftKVs(6), draftTensors())

	has, err := HasHead(draftWithoutHead)
	if err != nil {
		t.Fatalf("HasHead draftWithoutHead error: %v", err)
	}
	if has {
		t.Fatal("expected HasHead to be false for draftWithoutHead")
	}

	tgtWithHead := filepath.Join(dir, "tgt-withhead.gguf")
	headData := fill(48, 11)
	writeGGUF(t, tgtWithHead, []kv{{"general.alignment", uint32(128)}}, []tensor{
		{"output.weight", []uint64{4, 6}, 1, headData},
	})

	has, err = HasHead(tgtWithHead)
	if err != nil {
		t.Fatalf("HasHead tgtWithHead error: %v", err)
	}
	if !has {
		t.Fatal("expected HasHead to be true for tgtWithHead")
	}
}
