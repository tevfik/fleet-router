package server

import (
	"encoding/json"
	"net/http"

	"github.com/tevfik/fleet-router/internal/config"
)

// nodeSpecHandler: POST /fleet/node-spec {"name": N, "spec": {addr, mem_gib, weight_gbps, expert_gbps, notes}} sets the
// nodes: entry of a machine (what `fleet config node set` writes); {"name": N, "remove": true} drops it. Applied hot.
func (s *Server) nodeSpecHandler(mux *http.ServeMux, adm func(http.HandlerFunc) http.HandlerFunc) {
	mux.HandleFunc("POST /fleet/node-spec", adm(func(w http.ResponseWriter, r *http.Request) {
		var in struct {
			Name   string          `json:"name"`
			Spec   config.NodeSpec `json:"spec"`
			Remove bool            `json:"remove"`
		}
		if err := json.NewDecoder(r.Body).Decode(&in); err != nil || in.Name == "" {
			httpErr(w, 400, "node-spec: {name, spec | remove}")
			return
		}
		cfg := s.Config()
		nodes := make(map[string]config.NodeSpec, len(cfg.Nodes)+1) // the copy of the config shares the map: do not edit it in place
		for k, v := range cfg.Nodes {
			nodes[k] = v
		}
		if in.Remove {
			if _, ok := nodes[in.Name]; !ok {
				httpErr(w, 404, "no node "+in.Name+" in the nodes: section")
				return
			}
			delete(nodes, in.Name)
		} else {
			if in.Spec.MemGiB < 0 || in.Spec.WeightGBps < 0 || in.Spec.ExpertGBps < 0 {
				httpErr(w, 400, "mem_gib, weight_gbps and expert_gbps cannot be negative")
				return
			}
			nodes[in.Name] = in.Spec
		}
		if len(nodes) == 0 {
			nodes = nil
		}
		cfg.Nodes = nodes
		if err := s.SetConfig(cfg); err != nil {
			httpErr(w, 400, err.Error())
			return
		}
		writeJSON(w, map[string]any{"ok": true, "nodes": nodes})
	}))
}
