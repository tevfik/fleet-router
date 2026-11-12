package deploy

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/tevfik/fleet-router/internal/config"
)

// A configuration that does not parse must not be replaced by the defaults plus the new endpoint.
func TestRegisterEndpoint_UnreadableConfigIsKept(t *testing.T) {
	p := filepath.Join(t.TempDir(), "config.yaml")
	bad := []byte("listen: 127.0.0.1:4711\nendpoints:\n  - name: keep-me\n    url: [unclosed\n")
	if err := os.WriteFile(p, bad, 0o600); err != nil {
		t.Fatal(err)
	}
	_, _, err := RegisterEndpointInConfig(p, config.Endpoint{Name: "new", URL: "http://127.0.0.1:1/v1", Model: "m"}, "", nil, true)
	if err == nil {
		t.Fatal("an unreadable configuration must be an error")
	}
	if got, _ := os.ReadFile(p); string(got) != string(bad) {
		t.Fatalf("the file was rewritten:\n%s", got)
	}
}
