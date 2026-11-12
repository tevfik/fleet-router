package board_test

import (
	"fmt"
	"strings"
	"testing"

	"github.com/tevfik/fleet-router/internal/board"
	"github.com/tevfik/fleet-router/internal/server"
	"github.com/tevfik/fleet-router/internal/store"
)

func nodeStore(node string, files ...store.Entry) server.NodeStore {
	return server.NodeStore{Node: node, Store: &store.Inventory{Files: files}}
}

func TestStoreRowsMergeByContent(t *testing.T) {
	a := store.Entry{SHA256: "aaaaaaaa11", Name: "qwen", Size: 100 << 30, Complete: true}
	b := store.Entry{SHA256: "bbbbbbbb22", Name: "llama", Size: 5 << 30, Complete: true}
	partial := a
	partial.Complete = false
	rows := board.StoreRows([]server.NodeStore{nodeStore("dino", a, b), nodeStore("p1", partial), {Node: "old"}})
	if len(rows) != 2 || rows[0].Name != "llama" || rows[1].Name != "qwen" {
		t.Fatalf("%+v", rows)
	}
	if !rows[1].Has["dino"] || rows[1].Has["p1"] || rows[1].Has["old"] {
		t.Fatalf("a file that is not whole is not 'has': %+v", rows[1].Has)
	}
}

func TestStoreLines(t *testing.T) {
	a := store.Entry{SHA256: "aaaaaaaa11", Name: "qwen4exp-UD-Q4_K_XL", Size: 100 << 30, Complete: true}
	dino := nodeStore("dino", a)
	dino.Store.Hashing = &store.Progress{Path: "/m/big.gguf", Done: 25, Total: 100}
	p1 := nodeStore("papa")
	p1.Store.Cache = &store.CacheInfo{Dir: "/c/rpc", Bytes: 12 << 30, Entries: 400}
	p1.Store.Error = "x.gguf: not a GGUF file"
	old := server.NodeStore{Node: "victor", Err: "its fleet is too old to list model files"}
	out := strings.Join(board.StoreLines([]server.NodeStore{dino, p1, old}), "\n")
	for _, want := range []string{"FILE", "SHA256", "SIZE", "dino", "qwen4exp-UD-Q4_K_XL", "aaaaaaaa", "100.0 GiB", "✓", "—", "?",
		"worker caches: papa 12.0 GiB (400 entries)", "hashing on dino: big.gguf 25%", "papa: last add failed", "victor: its fleet is too old"} {
		if !strings.Contains(out, want) {
			t.Errorf("missing %q in\n%s", want, out)
		}
	}
	if out := strings.Join(board.StoreLines([]server.NodeStore{nodeStore("dino")}), "\n"); !strings.Contains(out, "no model files registered") {
		t.Fatal(out)
	}
}

func TestResolveFile(t *testing.T) {
	a := store.Entry{SHA256: "aaaa1111", Name: "Qwen", Size: 1, Complete: true}
	b := store.Entry{SHA256: "aaaa2222", Name: "llama", Size: 1, Complete: true}
	nodes := []server.NodeStore{nodeStore("dino", a, b)}
	if sha, err := board.ResolveFile(nodes, "qwen"); err != nil || sha != "aaaa1111" {
		t.Fatalf("by name, any case: %q %v", sha, err)
	}
	if sha, err := board.ResolveFile(nodes, "AAAA2"); err != nil || sha != "aaaa2222" {
		t.Fatalf("by prefix: %q %v", sha, err)
	}
	if _, err := board.ResolveFile(nodes, "aaaa"); err == nil || !strings.Contains(err.Error(), "matches 2") {
		t.Fatalf("%v", err)
	}
	if _, err := board.ResolveFile(nodes, "nothing"); err == nil {
		t.Fatal("no match")
	}
	if _, err := board.ResolveFile(nodes, "  "); err == nil {
		t.Fatal("empty")
	}
}

func TestTransferLines(t *testing.T) {
	n := nodeStore("p1")
	n.Store.Transfers = []store.Transfer{
		{ID: 3, Kind: "fill", What: "m: 12 tensors", Source: "dino", Done: 5 << 30, Total: 10 << 30, Running: true},
		{ID: 2, Kind: "pull", What: "big", Source: "dino", Err: "chunk 1 does not match the manifest"},
		{ID: 1, Kind: "pull", What: "ok", Source: "dino"},
	}
	out := strings.Join(board.TransferLines([]server.NodeStore{n, {Node: "old"}}), "\n")
	for _, want := range []string{"p1: fill m: 12 tensors from dino — 5.0 GiB of 10.0 GiB (50%)", "pull big from dino failed — chunk 1"} {
		if !strings.Contains(out, want) {
			t.Errorf("missing %q in\n%s", want, out)
		}
	}
	if strings.Contains(out, "ok") {
		t.Fatalf("a finished transfer is not listed:\n%s", out)
	}
	if !strings.Contains(strings.Join(board.StoreLines([]server.NodeStore{n}), "\n"), "p1: fill m: 12 tensors") {
		t.Fatal("the store table shows the transfers too")
	}
}

func TestStoreModelsGroupShards(t *testing.T) {
	sh := func(i int, sha string) store.Entry {
		return store.Entry{SHA256: sha, Name: fmt.Sprintf("big-0000%d-of-00003.gguf", i), Size: 10 << 30, Complete: true}
	}
	a := nodeStore("a", sh(1, "s1"), sh(2, "s2"), sh(3, "s3"), store.Entry{SHA256: "m1", Name: "mm.gguf", Size: 1 << 20, Complete: true, Kind: "mmproj"})
	b := nodeStore("b", sh(1, "s1"), sh(2, "s2")) // two of three
	ms := board.StoreModels(board.StoreRows([]server.NodeStore{a, b}))
	if len(ms) != 2 {
		t.Fatalf("a split model is one row: %+v", ms)
	}
	var big board.ModelRow
	for _, m := range ms {
		if m.Total == 3 {
			big = m
		}
	}
	if big.Name != "big" || len(big.Shards) != 3 || big.Count["a"] != 3 || big.Count["b"] != 2 || big.Size != 30<<30 {
		t.Fatalf("%+v", big)
	}
	out := strings.Join(board.StoreLines([]server.NodeStore{a, b}), "\n")
	for _, want := range []string{"big ×3", "2/3", "[mmproj] mm.gguf"} {
		if !strings.Contains(out, want) {
			t.Errorf("missing %q in\n%s", want, out)
		}
	}
	if strings.Contains(out, "00002-of") {
		t.Fatalf("shards are not listed one by one:\n%s", out)
	}
}

func TestResolveModelTakesEveryShard(t *testing.T) {
	mk := func(i int, sha string) store.Entry {
		return store.Entry{SHA256: sha, Name: fmt.Sprintf("big-0000%d-of-00003.gguf", i), Size: 1 << 20, Complete: true}
	}
	nodes := []server.NodeStore{nodeStore("a", mk(1, "aa1"), mk(2, "aa2"), mk(3, "aa3"))}
	for _, ref := range []string{"big", "big.gguf", "big-00002-of-00003.gguf", "aa2"} {
		rows, err := board.ResolveModel(nodes, ref)
		if err != nil || len(rows) != 3 {
			t.Fatalf("%q: %v %v", ref, rows, err)
		}
	}
	nodes = []server.NodeStore{nodeStore("a", mk(1, "aa1"), mk(3, "aa3"))}
	if _, err := board.ResolveModel(nodes, "big"); err == nil || !strings.Contains(err.Error(), "only 2 are registered") {
		t.Fatalf("a missing shard must stop the fetch: %v", err)
	}
}

func TestConsistencyLines(t *testing.T) {
	a := nodeStore("a", store.Entry{SHA256: "aaaaaaaa11", Name: "m.gguf", Complete: true}, store.Entry{SHA256: "gone0000", Name: "old.gguf", Complete: false})
	b := nodeStore("b", store.Entry{SHA256: "bbbbbbbb22", Name: "m.gguf", Complete: true})
	st := server.Status{Endpoints: []server.EpStatus{{Known: true, OK: false, Err: "refused"}}}
	st.Endpoints[0].Name, st.Endpoints[0].Node = "ep1", "a"
	out := strings.Join(board.ConsistencyLines(st, []server.NodeStore{a, b}, []server.Run{{Stale: "x"}}), "\n")
	for _, want := range []string{"endpoint ep1 (a) is down (refused)", "m.gguf stands for 2 different contents", "aaaaaaaa on a", "bbbbbbbb on b", "a: 1 registered file(s) are gone", "1 run(s) look dead"} {
		if !strings.Contains(out, want) {
			t.Errorf("missing %q in\n%s", want, out)
		}
	}
	if ok := board.ConsistencyLines(server.Status{}, nil, nil); !strings.Contains(ok[0], "nothing stale") {
		t.Fatal(ok)
	}
}

// The header's split.no / split.count group shards whose file names say nothing (the name is general.name).
func TestStoreModelsGroupByHeaderSplit(t *testing.T) {
	e := func(i int, sha string) store.Entry {
		return store.Entry{SHA256: sha, Name: "Big Model GSQ", Size: 1 << 30, Complete: true, Shard: i, ShardOf: 3}
	}
	n := nodeStore("a", e(1, "s1"), e(2, "s2"))
	ms := board.StoreModels(board.StoreRows([]server.NodeStore{n}))
	if len(ms) != 1 || ms[0].Total != 3 || len(ms[0].Shards) != 2 {
		t.Fatalf("%+v", ms)
	}
	if _, err := board.ResolveModel([]server.NodeStore{n}, "Big Model GSQ"); err == nil || !strings.Contains(err.Error(), "only 2 are registered") {
		t.Fatalf("%v", err)
	}
	out := strings.Join(board.StoreLines([]server.NodeStore{n}), "\n")
	if !strings.Contains(out, "×3") || !strings.Contains(out, "only 2 of 3 shards") {
		t.Fatal(out)
	}
}

func TestConsistencyLinesSameModelNameDifferentContent(t *testing.T) {
	mk := func(name, sha string) server.EpStatus {
		var e server.EpStatus
		e.Name, e.Model, e.ModelSHA256, e.Known, e.OK = name, "gemma4-12b", sha, true, true
		return e
	}
	st := server.Status{Endpoints: []server.EpStatus{mk("a", "aaaaaaaa11"), mk("b", "bbbbbbbb22"), mk("c", "aaaaaaaa11")}}
	out := strings.Join(board.ConsistencyLines(st, nil, nil), "\n")
	if !strings.Contains(out, "gemma4-12b is served with 2 different contents") || !strings.Contains(out, "aaaaaaaa: a,c") || !strings.Contains(out, "bbbbbbbb: b") {
		t.Fatalf("%s", out)
	}
	st.Endpoints = st.Endpoints[:1]
	st.Endpoints = append(st.Endpoints, mk("d", ""))
	if out := strings.Join(board.ConsistencyLines(st, nil, nil), "\n"); strings.Contains(out, "different contents") {
		t.Fatalf("an unknown hash proves nothing: %s", out)
	}
}
