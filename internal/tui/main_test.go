package tui

import (
	"os"
	"testing"

	"github.com/tevfik/fleet-router/internal/feature"
)

// The package's tests cover the full fleet (with fleet-engine); the router-only fleet has its own tests (lite_test.go).
func TestMain(m *testing.M) {
	feature.SetRunner(true)
	os.Exit(m.Run())
}
