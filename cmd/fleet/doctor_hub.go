package main

import (
	"fmt"
	"os"
	"path/filepath"

	"github.com/tevfik/fleet-router/internal/board"
	"github.com/tevfik/fleet-router/internal/config"
)

// doctorSecretLines says where the secrets of this machine come from and whether their files are safe. It never
// prints a value: only names, where they come from and file modes.
func doctorSecretLines(cfg *config.Config) []string {
	var out []string
	env := filepath.Join(config.Dir(), ".env")
	out = append(out, "secret lookup order: environment > "+env+" (then ~/.config/opencode/.env) > config > (hub key only) api_key_env")
	if _, src := config.ResolveHubAPIKey(cfg); src != "" {
		out = append(out, "✓ hub API key: set, used from "+src)
	} else {
		out = append(out, "! hub API key: none (fleet key set <key>) — streams and a hub with hub_require_key will refuse this machine")
	}
	if cfg != nil && cfg.APIKeyEnv != "" {
		src := config.SecretSource(cfg.APIKeyEnv)
		if src == "" {
			out = append(out, "✗ "+cfg.APIKeyEnv+" (api_key_env): not set in the environment or any .env")
		} else {
			out = append(out, "✓ "+cfg.APIKeyEnv+" (api_key_env): used from "+src)
		}
	}
	if st, err := os.Stat(env); err == nil {
		switch {
		case st.Size() == 0:
			out = append(out, "! "+env+" is empty (0 bytes): fleet key rm / set rewrite it; remove it")
		case st.Mode().Perm()&0o077 != 0:
			out = append(out, fmt.Sprintf("✗ %s is mode %o, readable by others: chmod 600 %s", env, st.Mode().Perm(), env))
		default:
			out = append(out, fmt.Sprintf("✓ %s is mode %o", env, st.Mode().Perm()))
		}
	}
	if cp := config.Path(); cfg != nil && (cfg.HubAPIKey != "" || cfg.HubToken != "") {
		if st, err := os.Stat(cp); err == nil && st.Mode().Perm()&0o077 != 0 {
			out = append(out, fmt.Sprintf("✗ %s holds hub secrets and is mode %o: chmod 600", cp, st.Mode().Perm()))
		}
	}
	return out
}

// doctorVersionLines: this binary's build and the drift between the machines of the fleet.
func doctorVersionLines(self string, nodes []board.NodeVersion) []string {
	var out []string
	if board.ParseVersion(self).IsDev() {
		out = append(out, "! this fleet is a dev build (no version: built without ldflags); `make build` or a release sets one")
	}
	for _, l := range board.VersionDrift(nodes) {
		out = append(out, "! "+l)
	}
	return out
}
