package main

import (
	"net"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// A second `fleet stream connect` on a taken port says which one holds it; pid files of dead processes are dropped.
func TestStreamListenConflictNamesTheHolder(t *testing.T) {
	t.Setenv("FLEET_STATS", filepath.Join(t.TempDir(), "stats.json"))
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	addr := ln.Addr().String()
	pf, err := writeStreamPID(streamInfo{PID: os.Getpid(), Node: "papa", Port: 50160, Listen: addr, Hub: "https://hub.example"})
	if err != nil {
		t.Fatal(err)
	}
	_, lerr := net.Listen("tcp", addr)
	if lerr == nil {
		t.Fatal("port must be taken")
	}
	msg := listenError(addr, lerr).Error()
	if !strings.Contains(msg, "already served by fleet stream connect pid") || !strings.Contains(msg, "papa:50160") {
		t.Fatalf("holder not named: %s", msg)
	}
	// a dead pid's file is not a holder and is cleaned
	_ = os.WriteFile(pf, []byte(`{"pid":2147483000,"node":"x","port":1,"listen":"`+addr+`"}`), 0o600)
	if got := runningStreams(); len(got) != 0 {
		t.Fatalf("dead process listed: %+v", got)
	}
	if _, err := os.Stat(pf); !os.IsNotExist(err) {
		t.Fatal("stale pid file kept")
	}
	if msg := listenError(addr, lerr).Error(); !strings.Contains(msg, "another program") {
		t.Fatalf("foreign holder: %s", msg)
	}
}

func TestStreamPIDFileIs0600(t *testing.T) {
	t.Setenv("FLEET_STATS", filepath.Join(t.TempDir(), "stats.json"))
	p, err := writeStreamPID(streamInfo{PID: os.Getpid(), Node: "n", Port: 1, Listen: "127.0.0.1:51000"})
	if err != nil {
		t.Fatal(err)
	}
	if st, _ := os.Stat(p); st.Mode().Perm() != 0o600 {
		t.Fatalf("mode %v", st.Mode())
	}
	if got := runningStreams(); len(got) != 1 || got[0].Node != "n" {
		t.Fatalf("%+v", got)
	}
}
