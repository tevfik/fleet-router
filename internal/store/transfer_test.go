package store

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func smallChunks(t *testing.T, chunk int) {
	old := ChunkSize
	ChunkSize = chunk
	t.Cleanup(func() { ChunkSize = old })
}

// origin registers a file in a store of its own and serves it the way a fleet does; wrap lets a test break things.
func origin(t *testing.T, path string, wrap func(http.Handler) http.Handler) (*Store, Source, *atomic.Int64, Entry) {
	t.Helper()
	st := Open(filepath.Join(t.TempDir(), "origin-store"))
	e, err := st.Add(context.Background(), path)
	if err != nil {
		t.Fatal(err)
	}
	mux := http.NewServeMux()
	mux.HandleFunc("GET /fleet/store/manifest/{sha}", st.ManifestHandler())
	mux.HandleFunc("GET /fleet/store/blob/{sha}", st.BlobHandler())
	var blobs atomic.Int64
	counted := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.Contains(r.URL.Path, "/blob/") {
			blobs.Add(1)
		}
		mux.ServeHTTP(w, r)
	})
	var h http.Handler = counted
	if wrap != nil {
		h = wrap(counted)
	}
	srv := httptest.NewServer(h)
	t.Cleanup(srv.Close)
	return st, Source{Base: srv.URL, Name: "origin"}, &blobs, e
}

func readAll(t *testing.T, p string) []byte {
	t.Helper()
	b, err := os.ReadFile(p)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

func TestPull_CopiesVerifiesRegisters(t *testing.T) {
	smallChunks(t, 64<<10)
	src := ggufNamed(t, t.TempDir(), "m.gguf", 3, spec{"blk.0.w.weight", 400 << 10}, spec{"blk.1.w.weight", 300 << 10}, spec{"blk.2.w.weight", 100 << 10})
	_, source, _, e := origin(t, src, nil)
	dst := Open(filepath.Join(t.TempDir(), "dst-store"))
	dir := filepath.Join(t.TempDir(), "files")
	got, err := dst.Pull(context.Background(), source, e.SHA256, dir, PullOptions{Parallel: 3})
	if err != nil {
		t.Fatal(err)
	}
	if got.SHA256 != e.SHA256 || !got.Complete {
		t.Fatalf("%+v", got)
	}
	p := dst.Path(e.SHA256)
	if p == "" || !bytes.Equal(readAll(t, p), readAll(t, src)) {
		t.Fatalf("the copy differs from the original (%q)", p)
	}
	if strings.HasSuffix(p, ".part") || !strings.HasPrefix(filepath.Base(p), "m-"+e.SHA256[:8]) {
		t.Fatalf("name %q", p)
	}
	for _, f := range []string{p + ".part", p + ".part.map"} {
		if _, err := os.Stat(f); err == nil {
			t.Fatalf("%s is left behind", f)
		}
	}
	inv := dst.Inventory("")
	if len(inv.Files) != 1 || !inv.Files[0].Complete {
		t.Fatalf("%+v", inv.Files)
	}
	if len(inv.Transfers) != 1 || inv.Transfers[0].Running || inv.Transfers[0].Err != "" || inv.Transfers[0].Done != inv.Transfers[0].Total || inv.Transfers[0].Kind != "pull" {
		t.Fatalf("%+v", inv.Transfers)
	}
	// the registry survives a restart and a second pull is a no-op
	again, err := Open(dst.dir).Pull(context.Background(), Source{Base: "http://127.0.0.1:1"}, e.SHA256, dir, PullOptions{})
	if err != nil || again.SHA256 != e.SHA256 {
		t.Fatalf("a file that is already here is not fetched again: %v", err)
	}
}

func TestPull_ResumesWhereItStopped(t *testing.T) {
	smallChunks(t, 64<<10)
	src := ggufNamed(t, t.TempDir(), "m.gguf", 1, spec{"blk.0.w.weight", 1 << 20})
	var served atomic.Int64
	failAfter := int64(6)
	_, source, blobs, e := origin(t, src, func(h http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if strings.Contains(r.URL.Path, "/blob/") && failAfter > 0 && served.Add(1) > failAfter {
				http.Error(w, "boom", 500)
				return
			}
			h.ServeHTTP(w, r)
		})
	})
	dst := Open(filepath.Join(t.TempDir(), "s"))
	dir := t.TempDir()
	_, err := dst.Pull(context.Background(), source, e.SHA256, dir, PullOptions{Parallel: 1})
	if err == nil {
		t.Fatal("the source failed half way")
	}
	if dst.Path(e.SHA256) != "" {
		t.Fatal("an unfinished file is not registered")
	}
	parts, _ := filepath.Glob(filepath.Join(dir, "*.part"))
	if len(parts) != 1 {
		t.Fatalf("the partial copy is kept: %v", parts)
	}
	chunks := (len(readAll(t, src)) + 64<<10 - 1) / (64 << 10)
	before := blobs.Load()
	failAfter = 0 // the source is fine now
	if _, err := dst.Pull(context.Background(), source, e.SHA256, dir, PullOptions{Parallel: 1}); err != nil {
		t.Fatal(err)
	}
	if fetched := int(blobs.Load() - before); fetched >= chunks || fetched < 1 {
		t.Fatalf("the second pull fetched %d of %d chunks: it must continue, not restart", fetched, chunks)
	}
	if !bytes.Equal(readAll(t, dst.Path(e.SHA256)), readAll(t, src)) {
		t.Fatal("the resumed copy differs")
	}
	if inv := dst.Inventory(""); len(inv.Transfers) != 2 || inv.Transfers[1].Err == "" {
		t.Fatalf("both attempts are listed, the first failed: %+v", inv.Transfers)
	}
}

func TestPull_CorruptChunkIsRefused(t *testing.T) {
	smallChunks(t, 64<<10)
	src := ggufNamed(t, t.TempDir(), "m.gguf", 1, spec{"blk.0.w.weight", 300 << 10})
	_, source, _, e := origin(t, src, func(h http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if strings.Contains(r.URL.Path, "/blob/") && strings.HasPrefix(r.Header.Get("Range"), "bytes=131072-") {
				rec := httptest.NewRecorder()
				h.ServeHTTP(rec, r)
				b := rec.Body.Bytes()
				b[10] ^= 0xff // flip a byte in the third chunk
				for k, v := range rec.Header() {
					w.Header()[k] = v
				}
				w.WriteHeader(rec.Code)
				w.Write(b)
				return
			}
			h.ServeHTTP(w, r)
		})
	})
	dst := Open(filepath.Join(t.TempDir(), "s"))
	_, err := dst.Pull(context.Background(), source, e.SHA256, t.TempDir(), PullOptions{Parallel: 1})
	if err == nil || !strings.Contains(err.Error(), "does not match the manifest") {
		t.Fatalf("%v", err)
	}
	if dst.Path(e.SHA256) != "" {
		t.Fatal("a corrupt file must not be registered")
	}
}

func TestPull_ManifestThatLiesAboutTheFile(t *testing.T) {
	smallChunks(t, 64<<10)
	src := ggufNamed(t, t.TempDir(), "m.gguf", 1, spec{"blk.0.w.weight", 200 << 10})
	st, _, _, e := origin(t, src, nil)
	m, _ := st.Manifest(e.SHA256)
	fake := strings.Repeat("ab", 32)
	lie := *m
	lie.SHA256 = fake // the chunks are right, the claimed identity of the whole is not
	mux := http.NewServeMux()
	mux.HandleFunc("GET /fleet/store/manifest/{sha}", func(w http.ResponseWriter, r *http.Request) { json.NewEncoder(w).Encode(lie) })
	mux.HandleFunc("GET /fleet/store/blob/{sha}", func(w http.ResponseWriter, r *http.Request) {
		f, _ := os.Open(src)
		defer f.Close()
		http.ServeContent(w, r, "", time.Time{}, f)
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()
	dst := Open(filepath.Join(t.TempDir(), "s"))
	dir := t.TempDir()
	_, err := dst.Pull(context.Background(), Source{Base: srv.URL}, fake, dir, PullOptions{Parallel: 1})
	if err == nil || !strings.Contains(err.Error(), "does not match its sha256") {
		t.Fatalf("the whole-file check catches it: %v", err)
	}
	if left, _ := filepath.Glob(filepath.Join(dir, "*")); len(left) != 0 {
		t.Fatalf("nothing is left behind: %v", left)
	}
	// a manifest for another file than asked
	_, err = dst.Pull(context.Background(), Source{Base: srv.URL}, strings.Repeat("cd", 32), dir, PullOptions{})
	if err == nil || !strings.Contains(err.Error(), "another file") {
		t.Fatalf("%v", err)
	}
}

func TestPull_BadInputsAndSources(t *testing.T) {
	dst := Open(filepath.Join(t.TempDir(), "s"))
	if _, err := dst.Pull(context.Background(), Source{Base: "http://127.0.0.1:1"}, "abc", t.TempDir(), PullOptions{}); err == nil || !strings.Contains(err.Error(), "full sha256") {
		t.Fatalf("%v", err)
	}
	if _, err := dst.Pull(context.Background(), Source{Base: "http://127.0.0.1:1"}, strings.Repeat("a", 64), t.TempDir(), PullOptions{}); err == nil {
		t.Fatal("unreachable source")
	}
	// the source does not have the file
	smallChunks(t, 64<<10)
	src := ggufNamed(t, t.TempDir(), "m.gguf", 1, spec{"blk.0.w.weight", 100 << 10})
	_, source, _, _ := origin(t, src, nil)
	if _, err := dst.Pull(context.Background(), source, strings.Repeat("e", 64), t.TempDir(), PullOptions{}); err == nil || !strings.Contains(err.Error(), "404") {
		t.Fatalf("%v", err)
	}
	// a cancelled pull stops
	_, source2, _, e := origin(t, src, nil)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := dst.Pull(ctx, source2, e.SHA256, t.TempDir(), PullOptions{}); err == nil {
		t.Fatal("cancelled")
	}
}

func TestPull_SourceThatIgnoresRanges(t *testing.T) {
	smallChunks(t, 64<<10)
	src := ggufNamed(t, t.TempDir(), "m.gguf", 1, spec{"blk.0.w.weight", 200 << 10})
	_, source, _, e := origin(t, src, func(h http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			r.Header.Del("Range") // answers 200 with everything
			h.ServeHTTP(w, r)
		})
	})
	dst := Open(filepath.Join(t.TempDir(), "s"))
	if _, err := dst.Pull(context.Background(), source, e.SHA256, t.TempDir(), PullOptions{Parallel: 1}); err == nil {
		t.Fatal("a source that sends the whole file for every chunk is refused")
	}
}

func TestLimit_SlowsTheTransfer(t *testing.T) {
	smallChunks(t, 64<<10)
	src := ggufNamed(t, t.TempDir(), "m.gguf", 1, spec{"blk.0.w.weight", 256 << 10})
	_, source, _, e := origin(t, src, nil)
	dst := Open(filepath.Join(t.TempDir(), "s"))
	t0 := time.Now()
	if _, err := dst.Pull(context.Background(), source, e.SHA256, t.TempDir(), PullOptions{Parallel: 2, Limit: NewLimit(1 << 20)}); err != nil { // 1 MiB/s
		t.Fatal(err)
	}
	if d := time.Since(t0); d < 200*time.Millisecond {
		t.Fatalf("a 1 MiB/s limit on ~0.3 MiB took %v", d)
	}
	var l *Limit
	if l.wait(context.Background(), 1<<30) != nil || NewLimit(0).wait(context.Background(), 1<<30) != nil {
		t.Fatal("no limit")
	}
	ctx, cancel := context.WithCancel(context.Background())
	lim := NewLimit(1)
	lim.wait(ctx, 10) // books the next 10 s
	cancel()
	if err := lim.wait(ctx, 10); err == nil {
		t.Fatal("waiting stops when cancelled")
	}
}

func TestServe_OnlyRegisteredCompleteFiles(t *testing.T) {
	smallChunks(t, 64<<10)
	src := ggufNamed(t, t.TempDir(), "m.gguf", 1, spec{"blk.0.w.weight", 200 << 10})
	st, source, _, e := origin(t, src, nil)
	get := func(path, rng string) (int, []byte) {
		req, _ := http.NewRequest("GET", source.Base+path, nil)
		if rng != "" {
			req.Header.Set("Range", rng)
		}
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		var b bytes.Buffer
		b.ReadFrom(resp.Body)
		return resp.StatusCode, b.Bytes()
	}
	if code, b := get("/fleet/store/blob/"+e.SHA256, "bytes=10-19"); code != 206 || !bytes.Equal(b, readAll(t, src)[10:20]) {
		t.Fatalf("%d %v", code, b)
	}
	if code, _ := get("/fleet/store/manifest/"+e.SHA256, ""); code != 200 {
		t.Fatal(code)
	}
	for _, p := range []string{"/fleet/store/blob/" + strings.Repeat("0", 64), "/fleet/store/blob/..%2f..%2fetc%2fpasswd", "/fleet/store/blob/short", "/fleet/store/manifest/" + strings.Repeat("0", 64), "/fleet/store/manifest/zz"} {
		if code, _ := get(p, ""); code != 404 {
			t.Errorf("%s: %d", p, code)
		}
	}
	os.Remove(src) // the file is gone: it is not promised any more
	if code, _ := get("/fleet/store/blob/"+e.SHA256, ""); code != 404 {
		t.Fatalf("a deleted file: %d", code)
	}
	if code, _ := get("/fleet/store/manifest/"+e.SHA256, ""); code != 404 {
		t.Fatalf("a deleted file's manifest: %d", code)
	}
	_ = st
}

func TestManifestCheck(t *testing.T) {
	good := &Manifest{SHA256: strings.Repeat("a", 64), Size: 100, ChunkSize: 1 << 10, Chunks: []string{strings.Repeat("b", 64)}, Tensors: []Tensor{{Name: "x", Offset: 10, Size: 50}}}
	if err := good.check(); err != nil {
		t.Fatal(err)
	}
	for name, mut := range map[string]func(m *Manifest){
		"bad sha":         func(m *Manifest) { m.SHA256 = "x" },
		"zero size":       func(m *Manifest) { m.Size = 0 },
		"tiny chunk":      func(m *Manifest) { m.ChunkSize = 1 },
		"huge chunk":      func(m *Manifest) { m.ChunkSize = 1 << 40 },
		"wrong count":     func(m *Manifest) { m.Chunks = nil },
		"bad chunk hash":  func(m *Manifest) { m.Chunks = []string{"short"} },
		"tensor outside":  func(m *Manifest) { m.Tensors = []Tensor{{Name: "x", Offset: 90, Size: 50}} },
		"negative tensor": func(m *Manifest) { m.Tensors = []Tensor{{Name: "x", Offset: -1, Size: 5}} },
	} {
		m := *good
		mut(&m)
		if err := m.check(); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
}

func TestFreeBytes(t *testing.T) {
	if n, err := FreeBytes(t.TempDir()); err != nil || n <= 0 {
		t.Fatalf("%d %v", n, err)
	}
	if _, err := FreeBytes("/no/such/dir"); err == nil {
		t.Fatal("a missing directory")
	}
}

func TestSafeName(t *testing.T) {
	if safeName("Qwen 3/8B:Q4") != "Qwen_3_8B_Q4" || safeName("") != "model" || safeName("a-b_c.d") != "a-b_c.d" {
		t.Fatal(safeName("Qwen 3/8B:Q4"))
	}
}

// ---------------------------------------------------------------- the worker cache

func fillFile(t *testing.T) (string, []byte) {
	withThreshold(t, 100<<10)
	smallChunks(t, 64<<10)
	p := ggufNamed(t, t.TempDir(), "m.gguf", 4,
		spec{"token_embd.weight", 150 << 10},
		spec{"blk.0.attn.weight", 300 << 10}, spec{"blk.0.norm.weight", 4 << 10},
		spec{"blk.1.attn.weight", 250 << 10},
		spec{"blk.2.attn.weight", 220 << 10},
		spec{"blk.3.attn.weight", 210 << 10},
		spec{"output_norm.weight", 4 << 10}, spec{"output.weight", 180 << 10})
	return p, readAll(t, p)
}

func TestParseBlocks(t *testing.T) {
	got, err := ParseBlocks("0-2, 7 ,10-11")
	if err != nil || fmt.Sprint(got) != "[0 1 2 7 10 11]" {
		t.Fatalf("%v %v", got, err)
	}
	for _, bad := range []string{"a", "3-1", "1-x", "-5", "0-99999"} {
		if _, err := ParseBlocks(bad); err == nil {
			t.Errorf("%q accepted", bad)
		}
	}
	if got, err := ParseBlocks(""); err != nil || len(got) != 0 {
		t.Fatal("empty is no blocks")
	}
}

func TestSelect(t *testing.T) {
	p, _ := fillFile(t)
	st := Open(filepath.Join(t.TempDir(), "s"))
	e, err := st.Add(context.Background(), p)
	if err != nil {
		t.Fatal(err)
	}
	m, _ := st.Manifest(e.SHA256)
	names := func(ts []Tensor) string {
		var n []string
		for _, x := range ts {
			n = append(n, x.Name)
		}
		return strings.Join(n, ",")
	}
	if got := names(m.Select(BlockSet{Blocks: []int{1, 3}})); got != "blk.1.attn.weight,blk.3.attn.weight" {
		t.Fatalf("only the large tensors of those blocks: %s", got)
	}
	if got := names(m.Select(BlockSet{Blocks: []int{0}, Output: true})); got != "blk.0.attn.weight,output.weight" {
		t.Fatalf("the output projection, never the embedding: %s", got)
	}
	if got := m.Select(BlockSet{Blocks: []int{9}}); len(got) != 0 {
		t.Fatalf("%v", got)
	}
}

func TestFillCache_WritesTheNamesTheWorkerLooksFor(t *testing.T) {
	p, data := fillFile(t)
	_, source, _, e := origin(t, p, nil)
	local := Open(filepath.Join(t.TempDir(), "s"))
	cache := filepath.Join(t.TempDir(), "cache", "rpc")
	res, err := local.FillCache(context.Background(), source, e.SHA256, cache, BlockSet{Blocks: []int{1, 2}, Output: true}, PullOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if res.Tensors != 3 || res.Written != 3 || res.Present != 0 || res.Bytes <= 0 {
		t.Fatalf("%+v", res)
	}
	m, _ := Open(filepath.Join(t.TempDir(), "x")).Manifest("") // none: use the origin's manifest through the source
	_ = m
	mm, err := source.FetchManifest(context.Background(), e.SHA256)
	if err != nil {
		t.Fatal(err)
	}
	files := ScanCache(cache).Files
	for _, tn := range mm.Select(BlockSet{Blocks: []int{1, 2}, Output: true}) {
		b := readAll(t, filepath.Join(cache, tn.FNV))
		if !bytes.Equal(b, data[tn.Offset:tn.Offset+tn.Size]) || FNVHex(b) != tn.FNV || files[tn.FNV] != tn.Size {
			t.Errorf("%s: the cache entry is not the tensor", tn.Name)
		}
	}
	if len(files) != 3 {
		t.Fatalf("only what was asked for: %v", files)
	}
	if ents, _ := os.ReadDir(cache); len(ents) != 3 {
		t.Fatalf("no temp files left: %d", len(ents))
	}
	// the planner's view agrees: this worker's cache now covers those tensors
	if have, total := mm.CacheHit(files); have != 250<<10+220<<10+180<<10 || total <= have {
		t.Fatalf("%d of %d", have, total)
	}
	// again: everything is there already, nothing is fetched
	res2, err := local.FillCache(context.Background(), source, e.SHA256, cache, BlockSet{Blocks: []int{1, 2}, Output: true}, PullOptions{})
	if err != nil || res2.Present != 3 || res2.Written != 0 || res2.Bytes != 0 {
		t.Fatalf("%+v %v", res2, err)
	}
	inv := local.Inventory(cache)
	if len(inv.Transfers) != 1 || inv.Transfers[0].Kind != "fill" || inv.Transfers[0].Running || inv.Cache == nil || inv.Cache.Entries != 3 {
		t.Fatalf("%+v", inv)
	}
	// a truncated entry is replaced
	f := filepath.Join(cache, mm.Select(BlockSet{Blocks: []int{1}})[0].FNV)
	os.Truncate(f, 10)
	res3, err := local.FillCache(context.Background(), source, e.SHA256, cache, BlockSet{Blocks: []int{1}}, PullOptions{})
	if err != nil || res3.Written != 1 || res3.Present != 0 {
		t.Fatalf("%+v %v", res3, err)
	}
}

func TestFillCache_RefusesBadData(t *testing.T) {
	p, _ := fillFile(t)
	_, source, _, e := origin(t, p, func(h http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if strings.Contains(r.URL.Path, "/blob/") {
				rec := httptest.NewRecorder()
				h.ServeHTTP(rec, r)
				b := rec.Body.Bytes()
				if len(b) > 0 {
					b[len(b)/2] ^= 1
				}
				for k, v := range rec.Header() {
					w.Header()[k] = v
				}
				w.WriteHeader(rec.Code)
				w.Write(b)
				return
			}
			h.ServeHTTP(w, r)
		})
	})
	local := Open(filepath.Join(t.TempDir(), "s"))
	cache := t.TempDir()
	_, err := local.FillCache(context.Background(), source, e.SHA256, cache, BlockSet{Blocks: []int{1}}, PullOptions{Parallel: 1})
	if err == nil || !strings.Contains(err.Error(), "does not match the manifest") {
		t.Fatalf("%v", err)
	}
	if ents, _ := os.ReadDir(cache); len(ents) != 0 {
		t.Fatalf("a failed fill leaves nothing in the cache: %v", ents)
	}
	if inv := local.Inventory(""); len(inv.Transfers) != 1 || inv.Transfers[0].Err == "" || inv.Transfers[0].Running {
		t.Fatalf("%+v", inv.Transfers)
	}
}

func TestFillCache_Errors(t *testing.T) {
	p, _ := fillFile(t)
	_, source, _, e := origin(t, p, nil)
	local := Open(filepath.Join(t.TempDir(), "s"))
	if _, err := local.FillCache(context.Background(), source, e.SHA256, "", BlockSet{Blocks: []int{1}}, PullOptions{}); err == nil || !strings.Contains(err.Error(), "no worker cache directory") {
		t.Fatalf("%v", err)
	}
	if _, err := local.FillCache(context.Background(), source, "abc", t.TempDir(), BlockSet{Blocks: []int{1}}, PullOptions{}); err == nil {
		t.Fatal("short sha")
	}
	if _, err := local.FillCache(context.Background(), source, e.SHA256, t.TempDir(), BlockSet{Blocks: []int{99}}, PullOptions{}); err == nil || !strings.Contains(err.Error(), "no cacheable tensor") {
		t.Fatalf("%v", err)
	}
	if _, err := local.FillCache(context.Background(), source, strings.Repeat("0", 64), t.TempDir(), BlockSet{Blocks: []int{1}}, PullOptions{}); err == nil {
		t.Fatal("unknown file")
	}
}

func TestChunkHashesAreWhatTheSourceVerifies(t *testing.T) {
	smallChunks(t, 64<<10)
	src := ggufNamed(t, t.TempDir(), "m.gguf", 1, spec{"blk.0.w.weight", 150 << 10})
	st := Open(filepath.Join(t.TempDir(), "s"))
	e, _ := st.Add(context.Background(), src)
	m, _ := st.Manifest(e.SHA256)
	if m.ChunkSize != 64<<10 || len(m.Chunks) != 3 {
		t.Fatalf("a manifest records the chunk size it was built with: %d, %d chunks", m.ChunkSize, len(m.Chunks))
	}
	data := readAll(t, src)
	sum := sha256.Sum256(data[64<<10 : 128<<10])
	if m.Chunks[1] != hex.EncodeToString(sum[:]) {
		t.Fatal("chunk 1")
	}
}
