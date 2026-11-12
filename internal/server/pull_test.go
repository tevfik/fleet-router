package server

import (
	"bytes"
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/tevfik/fleet-router/internal/config"
	"github.com/tevfik/fleet-router/internal/store"
)

// twoFleets builds a source fleet (it has a model file) and a target fleet that reaches it as the machine
// "nodeS". Each has its own model store.
func twoFleets(t *testing.T, wrapSource func(http.Handler) http.Handler) (src, dst *Server, file string, sha string) {
	t.Helper()
	t.Setenv("LLAMA_CACHE", "")
	dir := t.TempDir()
	srcS := planServer(t)
	srcS.files.dir = filepath.Join(dir, "src-store")
	dstS := planServer(t)
	dstS.files.dir = filepath.Join(dir, "dst-store")

	file = filepath.Join(dir, "models", "m.gguf")
	srvGGUF(t, file, 5)
	e, err := srcS.Files().Add(context.Background(), file)
	if err != nil {
		t.Fatal(err)
	}
	h := srcS.Handler()
	if wrapSource != nil {
		h = wrapSource(h)
	}
	srv := httptest.NewServer(h)
	t.Cleanup(srv.Close)

	cfg := dstS.Config()
	cfg.Endpoints = append([]config.Endpoint{}, cfg.Endpoints...)
	cfg.Endpoints = append(cfg.Endpoints, rtEp("far", "nodeS", "http://10.255.255.1:11434/v1", "mf"))
	dstS.cfg = cfg
	dstS.routes = func(node string) []nodeRoute {
		if node == "nodeS" {
			return []nodeRoute{{source: "test", base: srv.URL}}
		}
		return nil
	}
	return srcS, dstS, file, e.SHA256
}

func waitTransfers(t *testing.T, s *Server, n int) []store.Transfer {
	t.Helper()
	var tr []store.Transfer
	rtEventually(t, "the transfer finishes", func() bool {
		tr = s.LocalStore().Transfers
		if len(tr) < n {
			return false
		}
		for _, x := range tr {
			if x.Running {
				return false
			}
		}
		return true
	})
	return tr
}

func TestPull_FromAnotherFleetOverHTTP(t *testing.T) {
	src, dst, file, sha := twoFleets(t, nil)
	// the target learns what the source has through the machine readings it already exchanges
	var all struct{ Nodes []NodeStore }
	rtJSON(t, rtDo(t, dst, "GET", "/fleet/store/all", nil), &all)
	found := false
	for _, n := range all.Nodes {
		if n.Node == "nodeS" && n.Store != nil && len(n.Store.Files) == 1 && n.Store.Files[0].SHA256 == sha {
			found = true
		}
	}
	if !found {
		t.Fatalf("%+v", all.Nodes)
	}
	rec := rtDo(t, dst, "POST", "/fleet/store/pull", PullRequest{SHA: sha})
	if rec.Code != 202 {
		t.Fatalf("%d %s", rec.Code, rec.Body)
	}
	tr := waitTransfers(t, dst, 1)
	if tr[0].Err != "" || tr[0].Kind != "pull" || tr[0].Source != "nodeS" || tr[0].Done != tr[0].Total {
		t.Fatalf("%+v", tr)
	}
	p := dst.Files().Path(sha)
	want, _ := os.ReadFile(file)
	if got, _ := os.ReadFile(p); p == "" || !bytes.Equal(got, want) || !strings.Contains(p, "dst-store") {
		t.Fatalf("the copy: %q", p)
	}
	// from the target's side the file is complete now, and the source is unchanged
	if inv := dst.LocalStore(); len(inv.Files) != 1 || !inv.Files[0].Complete {
		t.Fatalf("%+v", inv.Files)
	}
	if len(src.LocalStore().Files) != 1 {
		t.Fatal("the source keeps its file")
	}
	// asking for it again: it is here, nothing happens (no other machine needs to be asked)
	if rec := rtDo(t, dst, "POST", "/fleet/store/pull", PullRequest{SHA: sha, From: "nodeS"}); rec.Code != 202 {
		t.Fatalf("%d %s", rec.Code, rec.Body)
	}
}

func TestPull_RefusesWhatCannotWork(t *testing.T) {
	_, dst, _, sha := twoFleets(t, nil)
	for _, c := range []struct {
		name string
		req  PullRequest
		want string
	}{
		{"short sha", PullRequest{SHA: "abc"}, "full sha256"},
		{"unknown file", PullRequest{SHA: strings.Repeat("0", 64)}, "no other machine has that file"},
		{"wrong machine", PullRequest{SHA: sha, From: "ghost"}, "does not have that file"},
	} {
		rec := rtDo(t, dst, "POST", "/fleet/store/pull", c.req)
		if rec.Code != 400 || !strings.Contains(rec.Body.String(), c.want) {
			t.Errorf("%s: %d %s", c.name, rec.Code, rec.Body)
		}
	}
	if rtDo(t, dst, "POST", "/fleet/store/pull", "nonsense").Code != 400 {
		t.Fatal("not JSON")
	}
	if rtDo(t, dst, "POST", "/fleet/store/pull", PullRequest{SHA: sha}, rtRemote).Code != 403 {
		t.Fatal("starting a transfer is an admin call")
	}
}

func TestPull_OneTransferAtATime(t *testing.T) {
	_, dst, _, sha := twoFleets(t, nil)
	cfg := dst.Config()
	cfg.PullMbps = 1 // 125 kB/s: the 130 kB file takes about a second
	dst.cfg = cfg
	if rec := rtDo(t, dst, "POST", "/fleet/store/pull", PullRequest{SHA: sha}); rec.Code != 202 {
		t.Fatalf("%d %s", rec.Code, rec.Body)
	}
	if rec := rtDo(t, dst, "POST", "/fleet/store/pull", PullRequest{SHA: sha}); rec.Code != 409 {
		t.Fatalf("a second transfer while one runs: %d %s", rec.Code, rec.Body)
	}
	waitTransfers(t, dst, 1)
	if dst.Files().Path(sha) == "" {
		t.Fatal("the first transfer finished")
	}
}

func TestFill_RefusesWhatCannotWork(t *testing.T) {
	_, dst, _, sha := twoFleets(t, nil)
	cfg := dst.Config()
	if rec := rtDo(t, dst, "POST", "/fleet/store/fill", FillRequest{SHA: sha, Blocks: "0"}); rec.Code != 400 || !strings.Contains(rec.Body.String(), "no worker cache directory") {
		t.Fatalf("%d %s", rec.Code, rec.Body)
	}
	cfg.RingCache = filepath.Join(t.TempDir(), "rpc")
	dst.cfg = cfg
	if rec := rtDo(t, dst, "POST", "/fleet/store/fill", FillRequest{SHA: sha, Blocks: "x"}); rec.Code != 400 {
		t.Fatalf("bad blocks: %d", rec.Code)
	}
	if rec := rtDo(t, dst, "POST", "/fleet/store/fill", FillRequest{SHA: sha}); rec.Code != 400 || !strings.Contains(rec.Body.String(), "blocks") {
		t.Fatalf("nothing to fill: %d %s", rec.Code, rec.Body)
	}
	if rec := rtDo(t, dst, "POST", "/fleet/store/fill", FillRequest{SHA: "abc", Blocks: "0"}); rec.Code != 400 {
		t.Fatalf("short sha: %d", rec.Code)
	}
	if rtDo(t, dst, "POST", "/fleet/store/fill", FillRequest{SHA: sha, Blocks: "0"}, rtRemote).Code != 403 {
		t.Fatal("admin only")
	}
}

func TestFill_EndToEnd(t *testing.T) {
	old := store.CacheThreshold
	store.CacheThreshold = 1 << 10 // the test file's tensors are 64 KiB
	t.Cleanup(func() { store.CacheThreshold = old })
	_, dst, file, sha := twoFleets(t, nil)
	cache := filepath.Join(t.TempDir(), "rpc")
	cfg := dst.Config()
	cfg.RingCache = cache
	dst.cfg = cfg
	if rec := rtDo(t, dst, "POST", "/fleet/store/fill", FillRequest{SHA: sha, Blocks: "0-1"}); rec.Code != 202 {
		t.Fatalf("%d %s", rec.Code, rec.Body)
	}
	tr := waitTransfers(t, dst, 1)
	if tr[0].Kind != "fill" || tr[0].Err != "" || tr[0].Done != tr[0].Total {
		t.Fatalf("%+v", tr)
	}
	m, err := dst.routes("nodeS")[0], error(nil)
	_ = m
	_ = err
	data, _ := os.ReadFile(file)
	inv := dst.LocalStore()
	if inv.Cache == nil || inv.Cache.Entries != 2 {
		t.Fatalf("one cache file per tensor: %+v", inv.Cache)
	}
	for name, size := range inv.Cache.Files {
		b, _ := os.ReadFile(filepath.Join(cache, name))
		if int64(len(b)) != size || store.FNVHex(b) != name || !bytes.Contains(data, b) {
			t.Errorf("%s is not a tensor of the file", name)
		}
	}
}

func TestStore_FilesAreServedOnlyToThoseWithTheKey(t *testing.T) {
	t.Setenv("FLEET_TEST_KEY", "sekrit")
	t.Setenv("LLAMA_CACHE", "")
	s := planServer(t)
	s.files.dir = t.TempDir()
	cfg := s.Config()
	cfg.APIKeyEnv = "FLEET_TEST_KEY"
	s.cfg = cfg
	f := filepath.Join(t.TempDir(), "a.gguf")
	srvGGUF(t, f, 9)
	e, err := s.Files().Add(context.Background(), f)
	if err != nil {
		t.Fatal(err)
	}
	for _, p := range []string{"/fleet/store/manifest/" + e.SHA256, "/fleet/store/blob/" + e.SHA256} {
		if rec := rtDo(t, s, "GET", p, nil, rtRemote); rec.Code != 401 {
			t.Errorf("%s without a key from another machine: %d", p, rec.Code)
		}
		if rec := rtDo(t, s, "GET", p, nil, rtRemote, rtBearer("sekrit")); rec.Code != 200 {
			t.Errorf("%s with the key: %d", p, rec.Code)
		}
	}
	rec := rtDo(t, s, "GET", "/fleet/store/blob/"+e.SHA256, nil, rtRemote, rtBearer("sekrit"), func(r *http.Request) { r.Header.Set("Range", "bytes=0-9") })
	if rec.Code != 206 || rec.Body.Len() != 10 {
		t.Fatalf("%d %d", rec.Code, rec.Body.Len())
	}
	if rec := rtDo(t, s, "GET", "/fleet/store/blob/"+strings.Repeat("1", 64), nil, rtRemote, rtBearer("sekrit")); rec.Code != 404 {
		t.Fatalf("%d", rec.Code)
	}
}

func TestForward_CommandToAnotherMachine(t *testing.T) {
	// the far fleet answers: it is a fleet that has no other machine with the file, so its answer is the refusal
	_, dst, _, sha := twoFleets(t, nil)
	rec := rtDo(t, dst, "POST", "/fleet/store/pull", PullRequest{Node: "nodeS", SHA: sha})
	if rec.Code != 400 || !strings.Contains(rec.Body.String(), "no other machine has that file") {
		t.Fatalf("the far fleet's answer comes back as it is: %d %s", rec.Code, rec.Body)
	}
	if rec := rtDo(t, dst, "POST", "/fleet/store/pull", PullRequest{Node: "ghost", SHA: sha}); rec.Code != 502 || !strings.Contains(rec.Body.String(), "no route") {
		t.Fatalf("%d %s", rec.Code, rec.Body)
	}
}

func TestForward_AFleetThatDoesNotTrustTheRoute(t *testing.T) {
	_, dst, _, sha := twoFleets(t, func(h http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			r.RemoteAddr = "10.0.1.9:5555" // the far fleet sees an outsider
			h.ServeHTTP(w, r)
		})
	})
	rec := rtDo(t, dst, "POST", "/fleet/store/pull", PullRequest{Node: "nodeS", SHA: sha})
	if rec.Code != 502 || !strings.Contains(rec.Body.String(), "run it on nodeS itself") {
		t.Fatalf("%d %s", rec.Code, rec.Body)
	}
}

func TestPull_Timeout(t *testing.T) { // the source stops answering: the target does not hang forever on the manifest
	_, dst, _, sha := twoFleets(t, func(h http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if strings.Contains(r.URL.Path, "/fleet/store/manifest/") {
				time.Sleep(9 * time.Second)
			}
			h.ServeHTTP(w, r)
		})
	})
	t0 := time.Now()
	rec := rtDo(t, dst, "POST", "/fleet/store/pull", PullRequest{SHA: sha})
	if rec.Code != 400 || time.Since(t0) > 9*time.Second {
		t.Fatalf("%d after %v: %s", rec.Code, time.Since(t0), rec.Body)
	}
}

func TestPull_AbortCancelsActiveTransfer(t *testing.T) {
	_, dst, _, sha := twoFleets(t, nil)
	// Abort when nothing is running returns 400
	rec := rtDo(t, dst, "POST", "/fleet/store/abort", AbortRequest{})
	if rec.Code != 400 || !strings.Contains(rec.Body.String(), "no transfer running") {
		t.Fatalf("abort with no active transfer: %d %s", rec.Code, rec.Body)
	}
	// Start pull
	if rec := rtDo(t, dst, "POST", "/fleet/store/pull", PullRequest{SHA: sha}); rec.Code != 202 {
		t.Fatalf("start pull: %d %s", rec.Code, rec.Body)
	}
	// Abort while running succeeds with 202
	rec = rtDo(t, dst, "POST", "/fleet/store/abort", AbortRequest{})
	if rec.Code != 202 {
		t.Fatalf("abort running transfer: %d %s", rec.Code, rec.Body)
	}
}

func TestStoreAbortDiscard_RemovesPartialsAndWarnings(t *testing.T) {
	t.Setenv("LLAMA_CACHE", "")
	s := planServer(t)
	s.files.dir = t.TempDir()
	dir := s.filesDir()
	os.MkdirAll(dir, 0o755)
	os.WriteFile(filepath.Join(dir, "x-1234abcd.gguf.part"), []byte("half"), 0o644)
	os.WriteFile(filepath.Join(dir, "x-1234abcd.gguf.part.map"), []byte("{}"), 0o644)
	os.WriteFile(filepath.Join(dir, "done-aaaaaaaa.gguf"), []byte("whole"), 0o644)
	removed, _ := s.DiscardPartials()
	if len(removed) != 2 {
		t.Fatalf("removed %v, want the .part and its map", removed)
	}
	if _, err := os.Stat(filepath.Join(dir, "done-aaaaaaaa.gguf")); err != nil {
		t.Fatal("a finished file was touched")
	}
	if rec := rtDo(t, s, "POST", "/fleet/store/abort", AbortRequest{Discard: true}); rec.Code != 202 {
		t.Fatalf("discard with nothing running: %d %s", rec.Code, rec.Body)
	}
}
