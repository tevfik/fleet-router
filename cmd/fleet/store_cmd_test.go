package main

import (
	"bytes"
	"encoding/binary"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/tevfik/fleet-router/internal/config"
)

// writeGGUF makes a small valid GGUF file of a 4-block llama (each block one 64 KiB tensor).
func writeGGUF(t *testing.T, path string, fill ...byte) {
	t.Helper()
	var b bytes.Buffer
	u32 := func(v uint32) { binary.Write(&b, binary.LittleEndian, v) }
	u64 := func(v uint64) { binary.Write(&b, binary.LittleEndian, v) }
	str := func(s string) { u64(uint64(len(s))); b.WriteString(s) }
	b.WriteString("GGUF")
	u32(3)
	u64(4)
	u64(3)
	str("general.architecture")
	u32(8)
	str("llama")
	str("llama.block_count")
	u32(4)
	u32(4)
	str("llama.embedding_length")
	u32(4)
	u32(64)
	for i := 0; i < 4; i++ {
		str("blk." + string(rune('0'+i)) + ".w.weight")
		u32(1)
		u64(8)
		u32(0)
		u64(uint64(i) * 65536)
	}
	for b.Len()%32 != 0 {
		b.WriteByte(0)
	}
	data := make([]byte, 4*65536)
	for i := range data {
		if len(fill) > 0 {
			data[i] = fill[0]
		}
	}
	b.Write(data)
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, b.Bytes(), 0o644); err != nil {
		t.Fatal(err)
	}
}

func TestStoreCommands(t *testing.T) {
	dir := isolate(t)
	t.Setenv("LLAMA_CACHE", "")
	d := startDaemon(t, &config.Config{Name: "alpha"}, true)
	_ = d
	models := filepath.Join(dir, "models")
	writeGGUF(t, filepath.Join(models, "tiny.gguf"))
	writeGGUF(t, filepath.Join(models, "sub", "nested.gguf"), 7) // other content: another file
	os.WriteFile(filepath.Join(models, "notes.txt"), []byte("not a model"), 0o644)

	out := runOK(t, func() error { return cmdStore(nil) })
	contains(t, out, "FILE", "no model files registered")

	out, errOut, err := run(t, func() error { return cmdStore([]string{"add", models}) })
	if err != nil {
		t.Fatalf("%v\n%s", err, errOut)
	}
	contains(t, out, "hashing 2 file(s)", "tiny", "nested", "✓")
	if strings.Contains(out, "notes") {
		t.Fatalf("only .gguf files:\n%s", out)
	}
	_, _, err = run(t, func() error { return cmdStore([]string{"add"}) })
	wantErr(t, err, "usage: fleet store add")
	_, _, err = run(t, func() error { return cmdStore([]string{"add", filepath.Join(dir, "nowhere.gguf")}) })
	wantErr(t, err, "nowhere.gguf")

	out = runOK(t, func() error { return cmdStore([]string{"info", "tiny.gguf"}) })
	contains(t, out, "tiny", "arch     llama · 4 blocks", "header says it can: chat")
	_, _, err = run(t, func() error { return cmdStore([]string{"info", "ghost"}) })
	wantErr(t, err, "no machine has a file")
	out = runOK(t, func() error { return cmdStore([]string{"prune"}) })
	contains(t, out, "nothing to forget")
	out = runOK(t, func() error { return cmdStore([]string{"status"}) })
	contains(t, out, "last add failed: nowhere.gguf", "2 file(s) registered", "no worker cache")
	runOK(t, func() error { return cmdStore([]string{"add", filepath.Join(models, "tiny.gguf")}) }) // known: nothing to hash, and the failure is forgotten
	out = runOK(t, func() error { return cmdStore([]string{"status"}) })
	contains(t, out, "idle")
	cache := filepath.Join(dir, "cache", "rpc")
	os.MkdirAll(cache, 0o755)
	os.WriteFile(filepath.Join(cache, "00112233445566ff"), make([]byte, 1000), 0o644)
	t.Setenv("LLAMA_CACHE", filepath.Dir(cache)) // the daemon runs in this process: it reads the variable
	time.Sleep(10 * time.Millisecond)
	out = runOK(t, func() error { return cmdStore([]string{"status"}) })
	contains(t, out, "1 entries")

	out = runOK(t, func() error { return cmdStore([]string{"rm", "tiny.gguf"}) })
	contains(t, out, "forgot tiny", "untouched")
	if _, err := os.Stat(filepath.Join(models, "tiny.gguf")); err != nil {
		t.Fatal("rm must not delete the file")
	}
	_, _, err = run(t, func() error { return cmdStore([]string{"rm", "ghost"}) })
	wantErr(t, err, "no file in the store matches")
	_, _, err = run(t, func() error { return cmdStore([]string{"rm"}) })
	wantErr(t, err, "usage: fleet store rm")
	_, _, err = run(t, func() error { return cmdStore([]string{"frobnicate"}) })
	wantErr(t, err, "usage: fleet store")
	out = runOK(t, func() error { return cmdStore([]string{"ls"}) })
	contains(t, out, "nested")
	if strings.Contains(out, "tiny") {
		t.Fatalf("forgotten:\n%s", out)
	}
}

func TestPlanCommand_KnownFilePricesTransfers(t *testing.T) {
	dir := isolate(t)
	t.Setenv("LLAMA_CACHE", "")
	startDaemon(t, &config.Config{Name: "alpha"}, true)
	p := filepath.Join(dir, "models", "tiny.gguf")
	writeGGUF(t, p)

	out := runOK(t, func() error { return cmdPlan([]string{p, "--ctx", "256", "--np", "2"}) })
	contains(t, out, "tiny", "weights", "256 tokens", "left out", "to move: unknown")
	_, _, err := run(t, func() error { return cmdPlan([]string{}) })
	wantErr(t, err, "usage: fleet plan")
	_, _, err = run(t, func() error { return cmdPlan([]string{filepath.Join(dir, "none.gguf")}) })
	wantErr(t, err, "none.gguf")
	_, _, err = run(t, func() error { return cmdPlan([]string{p, "--rtt", "nonsense"}) })
	wantErr(t, err, "--rtt")

	runOK(t, func() error { return cmdStore([]string{"add", p}) })
	out = runOK(t, func() error { return cmdPlan([]string{p, "--kv-blocks", "2", "--boundary-bytes", "1000", "--json"}) })
	contains(t, out, `"known": true`, `"model": "tiny"`)
	out = runOK(t, func() error { return cmdPlan([]string{p}) })
	if strings.Contains(out, "to move: unknown") {
		t.Fatalf("a registered file is known:\n%s", out)
	}
}

func TestPlanCommand_MasterRemoteReportsRTTOrFallsBack(t *testing.T) {
	dir := isolate(t)
	t.Setenv("LLAMA_CACHE", "")
	startDaemon(t, &config.Config{Name: "alpha"}, true)
	p := filepath.Join(dir, "models", "tiny.gguf")
	writeGGUF(t, p)

	// When master is remote and reports no RTT, stderr explains the fallback
	_, stderr, err := run(t, func() error { return cmdPlan([]string{p, "--master", "remote", "--json"}) })
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(stderr, "rtt: master remote is not this machine") || !strings.Contains(stderr, "not master<->worker, using the pings") {
		t.Fatalf("expected fallback explanation in stderr, got: %s", stderr)
	}
}

func TestRouteCommand(t *testing.T) {
	isolate(t)
	be := newUpstream(t)
	cfg := baseConfig(be)
	cfg.Endpoints[0].TPS, cfg.Endpoints[1].TPS = 30, 90
	cfg.Roles = map[string]config.Role{"scout": {Preferred: "loc-tiny", Fallback: []string{"loc-gemma"}, Strategy: "cost"}}
	d := startDaemon(t, cfg, true)
	d.waitHealthy(t, "loc-tiny", "loc-gemma")
	out := runOK(t, func() error { return cmdRoute([]string{"scout", "--in", "4000", "--out", "100"}) })
	contains(t, out, "role scout", "strategy cost", "4000 tokens in / 100 out", "loc-gemma", "loc-tiny", "benchmark speed")
	if strings.Index(out, "loc-gemma") > strings.Index(out, "loc-tiny") {
		t.Fatalf("the faster model comes first:\n%s", out)
	}
	_, _, err := run(t, func() error { return cmdRoute(nil) })
	wantErr(t, err, "usage: fleet route")
}

func TestStorePullAndFillCommands(t *testing.T) {
	dir := isolate(t)
	t.Setenv("LLAMA_CACHE", "")
	startDaemon(t, &config.Config{Name: "alpha"}, true)
	p := filepath.Join(dir, "models", "tiny.gguf")
	writeGGUF(t, p)
	runOK(t, func() error { return cmdStore([]string{"add", p}) })

	// the only machine is this one and it has the file: nothing to fetch, and the command says so
	out := runOK(t, func() error { return cmdStore([]string{"pull", "tiny"}) })
	contains(t, out, "already has every file")
	_, _, err := run(t, func() error { return cmdStore([]string{"pull", "ghost", "--node", "x"}) })
	wantErr(t, err, "no machine has a file matching")
	_, _, err = run(t, func() error { return cmdStore([]string{"pull", "ghost"}) })
	wantErr(t, err, "no machine has a file matching")
	_, _, err = run(t, func() error { return cmdStore([]string{"pull"}) })
	wantErr(t, err, "usage: fleet store pull")
	_, _, err = run(t, func() error { return cmdStore([]string{"fill"}) })
	wantErr(t, err, "usage: fleet store fill")
	_, _, err = run(t, func() error { return cmdStore([]string{"fill", "tiny", "--node", "x"}) })
	wantErr(t, err, "fill needs --blocks")
	_, _, err = run(t, func() error { return cmdStore([]string{"fill", "tiny", "--blocks", "0-1"}) })
	wantErr(t, err, "no worker cache directory")
	_, _, err = run(t, func() error { return cmdStore([]string{"pull", "tiny", "--node", "ghost"}) })
	wantErr(t, err, "no route")

	out = runOK(t, func() error { return cmdStore([]string{"status"}) })
	contains(t, out, "1 file(s) registered")

	// planning with --fetch: no benchmark here, so there is no candidate to make runnable
	_, _, err = run(t, func() error { return cmdPlan([]string{p, "--fetch"}) })
	wantErr(t, err, "no number 1")
}
