package deploy

import (
	"fmt"
	"regexp"
	"sort"
	"strings"
)

var envKeyRe = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*$`)

// ValidateExtras checks the extra environment and master arguments (a profile's env: / extra_master_args:): keys are
// environment names, values and arguments are plain words (letters, digits and _ . : , / - = +, the set worker
// environments are already held to), so nothing can reach a shell as code. An argument must not replace what fleet
// sets itself (-m, --port, --host, -dev, -ts, RING_SERVERS, ...).
func ValidateExtras(o DeployOptions) error {
	check := func(where, k, v string) error {
		if !envKeyRe.MatchString(k) {
			return fmt.Errorf("%s: %q is not an environment variable name", where, k)
		}
		if !safeWord(v) {
			return fmt.Errorf("%s: the value of %s may only hold letters, digits and _ . : , / - = +", where, k)
		}
		switch k {
		case "RING_SERVERS", "RING_PEER_SERVERS", "RING_STATS_PORT", "GGML_BACKEND_PATH", "LD_LIBRARY_PATH", "LD_PRELOAD":
			return fmt.Errorf("%s: %s is set by fleet itself", where, k)
		}
		return nil
	}
	for k, v := range o.MasterEnvExtra {
		if err := check("master env", k, v); err != nil {
			return err
		}
	}
	for node, env := range o.WorkerEnvExtra {
		for k, v := range env {
			if err := check("worker "+node+" env", k, v); err != nil {
				return err
			}
		}
	}
	reserved := map[string]bool{"-m": true, "--model": true, "--port": true, "--host": true, "-dev": true, "--device": true, "-ts": true, "--tensor-split": true, "-ngl": true, "--n-gpu-layers": true}
	for _, a := range o.MasterArgs {
		if a == "" || !safeWord(a) {
			return fmt.Errorf("extra master argument %q: one word of letters, digits and _ . : , / - = + only", a)
		}
		name, _, _ := strings.Cut(a, "=")
		if reserved[name] {
			return fmt.Errorf("extra master argument %s: fleet sets it itself (use the matching deploy option)", name)
		}
	}
	return nil
}

func safeWord(s string) bool {
	for _, r := range s {
		if !(r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r >= '0' && r <= '9' || strings.ContainsRune("_.:,/-=+", r)) {
			return false
		}
	}
	return true
}

// sortedEnv turns KEY -> VALUE into KEY=VALUE words in key order (a stable plan).
func sortedEnv(m map[string]string) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	out := make([]string, 0, len(keys))
	for _, k := range keys {
		out = append(out, k+"="+m[k])
	}
	return out
}
