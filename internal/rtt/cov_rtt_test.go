package rtt

import (
	"context"
	"net"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestTCPConnect_SuccessAndErrors(t *testing.T) {
	// 1. Success with local loopback listener
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("failed to listen on loopback: %v", err)
	}
	defer ln.Close()

	go func() {
		for {
			conn, err := ln.Accept()
			if err != nil {
				return
			}
			_ = conn.Close()
		}
	}()

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()

	dur, err := TCPConnect(ctx, ln.Addr().String())
	if err != nil {
		t.Fatalf("TCPConnect failed on loopback: %v", err)
	}
	if dur <= 0 {
		t.Errorf("expected positive duration, got %v", dur)
	}

	// Also test with "localhost" to hit DNS / LookupHost branch
	_, portStr, _ := net.SplitHostPort(ln.Addr().String())
	durLocalhost, err := TCPConnect(ctx, "localhost:"+portStr)
	if err == nil && durLocalhost <= 0 {
		t.Errorf("expected positive duration for localhost, got %v", durLocalhost)
	}

	// 2. Invalid addr format
	_, err = TCPConnect(ctx, "no-port-addr")
	if err == nil {
		t.Error("expected error for address missing port, got nil")
	}

	// 3. Unresolvable host
	_, err = TCPConnect(ctx, "nonexistent-domain-xyz-404.invalid:1234")
	if err == nil {
		t.Error("expected error for unresolvable host, got nil")
	}

	// 4. Dial failure (closed port)
	_, err = TCPConnect(ctx, net.JoinHostPort("127.0.0.1", "1"))
	if err == nil {
		// Try another unlikely port if 1 happened to succeed
		_, err = TCPConnect(ctx, "127.0.0.1:54321")
	}
	if err == nil {
		t.Log("warning: closed port dial unexpectedly succeeded (firewall redirect?)")
	}
}

func TestMeasure_DefaultsAndContextCancel(t *testing.T) {
	// n <= 0 triggers default n=5
	called := 0
	probe := func(ctx context.Context, addr string) (time.Duration, error) {
		called++
		return 10 * time.Millisecond, nil
	}

	sample, err := Measure(context.Background(), probe, "worker:22", 0)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if called != 5 {
		t.Errorf("expected 5 probes when n<=0, got %d", called)
	}
	if sample.N != 5 {
		t.Errorf("expected sample.N = 5, got %d", sample.N)
	}

	// Context canceled early
	ctx, cancel := context.WithCancel(context.Background())
	cancel() // cancel immediately
	_, err = Measure(ctx, probe, "worker:22", 5)
	if err == nil {
		t.Error("expected error when context canceled before probes, got nil")
	}
}

func TestCache_LoadAndStoreErrors(t *testing.T) {
	tmpDir := t.TempDir()

	// Load non-existent file
	cache := &Cache{Path: filepath.Join(tmpDir, "does-not-exist.json")}
	m := cache.Load()
	if len(m) != 0 {
		t.Errorf("expected empty map for missing cache, got %v", m)
	}

	// Load corrupted file
	corruptPath := filepath.Join(tmpDir, "corrupted.json")
	if err := os.WriteFile(corruptPath, []byte("invalid-json{"), 0644); err != nil {
		t.Fatal(err)
	}
	cacheCorrupt := &Cache{Path: corruptPath}
	mCorrupt := cacheCorrupt.Load()
	if len(mCorrupt) != 0 {
		t.Errorf("expected empty map for corrupted cache, got %v", mCorrupt)
	}

	// Store error when MkdirAll fails
	blockerFile := filepath.Join(tmpDir, "blocker-file")
	if err := os.WriteFile(blockerFile, []byte("data"), 0644); err != nil {
		t.Fatal(err)
	}
	badCache := &Cache{Path: filepath.Join(blockerFile, "sub", "cache.json")}
	err := badCache.Store(map[string]Sample{"a>b": {MS: 12.3}})
	if err == nil {
		t.Error("expected error storing to path where parent is a file, got nil")
	}

	// Store error when writing temp file fails (e.g. read-only directory)
	if os.Geteuid() == 0 {
		return // root writes into a 0500 directory: the read-only case cannot fail
	}
	roDir := filepath.Join(tmpDir, "ro_dir")
	if err := os.Mkdir(roDir, 0500); err != nil {
		t.Fatal(err)
	}
	roCache := &Cache{Path: filepath.Join(roDir, "cache.json")}
	err = roCache.Store(map[string]Sample{"a>b": {MS: 10.0}})
	if err == nil {
		t.Error("expected error storing in read-only dir, got nil")
	}
}

func TestCollector_DefaultsAndDirectSkip(t *testing.T) {
	// Test collector with all default options (Probe=nil, Now=nil, MaxAge<=0, Timeout<=0, Cache=nil)
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen failed: %v", err)
	}
	defer ln.Close()

	go func() {
		for {
			conn, err := ln.Accept()
			if err != nil {
				return
			}
			_ = conn.Close()
		}
	}()

	c := &Collector{
		From:    "nodeA",
		Probe:   nil, // test default probe: TCPConnect
		Cache:   nil, // test no cache
		Now:     nil, // test default now
		MaxAge:  0,   // test default maxAge
		Timeout: 0,   // test default timeout
		Samples: 1,
		Skip: map[string]bool{
			"nodeA>skippedNode": true,
		},
	}

	targets := []Target{
		{Node: "skippedNode", Addr: "127.0.0.1:22"},
		{Node: "liveNode", Addr: ln.Addr().String()},
	}

	out, detail := c.Collect(context.Background(), targets)
	if _, ok := out["nodeA>skippedNode"]; ok {
		t.Error("skippedNode should not be in out map")
	}
	if detail["nodeA>skippedNode"].Skipped != "given with --rtt" {
		t.Errorf("expected 'given with --rtt', got %q", detail["nodeA>skippedNode"].Skipped)
	}

	if val, ok := out["nodeA>liveNode"]; !ok || val <= 0 {
		t.Errorf("expected positive RTT for liveNode, got %v", val)
	}
}
