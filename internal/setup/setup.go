// Package setup holds the install actions — binary install, Claude Code / OpenCode registration, systemd service.
// Both the CLI (`fleet setup …`) and the TUI call the same functions.
package setup

import (
	"encoding/json"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"
)

func Exe() (string, error) {
	p, err := os.Executable()
	if err != nil {
		return "", err
	}
	return filepath.EvalSymlinks(p)
}

func home() string { h, _ := os.UserHomeDir(); return h }

// InstallBinary copies the running binary into dir (default ~/.local/bin). Uses tmp+rename to avoid "text file busy".
func InstallBinary(dir string) (string, error) {
	if dir == "" {
		dir = filepath.Join(home(), ".local", "bin")
	}
	exe, err := Exe()
	if err != nil {
		return "", err
	}
	dst := filepath.Join(dir, "fleet")
	if abs, _ := filepath.Abs(dst); abs == exe {
		return dst, nil
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return "", err
	}
	src, err := os.Open(exe)
	if err != nil {
		return "", err
	}
	defer src.Close()
	tmp := dst + ".tmp"
	out, err := os.OpenFile(tmp, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0o755)
	if err != nil {
		return "", err
	}
	if _, err := io.Copy(out, src); err != nil {
		out.Close()
		os.Remove(tmp)
		return "", err
	}
	out.Close()
	return dst, os.Rename(tmp, dst)
}

// Claude runs `claude mcp add --scope user fleet -- <exe> mcp` (removes an existing entry first → idempotent).
func Claude(exe, listen string) (string, error) {
	if _, err := exec.LookPath("claude"); err != nil {
		return "", fmt.Errorf("`claude` is not in PATH")
	}
	_ = exec.Command("claude", "mcp", "remove", "fleet", "--scope", "user").Run()
	args := []string{"mcp", "add", "--scope", "user"}
	if listen != "" {
		args = append(args, "-e", "FLEET_LISTEN="+listen)
	}
	args = append(args, "fleet", "--", exe, "mcp")
	out, err := exec.Command("claude", args...).CombinedOutput()
	return strings.TrimSpace(string(out)), err
}

// OpenCode merges the provider into ~/.config/opencode/opencode.json → providers.fleet (with a backup).
// Note: the JSON is rewritten; key order/whitespace may change — backup is <file>.bak.<time>.
func OpenCode(provider map[string]any) (string, error) {
	path := filepath.Join(home(), ".config", "opencode", "opencode.json")
	if iss := checkDirSymlinks(filepath.Dir(path)); iss != nil {
		return "", iss
	}
	if iss := checkFileSymlink(path); iss != nil {
		return "", iss
	}
	root := map[string]any{}
	b, err := os.ReadFile(path)
	switch {
	case err == nil:
		if err := json.Unmarshal(b, &root); err != nil {
			return "", fmt.Errorf("could not parse %s (may be JSONC): %w", path, err)
		}
		bak := fmt.Sprintf("%s.bak.%s", path, time.Now().Format("20060102-150405"))
		if err := os.WriteFile(bak, b, 0o600); err != nil {
			return "", err
		}
	case os.IsNotExist(err):
		os.MkdirAll(filepath.Dir(path), 0o755)
	default:
		return "", err
	}
	provs, _ := root["providers"].(map[string]any)
	if provs == nil {
		provs = map[string]any{}
	}
	for k, v := range provider {
		provs[k] = v
	}
	root["providers"] = provs
	out, _ := json.MarshalIndent(root, "", "  ")
	if err := os.WriteFile(path, append(out, '\n'), 0o600); err != nil {
		return "", err
	}
	return path, nil
}

const unit = `[Unit]
Description=fleet — model fleet router + SSH tunnels
After=network-online.target

[Service]
ExecStart=%s serve
Restart=on-failure
RestartSec=3
Environment=PATH=%s

[Install]
WantedBy=default.target
`

func ServiceInstall(exe string) (string, error) {
	dir := filepath.Join(home(), ".config", "systemd", "user")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return "", err
	}
	p := filepath.Join(dir, "fleet.service")
	path := os.Getenv("PATH")
	if err := os.WriteFile(p, []byte(fmt.Sprintf(unit, exe, path)), 0o644); err != nil {
		return "", err
	}
	for _, a := range [][]string{{"daemon-reload"}, {"enable", "--now", "fleet.service"}} {
		if out, err := exec.Command("systemctl", append([]string{"--user"}, a...)...).CombinedOutput(); err != nil {
			return p, fmt.Errorf("systemctl %v: %s", a, strings.TrimSpace(string(out)))
		}
	}
	return p, nil
}

const systemUnit = `[Unit]
Description=fleet — model fleet router + SSH tunnels
Wants=network-online.target
After=network-online.target

[Service]
User=%s
Environment=HOME=%s
Environment=PATH=/usr/local/bin:/usr/bin:/bin
ExecStart=%s serve
Restart=on-failure
RestartSec=3

[Install]
WantedBy=multi-user.target
`

// SystemUser is the account the system-wide service runs as: the one that invoked sudo (its ~/.ssh and
// ~/.config/fleet are what the daemon needs), else the current one. Running as plain root is refused.
func SystemUser() (name, homeDir string, err error) {
	name = os.Getenv("SUDO_USER")
	if name == "" {
		name = os.Getenv("USER")
	}
	if name == "" || name == "root" {
		return "", "", fmt.Errorf("run it as `sudo fleet setup service --system` from the account whose ~/.ssh and ~/.config/fleet the daemon should use")
	}
	out, err := exec.Command("getent", "passwd", name).Output()
	if err != nil {
		return "", "", fmt.Errorf("getent passwd %s: %v", name, err)
	}
	f := strings.Split(strings.TrimSpace(string(out)), ":")
	if len(f) < 6 {
		return "", "", fmt.Errorf("unexpected passwd entry for %s", name)
	}
	return name, f[5], nil
}

// SystemServiceInstall writes /etc/systemd/system/fleet.service (starts at boot, no login needed) and enables it.
// exe should be a system-wide path (see InstallBinary with /usr/local/bin).
func SystemServiceInstall(exe string) (string, error) {
	if os.Geteuid() != 0 {
		return "", fmt.Errorf("a system-wide service needs root: sudo fleet setup service --system")
	}
	user, h, err := SystemUser()
	if err != nil {
		return "", err
	}
	p := "/etc/systemd/system/fleet.service"
	if err := os.WriteFile(p, []byte(fmt.Sprintf(systemUnit, user, h, exe)), 0o644); err != nil {
		return "", err
	}
	for _, a := range [][]string{{"daemon-reload"}, {"enable", "--now", "fleet.service"}} {
		if out, err := exec.Command("systemctl", a...).CombinedOutput(); err != nil {
			return p, fmt.Errorf("systemctl %v: %s", a, strings.TrimSpace(string(out)))
		}
	}
	return p, nil
}
