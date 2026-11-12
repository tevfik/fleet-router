package server

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/tevfik/fleet-router/internal/config"
	"github.com/tevfik/fleet-router/internal/feature"
)

// Without fleet-engine the daemon is only the router: the runner's admin routes do not exist, the model API does.
func TestLite_RunnerRoutesAreNotMounted(t *testing.T) {
	defer feature.SetRunner(false)()
	s := &Server{cfg: &config.Config{}, state: map[string]*EpState{}, lims: map[string]*limiter{}, client: http.DefaultClient}
	ts := httptest.NewServer(s.Handler())
	defer ts.Close()
	for _, c := range []struct{ method, path string }{
		{"GET", "/fleet/runs"}, {"POST", "/fleet/plan"}, {"GET", "/fleet/store"}, {"GET", "/fleet/profiles"},
		{"POST", "/fleet/runs/stop"}, {"POST", "/fleet/node-spec"}, {"POST", "/fleet/store/pull"},
	} {
		req, _ := http.NewRequest(c.method, ts.URL+c.path, strings.NewReader("{}"))
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
		if resp.StatusCode != http.StatusNotFound && resp.StatusCode != http.StatusMethodNotAllowed {
			t.Errorf("%s %s: status %d, want 404/405 (route must not exist without fleet-engine)", c.method, c.path, resp.StatusCode)
		}
	}
	for _, path := range []string{"/v1/models", "/fleet/status", "/healthz"} {
		resp, err := http.Get(ts.URL + path)
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
		if resp.StatusCode != http.StatusOK {
			t.Errorf("GET %s: status %d, want 200", path, resp.StatusCode)
		}
	}
}
