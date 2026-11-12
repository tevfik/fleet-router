package server

import (
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"testing"

	"github.com/tevfik/fleet-router/internal/config"
)

// A reverse proxy on the same host reaches the daemon from 127.0.0.1; the callers it forwards are not local and need
// the API key / admin token like any remote client.
func TestForwardedCallersAreNotLoopback(t *testing.T) {
	t.Setenv("FLEET_TEST_KEY", "k-secret")
	p := filepath.Join(t.TempDir(), "c.json")
	if err := (&config.Config{Name: "hub", APIKeyEnv: "FLEET_TEST_KEY"}).Save(p); err != nil {
		t.Fatal(err)
	}
	s, err := New(p)
	if err != nil {
		t.Fatal(err)
	}
	h := s.Handler()
	do := func(path string, fwd bool) int {
		req := httptest.NewRequest("GET", path, nil)
		req.RemoteAddr = "127.0.0.1:5555"
		if fwd {
			req.Header.Set("X-Forwarded-For", "203.0.113.9")
		}
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		return rec.Code
	}
	if c := do("/v1/models", false); c != http.StatusOK {
		t.Errorf("a direct local caller needs no key: %d", c)
	}
	if c := do("/v1/models", true); c != http.StatusUnauthorized {
		t.Errorf("a forwarded caller needs the key: %d", c)
	}
	if c := do("/fleet/apikeys", false); c != http.StatusOK {
		t.Errorf("a direct local admin call: %d", c)
	}
	if c := do("/fleet/apikeys", true); c != http.StatusForbidden {
		t.Errorf("a forwarded admin call must be refused: %d", c)
	}
}
