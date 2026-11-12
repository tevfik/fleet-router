package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func isolateKey(t *testing.T) {
	t.Helper()
	isolate(t)
	t.Setenv(HubKeyEnv, "")
	os.Unsetenv(HubKeyEnv) // an exported key would win over the files
}

func TestGenerateHubKeyShapeAndUniqueness(t *testing.T) {
	seen := map[string]bool{}
	for i := 0; i < 50; i++ {
		k := GenerateHubKey()
		if !strings.HasPrefix(k, "flt_hub_") || len(k) != len("flt_hub_")+48 {
			t.Fatalf("shape: %q", k)
		}
		if seen[k] {
			t.Fatal("repeated key")
		}
		seen[k] = true
	}
}

// First start: no key anywhere → one is generated and saved (mode 600); the next start keeps it.
func TestEnsureHubKeyGeneratesOnceAndKeeps(t *testing.T) {
	isolateKey(t)
	cfg := Default()
	k1, gen, err := EnsureHubKey(cfg)
	if err != nil || !gen || k1 == "" {
		t.Fatalf("%q %v %v", k1, gen, err)
	}
	if st, err := os.Stat(filepath.Join(Dir(), ".env")); err != nil || st.Mode().Perm() != 0o600 {
		t.Fatalf("the key file must be 0600: %v %v", st, err)
	}
	cfg2, _ := Load(Path())
	k2, gen2, err := EnsureHubKey(cfg2)
	if err != nil || gen2 || k2 != k1 {
		t.Fatalf("a second start must keep the key: %q %v %v", k2, gen2, err)
	}
}

func TestEnsureHubKeyKeepsAnExistingOne(t *testing.T) {
	isolateKey(t)
	if err := SaveHubKey("mine-already"); err != nil {
		t.Fatal(err)
	}
	cfg, _ := Load(Path())
	k, gen, err := EnsureHubKey(cfg)
	if err != nil || gen || k != "mine-already" {
		t.Fatalf("%q %v %v", k, gen, err)
	}
}
