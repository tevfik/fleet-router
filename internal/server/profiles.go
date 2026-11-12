package server

import (
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"

	"github.com/tevfik/fleet-router/internal/profile"
)

// profileStore is where the daemon keeps the deploy profiles: $FLEET_PROFILES, else profiles/ next to its
// configuration (the same directory `fleet profile` uses).
func (s *Server) profileStore() profile.Store {
	if d := os.Getenv("FLEET_PROFILES"); d != "" {
		return profile.Store{Dir: d}
	}
	return profile.Store{Dir: filepath.Join(filepath.Dir(s.path), "profiles")}
}

// profileHandlers: GET /fleet/profiles (all), GET /fleet/profiles/{name} (JSON, or YAML with ?format=yaml),
// PUT /fleet/profiles/{name} (a profile as YAML or JSON; the runs of an existing profile are kept unless the body
// has some), DELETE /fleet/profiles/{name}.
func (s *Server) profileHandlers(mux *http.ServeMux, adm func(http.HandlerFunc) http.HandlerFunc) {
	mux.HandleFunc("GET /fleet/profiles", adm(func(w http.ResponseWriter, _ *http.Request) {
		list, bad, err := s.profileStore().List()
		if err != nil {
			httpErr(w, 500, err.Error())
			return
		}
		broken := map[string]string{}
		for n, e := range bad {
			broken[n] = e.Error()
		}
		if list == nil {
			list = []*profile.Profile{}
		}
		writeJSON(w, map[string]any{"profiles": list, "broken": broken})
	}))
	mux.HandleFunc("GET /fleet/profiles/{name}", adm(func(w http.ResponseWriter, r *http.Request) {
		p, err := s.profileStore().Load(r.PathValue("name"))
		if err != nil {
			httpErr(w, 404, err.Error())
			return
		}
		if r.URL.Query().Get("format") == "yaml" {
			b, _ := p.Marshal()
			w.Header().Set("Content-Type", "application/yaml")
			w.Write(b)
			return
		}
		writeJSON(w, p)
	}))
	mux.HandleFunc("PUT /fleet/profiles/{name}", adm(func(w http.ResponseWriter, r *http.Request) {
		name := r.PathValue("name")
		if err := profile.ValidName(name); err != nil {
			httpErr(w, 400, err.Error())
			return
		}
		b, err := io.ReadAll(io.LimitReader(r.Body, 1<<20))
		if err != nil {
			httpErr(w, 400, err.Error())
			return
		}
		p, err := profile.Parse(b) // YAML is a superset of JSON
		if err != nil {
			httpErr(w, 400, err.Error())
			return
		}
		p.Name = name
		st := s.profileStore()
		if old, err := st.Load(name); err == nil && len(p.Runs) == 0 {
			p.Runs = old.Runs
		}
		if err := st.Save(p); err != nil {
			code := 500
			if strings.Contains(err.Error(), "profile") || strings.Contains(err.Error(), "layout") {
				code = 400
			}
			httpErr(w, code, err.Error())
			return
		}
		writeJSON(w, p)
	}))
	mux.HandleFunc("DELETE /fleet/profiles/{name}", adm(func(w http.ResponseWriter, r *http.Request) {
		if err := s.profileStore().Remove(r.PathValue("name")); err != nil {
			httpErr(w, 404, err.Error())
			return
		}
		writeJSON(w, map[string]bool{"ok": true})
	}))
}
