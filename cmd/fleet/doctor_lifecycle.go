package main

import (
	"fmt"
	"sort"
	"strings"

	"github.com/tevfik/fleet-router/internal/profile"
	"github.com/tevfik/fleet-router/internal/server"
	"github.com/tevfik/fleet-router/internal/setup"
)

// checkLinger evaluates systemd linger status for doctor.
// If lingerFunc returns an error (e.g. loginctl missing or non-linux), it returns ok=true, msg="" to skip.
// If linger is enabled ("yes"), it returns ok=true, "systemd linger is enabled".
// If linger is disabled (e.g. "no"), it returns ok=!hasAutostart(profName) with a descriptive warning recommending 'loginctl enable-linger'.
func checkLinger(lingerFunc func() (string, error), user string) (bool, string) {
	if lingerFunc == nil {
		return true, ""
	}
	v, err := lingerFunc()
	if err != nil {
		return true, "" // systemd or loginctl not present/applicable
	}
	if v == "yes" {
		return true, "systemd linger is enabled"
	}
	if user == "" {
		user = "$USER"
	}
	return false, fmt.Sprintf("systemd linger is off (Linger=%s): user services (fleet.service) will not start at boot without active login — run `loginctl enable-linger %s`", v, user)
}

// checkDuplicateEndpointRuns identifies active runs sharing the same endpoint.
func checkDuplicateEndpointRuns(runs []server.Run) []string {
	byEp := make(map[string][]string)
	for _, r := range runs {
		if r.Ended.IsZero() && r.Lost == "" && r.Endpoint != "" {
			byEp[r.Endpoint] = append(byEp[r.Endpoint], r.ID)
		}
	}
	var eps []string
	for ep, ids := range byEp {
		if len(ids) > 1 {
			eps = append(eps, ep)
		}
	}
	sort.Strings(eps)
	var warnings []string
	for _, ep := range eps {
		ids := byEp[ep]
		sort.Strings(ids)
		warnings = append(warnings, fmt.Sprintf("endpoint %q has %d active runs (%s) — clean up stale records with 'fleet run end <id> --record-only'", ep, len(ids), strings.Join(ids, ", ")))
	}
	return warnings
}

// checkUnmanagedRuns inspects active runs and warns if any deployed model is running detached (PPID 1)
// without an associated systemd autostart service (fleet-profile-*.service).
func checkUnmanagedRuns(runs []server.Run, profiles []*profile.Profile, hasAutostart func(name string) bool) []string {
	if hasAutostart == nil {
		hasAutostart = setup.ProfileAutostartEnabled
	}

	// Build map of run ID -> profile name, and map of endpoint -> profile name
	runToProfile := make(map[string]string)
	epToProfile := make(map[string]string)
	for _, p := range profiles {
		if p == nil {
			continue
		}
		if p.Name != "" {
			epToProfile[p.Name] = p.Name
		}
		for _, pr := range p.Runs {
			if pr.RunID != "" {
				runToProfile[pr.RunID] = p.Name
			}
		}
	}

	var warnings []string
	for _, r := range runs {
		if !r.Ended.IsZero() || r.Lost != "" {
			continue
		}

		profName := runToProfile[r.ID]
		if profName == "" && r.Endpoint != "" {
			profName = epToProfile[r.Endpoint]
		}

		if profName != "" {
			if !hasAutostart(profName) {
				unit := setup.ProfileUnitName(profName)
				warnings = append(warnings, fmt.Sprintf("active run %s (%s, model %s) is running without systemd oversight (PPID 1, no associated %s) — will not restart on reboot; enable autostart with 'fleet profile autostart %s on'", r.ID, r.Endpoint, r.Model, unit, profName))
			}
		} else {
			warnings = append(warnings, fmt.Sprintf("active run %s (%s, model %s) is running without systemd oversight (PPID 1, no associated fleet-profile-*.service) — will not restart on reboot", r.ID, r.Endpoint, r.Model))
		}
	}
	sort.Strings(warnings)
	return warnings
}
