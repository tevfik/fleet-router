package archcert

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestEmbeddedParses(t *testing.T) {
	c, err := Load()
	if err != nil || len(c.Archs) == 0 {
		t.Fatalf("%v %v", c, err)
	}
}

func TestEmbeddedMatchesEngineRecord(t *testing.T) {
	// the copy is synced from fleet-engine in the private repository only (the public export renames hosts in it)
	if _, err := os.Stat("../../private.env"); err != nil {
		t.Skip("not the private fleet repository")
	}
	var b []byte
	var err error
	for _, cand := range []string{os.Getenv("FLEET_ENGINE_DIR"), "../../../fleet-engine", "../../../../fleet-engine"} {
		if cand == "" {
			continue
		}
		b, err = os.ReadFile(filepath.Join(cand, "docs", "arch-cert.json"))
		if err == nil {
			break
		}
	}
	if err != nil {
		t.Skip("fleet-engine checkout not beside fleet")
	}
	if string(b) != string(embedded) {
		t.Errorf("internal/archcert/arch-cert.json differs from fleet-engine/docs/arch-cert.json: copy it (make sync-arch-cert)")
	}
}

func TestWarn(t *testing.T) {
	f := false
	tr := true
	c := &Cert{Archs: map[string]Entry{
		"good":  {Status: Certified},
		"lossy": {Status: Certified, LossyWireOK: &f},
		"fine":  {Status: Certified, LossyWireOK: &tr},
		"bad":   {Status: Failed},
	}}
	for _, tc := range []struct {
		arch, wire, want string // want: substring of the warning, "" = none
	}{
		{"good", "", ""},
		{"good", "f32", ""},
		{"good", "f16", "never gated"},
		{"lossy", "fp8", "FAILED the gate"},
		{"fine", "f16", ""},
		{"bad", "", "FAILED the ring"},
		{"new", "", "has not passed"},
		{"", "", "unknown"},
	} {
		got := c.Warn(tc.arch, tc.wire)
		if (tc.want == "") != (got == "") || !strings.Contains(got, tc.want) {
			t.Errorf("%s/%s: got %q want %q", tc.arch, tc.wire, got, tc.want)
		}
	}
}

func TestLoadCustomFile(t *testing.T) {
	dir := t.TempDir()
	validPath := filepath.Join(dir, "custom-cert.json")
	customJSON := `{
		"archs": {
			"custom_arch": {
				"kind": "dense",
				"status": "certified",
				"requires_env": "CUSTOM_ENV=1"
			}
		}
	}`
	if err := os.WriteFile(validPath, []byte(customJSON), 0o600); err != nil {
		t.Fatalf("failed to write custom cert: %v", err)
	}

	t.Run("valid custom cert", func(t *testing.T) {
		t.Setenv("FLEET_ARCH_CERT", validPath)
		c, err := Load()
		if err != nil {
			t.Fatalf("Load failed: %v", err)
		}
		st, entry := c.Status("custom_arch")
		if st != Certified || entry.RequiresEnv != "CUSTOM_ENV=1" {
			t.Errorf("unexpected status or entry: st=%v, entry=%+v", st, entry)
		}
	})

	t.Run("non-existent custom cert", func(t *testing.T) {
		t.Setenv("FLEET_ARCH_CERT", filepath.Join(dir, "does-not-exist.json"))
		c, err := Load()
		if err == nil {
			t.Fatalf("expected error for non-existent file, got %v", c)
		}
	})

	t.Run("malformed json custom cert", func(t *testing.T) {
		badPath := filepath.Join(dir, "bad.json")
		if err := os.WriteFile(badPath, []byte("invalid-json{{{"), 0o600); err != nil {
			t.Fatal(err)
		}
		t.Setenv("FLEET_ARCH_CERT", badPath)
		c, err := Load()
		if err == nil {
			t.Fatalf("expected error for malformed json, got %v", c)
		}
	})
}

func TestPackageWarn(t *testing.T) {
	t.Run("default cert warn", func(t *testing.T) {
		t.Setenv("FLEET_ARCH_CERT", "")
		// qwen35 is certified in embedded arch-cert.json
		warn := Warn("qwen35", "")
		if warn != "" {
			t.Errorf("expected empty warning for certified qwen35 f32, got %q", warn)
		}
		// non-existent arch should warn
		warnUnknown := Warn("unknown-model-arch", "")
		if !strings.Contains(warnUnknown, "has not passed the ring correctness gate") {
			t.Errorf("unexpected warning for unknown arch: %q", warnUnknown)
		}
	})

	t.Run("load error returns empty warning", func(t *testing.T) {
		t.Setenv("FLEET_ARCH_CERT", "/path/to/definitely/nonexistent/file.json")
		if got := Warn("qwen35", ""); got != "" {
			t.Errorf("expected empty string on Load() error, got %q", got)
		}
	})
}

func TestRequiredEnv(t *testing.T) {
	t.Run("c.RequiredEnv variations", func(t *testing.T) {
		c := &Cert{
			Archs: map[string]Entry{
				"uncert":      {Status: Uncertified, RequiresEnv: "FOO=1"},
				"failed":      {Status: Failed, RequiresEnv: "FOO=1"},
				"cert_no_env": {Status: Certified, RequiresEnv: ""},
				"cert_with_env": {
					Status:      Certified,
					RequiresEnv: "  KEY1=val1   INVALID_TOKEN   KEY2=val2\tKEY3=val3  ",
				},
			},
		}

		if got := c.RequiredEnv("uncert"); got != nil {
			t.Errorf("expected nil for uncertified arch, got %v", got)
		}
		if got := c.RequiredEnv("failed"); got != nil {
			t.Errorf("expected nil for failed arch, got %v", got)
		}
		if got := c.RequiredEnv("cert_no_env"); len(got) != 0 {
			t.Errorf("expected empty/nil for cert_no_env, got %v", got)
		}

		env := c.RequiredEnv("cert_with_env")
		want := []string{"KEY1=val1", "KEY2=val2", "KEY3=val3"}
		if len(env) != len(want) {
			t.Fatalf("expected %v, got %v", want, env)
		}
		for i := range want {
			if env[i] != want[i] {
				t.Errorf("env[%d] = %q, want %q", i, env[i], want[i])
			}
		}
	})

	t.Run("package RequiredEnv embedded cert", func(t *testing.T) {
		t.Setenv("FLEET_ARCH_CERT", "")
		// In embedded cert, qwen4exp requires GGML_CUDA_DISABLE_FUSION=1
		env := RequiredEnv("qwen4exp")
		if len(env) != 1 || env[0] != "GGML_CUDA_DISABLE_FUSION=1" {
			t.Errorf("unexpected RequiredEnv for qwen4exp: %v", env)
		}

		// qwen35 has no required env
		if env35 := RequiredEnv("qwen35"); len(env35) != 0 {
			t.Errorf("expected empty env for qwen35, got %v", env35)
		}
	})

	t.Run("package RequiredEnv load error", func(t *testing.T) {
		t.Setenv("FLEET_ARCH_CERT", "/path/to/definitely/nonexistent/file.json")
		if got := RequiredEnv("qwen4exp"); got != nil {
			t.Errorf("expected nil on Load() error, got %v", got)
		}
	})
}
