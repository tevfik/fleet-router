package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func secretDirs(t *testing.T) string {
	t.Helper()
	d := t.TempDir()
	t.Setenv("XDG_CONFIG_HOME", d)
	t.Setenv("HOME", d)
	t.Setenv("FLEET_API_KEY", "")
	return filepath.Join(d, "fleet", ".env")
}

// fleet key rm used to leave a 0-byte .env behind; now the file goes with its last line, and a set .env is 0600
// even over a file that existed with a looser mode.
func TestEnvFileLifecycle(t *testing.T) {
	env := secretDirs(t)
	if err := os.MkdirAll(filepath.Dir(env), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(env, []byte("OTHER=1\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := SetSecret("FLEET_API_KEY", "k-123456789"); err != nil {
		t.Fatal(err)
	}
	st, err := os.Stat(env)
	if err != nil || st.Mode().Perm() != 0o600 {
		t.Fatalf("the .env must be 0600, got %v %v", st, err)
	}
	if err := RemoveSecret("OTHER"); err != nil {
		t.Fatal(err)
	}
	if err := RemoveSecret("FLEET_API_KEY"); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(env); !os.IsNotExist(err) {
		t.Fatalf("no 0-byte .env may remain after the last secret is removed: %v", err)
	}
	if _, err := os.Stat(env + ".tmp"); !os.IsNotExist(err) {
		t.Fatal("temp file left behind")
	}
}

func TestSetSecretRefusesEmptyAndMultiline(t *testing.T) {
	env := secretDirs(t)
	for _, v := range []string{"", "  ", "a\nB=c"} {
		if err := SetSecret("FLEET_API_KEY", v); err == nil {
			t.Errorf("value %q must be refused", v)
		}
	}
	if _, err := os.Stat(env); !os.IsNotExist(err) {
		t.Fatal("a refused value must not create the file")
	}
}

func TestSetSecretReplacesAndKeepsOthers(t *testing.T) {
	env := secretDirs(t)
	_ = os.MkdirAll(filepath.Dir(env), 0o700)
	_ = os.WriteFile(env, []byte("# note\nA=1\nexport FLEET_API_KEY=old\nFLEET_API_KEY=dup\n"), 0o600)
	if err := SetSecret("FLEET_API_KEY", "new-value"); err != nil {
		t.Fatal(err)
	}
	b, _ := os.ReadFile(env)
	if got := string(b); got != "# note\nA=1\nFLEET_API_KEY=new-value\n" {
		t.Fatalf("unexpected .env: %q", got)
	}
}

// Order: env > .env > config hub_api_key > api_key_env; the source is reported without the value.
func TestResolveHubAPIKeyOrder(t *testing.T) {
	env := secretDirs(t)
	t.Setenv("MYKEY", "from-api-key-env")
	cfg := &Config{HubAPIKey: "from-config", APIKeyEnv: "MYKEY"}
	if k, src := ResolveHubAPIKey(cfg); k != "from-config" || !strings.Contains(src, "config") {
		t.Fatalf("config before api_key_env: %q %q", k, src)
	}
	cfg.HubAPIKey = ""
	if k, src := ResolveHubAPIKey(cfg); k != "from-api-key-env" || !strings.Contains(src, "api_key_env") {
		t.Fatalf("api_key_env last: %q %q", k, src)
	}
	cfg.HubAPIKey = "from-config"
	_ = os.MkdirAll(filepath.Dir(env), 0o700)
	_ = os.WriteFile(env, []byte("export FLEET_API_KEY='from-dotenv'\n"), 0o600)
	if k, src := ResolveHubAPIKey(cfg); k != "from-dotenv" || src != env {
		t.Fatalf(".env before config: %q %q", k, src)
	}
	t.Setenv("FLEET_API_KEY", "from-env")
	if k, src := ResolveHubAPIKey(cfg); k != "from-env" || src != "env" {
		t.Fatalf("env first: %q %q", k, src)
	}
	if k, src := ResolveHubAPIKey(nil); k != "from-env" || src != "env" {
		t.Fatalf("nil config: %q %q", k, src)
	}
}

func TestMaskSecretNeverShowsMost(t *testing.T) {
	for _, v := range []string{"short", "0123456789abcdef", "sk-very-long-secret-value-0123456789"} {
		m := MaskSecret(v)
		if strings.Contains(m, v) || (len(v) >= 16 && strings.Contains(m, v[:6])) || (len(v) < 16 && strings.Contains(m, v[1:])) {
			t.Errorf("mask %q leaks %q", m, v)
		}
	}
	if MaskSecret("") != "(not set)" {
		t.Error("empty")
	}
}
