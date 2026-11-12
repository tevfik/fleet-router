package deploy

import (
	"fmt"
	"github.com/tevfik/fleet-router/internal/archcert"
	"github.com/tevfik/fleet-router/internal/plan"
	"strings"
)

// ringCertEnv is the environment the model architecture's ring gate result depends on, for a deploy that uses a ring
// (workers present). A single-machine deploy needs none: the difference comes from splitting the graph.
func ringCertEnv(m *plan.Model, ring bool) []string {
	if !ring || m == nil {
		return nil
	}
	var out []string
	for _, kv := range archcert.RequiredEnv(m.Arch) {
		// the file may come from FLEET_ARCH_CERT: its values go unquoted into the master's (remote) shell like the
		// user's extras do, so they must pass the same word check (a bad entry is dropped, never run)
		if k, v, ok := strings.Cut(kv, "="); ok && envKeyRe.MatchString(k) && safeWord(v) {
			out = append(out, kv)
		}
	}
	return out
}

// checkShellSafePath refuses a path that cannot be passed through a (remote) shell word unquoted.
func checkShellSafePath(what, p string) error {
	if p != "" && strings.ContainsAny(p, " \t\r\n'\"$;&|<>`\\()*?!#{}") {
		return fmt.Errorf("%s %q has a space or a character a remote shell would interpret: rename the file or use a path without them", what, p)
	}
	return nil
}
