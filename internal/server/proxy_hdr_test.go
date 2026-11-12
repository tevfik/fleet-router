package server

import (
	"net/http"
	"strings"
	"testing"
)

func TestTunnelRequestHeadersAllowlist(t *testing.T) {
	in := http.Header{
		"Authorization":   {"Bearer flt_live_SECRET"},
		"Cookie":          {"a=b"},
		"X-Forwarded-For": {"1.2.3.4"},
		"Accept":          {"text/event-stream"},
		"Content-Type":    {"text/plain"},
	}
	out := tunnelRequestHeaders(in)
	for _, k := range []string{"Authorization", "Cookie", "X-Forwarded-For"} {
		if out.Get(k) != "" {
			t.Errorf("%s must not be forwarded", k)
		}
	}
	if out.Get("Accept") != "text/event-stream" || out.Get("Content-Type") != "application/json" {
		t.Errorf("allowlisted headers lost: %v", out)
	}
}

func TestClipKeepsRunesWhole(t *testing.T) {
	if got := clip("abc", 5); got != "abc" {
		t.Fatal(got)
	}
	if got := clip("aüb", 2); got != "a" { // ü is 2 bytes: cut before it, never inside
		t.Fatalf("%q", got)
	}
	if got := clip(strings.Repeat("x", 500), 200); len(got) != 200 {
		t.Fatal(len(got))
	}
}
