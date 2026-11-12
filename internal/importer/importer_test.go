package importer

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/tevfik/fleet-router/internal/config"
)

func TestImport_Errors(t *testing.T) {
	t.Run("file not found", func(t *testing.T) {
		_, _, err := Import("non-existent-file-path-12345.yaml", "self")
		if err == nil {
			t.Error("expected error for non-existent file, got nil")
		}
	})

	t.Run("invalid yaml", func(t *testing.T) {
		tmpDir := t.TempDir()
		path := filepath.Join(tmpDir, "invalid.yaml")
		if err := os.WriteFile(path, []byte("invalid: yaml: : structure"), 0644); err != nil {
			t.Fatal(err)
		}
		_, _, err := Import(path, "self")
		if err == nil {
			t.Error("expected error for invalid yaml, got nil")
		}
	})
}

func TestImport_Success(t *testing.T) {
	tmpDir := t.TempDir()
	path := filepath.Join(tmpDir, "fleet.yaml")

	yamlContent := `
fleet:
  nodes:
    - id: node-local
      host: local
      aliases: ["alias-local"]
      auth_env: "NODE_AUTH"
      endpoints:
        - name: ep-local
          model: "llama3"
          url: "http://127.0.0.1:11434"
          ctx: 2048
          slots: 5
        - name: ep-local-key
          model: "llama3"
          url: "http://127.0.0.1:11434"
          api_key: "secret-api-key"
        - name: strata
          model: "llama3"
          url: "http://127.0.0.1:11434"
    - id: node-remote
      host: remote-host
      endpoints:
        - name: ep-remote
          model: "mistral"
          url: "http://remote-host:11434/v1"
    - id: node-gateway
      gateway: "http://gateway:8080/"
      endpoints:
        - name: ep-gw
          model: "gpt4"
          url: "http://something"
  routing:
    role-chat:
      preferred: "alias-local/llama3"
      fallback: "remote-host/mistral"
    role-embed:
      preferred: "local/llama3"
    role-bad:
      preferred: "none/none"
`
	if err := os.WriteFile(path, []byte(yamlContent), 0644); err != nil {
		t.Fatal(err)
	}

	// We set self to "alias-local" to trigger the direct URL logic for the local node via its alias
	cfg, notes, err := Import(path, "alias-local")
	if err != nil {
		t.Fatalf("Import failed: %v", err)
	}

	// 1. Assert Endpoints
	if len(cfg.Endpoints) != 5 {
		t.Errorf("expected 5 endpoints, got %d", len(cfg.Endpoints))
	}

	// Check ep-local (Direct connection because machine == self)
	var epLocal *config.Endpoint
	for i := range cfg.Endpoints {
		if cfg.Endpoints[i].Name == "ep-local" {
			epLocal = &cfg.Endpoints[i]
		}
	}
	if epLocal == nil {
		t.Fatal("ep-local not found")
	}
	if epLocal.Node != "alias-local" {
		t.Errorf("expected node alias-local, got %s", epLocal.Node)
	}
	if epLocal.URL != "http://127.0.0.1:11434" {
		t.Errorf("expected URL http://127.0.0.1:11434, got %s", epLocal.URL)
	}
	if epLocal.Ctx != 2048 {
		t.Errorf("expected ctx 2048, got %d", epLocal.Ctx)
	}
	if epLocal.Slots != 5 {
		t.Errorf("expected slots 5, got %d", epLocal.Slots)
	}
	if epLocal.KeyEnv != "NODE_AUTH" {
		t.Errorf("expected KeyEnv NODE_AUTH, got %s", epLocal.KeyEnv)
	}

	// Check ep-local-key (Verify plaintext api_key note)
	var epKeyNoteFound bool
	for _, note := range notes {
		if strings.Contains(note, "ep-local-key: plaintext api_key skipped") {
			epKeyNoteFound = true
		}
	}
	if !epKeyNoteFound {
		t.Error("expected note for plaintext api_key, but not found")
	}

	// Check ep-remote (SSH/Remote logic because machine != self)
	var epRemote *config.Endpoint
	for i := range cfg.Endpoints {
		if cfg.Endpoints[i].Name == "ep-remote" {
			epRemote = &cfg.Endpoints[i]
		}
	}
	if epRemote == nil {
		t.Fatal("ep-remote not found")
	}
	if epRemote.SSH != "remote-host" {
		t.Errorf("expected SSH remote-host, got %s", epRemote.SSH)
	}
	// url is http://remote-host:11434/v1, hp becomes remote-host:11434
	if epRemote.Remote != "remote-host:11434" {
		t.Errorf("expected remote remote-host:11434, got %s", epRemote.Remote)
	}

	// Check ep-gw (Gateway logic)
	var epGw *config.Endpoint
	for i := range cfg.Endpoints {
		if cfg.Endpoints[i].Name == "ep-gw" {
			epGw = &cfg.Endpoints[i]
		}
	}
	if epGw == nil {
		t.Fatal("ep-gw not found")
	}
	if epGw.URL != "http://gateway:8080" {
		t.Errorf("expected URL http://gateway:8080, got %s", epGw.URL)
	}
	if epGw.Probe != "chat" {
		t.Errorf("expected probe chat, got %s", epGw.Probe)
	}

	// 2. Assert Discovery
	if len(cfg.Discover.SSH) == 0 || cfg.Discover.SSH[0] != "remote-host" {
		t.Errorf("expected discovery SSH to contain remote-host, got %v", cfg.Discover.SSH)
	}

	// 3. Assert Roles
	// role-chat: preferred alias-local/llama3 -> resolves to ep-local
	chatRole, ok := cfg.Roles["role-chat"]
	if !ok {
		t.Fatal("role-chat not found")
	}
	if chatRole.Preferred != "ep-local" {
		t.Errorf("expected role-chat preferred ep-local, got %s", chatRole.Preferred)
	}
	if len(chatRole.Fallback) != 1 || chatRole.Fallback[0] != "ep-remote" {
		t.Errorf("expected role-chat fallback [ep-remote], got %v", chatRole.Fallback)
	}

	// role-embed: preferred local/llama3 -> resolves to "strata" via hardcoded check
	embedRole, ok := cfg.Roles["role-embed"]
	if !ok {
		t.Fatal("role-embed not found")
	}
	if embedRole.Preferred != "strata" {
		t.Errorf("expected role-embed preferred strata, got %s", embedRole.Preferred)
	}

	// role-bad: no endpoint found -> should produce a note
	var roleBadNoteFound bool
	for _, note := range notes {
		if strings.Contains(note, "role role-bad: no endpoint found for \"none/none\"") {
			roleBadNoteFound = true
		}
	}
	if !roleBadNoteFound {
		t.Error("expected note for role-bad, but not found")
	}
}
