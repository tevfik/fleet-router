package board

import (
	"fmt"
	"sort"
	"strings"

	"github.com/tevfik/fleet-router/internal/server"
)

// ConsistencyLines looks for records that no longer match reality and says what fixes each: endpoints that are down,
// one file name that stands for different contents on different machines, store records whose file is gone, runs
// that look dead. It reads only what the daemon already knows.
func ConsistencyLines(st server.Status, nodes []server.NodeStore, runs []server.Run) []string {
	var l []string
	for _, e := range st.Endpoints {
		if e.Peer == "" && e.Known && !e.OK {
			l = append(l, fmt.Sprintf("endpoint %s (%s) is down%s: its process may be gone — fleet endpoint stop %s --dry-run looks, fleet endpoint rm %s removes the record", e.Name, e.Node, errSuffix(e.Err), e.Name, e.Name))
		}
	}
	// the same file name with different contents (a different quantization or revision under one name)
	type holder struct{ node, sha string }
	byName := map[string][]holder{}
	missing := map[string][]string{}
	for _, n := range nodes {
		if n.Store == nil {
			continue
		}
		for _, f := range n.Store.Files {
			if !f.Complete {
				missing[n.Node] = append(missing[n.Node], f.Name)
				continue
			}
			byName[strings.ToLower(f.Name)] = append(byName[strings.ToLower(f.Name)], holder{n.Node, f.SHA256})
		}
	}
	var names []string
	for n := range byName {
		names = append(names, n)
	}
	sort.Strings(names)
	for _, n := range names {
		shas := map[string][]string{}
		for _, h := range byName[n] {
			shas[h.sha] = append(shas[h.sha], h.node)
		}
		if len(shas) > 1 {
			var parts []string
			for sha, ns := range shas {
				parts = append(parts, fmt.Sprintf("%s on %s", sha[:min(8, len(sha))], strings.Join(ns, ",")))
			}
			sort.Strings(parts)
			l = append(l, fmt.Sprintf("file name %s stands for %d different contents (%s): endpoints named by it may not serve the same model — fleet store info %s", n, len(shas), strings.Join(parts, "; "), n))
		}
	}
	// endpoints serving one model name with different content (the hash fleet knew at deploy time)
	epSHA := map[string]map[string][]string{}
	for _, e := range st.Endpoints {
		if e.Peer != "" || e.ModelSHA256 == "" || e.Model == "" {
			continue
		}
		k := strings.ToLower(e.Model)
		if epSHA[k] == nil {
			epSHA[k] = map[string][]string{}
		}
		epSHA[k][e.ModelSHA256] = append(epSHA[k][e.ModelSHA256], e.Name)
	}
	var ek []string
	for k := range epSHA {
		ek = append(ek, k)
	}
	sort.Strings(ek)
	for _, k := range ek {
		if len(epSHA[k]) < 2 {
			continue
		}
		var parts []string
		for sha, eps := range epSHA[k] {
			sort.Strings(eps)
			parts = append(parts, fmt.Sprintf("%s: %s", sha[:min(8, len(sha))], strings.Join(eps, ",")))
		}
		sort.Strings(parts)
		l = append(l, fmt.Sprintf("model name %s is served with %d different contents (%s): the endpoints do not serve the same model — redeploy one of them", k, len(parts), strings.Join(parts, "; ")))
	}
	var mn []string
	for n := range missing {
		mn = append(mn, n)
	}
	sort.Strings(mn)
	for _, n := range mn {
		l = append(l, fmt.Sprintf("%s: %d registered file(s) are gone or changed (%s): fleet store prune on %s forgets them", n, len(missing[n]), strings.Join(missing[n], ", "), n))
	}
	stale := 0
	for _, r := range runs {
		if r.Stale != "" {
			stale++
		}
	}
	if stale > 0 {
		l = append(l, fmt.Sprintf("%d run(s) look dead: fleet run prune --stale --dry-run", stale))
	}
	if len(l) == 0 {
		l = append(l, "nothing stale: every endpoint answers, file names mean one content, no registered file is missing")
	}
	return l
}

func errSuffix(e string) string {
	if e == "" {
		return ""
	}
	return " (" + e + ")"
}
