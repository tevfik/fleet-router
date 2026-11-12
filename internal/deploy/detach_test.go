package deploy

import (
	"strings"
	"testing"
)

func TestDetachedCommandKeepsTheCommand(t *testing.T) {
	bin, args, attr := detachedCommand("/bin/sleep", []string{"5"})
	if attr == nil || !attr.Setsid {
		t.Fatal("must start in its own session")
	}
	if bin == "systemd-run" {
		if strings.Join(args, " ") != "--user --scope --collect --quiet -- /bin/sleep 5" {
			t.Fatalf("args %v", args)
		}
	} else if bin != "/bin/sleep" || len(args) != 1 {
		t.Fatalf("%s %v", bin, args)
	}
	if !strings.Contains(detachShell, "systemd-run --user --scope") || !strings.Contains(detachShell, "setsid") {
		t.Fatal("shell prefix lost its parts")
	}
}
