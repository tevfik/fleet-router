package main

import (
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"

	"github.com/tevfik/fleet-router/internal/board"
	"github.com/tevfik/fleet-router/internal/client"
	"github.com/tevfik/fleet-router/internal/config"
	"github.com/tevfik/fleet-router/internal/eval"
	"github.com/tevfik/fleet-router/internal/plan"
	"github.com/tevfik/fleet-router/internal/profile"
	"github.com/tevfik/fleet-router/internal/server"
	"github.com/tevfik/fleet-router/internal/store"
)

func TestCovKey(t *testing.T) {
	isolate(t)

	// usage error
	_, _, err := run(t, func() error { return cmdKey(nil) })
	if err == nil || !strings.Contains(err.Error(), "usage: fleet key") {
		t.Fatalf("expected usage error, got: %v", err)
	}

	_, _, err = run(t, func() error { return cmdKey([]string{"unknown"}) })
	if err == nil || !strings.Contains(err.Error(), "usage: fleet key") {
		t.Fatalf("expected usage error, got: %v", err)
	}

	// set without argument
	_, _, err = run(t, func() error { return cmdKey([]string{"set"}) })
	if err == nil || !strings.Contains(err.Error(), "usage: fleet key set <key>") {
		t.Fatalf("expected set usage error, got: %v", err)
	}

	// set empty key
	_, _, err = run(t, func() error { return cmdKey([]string{"set", "   "}) })
	if err == nil || !strings.Contains(err.Error(), "key cannot be empty") {
		t.Fatalf("expected empty key error, got: %v", err)
	}

	// show when no key configured
	out, _, err := run(t, func() error { return cmdKey([]string{"show"}) })
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, "no hub API key configured") {
		t.Fatalf("expected 'no hub API key configured', got: %s", out)
	}

	// set a valid key
	testKey := "flt_test_key_cov_12345"
	out, _, err = run(t, func() error { return cmdKey([]string{"set", testKey}) })
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, "hub API key saved to") {
		t.Fatalf("expected save confirmation, got: %s", out)
	}

	// set when FLEET_API_KEY is different in env
	t.Setenv("FLEET_API_KEY", "different_env_key")
	out, _, err = run(t, func() error { return cmdKey([]string{"set", testKey}) })
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, "wins over the saved one") {
		t.Fatalf("expected env wins warning, got: %s", out)
	}
	t.Setenv("FLEET_API_KEY", "")

	// show masked
	out, _, err = run(t, func() error { return cmdKey([]string{"show"}) })
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, "hub API key:") || strings.Contains(out, testKey) {
		t.Fatalf("expected masked key output, got: %s", out)
	}

	// show --reveal
	out, _, err = run(t, func() error { return cmdKey([]string{"show", "--reveal"}) })
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, testKey) {
		t.Fatalf("expected revealed key %q, got: %s", testKey, out)
	}

	// generate
	out, _, err = run(t, func() error { return cmdKey([]string{"generate"}) })
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, "new hub API key generated and saved") {
		t.Fatalf("expected generate confirmation, got: %s", out)
	}

	// rm
	out, _, err = run(t, func() error { return cmdKey([]string{"rm"}) })
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, "hub API key removed") {
		t.Fatalf("expected remove confirmation, got: %s", out)
	}

	// rm when FLEET_API_KEY is in env
	t.Setenv("FLEET_API_KEY", "env_key")
	out, _, err = run(t, func() error { return cmdKey([]string{"rm"}) })
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, "is still set in this environment") {
		t.Fatalf("expected still set warning, got: %s", out)
	}
	t.Setenv("FLEET_API_KEY", "")
}

func TestCovConfig(t *testing.T) {
	isolate(t)

	// history empty
	out, _, err := run(t, func() error { return cmdConfig([]string{"history"}) })
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, "no earlier configurations kept yet") {
		t.Fatalf("expected no earlier configurations, got: %s", out)
	}

	// usage error on unknown
	_, _, err = run(t, func() error { return cmdConfig([]string{"unknown"}) })
	if err == nil || !strings.Contains(err.Error(), "usage: fleet config") {
		t.Fatalf("expected usage error, got: %v", err)
	}

	// config key delegated to cmdKey
	out, _, err = run(t, func() error { return cmdConfig([]string{"key", "show"}) })
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, "no hub API key configured") {
		t.Fatalf("expected key delegation, got: %s", out)
	}

	// node list empty
	cfg := &config.Config{Listen: "127.0.0.1:8090"}
	if err := cfg.Save(config.Path()); err != nil {
		t.Fatal(err)
	}
	out, _, err = run(t, func() error { return cmdConfig([]string{"node", "list"}) })
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, "no nodes declared") {
		t.Fatalf("expected no nodes declared, got: %s", out)
	}

	// node set usage error
	_, _, err = run(t, func() error { return cmdConfig([]string{"node", "set"}) })
	if err == nil || !strings.Contains(err.Error(), "usage: fleet config node") {
		t.Fatalf("expected node set usage error, got: %v", err)
	}

	// node set invalid key-value
	_, _, err = run(t, func() error { return cmdConfig([]string{"node", "set", "n1", "notkv"}) })
	if err == nil || !strings.Contains(err.Error(), "key=value") {
		t.Fatalf("expected key=value error, got: %v", err)
	}

	// node set unknown key
	_, _, err = run(t, func() error { return cmdConfig([]string{"node", "set", "n1", "badkey=1"}) })
	if err == nil || !strings.Contains(err.Error(), "unknown key") {
		t.Fatalf("expected unknown key error, got: %v", err)
	}

	// node set invalid float
	_, _, err = run(t, func() error { return cmdConfig([]string{"node", "set", "n1", "mem_gib=notnum"}) })
	if err == nil {
		t.Fatalf("expected parse float error, got nil")
	}

	// node set valid
	out, _, err = run(t, func() error {
		return cmdConfig([]string{"node", "set", "n1", "addr=10.0.1.50", "mem_gib=64", "weight_gbps=10", "expert_gbps=5", "notes=dino"})
	})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, "nodes: section saved") {
		t.Fatalf("expected saved confirmation, got: %s", out)
	}

	// node list non-empty
	out, _, err = run(t, func() error { return cmdConfig([]string{"node", "list"}) })
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, "n1") || !strings.Contains(out, "10.0.1.50") {
		t.Fatalf("expected n1 in list, got: %s", out)
	}

	// node rm non-existent
	_, _, err = run(t, func() error { return cmdConfig([]string{"node", "rm", "nonexistent"}) })
	if err == nil || !strings.Contains(err.Error(), "no node \"nonexistent\"") {
		t.Fatalf("expected no node error, got: %v", err)
	}

	// node rm missing argument
	_, _, err = run(t, func() error { return cmdConfig([]string{"node", "rm"}) })
	if err == nil || !strings.Contains(err.Error(), "usage: fleet config node") {
		t.Fatalf("expected node rm usage error, got: %v", err)
	}

	// node rm valid
	out, _, err = run(t, func() error { return cmdConfig([]string{"node", "rm", "n1"}) })
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, "nodes: section saved") {
		t.Fatalf("expected saved confirmation, got: %s", out)
	}

	// test undo with snapshots:
	out, _, err = run(t, func() error { return cmdConfig([]string{"undo"}) })
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, "restored") {
		t.Fatalf("expected restored confirmation, got: %s", out)
	}
}

func TestCovEconomy(t *testing.T) {
	isolate(t)

	// unknown command
	_, _, err := run(t, func() error { return cmdEconomy([]string{"invalid"}) })
	if err == nil || !strings.Contains(err.Error(), "unknown economy command") {
		t.Fatalf("expected unknown command error, got: %v", err)
	}

	// status
	out, _, err := run(t, func() error { return cmdEconomy([]string{"status"}) })
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, "TOKEN ECONOMY SETTINGS & STATUS") {
		t.Fatalf("expected economy status header, got: %s", out)
	}

	// enable
	out, _, err = run(t, func() error { return cmdEconomy([]string{"on"}) })
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, "Token economy ENABLED") {
		t.Fatalf("expected enabled, got: %s", out)
	}

	// disable
	out, _, err = run(t, func() error { return cmdEconomy([]string{"off"}) })
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, "Token economy DISABLED") {
		t.Fatalf("expected disabled, got: %s", out)
	}

	// queuing missing arg
	_, _, err = run(t, func() error { return cmdEconomy([]string{"queuing"}) })
	if err == nil || !strings.Contains(err.Error(), "usage: fleet economy queuing") {
		t.Fatalf("expected queuing usage error, got: %v", err)
	}

	// queuing bad arg
	_, _, err = run(t, func() error { return cmdEconomy([]string{"queuing", "maybe"}) })
	if err == nil || !strings.Contains(err.Error(), "usage: fleet economy queuing") {
		t.Fatalf("expected queuing usage error, got: %v", err)
	}

	// queuing on
	out, _, err = run(t, func() error { return cmdEconomy([]string{"queuing", "on"}) })
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, "Fair Queuing & QoS Priority Boost ENABLED") {
		t.Fatalf("expected queuing enabled, got: %s", out)
	}

	// queuing off
	out, _, err = run(t, func() error { return cmdEconomy([]string{"queuing", "off"}) })
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, "Fair Queuing & QoS Priority Boost DISABLED") {
		t.Fatalf("expected queuing disabled, got: %s", out)
	}
}

func TestCovImport(t *testing.T) {
	isolate(t)

	// missing args
	_, _, err := run(t, func() error { return cmdImport(nil) })
	if err == nil || !strings.Contains(err.Error(), "usage: fleet import") {
		t.Fatalf("expected usage error, got: %v", err)
	}

	// non-existent file
	_, _, err = run(t, func() error { return cmdImport([]string{"nonexistent.yaml"}) })
	if err == nil {
		t.Fatalf("expected read error on nonexistent file, got nil")
	}

	// create a valid old fleet.yaml
	tmpDir := t.TempDir()
	oldYaml := filepath.Join(tmpDir, "old-fleet.yaml")
	content := `endpoints:
  - name: test-ep
    url: http://127.0.0.1:8080/v1
    model: test-model
roles:
  coder:
    preferred: test-ep
`
	if err := os.WriteFile(oldYaml, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}

	// dry run import
	out, _, err := run(t, func() error { return cmdImport([]string{oldYaml}) })
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, "use --write to write") {
		t.Fatalf("expected --write hint, got: %s", out)
	}

	// import --write when config exists
	cfgPath := config.Path()
	_ = os.WriteFile(cfgPath, []byte("listen: 127.0.0.1:8090\n"), 0o644)
	_, _, err = run(t, func() error { return cmdImport([]string{oldYaml, "--write"}) })
	if err == nil || !strings.Contains(err.Error(), "already exists") {
		t.Fatalf("expected already exists error, got: %v", err)
	}

	// remove config and write
	_ = os.Remove(cfgPath)
	out, _, err = run(t, func() error { return cmdImport([]string{oldYaml, "--write"}) })
	if err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(cfgPath); err != nil {
		t.Fatalf("expected config to be written, stat error: %v", err)
	}
}

func TestCovStreamCmd(t *testing.T) {
	isolate(t)

	// portList String and Set
	var pl portList
	if pl.String() != "" {
		t.Fatalf("expected empty string for empty portList, got: %s", pl.String())
	}
	if err := pl.Set("8080,8081"); err != nil {
		t.Fatal(err)
	}
	if pl.String() != "8080,8081" {
		t.Fatalf("expected '8080,8081', got: %s", pl.String())
	}
	if err := pl.Set("badport"); err == nil {
		t.Fatal("expected error on bad port")
	}
	if err := pl.Set("70000"); err == nil {
		t.Fatal("expected error on out of range port")
	}

	// cmdStreamLs when empty
	out, _, err := run(t, func() error { return cmdStream([]string{"ls"}) })
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, "no stream connects running") {
		t.Fatalf("expected no streams running, got: %s", out)
	}

	// cmdStream usage error
	_, _, err = run(t, func() error { return cmdStream(nil) })
	if err == nil || !strings.Contains(err.Error(), "usage: fleet stream") {
		t.Fatalf("expected stream usage error, got: %v", err)
	}

	// cmdStream connect missing arguments
	_, _, err = run(t, func() error { return cmdStream([]string{"connect"}) })
	if err == nil || !strings.Contains(err.Error(), "usage: fleet stream") {
		t.Fatalf("expected connect usage error, got: %v", err)
	}

	// cmdStream connect bad port
	_, _, err = run(t, func() error { return cmdStream([]string{"connect", "node1", "notaport"}) })
	if err == nil || !strings.Contains(err.Error(), "bad remote port") {
		t.Fatalf("expected bad remote port error, got: %v", err)
	}

	// cmdStream connect missing API key
	_, _, err = run(t, func() error { return cmdStream([]string{"connect", "node1", "8080"}) })
	if err == nil || !strings.Contains(err.Error(), "no API key for the hub") {
		t.Fatalf("expected no API key error, got: %v", err)
	}

	// writeStreamPID, runningStreams, and cmdStreamLs with active stream
	si := streamInfo{
		PID:    os.Getpid(),
		Node:   "test-node",
		Port:   8080,
		Listen: "127.0.0.1:8080",
		Hub:    "https://hub.example.com",
	}
	pf, err := writeStreamPID(si)
	if err != nil {
		t.Fatal(err)
	}
	defer os.Remove(pf)

	rs := runningStreams()
	if len(rs) == 0 {
		t.Fatal("expected running stream to be found")
	}

	out, _, err = run(t, func() error { return cmdStreamLs() })
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, "test-node:8080") {
		t.Fatalf("expected test-node:8080 in stream ls, got: %s", out)
	}

	// pidAlive helper
	if !pidAlive(os.Getpid()) {
		t.Fatal("expected current pid to be alive")
	}
	if pidAlive(-1) {
		t.Fatal("expected negative pid to not be alive")
	}

	// listenError helper
	errInUse := listenError("127.0.0.1:8080", syscall.EADDRINUSE)
	if !strings.Contains(errInUse.Error(), "already served by fleet stream connect") {
		t.Fatalf("expected served by fleet stream connect error, got: %v", errInUse)
	}

	errOther := listenError("127.0.0.1:9999", syscall.EADDRINUSE)
	if !strings.Contains(errOther.Error(), "in use by another program") {
		t.Fatalf("expected in use by another program error, got: %v", errOther)
	}

	errGeneric := listenError("127.0.0.1:8080", syscall.EACCES)
	if !strings.Contains(errGeneric.Error(), "choose another with --listen") {
		t.Fatalf("expected choose another with --listen, got: %v", errGeneric)
	}
}

func TestCovRingAndChain(t *testing.T) {
	isolate(t)
	t.Setenv("PATH", "/usr/bin:/bin")

	// cmdRing usage error
	_, _, err := run(t, func() error { return cmdRing(nil) })
	if err == nil || !strings.Contains(err.Error(), "usage: fleet ring") {
		t.Fatalf("expected ring usage error, got: %v", err)
	}

	// cmdRing version
	out, _, err := run(t, func() error { return cmdRing([]string{"version"}) })
	if err != nil {
		t.Fatal(err)
	}
	if out == "" {
		t.Fatal("expected version output from cmdRing version")
	}

	// cmdChain usage error
	_, _, err = run(t, func() error { return cmdChain(nil) })
	if err == nil || !strings.Contains(err.Error(), "usage: fleet chain") {
		t.Fatalf("expected chain usage error, got: %v", err)
	}

	// cmdDFlashHead usage errors
	_, _, err = run(t, func() error { return cmdChain([]string{"dflash-head"}) })
	if err == nil || !strings.Contains(err.Error(), "usage: fleet chain dflash-head") {
		t.Fatalf("expected dflash-head usage error, got: %v", err)
	}

	_, _, err = run(t, func() error { return cmdChain([]string{"dflash-head", "a", "b"}) })
	if err == nil || !strings.Contains(err.Error(), "usage: fleet chain dflash-head") {
		t.Fatalf("expected dflash-head usage error, got: %v", err)
	}

	// stageInvocation helper
	args := stageInvocation("fleet-engine-stage", []string{"serve", "--port", "8080"})
	if len(args) == 0 || args[0] != "chain" {
		t.Fatalf("expected prepended 'chain', got: %v", args)
	}

	args2 := stageInvocation("fleet", []string{"serve"})
	if len(args2) == 0 || args2[0] != "serve" {
		t.Fatalf("expected unchanged args, got: %v", args2)
	}
}

func TestCovShardsAndTransferHelpers(t *testing.T) {
	isolate(t)

	// shardNames helper
	names := shardNames([]board.StoreRow{{Name: "row1"}, {Name: "row2"}})
	if len(names) != 2 || names[0] != "row1" || names[1] != "row2" {
		t.Fatalf("unexpected shardNames result: %v", names)
	}

	// orDefault helper
	if orDefault("", "def") != "def" {
		t.Fatal("expected fallback")
	}
	if orDefault("given", "def") != "given" {
		t.Fatal("expected given")
	}

	// lastTransferID helper
	lastID := lastTransferID([]server.NodeStore{
		{Node: "n1", Store: &store.Inventory{Transfers: []store.Transfer{{ID: 5}, {ID: 12}}}},
		{Node: "n2", Store: &store.Inventory{Transfers: []store.Transfer{{ID: 9}}}},
	}, "n1")
	if lastID != 12 {
		t.Fatalf("expected transfer ID 12, got: %d", lastID)
	}

	// checkLine helper
	cl := client.New("127.0.0.1:1")
	if checkLine(cl, "n1", "n2", "sha", 100, false) != "" {
		t.Fatal("expected empty checkLine for small transfer")
	}

	// cmdShards usage error
	_, _, err := run(t, func() error { return cmdShards(nil) })
	if err == nil || !strings.Contains(err.Error(), "usage: fleet shards") {
		t.Fatalf("expected shards usage error, got: %v", err)
	}

	// cmdShards on valid gguf file
	tmpDir := t.TempDir()
	ggufPath := filepath.Join(tmpDir, "model.gguf")
	writeGGUF(t, ggufPath)

	out, _, err := run(t, func() error { return cmdShards([]string{ggufPath, "--no-hash"}) })
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, "llama") {
		t.Fatalf("expected shards json output, got: %s", out)
	}

	// cmdShards with --need
	out, _, err = run(t, func() error { return cmdShards([]string{ggufPath, "--need", "0-2"}) })
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, "blocks 0-2:") {
		t.Fatalf("expected blocks 0-2 output, got: %s", out)
	}

	// cmdShards with bad --need
	_, _, err = run(t, func() error { return cmdShards([]string{ggufPath, "--need", "notarange"}) })
	if err == nil || !strings.Contains(err.Error(), "is not first-last") {
		t.Fatalf("expected bad --need error, got: %v", err)
	}

	// cmdShards with --out
	outFile := filepath.Join(tmpDir, "out-shards.json")
	out, _, err = run(t, func() error { return cmdShards([]string{ggufPath, "--out", outFile, "--no-hash"}) })
	if err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(outFile); err != nil {
		t.Fatalf("expected shards output file to exist: %v", err)
	}
}

func TestCovVision(t *testing.T) {
	// help / no args
	out, _, err := run(t, func() error { return cmdVision(nil) })
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, "usage: fleet vision") {
		t.Fatalf("expected vision usage, got: %s", out)
	}

	out, _, err = run(t, func() error { return cmdVision([]string{"help"}) })
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, "usage: fleet vision") {
		t.Fatalf("expected vision usage, got: %s", out)
	}

	// unknown command
	_, _, err = run(t, func() error { return cmdVision([]string{"unknown"}) })
	if err == nil || !strings.Contains(err.Error(), "unknown vision command") {
		t.Fatalf("expected unknown vision command error, got: %v", err)
	}

	// status unreachable
	out, _, err = run(t, func() error { return cmdVision([]string{"status", "--url", "http://127.0.0.1:1"}) })
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, "is unreachable") {
		t.Fatalf("expected unreachable message, got: %s", out)
	}

	// describe missing image arg
	_, _, err = run(t, func() error { return cmdVision([]string{"describe"}) })
	if err == nil || !strings.Contains(err.Error(), "usage: fleet vision describe") {
		t.Fatalf("expected describe usage error, got: %v", err)
	}

	// describe non-existent file
	_, _, err = run(t, func() error { return cmdVision([]string{"describe", "nonexistent.png"}) })
	if err == nil {
		t.Fatal("expected file read error on nonexistent image")
	}

	// describe with url (unreachable)
	_, _, err = run(t, func() error {
		return cmdVision([]string{"describe", "http://example.com/test.jpg", "--url", "http://127.0.0.1:1"})
	})
	if err == nil {
		t.Fatal("expected describe network error")
	}
}

func TestCovWatchdog(t *testing.T) {
	isolate(t)

	// with invalid YAML config -> error
	_ = os.WriteFile(config.Path(), []byte("listen: [invalid yaml"), 0o644)
	_, _, err := run(t, func() error { return cmdWatchdog([]string{"--once"}) })
	if err == nil {
		t.Fatal("expected error on invalid config yaml")
	}

	// with valid config
	cfg := &config.Config{Listen: "127.0.0.1:8090"}
	if err := cfg.Save(config.Path()); err != nil {
		t.Fatal(err)
	}

	out, _, err := run(t, func() error { return cmdWatchdog([]string{"--once"}) })
	if err != nil {
		t.Fatalf("unexpected watchdog error: %v (output: %s)", err, out)
	}
}

func TestCovEval(t *testing.T) {
	isolate(t)

	// eval --show when no eval file exists
	out, _, err := run(t, func() error { return cmdEval([]string{"--show"}) })
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, "no scores yet") {
		t.Fatalf("expected 'no scores yet', got: %s", out)
	}

	// printEval with populated mock results
	res := []eval.Result{
		{
			Target: "test-model",
			Items: []eval.ItemResult{
				{ID: "prompt-1", Pass: true},
				{ID: "prompt-2", Pass: false, Err: "failed response format"},
			},
		},
	}
	out, _, err = run(t, func() error {
		printEval(res)
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, "test-model") || !strings.Contains(out, "failed response format") {
		t.Fatalf("expected model and error in printEval output, got: %s", out)
	}

	// eval with daemon running
	cfg := &config.Config{Listen: "127.0.0.1:8090"}
	d := startDaemon(t, cfg, true)
	_ = d

	// missing target usage
	_, _, err = run(t, func() error { return cmdEval(nil) })
	if err == nil || !strings.Contains(err.Error(), "usage: fleet eval") {
		t.Fatalf("expected eval usage error, got: %v", err)
	}

	// eval nonexistent role
	_, _, err = run(t, func() error { return cmdEval([]string{"--role", "nonexistent"}) })
	if err == nil || !strings.Contains(err.Error(), "no such role") {
		t.Fatalf("expected no such role error, got: %v", err)
	}
}

func TestCovDaemonCommands(t *testing.T) {
	isolate(t)
	be := newUpstream(t)
	cfg := baseConfig(be)
	d := startDaemon(t, cfg, true)
	d.waitHealthy(t, "loc-tiny", "loc-gemma")

	// cmdAPIKey tests
	_, _, err := run(t, func() error { return cmdAPIKey([]string{"unknown"}) })
	if err == nil || !strings.Contains(err.Error(), "unknown apikey subcommand") {
		t.Fatalf("expected unknown subcommand error, got: %v", err)
	}

	out, _, err := run(t, func() error { return cmdAPIKey([]string{"list"}) })
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, "No API keys found") {
		t.Fatalf("expected No API keys found, got: %s", out)
	}

	_, _, err = run(t, func() error { return cmdAPIKey([]string{"create"}) })
	if err == nil || !strings.Contains(err.Error(), "usage: fleet apikey create") {
		t.Fatalf("expected create usage error, got: %v", err)
	}

	out, _, err = run(t, func() error {
		return cmdAPIKey([]string{"create", "key1", "--qos", "ultra-low-latency", "--role", "scout", "--models", "tiny,gemma"})
	})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, "Created API key for key1") {
		t.Fatalf("expected created key1, got: %s", out)
	}

	out, _, err = run(t, func() error {
		return cmdAPIKey([]string{"create", "key2"})
	})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, "Created API key for key2") {
		t.Fatalf("expected created key2, got: %s", out)
	}

	out, _, err = run(t, func() error { return cmdAPIKey([]string{"list"}) })
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, "key1") || !strings.Contains(out, "key2") {
		t.Fatalf("expected keys listed, got: %s", out)
	}

	_, _, err = run(t, func() error { return cmdAPIKey([]string{"rm"}) })
	if err == nil || !strings.Contains(err.Error(), "usage: fleet apikey rm") {
		t.Fatalf("expected rm usage error, got: %v", err)
	}

	keys, _ := d.cl.ListAPIKeys()
	if len(keys) > 0 {
		out, _, err = run(t, func() error { return cmdAPIKey([]string{"rm", keys[0].Key}) })
		if err != nil {
			t.Fatal(err)
		}
		if !strings.Contains(out, "Deleted API key") {
			t.Fatalf("expected deleted confirmation, got: %s", out)
		}
	}

	// cmdLeave
	out, _, err = run(t, func() error { return cmdLeave(nil) })
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, "left the line") {
		t.Fatalf("expected left the line, got: %s", out)
	}

	out, _, err = run(t, func() error { return cmdLeave([]string{"--back"}) })
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, "back in the line") {
		t.Fatalf("expected back in the line, got: %s", out)
	}

	// cmdJoin
	_, _, err = run(t, func() error { return cmdJoin([]string{"--insecure", "--secure"}) })
	if err == nil || !strings.Contains(err.Error(), "--insecure and --secure exclude each other") {
		t.Fatalf("expected conflict error, got: %v", err)
	}

	_, _, err = run(t, func() error {
		return cmdJoin([]string{"--stream-port", "8080", "--add-stream-port", "8081"})
	})
	if err == nil || !strings.Contains(err.Error(), "use either --stream-port") {
		t.Fatalf("expected port conflict error, got: %v", err)
	}

	out, _, err = run(t, func() error { return cmdJoin([]string{"--leave"}) })
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, "Disconnected from Fleet Hub") {
		t.Fatalf("expected disconnected, got: %s", out)
	}

	// cmdRun and formatRunsLost
	if msg := formatRunsLost(nil, false); msg != "no stale runs" {
		t.Fatalf("expected 'no stale runs', got: %s", msg)
	}
	if msg := formatRunsLost([]server.Run{{ID: "r1", Model: "m1", Stale: "silent"}}, true); !strings.Contains(msg, "would be marked lost") {
		t.Fatalf("expected 'would be marked lost', got: %s", msg)
	}
	if msg := formatRunsLost([]server.Run{{ID: "r1", Model: "m1", Stale: "silent"}}, false); !strings.Contains(msg, "marked lost") {
		t.Fatalf("expected 'marked lost', got: %s", msg)
	}

	out, _, err = run(t, func() error { return cmdRun([]string{"list"}) })
	if err != nil {
		t.Fatal(err)
	}

	_, _, err = run(t, func() error { return cmdRun([]string{"show", "ghost-run-id"}) })
	if err == nil || !strings.Contains(err.Error(), "no such run") {
		t.Fatalf("expected no such run error, got: %v", err)
	}

	for _, sub := range []string{"bind", "stop", "end"} {
		_, _, err = run(t, func() error { return cmdRun([]string{sub}) })
		if err == nil || !strings.Contains(err.Error(), "usage: fleet run") {
			t.Fatalf("expected usage error on run %s, got: %v", sub, err)
		}
	}

	_, _, err = run(t, func() error { return cmdRun([]string{"prune", "extra"}) })
	if err == nil || !strings.Contains(err.Error(), "usage: fleet run prune") {
		t.Fatalf("expected prune usage error, got: %v", err)
	}

	out, _, err = run(t, func() error { return cmdRun([]string{"prune", "--dry-run"}) })
	if err != nil {
		t.Fatal(err)
	}

	out, _, err = run(t, func() error { return cmdRun([]string{"prune", "--stale"}) })
	if err != nil {
		t.Fatal(err)
	}

	_, _, err = run(t, func() error { return cmdRun([]string{"rm"}) })
	if err == nil || !strings.Contains(err.Error(), "usage: fleet run rm") {
		t.Fatalf("expected rm usage error, got: %v", err)
	}

	_, _, err = run(t, func() error { return cmdRun([]string{"rm", "ghost", "--dry-run"}) })
	if err == nil || !strings.Contains(err.Error(), "no such run here") {
		t.Fatalf("expected no such run here error, got: %v", err)
	}

	_, _, err = run(t, func() error { return cmdRun([]string{"unknown"}) })
	if err == nil || !strings.Contains(err.Error(), "usage: fleet run") {
		t.Fatalf("expected unknown run subcmd error, got: %v", err)
	}

	// cmdNodes
	_, _, err = run(t, func() error { return cmdNodes([]string{"add"}) })
	if err == nil || !strings.Contains(err.Error(), "usage: fleet nodes add") {
		t.Fatalf("expected nodes add usage error, got: %v", err)
	}

	out, _, err = run(t, func() error { return cmdNodes([]string{"add", "remote-box"}) })
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, "added 1 machine(s)") {
		t.Fatalf("expected added 1 machine, got: %s", out)
	}

	_, _, err = run(t, func() error { return cmdNodes([]string{"rm"}) })
	if err == nil || !strings.Contains(err.Error(), "usage: fleet nodes rm") {
		t.Fatalf("expected nodes rm usage error, got: %v", err)
	}

	out, _, err = run(t, func() error { return cmdNodes([]string{"rm", "remote-box"}) })
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, "removed 1 machine(s)") {
		t.Fatalf("expected removed 1 machine, got: %s", out)
	}

	out, _, err = run(t, func() error { return cmdNodes([]string{"suggest"}) })
	if err != nil {
		t.Fatal(err)
	}

	_, _, err = run(t, func() error { return cmdNodes([]string{"nonexistent-node"}) })
	if err == nil || !strings.Contains(err.Error(), "no such node") {
		t.Fatalf("expected no such node error, got: %v", err)
	}

	out, _, err = run(t, func() error { return cmdNodes([]string{"rtt", "--samples", "1"}) })
	if err != nil {
		t.Fatal(err)
	}

	// cmdPeers
	out, _, err = run(t, func() error { return cmdPeers([]string{"lan"}) })
	if err != nil {
		t.Fatal(err)
	}

	out, _, err = run(t, func() error { return cmdPeers([]string{"pair"}) })
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, "pairing code:") {
		t.Fatalf("expected pairing code, got: %s", out)
	}

	out, _, err = run(t, func() error { return cmdPeers([]string{"sync"}) })
	if err != nil {
		t.Fatal(err)
	}

	_, _, err = run(t, func() error { return cmdPeers([]string{"rm", "ghost-peer"}) })
	if err == nil || !strings.Contains(err.Error(), "no such peer") {
		t.Fatalf("expected no such peer error, got: %v", err)
	}

	_, _, err = run(t, func() error { return cmdPeers([]string{"add", "peer1", "ssh-box", "max_hops=bad"}) })
	if err == nil || !strings.Contains(err.Error(), "max_hops must be a number") {
		t.Fatalf("expected max_hops error, got: %v", err)
	}

	_, _, err = run(t, func() error { return cmdPeers([]string{"add", "peer1", "ssh-box", "badkey=1"}) })
	if err == nil || !strings.Contains(err.Error(), "unknown setting") {
		t.Fatalf("expected unknown setting error, got: %v", err)
	}

	out, _, err = run(t, func() error {
		return cmdPeers([]string{"add", "p1", "http://127.0.0.1:1/v1", "only=tag:fast", "skip=model:x", "max_hops=2", "key_env=KEY", "remote=h:p"})
	})
	if err != nil {
		t.Fatal(err)
	}

	_, _, err = run(t, func() error { return cmdPeers([]string{"unknown"}) })
	if err == nil || !strings.Contains(err.Error(), "usage: fleet peers") {
		t.Fatalf("expected peers usage error, got: %v", err)
	}

	// cmdRole
	_, _, err = run(t, func() error { return cmdRole(nil) })
	if err == nil || !strings.Contains(err.Error(), "usage: fleet role") {
		t.Fatalf("expected role usage error, got: %v", err)
	}

	out, _, err = run(t, func() error { return cmdRole([]string{"set", "coder", "loc-tiny", "loc-gemma"}) })
	if err != nil {
		t.Fatal(err)
	}

	out, _, err = run(t, func() error {
		return cmdRole([]string{"opt", "coder", "priority=10", "max_parallel=4", "overflow=true", "strategy=least-busy"})
	})
	if err != nil {
		t.Fatal(err)
	}

	_, _, err = run(t, func() error { return cmdRole([]string{"opt", "coder", "priority=notnum"}) })
	if err == nil || !strings.Contains(err.Error(), "must be a number") {
		t.Fatalf("expected number error, got: %v", err)
	}

	_, _, err = run(t, func() error { return cmdRole([]string{"opt", "coder", "badopt=1"}) })
	if err == nil || !strings.Contains(err.Error(), "unknown setting") {
		t.Fatalf("expected unknown setting error, got: %v", err)
	}

	_, _, err = run(t, func() error { return cmdRole([]string{"test", "nonexistent"}) })
	if err == nil || !strings.Contains(err.Error(), "no such role") {
		t.Fatalf("expected no such role error, got: %v", err)
	}

	out, _, err = run(t, func() error { return cmdRole([]string{"init"}) })
	if err != nil {
		t.Fatal(err)
	}

	out, _, err = run(t, func() error { return cmdRole([]string{"match", "coder", "tag:fast", "node:box"}) })
	if err != nil {
		t.Fatal(err)
	}

	out, _, err = run(t, func() error { return cmdRole([]string{"tag", "loc-tiny", "fast", "gpu"}) })
	if err != nil {
		t.Fatal(err)
	}

	out, _, err = run(t, func() error { return cmdRole([]string{"group", "loc-tiny", "off"}) })
	if err != nil {
		t.Fatal(err)
	}

	out, _, err = run(t, func() error { return cmdRole([]string{"fleetport", "loc-tiny", "8080"}) })
	if err != nil {
		t.Fatal(err)
	}

	_, _, err = run(t, func() error { return cmdRole([]string{"fleetport", "loc-tiny", "999999"}) })
	if err == nil || !strings.Contains(err.Error(), "port must be a number") {
		t.Fatalf("expected port out of range error, got: %v", err)
	}

	out, _, err = run(t, func() error { return cmdRole([]string{"slots", "loc-tiny", "4"}) })
	if err != nil {
		t.Fatal(err)
	}

	_, _, err = run(t, func() error { return cmdRole([]string{"slots", "loc-tiny", "notnum"}) })
	if err == nil || !strings.Contains(err.Error(), "slots must be a number") {
		t.Fatalf("expected slots number error, got: %v", err)
	}

	out, _, err = run(t, func() error { return cmdRole([]string{"list"}) })
	if err != nil {
		t.Fatal(err)
	}

	out, _, err = run(t, func() error { return cmdRole([]string{"rm", "coder"}) })
	if err != nil {
		t.Fatal(err)
	}

	// cmdStatus
	out, _, err = run(t, func() error { return cmdStatus() })
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, "ROLES") || !strings.Contains(out, "box/tiny") {
		t.Fatalf("expected ROLES and box/tiny in status, got: %s", out)
	}

	// cmdUsage
	_, _, err = run(t, func() error { return cmdUsage([]string{"--by", "invalid"}) })
	if err == nil || !strings.Contains(err.Error(), "--by must be") {
		t.Fatalf("expected bad --by error, got: %v", err)
	}

	for _, by := range []string{"role", "model", "endpoint", "day", "client"} {
		out, _, err = run(t, func() error { return cmdUsage([]string{"--by", by}) })
		if err != nil {
			t.Fatalf("cmdUsage --by %s error: %v", by, err)
		}
	}

	out, _, err = run(t, func() error { return cmdUsage([]string{"--hub"}) })
	if err != nil {
		t.Fatal(err)
	}

	// cmdLog
	out, _, err = run(t, func() error { return cmdLog([]string{"-n", "10"}) })
	if err != nil {
		t.Fatal(err)
	}

	// cmdDoctor
	out, _, err = run(t, func() error { return cmdDoctor() })
	if err != nil {
		t.Fatal(err)
	}

	// cmdAsk
	out, _, err = run(t, func() error { return cmdAsk([]string{"-s", "system prompt", "scout", "hello agent"}) })
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, "echo[system prompt]: hello agent") {
		t.Fatalf("expected echo output, got: %s", out)
	}

	_, _, err = run(t, func() error { return cmdAsk([]string{"-f", "nonexistent.txt", "scout", "hello"}) })
	if err == nil {
		t.Fatal("expected file error")
	}

	_, _, err = run(t, func() error { return cmdAsk([]string{"-n", "badnum", "scout", "hello"}) })
	if err == nil {
		t.Fatal("expected badnum parse error")
	}

	// cmdProfile subcommands
	out, _, err = run(t, func() error { return cmdProfile([]string{"help"}) })
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, "usage: fleet profile") {
		t.Fatalf("expected profile usage, got: %s", out)
	}

	out, _, err = run(t, func() error { return cmdProfile([]string{"status"}) })
	if err != nil {
		t.Fatal(err)
	}

	for _, sub := range []string{"up", "down", "save", "show", "rm", "export", "import", "bench", "measure", "autostart"} {
		_, _, err = run(t, func() error { return cmdProfile([]string{sub}) })
		if err == nil {
			t.Fatalf("expected error on empty profile %s", sub)
		}
	}

	// cmdSetup
	_, _, err = run(t, func() error { return cmdSetup(nil) })
	if err == nil || !strings.Contains(err.Error(), "usage: fleet setup") {
		t.Fatalf("expected setup usage error, got: %v", err)
	}

	_, _, err = run(t, func() error { return cmdSetup([]string{"unknown"}) })
	if err == nil || !strings.Contains(err.Error(), "unknown:") {
		t.Fatalf("expected unknown setup error, got: %v", err)
	}

	out, _, err = run(t, func() error { return cmdSetup([]string{"skills"}) })
	if err != nil {
		t.Fatal(err)
	}

	// cmdRole test scout
	out, _, err = run(t, func() error { return cmdRole([]string{"test", "scout"}) })
	if err != nil {
		t.Fatal(err)
	}

	// cmdRole init --apply
	out, _, err = run(t, func() error { return cmdRole([]string{"init", "--apply"}) })
	if err != nil {
		t.Fatal(err)
	}

	// cmdNodes box detail
	out, _, err = run(t, func() error { return cmdNodes([]string{"box"}) })
	if err != nil {
		t.Fatal(err)
	}

	// cmdRun with recorded run
	recRun, rerr := d.cl.RecordRun(plan.Result{
		Model: "tiny.gguf",
		Candidates: []plan.Candidate{
			{Master: "box", Kind: "single"},
		},
	}, 1, "loc-tiny", "ring")
	if rerr == nil && recRun.ID != "" {
		out, _, err = run(t, func() error { return cmdRun([]string{"show", recRun.ID}) })
		if err != nil {
			t.Fatal(err)
		}
		out, _, err = run(t, func() error { return cmdRun([]string{"bind", recRun.ID, "loc-tiny"}) })
		if err != nil {
			t.Fatal(err)
		}
		_, _, err = run(t, func() error { return cmdRun([]string{"stop", recRun.ID, "--dry-run"}) })
		if err == nil || !strings.Contains(err.Error(), "refused or incomplete") {
			t.Fatalf("expected refused or incomplete error, got: %v", err)
		}
		out, _, err = run(t, func() error { return cmdRun([]string{"end", recRun.ID, "--record-only"}) })
		if err != nil {
			t.Fatal(err)
		}
	}

	// profile status & down with saved profile
	pStore := profileStore()
	testProf := &profile.Profile{Name: "prof-cov", Model: "tiny.gguf"}
	if err := pStore.Save(testProf); err == nil {
		out, _, err = run(t, func() error { return cmdProfile([]string{"status", "prof-cov"}) })
		if err != nil {
			t.Fatal(err)
		}
		out, _, err = run(t, func() error { return cmdProfile([]string{"down", "prof-cov"}) })
		if err != nil {
			t.Fatal(err)
		}
	}

	// cmdEval on healthy endpoint
	out, _, err = run(t, func() error { return cmdEval([]string{"loc-tiny"}) })
	if err != nil {
		t.Fatalf("cmdEval loc-tiny failed: %v (output: %s)", err, out)
	}

	// cmdDFlashHead with missing files
	_, _, err = run(t, func() error { return cmdChain([]string{"dflash-head", "draft.gguf", "target.gguf", "out.gguf"}) })
	if err == nil {
		t.Fatal("expected error on nonexistent files for dflash-head")
	}
}
