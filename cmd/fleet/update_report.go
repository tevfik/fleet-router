package main

import (
	"fmt"
	"regexp"
	"sort"
	"strings"
)

// updateResult is what `fleet update --all` learned about one machine.
type updateResult struct {
	Host   string
	State  string // updated | current | behind (--check) | skipped | failed
	Detail string
}

var (
	updatedRe = regexp.MustCompile(`✓ fleet (\S+) → (\S+)`)
	currentRe = regexp.MustCompile(`fleet (\S+) is up to date`)
	behindRe  = regexp.MustCompile(`fleet (\S+) → (\S+) available`)
)

// classifyUpdate reads what a machine's `fleet update` printed: updated (from → to), already current, behind (with
// --check), skipped (nothing installed, processes or streams that the update would kill) or failed (not reachable or
// the update itself failed, with the first line of why).
func classifyUpdate(host, out string, err error, check bool) updateResult {
	r := updateResult{Host: host}
	first := strings.TrimSpace(strings.SplitN(out, "\n", 2)[0])
	lower := strings.ToLower(out)
	switch {
	case behindRe.MatchString(out):
		m := behindRe.FindStringSubmatch(out)
		r.State, r.Detail = "behind", m[1]+" → "+m[2]
	case updatedRe.MatchString(out):
		m := updatedRe.FindStringSubmatch(out)
		r.State, r.Detail = "updated", m[1]+" → "+m[2]
	case currentRe.MatchString(out):
		r.State, r.Detail = "current", currentRe.FindStringSubmatch(out)[1]
	case strings.Contains(out, "no fleet installed"):
		r.State, r.Detail = "skipped", "no fleet installed on it (install.sh)"
	case strings.Contains(out, "would kill") || strings.Contains(out, "open stream"):
		r.State, r.Detail = "skipped", "not updated: "+first+" (rerun there with --force when that is fine)"
	case strings.Contains(lower, "permission denied (publickey"), strings.Contains(lower, "could not resolve"), strings.Contains(lower, "connection refused"),
		strings.Contains(lower, "timed out"), strings.Contains(lower, "no route to host"), strings.Contains(lower, "connection closed"), strings.Contains(lower, "host key verification"):
		r.State, r.Detail = "failed", "unreachable over ssh: "+first
	case err != nil:
		r.State, r.Detail = "failed", first
	default:
		r.State, r.Detail = "failed", "unrecognized answer: "+first
	}
	return r
}

// updateSummary is the table at the end of `fleet update --all`: which machines were updated, which were already
// current, which were skipped and why, which failed.
func updateSummary(rs []updateResult, check bool) []string {
	groups := map[string][]string{}
	for _, r := range rs {
		groups[r.State] = append(groups[r.State], r.Host+" ("+r.Detail+")")
	}
	var out []string
	title := map[string]string{"updated": "updated", "current": "already current", "behind": "behind (not updated: --check)", "skipped": "SKIPPED", "failed": "FAILED"}
	out = append(out, "update --all summary:")
	for _, st := range []string{"updated", "behind", "current", "skipped", "failed"} {
		hs := groups[st]
		if len(hs) == 0 {
			continue
		}
		sort.Strings(hs)
		if st == "skipped" || st == "failed" {
			out = append(out, fmt.Sprintf("  ✗ %s (%d):", title[st], len(hs)))
			for _, h := range hs {
				out = append(out, "      "+h)
			}
			continue
		}
		out = append(out, fmt.Sprintf("  ✓ %s (%d): %s", title[st], len(hs), strings.Join(hs, ", ")))
	}
	if len(groups["failed"])+len(groups["skipped"]) > 0 {
		out = append(out, "  these machines still run the old version: fix and rerun `fleet update --all`")
	}
	return out
}
