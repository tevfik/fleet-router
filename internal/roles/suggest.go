// Package roles proposes starter roles from the registered endpoints.
package roles

import (
	"regexp"
	"sort"
	"strconv"

	"github.com/tevfik/fleet-router/internal/config"
)

type Suggestion struct {
	Role   string
	Value  config.Role
	Reason string
}

var paramsRe = regexp.MustCompile(`(?i)(\d+(?:\.\d+)?)b`)

// size guesses the parameter count (in billions) from a model name; 0 when unknown.
func size(model string) float64 {
	best := 0.0
	for _, m := range paramsRe.FindAllStringSubmatch(model, -1) {
		if v, err := strconv.ParseFloat(m[1], 64); err == nil && v > best {
			best = v
		}
	}
	return best
}

// Suggest proposes the roles that do not exist yet (scout, verifier, architect, coder, tester, hunter, validator, vision, embed).
func Suggest(cfg *config.Config) []Suggestion {
	var chat, embed []config.Endpoint
	for _, e := range cfg.Endpoints {
		if e.IsEmbed() {
			embed = append(embed, e)
		} else {
			chat = append(chat, e)
		}
	}
	byTPS := append([]config.Endpoint{}, chat...)
	sort.SliceStable(byTPS, func(i, j int) bool { return byTPS[i].TPS > byTPS[j].TPS })
	bySize := append([]config.Endpoint{}, chat...)
	sort.SliceStable(bySize, func(i, j int) bool {
		if size(bySize[i].Model) != size(bySize[j].Model) {
			return size(bySize[i].Model) > size(bySize[j].Model)
		}
		return bySize[i].Ctx > bySize[j].Ctx
	})
	pick := func(list []config.Endpoint, n int, skip string) []string {
		var out []string
		for _, e := range list {
			if e.Name != skip && len(out) < n {
				out = append(out, e.Name)
			}
		}
		return out
	}
	var out []Suggestion
	add := func(role, reason string, names []string) {
		if _, exists := cfg.Roles[role]; exists || len(names) == 0 {
			return
		}
		var uniq []string // the same endpoint may come from two picks
		for _, n := range names {
			if !has(uniq, n) {
				uniq = append(uniq, n)
			}
		}
		names = uniq
		out = append(out, Suggestion{role, config.Role{Preferred: names[0], Fallback: names[1:]}, reason})
	}
	if len(byTPS) > 0 {
		fast := pick(byTPS, 3, "")
		add("scout", "fastest chat models (scanning is cheap and parallel)", fast)
		add("verifier", "fastest chat models (summarizing test output)", fast)
	}
	var thinkers []config.Endpoint // reasoning models by size
	for _, e := range bySize {
		if e.Has("thinking") {
			thinkers = append(thinkers, e)
		}
	}
	if len(thinkers) > 0 {
		add("architect", "largest reasoning models (deep reasoning)", pick(thinkers, 3, ""))
	} else if len(bySize) > 0 {
		add("architect", "largest models by parameter count / context (deep reasoning)", pick(bySize, 3, ""))
	}
	var coders []config.Endpoint
	for _, e := range chat {
		if regexp.MustCompile(`(?i)code`).MatchString(e.Model) {
			coders = append(coders, e)
		}
	}
	if len(coders) > 0 {
		add("coder", "models with \"code\" in their name", append(pick(coders, 2, ""), pick(bySize, 1, "")...))
	} else if len(bySize) > 0 {
		add("coder", "no code-specific model found — largest models", pick(bySize, 2, ""))
	}
	// QA roles. Accuracy matters more than speed here: the tester is the largest code model, the hunter the
	// largest reasoning model and the validator the next largest one (a second opinion on the hunter's claims).
	var bigCoders []config.Endpoint
	for _, e := range bySize {
		if regexp.MustCompile(`(?i)code`).MatchString(e.Model) {
			bigCoders = append(bigCoders, e)
		}
	}
	if len(bigCoders) > 0 {
		add("tester", "largest code models write the tests", append(pick(bigCoders, 2, ""), pick(bySize, 1, "")...))
	} else if len(bySize) > 0 {
		add("tester", "no code-specific model found — largest models", pick(bySize, 2, ""))
	}
	if len(thinkers) > 0 {
		add("hunter", "largest reasoning models (bug hunting needs depth)", pick(thinkers, 3, ""))
		second := thinkers
		if len(thinkers) > 1 {
			second = thinkers[1:] // not the hunter's first choice
		}
		add("validator", "next-largest reasoning models, not the hunter's first choice (a second opinion)", pick(second, 3, ""))
	}
	var vis []config.Endpoint
	for _, e := range byTPS {
		if e.Has("vision") {
			vis = append(vis, e)
		}
	}
	add("vision", "models that accept images", pick(vis, 3, ""))
	if len(embed) > 0 {
		first := embed[0]
		for _, e := range embed { // prefer one whose size is already known
			if e.Dim > 0 {
				first = e
				break
			}
		}
		names := []string{first.Name}
		for _, e := range embed { // fallbacks must have the same vector size
			if e.Name != first.Name && first.Dim > 0 && e.Dim == first.Dim {
				names = append(names, e.Name)
			}
		}
		add("embed", "embedding model; fallbacks only with the same vector size", names)
	}
	return out
}

func has(l []string, x string) bool {
	for _, v := range l {
		if v == x {
			return true
		}
	}
	return false
}
