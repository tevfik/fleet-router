package main

import "testing"

func TestFilterChildren(t *testing.T) {
	info := map[string][2]string{"10": {"fleet", "fleet serve"}, "11": {"ssh", "ssh -L 1:2 x"}, "12": {"llama-server", "llama-server -m x"}, "13": {"", ""}}
	got := filterChildren([]string{"10", "11", "12", "13", "x"}, "10", func(p string) (string, string) { return info[p][0], info[p][1] })
	if len(got) != 1 || got[0] != "pid 12: llama-server -m x" {
		t.Fatalf("got %v", got)
	}
	if cgroupChildren("/x", "1", "process") != nil {
		t.Fatal("KillMode=process kills only the main pid")
	}
}
