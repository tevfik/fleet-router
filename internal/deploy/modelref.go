package deploy

import (
	"path/filepath"
	"strings"

	"github.com/tevfik/fleet-router/internal/config"
	"github.com/tevfik/fleet-router/internal/store"
)

// Test seams: the Ollama manifest lookup and the store's hash record (both read-only).
var (
	ollamaNameOf = store.OllamaNameFor
	storeSHAOf   = func(path string) string {
		return store.Open(filepath.Join(filepath.Dir(config.StatsPath()), "store")).KnownSHA(path)
	}
)

// endpointSlug names an endpoint after its model: the Ollama name when the file is an Ollama blob a manifest names
// (gemma4:12b -> gemma4-12b), else, for a blob (sha256-<hex>, a name that says nothing), the GGUF's general.name, else
// the file name. general is the GGUF's own name ("" when unknown).
func endpointSlug(modelPath, general string) string {
	base := filepath.Base(modelPath)
	if n := ollamaNameOf(modelPath); n != "" {
		return config.Slug(n)
	}
	if store.OllamaBlobSHA(modelPath) != "" {
		if g := config.Slug(general); g != "" && !strings.HasPrefix(g, "sha256-") {
			return g
		}
		return "sha256-" + store.OllamaBlobSHA(modelPath)[:12]
	}
	return config.Slug(base)
}

// knownModelSHA is the content hash of the model file when it is already known without reading the file (an Ollama
// blob's name, or the store's record of it), else "". A big file is never hashed for this.
func knownModelSHA(path string) string {
	if h := store.OllamaBlobSHA(path); h != "" {
		return h
	}
	return storeSHAOf(path)
}
