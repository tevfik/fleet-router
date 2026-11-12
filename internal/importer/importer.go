// Package importer converts the old agent-fleet/fleet.yaml to the new format.
package importer

import (
	"fmt"
	"os"
	"strings"

	"gopkg.in/yaml.v3"

	"github.com/tevfik/fleet-router/internal/config"
)

type oldEndpoint struct {
	Name      string  `yaml:"name"`
	URL       string  `yaml:"url"`
	RemoteURL string  `yaml:"remote_url"`
	Model     string  `yaml:"model"`
	Ctx       int     `yaml:"ctx"`
	TPS       float64 `yaml:"tps"`
	Slots     any     `yaml:"slots"`
	APIKey    string  `yaml:"api_key"`
	AuthEnv   string  `yaml:"auth_env"`
}

type oldNode struct {
	ID        string        `yaml:"id"`
	Host      string        `yaml:"host"`
	Aliases   []string      `yaml:"aliases"`
	Gateway   string        `yaml:"gateway"`
	AuthEnv   string        `yaml:"auth_env"`
	Endpoints []oldEndpoint `yaml:"endpoints"`
}

type oldRoute struct {
	Preferred string `yaml:"preferred"`
	Fallback  string `yaml:"fallback"`
}

type oldFile struct {
	Fleet struct {
		Nodes   []oldNode           `yaml:"nodes"`
		Routing map[string]oldRoute `yaml:"routing"`
	} `yaml:"fleet"`
}

// Import: self = name of the machine fleet runs on (endpoints on this machine are direct, others go via SSH).
func Import(path, self string) (*config.Config, []string, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, nil, err
	}
	var old oldFile
	if err := yaml.Unmarshal(b, &old); err != nil {
		return nil, nil, err
	}
	cfg := config.Default()
	var notes []string
	for _, n := range old.Fleet.Nodes {
		machine := n.ID
		if n.Host == "local" && len(n.Aliases) > 0 {
			machine = n.Aliases[0] // "local" was only the old machine's view; the real name is the alias
		} else if n.Host != "" && n.Host != "local" {
			machine = n.Host
		}
		for _, e := range n.Endpoints {
			ne := config.Endpoint{Name: e.Name, Node: machine, Model: e.Model, Ctx: e.Ctx, TPS: e.TPS, KeyEnv: e.AuthEnv}
			if ne.KeyEnv == "" {
				ne.KeyEnv = n.AuthEnv
			}
			if e.APIKey != "" {
				notes = append(notes, fmt.Sprintf("%s: plaintext api_key skipped — use key_env", e.Name))
			}
			if n, ok := e.Slots.(int); ok && n > 0 {
				ne.Slots = n // non-numeric values such as "batch" stay unlimited
			}
			if config.IsEmbedModel(e.Model) {
				ne.Kind = "embed"
			}
			u := e.URL
			remote := false
			if e.RemoteURL != "" {
				u, remote = e.RemoteURL, true
			}
			switch {
			case n.Gateway != "" && !remote:
				ne.URL, ne.Probe = strings.TrimRight(n.Gateway, "/"), "chat" // LiteLLM also lists models that are down
			case u == "":
				continue
			case machine == self || (!remote && n.Host == "local" && machine == self):
				ne.URL = u
			default:
				hp := strings.TrimSuffix(strings.SplitN(u, "://", 2)[len(strings.SplitN(u, "://", 2))-1], "/v1")
				ne.SSH, ne.Remote = machine, strings.TrimSuffix(hp, "/")
			}
			cfg.Endpoints = append(cfg.Endpoints, ne)
		}
	}
	seen := map[string]bool{}
	for _, e := range cfg.Endpoints { // remote machines become discovery targets
		if e.SSH != "" && !seen[e.SSH] {
			seen[e.SSH] = true
			cfg.Discover.SSH = append(cfg.Discover.SSH, e.SSH)
		}
	}
	find := func(ref string) string { // "provider/model" → endpoint name
		prov, model, _ := strings.Cut(ref, "/")
		if prov == "local" {
			return "strata"
		}
		first := ""
		for _, e := range cfg.Endpoints {
			if e.Model != model {
				continue
			}
			if first == "" {
				first = e.Name
			}
			if e.Node == prov || strings.HasPrefix(e.Name, prov) {
				return e.Name
			}
		}
		return first
	}
	for role, r := range old.Fleet.Routing {
		role2 := config.Role{Preferred: find(r.Preferred)}
		if f := find(r.Fallback); f != "" && f != role2.Preferred {
			role2.Fallback = []string{f}
		}
		if role2.Preferred == "" {
			notes = append(notes, fmt.Sprintf("role %s: no endpoint found for %q", role, r.Preferred))
			continue
		}
		cfg.Roles[role] = role2
	}
	return cfg, notes, cfg.Validate()
}
