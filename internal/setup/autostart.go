package setup

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
)

// Profile autostart: a systemd --user oneshot unit per profile that runs `fleet profile up <name>` after fleet.service,
// so a model comes back after a reboot. The deploy itself puts the model processes in their own systemd scope; the unit
// only (re)issues the deploy and stops it again with `fleet profile down` (ExecStop).

// ProfileUnitPrefix names the units: fleet-profile-<name>.service.
const ProfileUnitPrefix = "fleet-profile-"

// SystemctlUser and LoginctlLinger are the seams of the host commands (tests replace them).
var (
	SystemctlUser = func(args ...string) ([]byte, error) {
		return exec.Command("systemctl", append([]string{"--user"}, args...)...).CombinedOutput()
	}
	LoginctlLinger = func() (string, error) {
		user := os.Getenv("USER")
		if user == "" {
			return "", fmt.Errorf("USER is not set")
		}
		out, err := exec.Command("loginctl", "show-user", user, "-p", "Linger", "--value").Output()
		return strings.TrimSpace(string(out)), err
	}
)

// UserUnitDir is ~/.config/systemd/user (XDG_CONFIG_HOME respected).
func UserUnitDir() string {
	if d := os.Getenv("XDG_CONFIG_HOME"); d != "" {
		return filepath.Join(d, "systemd", "user")
	}
	return filepath.Join(home(), ".config", "systemd", "user")
}

// ProfileUnitName is the unit of a profile.
func ProfileUnitName(name string) string { return ProfileUnitPrefix + name + ".service" }

// ProfileUnitPath is where the unit file of a profile lives.
func ProfileUnitPath(name string) string { return filepath.Join(UserUnitDir(), ProfileUnitName(name)) }

// ProfileUnit renders the unit. exe is the fleet binary; env carries FLEET_CONFIG / FLEET_PROFILES when they are set,
// so the unit sees the same configuration as the shell that enabled it.
func ProfileUnit(exe, name string, env map[string]string) string {
	var b strings.Builder
	fmt.Fprintf(&b, `# managed by fleet (fleet profile autostart %[2]s on|off)
[Unit]
Description=fleet profile %[2]s (deploy at start)
Wants=fleet.service network-online.target
After=fleet.service network-online.target

[Service]
Type=oneshot
RemainAfterExit=yes
# a large model takes minutes to load: the deploy waits for it
TimeoutStartSec=1h
ExecStart=%[1]s profile up %[2]s
ExecStop=%[1]s profile down %[2]s
Environment=PATH=%[3]s
`, exe, name, os.Getenv("PATH"))
	keys := make([]string, 0, len(env))
	for k := range env {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		fmt.Fprintf(&b, "Environment=%s=%s\n", k, env[k])
	}
	b.WriteString(`
[Install]
WantedBy=default.target
`)
	return b.String()
}

// LingerHint says what a missing linger means for autostart; "" when linger is on. It only reads loginctl: enabling
// linger changes the login policy of the account, so that stays the user's decision.
func LingerHint() string {
	v, err := LoginctlLinger()
	switch {
	case err != nil:
		return fmt.Sprintf("could not read the linger setting (%v): without linger, user services start only when you log in — enable it with `loginctl enable-linger %s`", err, os.Getenv("USER"))
	case v == "yes":
		return ""
	default:
		return fmt.Sprintf("linger is off (Linger=%s): user services (fleet.service, this unit) start only when you log in, not at boot — run `loginctl enable-linger %s` once (fleet does not change this for you)", v, os.Getenv("USER"))
	}
}

// ProfileAutostartOn writes and enables the unit. It does not start it: `enable` without --now, so a profile that is
// already up is not deployed a second time; the unit runs at the next boot (or `systemctl --user start <unit>`).
func ProfileAutostartOn(exe, name string) ([]string, error) {
	env := map[string]string{}
	for _, k := range []string{"FLEET_CONFIG", "FLEET_PROFILES"} {
		if v := os.Getenv(k); v != "" {
			env[k] = v
		}
	}
	p := ProfileUnitPath(name)
	if iss := checkDirSymlinks(filepath.Dir(p)); iss != nil {
		return nil, iss
	}
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		return nil, err
	}
	if err := os.WriteFile(p, []byte(ProfileUnit(exe, name, env)), 0o644); err != nil {
		return nil, err
	}
	out := []string{"written: " + p}
	for _, a := range [][]string{{"daemon-reload"}, {"enable", ProfileUnitName(name)}} {
		if o, err := SystemctlUser(a...); err != nil {
			return out, fmt.Errorf("systemctl --user %s: %v: %s", strings.Join(a, " "), err, strings.TrimSpace(string(o)))
		}
	}
	out = append(out, "enabled: "+ProfileUnitName(name)+" (runs `fleet profile up "+name+"` at the next boot; start now with `systemctl --user start "+ProfileUnitName(name)+"`)")
	if h := LingerHint(); h != "" {
		out = append(out, "! "+h)
	}
	return out, nil
}

// ProfileAutostartOff disables and removes the unit. The running model is left alone (`fleet profile down` stops it).
func ProfileAutostartOff(name string) ([]string, error) {
	p := ProfileUnitPath(name)
	if _, err := os.Stat(p); os.IsNotExist(err) {
		return []string{"no autostart unit for " + name + " (" + p + ")"}, nil
	}
	// disable first (removes the WantedBy link); a unit systemd never loaded is not an error here
	_, _ = SystemctlUser("disable", ProfileUnitName(name))
	if err := os.Remove(p); err != nil {
		return nil, err
	}
	if o, err := SystemctlUser("daemon-reload"); err != nil {
		return []string{"removed: " + p}, fmt.Errorf("systemctl --user daemon-reload: %v: %s", err, strings.TrimSpace(string(o)))
	}
	return []string{"removed: " + p + " (the running model keeps running: fleet profile down " + name + " stops it)"}, nil
}

// ProfileAutostartEnabled reports whether the unit file of a profile exists.
func ProfileAutostartEnabled(name string) bool {
	_, err := os.Stat(ProfileUnitPath(name))
	return err == nil
}

// AutostartProfiles lists the profiles that have a unit file.
func AutostartProfiles() []string {
	m, _ := filepath.Glob(filepath.Join(UserUnitDir(), ProfileUnitPrefix+"*.service"))
	var out []string
	for _, f := range m {
		out = append(out, strings.TrimSuffix(strings.TrimPrefix(filepath.Base(f), ProfileUnitPrefix), ".service"))
	}
	sort.Strings(out)
	return out
}
