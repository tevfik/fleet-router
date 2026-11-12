package store

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

const mib = 1 << 20

type spec struct {
	name string
	size int64
}

// ggufFile writes a GGUF file with tensors of the given sizes (blk.N.w.weight) and returns its path.
func ggufFile(t *testing.T, dir, name string, sizes ...int64) string {
	t.Helper()
	var specs []spec
	for i, sz := range sizes {
		specs = append(specs, spec{"blk." + string(rune('0'+i)) + ".w.weight", sz})
	}
	return ggufNamed(t, dir, name, len(sizes), specs...)
}

// ggufNamed writes a GGUF file with the named tensors (their bytes a repeating pattern, so a mistake in any
// offset changes a hash).
func ggufNamed(t *testing.T, dir, name string, blocks int, specs ...spec) string {
	t.Helper()
	var b bytes.Buffer
	u32 := func(v uint32) { binary.Write(&b, binary.LittleEndian, v) }
	u64 := func(v uint64) { binary.Write(&b, binary.LittleEndian, v) }
	str := func(s string) { u64(uint64(len(s))); b.WriteString(s) }
	b.WriteString("GGUF")
	u32(3)
	u64(uint64(len(specs)))
	u64(3)
	str("general.architecture")
	u32(8)
	str("llama")
	str("llama.block_count")
	u32(4)
	u32(uint32(blocks))
	str("llama.embedding_length")
	u32(4)
	u32(64)
	var off uint64
	for _, sp := range specs {
		str(sp.name)
		u32(1)
		u64(8)
		u32(0)
		u64(off)
		off += uint64(sp.size)
	}
	for b.Len()%32 != 0 {
		b.WriteByte(0)
	}
	for i, sp := range specs {
		d := make([]byte, sp.size)
		for j := range d {
			d[j] = byte((j*7 + i*13) % 251)
		}
		b.Write(d)
	}
	p := filepath.Join(dir, name)
	os.MkdirAll(dir, 0o755)
	if err := os.WriteFile(p, b.Bytes(), 0o644); err != nil {
		t.Fatal(err)
	}
	return p
}

func withThreshold(t *testing.T, v int64) {
	old := CacheThreshold
	CacheThreshold = v
	t.Cleanup(func() { CacheThreshold = old })
}

func TestFNVHex_KnownValues(t *testing.T) {
	// FNV-1a 64 test vectors
	for in, want := range map[string]string{"": "cbf29ce484222325", "a": "af63dc4c8601ec8c", "foobar": "85944171f73967e8"} {
		if got := FNVHex([]byte(in)); got != want {
			t.Errorf("FNV(%q) = %s, want %s", in, got, want)
		}
	}
}

func TestAdd_HashesEverythingTheWayAnIndependentReaderDoes(t *testing.T) {
	withThreshold(t, 4*mib)
	dir := t.TempDir()
	p := ggufFile(t, dir, "m.gguf", 12*mib, 12*mib, 1*mib) // 25 MiB: two chunks, tensors across the boundary
	s := Open(filepath.Join(dir, "store"))
	e, err := s.Add(context.Background(), p)
	if err != nil {
		t.Fatal(err)
	}
	data, _ := os.ReadFile(p)
	whole := sha256.Sum256(data)
	if e.SHA256 != hex.EncodeToString(whole[:]) || e.Size != int64(len(data)) || !e.Complete || e.Layers != 3 || e.Arch != "llama" {
		t.Fatalf("%+v", e)
	}
	m, err := s.Manifest(e.SHA256)
	if err != nil {
		t.Fatal(err)
	}
	if want := (len(data) + ChunkSize - 1) / ChunkSize; len(m.Chunks) != want || want != 2 {
		t.Fatalf("%d chunks, want %d", len(m.Chunks), want)
	}
	for i, c := range m.Chunks {
		lo, hi := i*ChunkSize, min((i+1)*ChunkSize, len(data))
		sum := sha256.Sum256(data[lo:hi])
		if c != hex.EncodeToString(sum[:]) {
			t.Fatalf("chunk %d", i)
		}
	}
	if len(m.Tensors) != 3 {
		t.Fatalf("%+v", m.Tensors)
	}
	for _, tn := range m.Tensors {
		raw := data[tn.Offset : tn.Offset+tn.Size]
		if tn.Size > 4*mib {
			if tn.FNV != FNVHex(raw) {
				t.Errorf("%s: cache name %s, want %s", tn.Name, tn.FNV, FNVHex(raw))
			}
		} else if tn.FNV != "" {
			t.Errorf("%s is below the cache threshold but has a name", tn.Name)
		}
	}
	if m.Tensors[0].Offset%32 != 0 || m.Tensors[1].Offset != m.Tensors[0].Offset+12*mib {
		t.Fatalf("offsets %+v", m.Tensors)
	}
}

func TestAdd_KnownFileIsNotHashedAgain_ChangedFileIs(t *testing.T) {
	withThreshold(t, 1*mib)
	dir := t.TempDir()
	p := ggufFile(t, dir, "m.gguf", 2*mib, 2*mib)
	s := Open(filepath.Join(dir, "store"))
	e1, err := s.Add(context.Background(), p)
	if err != nil {
		t.Fatal(err)
	}
	m1, _ := s.Manifest(e1.SHA256)
	time.Sleep(10 * time.Millisecond)
	e2, err := s.Add(context.Background(), p)
	if err != nil || e2.SHA256 != e1.SHA256 {
		t.Fatalf("%+v %v", e2, err)
	}
	if m2, _ := s.Manifest(e1.SHA256); !m2.BuiltAt.Equal(m1.BuiltAt) {
		t.Fatal("a known, unchanged file was hashed again")
	}
	// the registry survives a restart
	s = Open(filepath.Join(dir, "store"))
	if inv := s.Inventory(""); len(inv.Files) != 1 || !inv.Files[0].Complete {
		t.Fatalf("%+v", inv)
	}
	// the same path with other content: a new identity, the old entry is replaced
	ggufFile(t, dir, "m.gguf", 3*mib, 1*mib)
	os.Chtimes(p, time.Now().Add(time.Hour), time.Now().Add(time.Hour))
	e3, err := s.Add(context.Background(), p)
	if err != nil || e3.SHA256 == e1.SHA256 {
		t.Fatalf("%+v %v", e3, err)
	}
	if inv := s.Inventory(""); len(inv.Files) != 1 || inv.Files[0].SHA256 != e3.SHA256 {
		t.Fatalf("%+v", inv.Files)
	}
}

func TestInventory_FileThatIsGoneIsNotComplete(t *testing.T) {
	dir := t.TempDir()
	p := ggufFile(t, dir, "m.gguf", mib)
	s := Open(filepath.Join(dir, "store"))
	e, _ := s.Add(context.Background(), p)
	if s.Path(e.SHA256) != p {
		t.Fatalf("path %q", s.Path(e.SHA256))
	}
	os.Remove(p)
	inv := s.Inventory("")
	if len(inv.Files) != 1 || inv.Files[0].Complete || s.Path(e.SHA256) != "" {
		t.Fatalf("a missing file is listed but not promised: %+v", inv.Files)
	}
}

func TestInventory_SameContentTwoPathsIsOneEntry(t *testing.T) {
	dir := t.TempDir()
	a := ggufFile(t, dir, "a.gguf", mib, mib)
	os.MkdirAll(filepath.Join(dir, "x"), 0o755)
	b := ggufFile(t, filepath.Join(dir, "x"), "b.gguf", mib, mib)
	s := Open(filepath.Join(dir, "store"))
	s.Add(context.Background(), a)
	s.Add(context.Background(), b)
	if inv := s.Inventory(""); len(inv.Files) != 1 || !inv.Files[0].Complete {
		t.Fatalf("%+v", inv.Files)
	}
	os.Remove(a)
	if inv := s.Inventory(""); len(inv.Files) != 1 || !inv.Files[0].Complete {
		t.Fatalf("one path is enough: %+v", inv.Files)
	}
}

func TestRemove(t *testing.T) {
	dir := t.TempDir()
	p := ggufFile(t, dir, "tiny.gguf", mib)
	q := ggufFile(t, dir, "other.gguf", 2*mib)
	s := Open(filepath.Join(dir, "store"))
	e, _ := s.Add(context.Background(), p)
	s.Add(context.Background(), q)
	if _, err := s.Remove("nothing"); err == nil {
		t.Fatal("no match is an error")
	}
	if _, err := s.Remove(""); err == nil || !strings.Contains(err.Error(), "matches 2") {
		t.Fatalf("an empty reference matches everything: %v", err)
	}
	got, err := s.Remove(e.SHA256[:8])
	if err != nil || got.SHA256 != e.SHA256 {
		t.Fatalf("%+v %v", got, err)
	}
	if _, err := os.Stat(p); err != nil {
		t.Fatal("the file itself must never be deleted")
	}
	if _, err := s.Manifest(e.SHA256); err == nil {
		t.Fatal("the manifest is gone")
	}
	if inv := s.Inventory(""); len(inv.Files) != 1 || inv.Files[0].Name == "tiny" {
		t.Fatalf("%+v", inv.Files)
	}
	if _, err := s.Remove("other.gguf"); err != nil { // by file name
		t.Fatal(err)
	}
}

func TestAdd_Errors(t *testing.T) {
	dir := t.TempDir()
	s := Open(filepath.Join(dir, "store"))
	if _, err := s.Add(context.Background(), filepath.Join(dir, "none.gguf")); err == nil {
		t.Fatal("missing file")
	}
	if _, err := s.Add(context.Background(), dir); err == nil || !strings.Contains(err.Error(), "directory") {
		t.Fatalf("directory: %v", err)
	}
	junk := filepath.Join(dir, "junk.gguf")
	os.WriteFile(junk, []byte("not a model at all, just text"), 0o644)
	if _, err := s.Add(context.Background(), junk); err == nil {
		t.Fatal("a file that is not a GGUF")
	}
	if inv := s.Inventory(""); len(inv.Files) != 0 {
		t.Fatalf("nothing was registered: %+v", inv.Files)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	p := ggufFile(t, dir, "m.gguf", 2*mib)
	if _, err := s.Add(ctx, p); err == nil {
		t.Fatal("a cancelled hashing stops")
	}
	if inv := s.Inventory(""); inv.Hashing != nil || len(inv.Files) != 0 {
		t.Fatalf("a failed add leaves no trace: %+v", inv)
	}
}

func TestAdd_OneAtATimeAndProgress(t *testing.T) {
	withThreshold(t, 1*mib)
	dir := t.TempDir()
	p := ggufFile(t, dir, "big.gguf", 40*mib)
	s := Open(filepath.Join(dir, "store"))
	done := make(chan error, 1)
	go func() { _, err := s.Add(context.Background(), p); done <- err }()
	var saw bool
	for i := 0; i < 2000 && !saw; i++ {
		if h := s.Inventory("").Hashing; h != nil && h.Total > 0 {
			saw = true
			if _, err := s.Add(context.Background(), ggufFile(t, dir, "second.gguf", mib)); err == nil || !strings.Contains(err.Error(), "already hashing") {
				t.Fatalf("a second file while one is hashed: %v", err)
			}
		}
		time.Sleep(time.Millisecond)
	}
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	if !saw {
		t.Log("hashing finished before it could be observed")
	}
	if s.Inventory("").Hashing != nil {
		t.Fatal("no progress once done")
	}
}

func TestCache_ScanAndHit(t *testing.T) {
	withThreshold(t, 1*mib)
	dir := t.TempDir()
	p := ggufFile(t, dir, "m.gguf", 3*mib, 2*mib, mib/2) // the last is below the threshold
	s := Open(filepath.Join(dir, "store"))
	e, _ := s.Add(context.Background(), p)
	m, _ := s.Manifest(e.SHA256)
	cache := filepath.Join(dir, "cache", "rpc")
	os.MkdirAll(cache, 0o755)
	if c := ScanCache(filepath.Join(dir, "nowhere")); c != nil {
		t.Fatal("no directory, no cache")
	}
	if have, total := m.CacheHit(ScanCache(cache).Files); have != 0 || total != 5*mib {
		t.Fatalf("an empty cache: %d of %d", have, total)
	}
	// the worker holds the first tensor under its name; a wrong size under the second's name does not count
	os.WriteFile(filepath.Join(cache, m.Tensors[0].FNV), make([]byte, m.Tensors[0].Size), 0o644)
	os.WriteFile(filepath.Join(cache, m.Tensors[1].FNV), make([]byte, 10), 0o644)
	os.WriteFile(filepath.Join(cache, "notacachefile"), []byte("x"), 0o644)
	c := ScanCache(cache)
	if c.Entries != 2 || c.Bytes != 3*mib+10 {
		t.Fatalf("%+v", c)
	}
	if have, total := m.CacheHit(c.Files); have != 3*mib || total != 5*mib {
		t.Fatalf("a truncated entry is not a hit: %d of %d", have, total)
	}
	inv := s.Inventory(cache)
	if inv.Cache == nil || inv.Cache.Entries != 2 {
		t.Fatalf("%+v", inv.Cache)
	}
}

func TestCacheDirFollowsLLAMA_CACHE(t *testing.T) {
	t.Setenv("LLAMA_CACHE", "")
	if CacheDir() != "" {
		t.Fatal("unset")
	}
	t.Setenv("LLAMA_CACHE", "/x/y")
	if CacheDir() != "/x/y/rpc" {
		t.Fatal(CacheDir())
	}
}

func TestFind(t *testing.T) {
	dir := t.TempDir()
	p := ggufFile(t, dir, "m.gguf", 2*mib)
	s := Open(filepath.Join(dir, "store"))
	e, _ := s.Add(context.Background(), p)
	if m := s.Find(e.Size, "whatever"); m == nil || m.SHA256 != e.SHA256 {
		t.Fatal("the size identifies it")
	}
	if s.Find(e.Size+1, "") != nil {
		t.Fatal("another size")
	}
}

func TestIsCacheName(t *testing.T) {
	for name, want := range map[string]bool{
		"0123456789abcdef":    true,
		"0123456789ABCDEF":    false, // llama-ring writes lower case
		".fill-0123456789abc": false, // a transfer in progress
		"0123456789abcde":     false,
		"0123456789abcdeg":    false,
	} {
		if got := isCacheName(name); got != want {
			t.Errorf("%q: %v, want %v", name, got, want)
		}
	}
}

func TestKnownSHAReadsTheRecordOnly(t *testing.T) {
	dir := t.TempDir()
	f := filepath.Join(dir, "m.gguf")
	if err := os.WriteFile(f, []byte("abc"), 0o644); err != nil {
		t.Fatal(err)
	}
	s := Open(filepath.Join(dir, "st"))
	if s.KnownSHA(f) != "" {
		t.Fatal("not registered: nothing is hashed for it")
	}
	st, _ := os.Stat(f)
	s.rec = []record{{Path: f, Size: st.Size(), MTime: st.ModTime().UnixNano(), SHA: "cafe"}}
	s.ok = true
	if s.KnownSHA(f) != "cafe" {
		t.Fatal("recorded and unchanged")
	}
	_ = os.WriteFile(f, []byte("abcd"), 0o644)
	if s.KnownSHA(f) != "" {
		t.Fatal("a changed file's record is stale")
	}
}

func TestRemove_ForgetsARecordWhoseManifestIsGone(t *testing.T) {
	dir := t.TempDir()
	st := Open(dir)
	sha := strings.Repeat("ab", 32)
	st.rec, st.ok = []record{{Path: "/gone/model.gguf", SHA: sha}}, true
	if _, err := st.Remove(sha[:12]); err != nil {
		t.Fatal(err)
	}
	if len(st.rec) != 0 {
		t.Fatalf("record stays: %+v", st.rec)
	}
}
