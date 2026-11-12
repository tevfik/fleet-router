package store

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
)

// Ollama keeps its models as content-addressed blobs (blobs/sha256-<hex>, no .gguf extension) and says in a manifest
// which blob is the model and which is its vision projector. The model blob is a plain GGUF whose file name is its
// SHA-256, so the same file is known by the same hash on a machine that keeps it as model.gguf.

// OllamaModel is one model of an Ollama directory.
type OllamaModel struct {
	Name      string // "qwen3:8b"
	Model     string // path of the GGUF blob
	Projector string // path of the projector blob ("" when it has none)
}

const (
	ollamaModelLayer     = "application/vnd.ollama.image.model"
	ollamaProjectorLayer = "application/vnd.ollama.image.projector"
)

// IsOllamaDir: dir is an Ollama models directory (it has manifests and blobs).
func IsOllamaDir(dir string) bool {
	m, err1 := os.Stat(filepath.Join(dir, "manifests"))
	b, err2 := os.Stat(filepath.Join(dir, "blobs"))
	return err1 == nil && err2 == nil && m.IsDir() && b.IsDir()
}

// ScanOllama lists the models of an Ollama models directory, read from its manifests.
func ScanOllama(dir string) ([]OllamaModel, error) {
	root := filepath.Join(dir, "manifests")
	var out []OllamaModel
	err := filepath.Walk(root, func(p string, info os.FileInfo, err error) error {
		if err != nil || info.IsDir() {
			return nil
		}
		b, err := os.ReadFile(p)
		if err != nil {
			return nil
		}
		var mf struct {
			Layers []struct {
				MediaType string `json:"mediaType"`
				Digest    string `json:"digest"`
			} `json:"layers"`
		}
		if json.Unmarshal(b, &mf) != nil {
			return nil
		}
		m := OllamaModel{Name: ollamaName(root, p)}
		for _, l := range mf.Layers {
			blob := filepath.Join(dir, "blobs", strings.Replace(l.Digest, ":", "-", 1))
			if _, err := os.Stat(blob); err != nil {
				continue
			}
			switch l.MediaType {
			case ollamaModelLayer:
				m.Model = blob
			case ollamaProjectorLayer:
				m.Projector = blob
			}
		}
		if m.Model != "" {
			out = append(out, m)
		}
		return nil
	})
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out, err
}

// ollamaName turns manifests/registry.ollama.ai/library/qwen3/8b into "qwen3:8b" (other namespaces keep theirs).
func ollamaName(root, p string) string {
	rel, _ := filepath.Rel(root, p)
	parts := strings.Split(filepath.ToSlash(rel), "/")
	if len(parts) >= 3 { // host/namespace/name…/tag
		parts = parts[1:]
	}
	if len(parts) >= 2 && parts[0] == "library" {
		parts = parts[1:]
	}
	tag := parts[len(parts)-1]
	name := strings.Join(parts[:len(parts)-1], "/")
	if name == "" {
		return tag
	}
	return name + ":" + tag
}

// FileName is the file name an Ollama model is listed under: qwen3:8b → qwen3-8b.gguf, its projector
// qwen3-8b.mmproj.gguf.
func (m OllamaModel) FileName(projector bool) string {
	n := strings.NewReplacer(":", "-", "/", "-").Replace(m.Name)
	if projector {
		return n + ".mmproj.gguf"
	}
	return n + ".gguf"
}

// Describe is the line the store prints for an Ollama model that ships a projector.
func (m OllamaModel) Describe() string {
	if m.Projector == "" {
		return ""
	}
	return fmt.Sprintf("%s ships a vision projector (%s): the model file alone answers text only; give the projector with --mmproj when it is deployed", m.Name, m.FileName(true))
}

// OllamaBlobSHA is the SHA-256 an Ollama blob's own file name states (blobs/sha256-<hex>): the name IS the hash, so it
// is known without reading the file. "" for any other file.
func OllamaBlobSHA(path string) string {
	b := filepath.Base(path)
	hex := strings.TrimPrefix(b, "sha256-")
	if hex == b || len(hex) != 64 {
		return ""
	}
	for _, c := range hex {
		if !(c >= '0' && c <= '9' || c >= 'a' && c <= 'f') {
			return ""
		}
	}
	return hex
}

// OllamaNameFor is the Ollama name ("gemma4:12b") of a model blob, from the manifests next to its blobs directory;
// "" when path is not a blob of an Ollama directory or no manifest names it.
func OllamaNameFor(path string) string {
	abs, err := filepath.Abs(path)
	if err != nil || OllamaBlobSHA(abs) == "" {
		return ""
	}
	dir := filepath.Dir(filepath.Dir(abs))
	if filepath.Base(filepath.Dir(abs)) != "blobs" || !IsOllamaDir(dir) {
		return ""
	}
	ms, _ := ScanOllama(dir)
	for _, m := range ms {
		if m.Model == abs {
			return m.Name
		}
	}
	return ""
}
