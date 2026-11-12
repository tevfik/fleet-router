//go:build !llamaring

package ringengine

import (
	"strings"
	"testing"
)

func TestStubEngine(t *testing.T) {
	if Available() {
		t.Errorf("expected Available() == false in stub build")
	}
	if got := Version(); got != "not built in" {
		t.Errorf("Version() = %q, want %q", got, "not built in")
	}
	if got := Info(); got != "" {
		t.Errorf("Info() = %q, want empty", got)
	}
	if code := RunWorker([]string{"--foo"}); code != 127 {
		t.Errorf("RunWorker() = %d, want 127", code)
	}
	if code := RunServer([]string{"--foo"}); code != 127 {
		t.Errorf("RunServer() = %d, want 127", code)
	}
	if code := RunChainServe([]string{"--foo"}); code != 127 {
		t.Errorf("RunChainServe() = %d, want 127", code)
	}
	if code := RunChainDriver([]string{"--foo"}); code != 127 {
		t.Errorf("RunChainDriver() = %d, want 127", code)
	}
	if ErrNotBuilt == nil || !strings.Contains(ErrNotBuilt.Error(), "llama-ring engine") {
		t.Errorf("unexpected ErrNotBuilt: %v", ErrNotBuilt)
	}
}
