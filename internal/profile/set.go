package profile

import (
	"fmt"
	"strings"

	"gopkg.in/yaml.v3"
)

// Set changes one key of the profile: a dotted YAML path (layout.ts, spec.type, env.master.GGML_X,
// worker_models.papa, layout.worker_cpu_experts.papa, workers, extra_master_args, ...). The value is read as a
// YAML scalar ("4096", "true", "q8_0"); for a list key a comma-separated value is a list ("papa,victor"), and
// extra_master_args is split on spaces. An empty value removes the key. The result is validated as a whole profile
// (an unknown key is an error). Runs cannot be set.
func (p *Profile) Set(key, value string) error {
	key = strings.TrimSpace(key)
	if key == "" {
		return fmt.Errorf("empty key")
	}
	if key == "name" || key == "runs" || strings.HasPrefix(key, "runs.") {
		return fmt.Errorf("%s cannot be set (rename: export + import --name; runs are recorded by fleet)", key)
	}
	b, err := yaml.Marshal(p)
	if err != nil {
		return err
	}
	var doc map[string]any
	if err := yaml.Unmarshal(b, &doc); err != nil {
		return err
	}
	path := strings.Split(key, ".")
	cur := doc
	for _, k := range path[:len(path)-1] {
		next, ok := cur[k].(map[string]any)
		if !ok {
			if _, exists := cur[k]; exists {
				return fmt.Errorf("%s: %s is not a section", key, k)
			}
			next = map[string]any{}
			cur[k] = next
		}
		cur = next
	}
	last := path[len(path)-1]
	try := func(v any) (*Profile, error) {
		if v == nil {
			delete(cur, last)
		} else {
			cur[last] = v
		}
		out, err := yaml.Marshal(doc)
		if err != nil {
			return nil, err
		}
		np, err := Parse(out)
		if err != nil {
			return nil, err
		}
		np.Name = p.Name
		return np, nil
	}
	var np *Profile
	switch {
	case value == "":
		np, err = try(nil)
	case key == "extra_master_args":
		np, err = try(strings.Fields(value))
	default:
		var scalar any
		if yerr := yaml.Unmarshal([]byte(value), &scalar); yerr != nil {
			scalar = value
		}
		if _, isMap := scalar.(map[string]any); isMap {
			scalar = value // "a: b" is a string here, not a section
		}
		np, err = try(scalar)
		if err != nil && strings.Contains(value, ",") {
			parts := strings.Split(value, ",")
			l := make([]any, 0, len(parts))
			for _, s := range parts {
				if s = strings.TrimSpace(s); s != "" {
					l = append(l, s)
				}
			}
			if np2, err2 := try(l); err2 == nil {
				np, err = np2, nil
			}
		}
		if err != nil {
			if np2, err2 := try(value); err2 == nil { // a value that looks like a number for a string key
				np, err = np2, nil
			}
		}
		if err != nil { // a list key given a single value ("workers=papa")
			if np2, err2 := try([]any{value}); err2 == nil {
				np, err = np2, nil
			}
		}
	}
	if err != nil {
		return fmt.Errorf("--set %s=%s: %w", key, value, err)
	}
	runs := p.Runs
	*p = *np
	p.Runs = runs
	return nil
}
