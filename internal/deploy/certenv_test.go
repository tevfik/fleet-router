package deploy

import (
	"github.com/tevfik/fleet-router/internal/config"
	"os"
	"path/filepath"
	"reflect"
	"testing"

	"github.com/tevfik/fleet-router/internal/plan"
)

func TestRingCertEnvOnlyForARingAndACertifiedArchWithRequirements(t *testing.T) {
	f := filepath.Join(t.TempDir(), "cert.json")
	js := `{"archs":{"qwen4exp":{"kind":"moe","status":"certified","requires_env":"GGML_CUDA_DISABLE_FUSION=1","runs":[]},
	"plain":{"kind":"dense","status":"certified","runs":[]},
	"bad":{"kind":"moe","status":"failed","requires_env":"X=1","runs":[]}}}`
	if err := os.WriteFile(f, []byte(js), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("FLEET_ARCH_CERT", f)
	if got := ringCertEnv(&plan.Model{Arch: "qwen4exp"}, true); !reflect.DeepEqual(got, []string{"GGML_CUDA_DISABLE_FUSION=1"}) {
		t.Fatalf("ring + certified with requirements: %v", got)
	}
	if got := ringCertEnv(&plan.Model{Arch: "qwen4exp"}, false); got != nil {
		t.Fatalf("a single machine needs none: %v", got)
	}
	if got := ringCertEnv(&plan.Model{Arch: "plain"}, true); got != nil {
		t.Fatalf("no requirement: %v", got)
	}
	if got := ringCertEnv(&plan.Model{Arch: "bad"}, true); got != nil {
		t.Fatalf("a failed architecture gets no environment promise: %v", got)
	}
	if got := ringCertEnv(nil, true); got != nil {
		t.Fatalf("%v", got)
	}
}

func TestPlanRefusesPathsThatNeedShellQuoting(t *testing.T) {
	stubPlanner(t, "llama")
	for _, bad := range []string{"/m/a b.gguf", "/m/a;id.gguf", "/m/$(id).gguf", "/m/`id`.gguf"} {
		if _, err := PlanDeployment(config.Default(), DeployOptions{ModelPath: bad, DryRun: true}); err == nil {
			t.Errorf("model path %q must be refused", bad)
		}
	}
	if _, err := PlanDeployment(config.Default(), DeployOptions{ModelPath: "/m/ok.gguf", MMProj: "/m/x y.gguf", DryRun: true}); err == nil {
		t.Error("a mmproj path with a space must be refused")
	}
}
