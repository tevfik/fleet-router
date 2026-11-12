package client

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// fakeDaemon answers /fleet/status with statusJSON and nothing else (the mux's own 404 for every other route).
func fakeDaemon(t *testing.T, statusJSON string) *Client {
	t.Helper()
	mux := http.NewServeMux()
	mux.HandleFunc("GET /fleet/status", func(w http.ResponseWriter, _ *http.Request) { w.Write([]byte(statusJSON)) })
	ts := httptest.NewServer(mux)
	t.Cleanup(ts.Close)
	return New(strings.TrimPrefix(ts.URL, "http://"))
}

// A daemon without fleet-engine has no runner routes: the client says so, instead of calling it an old daemon.
func TestCompat_RunnerRouteOnALiteDaemon(t *testing.T) {
	_, err := fakeDaemon(t, `{"version":"v0.17.0","runner":false}`).Runs()
	if err == nil || !strings.Contains(err.Error(), "has no fleet-engine") {
		t.Fatalf("lite daemon: %v", err)
	}
}

// An older daemon (no "runner" in its status) that lacks a route is still reported as old.
func TestCompat_RunnerRouteOnAnOldDaemon(t *testing.T) {
	_, err := fakeDaemon(t, `{"version":"v0.15.0"}`).Runs()
	if err == nil || !strings.Contains(err.Error(), "older than this fleet") {
		t.Fatalf("old daemon: %v", err)
	}
}
