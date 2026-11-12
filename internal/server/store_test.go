package server

import (
	"bytes"
	"encoding/binary"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/tevfik/fleet-router/internal/hostinfo"
	"github.com/tevfik/fleet-router/internal/plan"
	"github.com/tevfik/fleet-router/internal/store"
)

func srvGGUF(t *testing.T, path string, fill byte) {
	t.Helper()
	var b bytes.Buffer
	u32 := func(v uint32) { binary.Write(&b, binary.LittleEndian, v) }
	u64 := func(v uint64) { binary.Write(&b, binary.LittleEndian, v) }
	str := func(s string) { u64(uint64(len(s))); b.WriteString(s) }
	b.WriteString("GGUF")
	u32(3)
	u64(2)
	u64(3)
	str("general.architecture")
	u32(8)
	str("llama")
	str("llama.block_count")
	u32(4)
	u32(2)
	str("llama.embedding_length")
	u32(4)
	u32(64)
	for i := 0; i < 2; i++ {
		str("blk." + string(rune('0'+i)) + ".w.weight")
		u32(1)
		u64(8)
		u32(0)
		u64(uint64(i) * 65536)
	}
	for b.Len()%32 != 0 {
		b.WriteByte(0)
	}
	d := make([]byte, 2*65536)
	for i := range d {
		d[i] = byte((i*7 + int(i/65536)*13 + int(fill)) % 251) // the two tensors differ
	}
	b.Write(d)
	os.MkdirAll(filepath.Dir(path), 0o755)
	if err := os.WriteFile(path, b.Bytes(), 0o644); err != nil {
		t.Fatal(err)
	}
}

func storeServer(t *testing.T) (*Server, string) {
	t.Setenv("LLAMA_CACHE", "")
	s := planServer(t)
	return s, filepath.Join(filepath.Dir(os.Getenv("FLEET_STATS")), "models")
}

func waitStored(t *testing.T, s *Server, n int) store.Inventory {
	t.Helper()
	var inv store.Inventory
	rtEventually(t, "the files are registered", func() bool {
		inv = s.LocalStore()
		return len(inv.Files) == n && inv.Hashing == nil
	})
	return inv
}

func TestStore_AddListRemoveOverHTTP(t *testing.T) {
	s, dir := storeServer(t)
	srvGGUF(t, filepath.Join(dir, "a.gguf"), 1)
	srvGGUF(t, filepath.Join(dir, "x", "b.gguf"), 2)
	os.WriteFile(filepath.Join(dir, "readme.txt"), []byte("x"), 0o644)

	rec := rtDo(t, s, "POST", "/fleet/store/add", map[string]any{"paths": []string{dir}})
	if rec.Code != 202 || !strings.Contains(rec.Body.String(), `"queued":2`) {
		t.Fatalf("%d %s", rec.Code, rec.Body)
	}
	inv := waitStored(t, s, 2)
	if inv.Files[0].Name != "a" || inv.Files[1].Name != "b" || !inv.Files[0].Complete || inv.Files[0].Layers != 2 {
		t.Fatalf("%+v", inv.Files)
	}
	var got store.Inventory
	rtJSON(t, rtDo(t, s, "GET", "/fleet/store", nil), &got)
	if len(got.Files) != 2 {
		t.Fatalf("%+v", got)
	}
	// the inventory travels with the machine's readings, without the files' paths
	rec = rtDo(t, s, "GET", "/fleet/host", nil)
	var h HostReport
	rtJSON(t, rec, &h)
	if h.Store == nil || len(h.Store.Files) != 2 || strings.Contains(rec.Body.String(), dir) {
		t.Fatalf("store in /fleet/host: %+v (a path must not leave the machine)", h.Store)
	}
	var all struct{ Nodes []NodeStore }
	rtJSON(t, rtDo(t, s, "GET", "/fleet/store/all", nil), &all)
	if len(all.Nodes) != 3 || all.Nodes[0].Store == nil || len(all.Nodes[0].Store.Files) != 2 {
		t.Fatalf("%+v", all)
	}
	// remove
	rec = rtDo(t, s, "POST", "/fleet/store/rm", map[string]any{"ref": inv.Files[0].SHA256[:10]})
	if rec.Code != 200 {
		t.Fatalf("%d %s", rec.Code, rec.Body)
	}
	if n := len(s.LocalStore().Files); n != 1 {
		t.Fatalf("%d left", n)
	}
	if rtDo(t, s, "POST", "/fleet/store/rm", map[string]any{"ref": "ghost"}).Code != 404 {
		t.Fatal("an unknown file is a 404")
	}
}

func TestStore_BadInputAndAdminOnly(t *testing.T) {
	s, dir := storeServer(t)
	if rtDo(t, s, "POST", "/fleet/store/add", map[string]any{"paths": []string{}}).Code != 400 {
		t.Fatal("no paths")
	}
	if rtDo(t, s, "POST", "/fleet/store/add", "nonsense").Code != 400 {
		t.Fatal("not JSON")
	}
	if rtDo(t, s, "POST", "/fleet/store/rm", map[string]any{}).Code != 400 {
		t.Fatal("no ref")
	}
	for _, c := range []struct{ m, p string }{{"GET", "/fleet/store"}, {"GET", "/fleet/store/all"}, {"POST", "/fleet/store/add"}, {"POST", "/fleet/store/rm"}} {
		if rec := rtDo(t, s, c.m, c.p, map[string]any{"paths": []string{dir}, "ref": "x"}, rtRemote); rec.Code != 403 {
			t.Errorf("%s %s from another machine: %d", c.m, c.p, rec.Code)
		}
	}
}

func TestStore_AFailedAddIsReportedAndOneRunAtATime(t *testing.T) {
	s, dir := storeServer(t)
	rtDo(t, s, "POST", "/fleet/store/add", map[string]any{"paths": []string{filepath.Join(dir, "missing.gguf")}})
	rtEventually(t, "the failure shows", func() bool { return strings.Contains(s.LocalStore().Error, "missing.gguf") })
	// a second request while the first runs is refused
	f := &s.files
	f.mu.Lock()
	f.running = true
	f.mu.Unlock()
	if rec := rtDo(t, s, "POST", "/fleet/store/add", map[string]any{"paths": []string{dir}}); rec.Code != 409 {
		t.Fatalf("%d", rec.Code)
	}
	f.mu.Lock()
	f.running = false
	f.mu.Unlock()
}

func TestStore_WorkerCacheIsListedAndRescannedSlowly(t *testing.T) {
	s, dir := storeServer(t)
	cache := filepath.Join(dir, "cache")
	os.MkdirAll(cache, 0o755)
	os.WriteFile(filepath.Join(cache, "00112233445566ff"), make([]byte, 100), 0o644)
	cfg := s.Config()
	cfg.RingCache = cache
	s.cfg = cfg
	inv := s.LocalStore()
	if inv.Cache == nil || inv.Cache.Entries != 1 || inv.Cache.Bytes != 100 {
		t.Fatalf("%+v", inv.Cache)
	}
	os.WriteFile(filepath.Join(cache, "aabbccddeeff0011"), make([]byte, 50), 0o644)
	if s.LocalStore().Cache.Entries != 1 {
		t.Fatal("the directory is scanned at most every 30 s, not on every readout")
	}
	s.files.mu.Lock()
	s.files.cacheAt = time.Now().Add(-time.Minute)
	s.files.mu.Unlock()
	if s.LocalStore().Cache.Entries != 2 {
		t.Fatal("rescanned after the interval")
	}
	// LLAMA_CACHE is the fallback
	cfg.RingCache = ""
	s.cfg = cfg
	t.Setenv("LLAMA_CACHE", filepath.Dir(cache))
	if c := s.LocalStore().Cache; c != nil && c.Dir == cache {
		t.Fatalf("a different directory: %+v", c)
	}
}

func TestStore_PlanPricesTransfersOnlyForARegisteredFile(t *testing.T) {
	s, dir := storeServer(t)
	p := filepath.Join(dir, "m.gguf")
	srvGGUF(t, p, 3)
	m, err := plan.ReadGGUF(p)
	if err != nil {
		t.Fatal(err)
	}
	m.Tensors = nil
	s.hwb.res = &hostinfo.Bench{Fingerprint: "x", FastMem: 4 << 30, FastBW: 50, FastWhere: "RAM", RAMTotal: 8 << 30, RAMBW: 50}
	s.hwb.loaded = true
	req := PlanRequest{Model: *m, Ctx: 128}
	var r plan.Result
	rtJSON(t, rtDo(t, s, "POST", "/fleet/plan", req), &r)
	if r.Known {
		t.Fatal("an unregistered file: the transfers are not known")
	}
	rtDo(t, s, "POST", "/fleet/store/add", map[string]any{"paths": []string{p}})
	waitStored(t, s, 1)
	rtJSON(t, rtDo(t, s, "POST", "/fleet/plan", req), &r)
	if !r.Known {
		t.Fatal("a registered file is known")
	}
	// the nodes of this test server all answer from this machine, which has the file
	if len(r.Candidates) == 0 {
		t.Fatalf("no candidates: %s", r.Verdict)
	}
	for _, c := range r.Candidates {
		if c.FetchGB != 0 {
			t.Fatalf("nothing to move when the machine has the file: %+v", c)
		}
	}
}

// forgetting on another machine goes over its route; a machine without one is a 502, never a silent local removal
func TestStoreRm_NodeWithoutRouteIs502(t *testing.T) {
	s, _ := storeServer(t)
	rec := rtDo(t, s, "POST", "/fleet/store/rm", map[string]any{"ref": "aaaaaaaa", "node": "ghost"})
	if rec.Code != 502 || !strings.Contains(rec.Body.String(), "ghost") {
		t.Fatalf("%d %s", rec.Code, rec.Body.String())
	}
}
