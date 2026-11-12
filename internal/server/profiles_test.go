package server

import (
	"encoding/json"
	"io"
	"net/http"
	"path/filepath"
	"strings"
	"testing"

	"github.com/tevfik/fleet-router/internal/config"
)

func TestProfilesAPI(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "config.yaml")
	if err := (&config.Config{Name: "x"}).Save(p); err != nil {
		t.Fatal(err)
	}
	t.Setenv("FLEET_PROFILES", "")
	s, err := New(p)
	if err != nil {
		t.Fatal(err)
	}
	h := s.Handler()
	do := func(method, path, body string) (int, string) {
		req, _ := http.NewRequest(method, "http://127.0.0.1"+path, strings.NewReader(body))
		req.RemoteAddr = "127.0.0.1:5555"
		rw := &recorder{h: http.Header{}}
		h.ServeHTTP(rw, req)
		return rw.code, rw.body.String()
	}
	if c, b := do("PUT", "/fleet/profiles/ornith", "model: /m/o.gguf\nmaster: dino\nlayout: {mode: auto, via_hub: true}\n"); c != 200 {
		t.Fatalf("%d %s", c, b)
	}
	if c, _ := do("PUT", "/fleet/profiles/bad", "model: /m/o.gguf\nmastr: dino\n"); c != 400 {
		t.Errorf("unknown key: %d", c)
	}
	if c, _ := do("PUT", "/fleet/profiles/..x", "model: m\n"); c != 400 && c != 404 {
		t.Errorf("bad name: %d", c)
	}
	c, b := do("GET", "/fleet/profiles", "")
	var list struct {
		Profiles []struct {
			Name   string `json:"name"`
			Master string `json:"master"`
		} `json:"profiles"`
	}
	if json.Unmarshal([]byte(b), &list); c != 200 || len(list.Profiles) != 1 || list.Profiles[0].Master != "dino" {
		t.Fatalf("%d %s", c, b)
	}
	if c, b := do("GET", "/fleet/profiles/ornith?format=yaml", ""); c != 200 || !strings.Contains(b, "via_hub: true") {
		t.Fatalf("%d %s", c, b)
	}
	if c, _ := do("DELETE", "/fleet/profiles/ornith", ""); c != 200 {
		t.Fatal(c)
	}
	if c, _ := do("GET", "/fleet/profiles/ornith", ""); c != 404 {
		t.Fatal(c)
	}
}

type recorder struct {
	h    http.Header
	code int
	body strings.Builder
}

func (r *recorder) Header() http.Header { return r.h }
func (r *recorder) Write(b []byte) (int, error) {
	if r.code == 0 {
		r.code = 200
	}
	return r.body.Write(b)
}
func (r *recorder) WriteHeader(c int) { r.code = c }

var _ io.Writer = (*recorder)(nil)
