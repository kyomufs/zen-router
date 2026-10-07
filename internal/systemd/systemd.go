// Package systemd renders, installs and removes the zen-router systemd user
// unit (plan Task 8, spec §8): `ExecStart=<absolute binary> up` in the
// foreground — journald owns stderr, so NEVER `--detach` (Task 3 ruling) —
// plus the `systemctl --user` reload/enable/disable drive.
//
// SAFETY (spec §12): nothing here runs by itself; it is only reached through
// the `zen-router install-systemd` subcommand. The tests exercise it against
// a fake `systemctl` at the front of PATH and a temp HOME/XDG_CONFIG_HOME;
// running the command against the live environment stays behind the отмашка.
package systemd

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
)

// UnitName is both the on-disk file name and the systemd unit name.
const UnitName = "zen-router.service"

// serviceEnvironment is the Environment= line of the unit. systemd user
// managers start with a minimal PATH that lacks both directories this daemon
// actually execs from on NixOS (spec §14:352-353): internal/warp/tunnel.go
// runs `sudo ip …`, where `sudo` is the setuid wrapper under
// /run/wrappers/bin and `ip` is /run/current-system/sw/bin/ip. /usr/bin and
// /bin keep the unit usable off NixOS.
const serviceEnvironment = "Environment=PATH=/run/wrappers/bin:/run/current-system/sw/bin:/usr/bin:/bin"

// Render returns the unit file content for an absolute binary path (spec §8):
// foreground `up`, Restart=on-failure, RestartSec, the NixOS PATH above and
// WantedBy=default.target so the unit tracks the graphical/user session.
func Render(binary string) string {
	var b strings.Builder
	b.WriteString("[Unit]\n")
	b.WriteString("Description=zen-router local gateway for OpenCode Zen with WARP IP rotation\n")
	b.WriteString("\n[Service]\n")
	fmt.Fprintf(&b, "ExecStart=%s up\n", binary)
	b.WriteString("Restart=on-failure\n")
	b.WriteString("RestartSec=5\n")
	b.WriteString(serviceEnvironment)
	b.WriteString("\n\n[Install]\n")
	b.WriteString("WantedBy=default.target\n")
	return b.String()
}

// UnitPath resolves $XDG_CONFIG_HOME/systemd/user/zen-router.service,
// falling back to $HOME/.config per the XDG base-directory specification
// (same convention as config.DefaultPaths) — spec §8's
// ~/.config/systemd/user/zen-router.service.
func UnitPath() (string, error) {
	configHome := os.Getenv("XDG_CONFIG_HOME")
	if configHome == "" {
		home, err := os.UserHomeDir()
		if err != nil {
			return "", fmt.Errorf("resolve home directory (set HOME or XDG_CONFIG_HOME): %w", err)
		}
		configHome = filepath.Join(home, ".config")
	}
	return filepath.Join(configHome, "systemd", "user", UnitName), nil
}

// systemctl runs `systemctl --user <args...>`, resolved through PATH — the
// exec seam the tests replace with a recording stub.
func systemctl(args ...string) error {
	full := append([]string{"--user"}, args...)
	cmd := exec.Command("systemctl", full...)
	out, err := cmd.CombinedOutput()
	if err == nil {
		return nil
	}
	if msg := strings.TrimSpace(string(out)); msg != "" {
		return fmt.Errorf("systemctl %s: %w: %s", strings.Join(full, " "), err, msg)
	}
	return fmt.Errorf("systemctl %s: %w", strings.Join(full, " "), err)
}

// Install writes the unit file (overwriting any previous copy — re-running
// is idempotent) and then runs `systemctl --user daemon-reload` followed by
// `systemctl --user enable --now` (spec §8). It returns the unit path, also
// on a systemctl failure (the file WAS written; the caller reports the error).
func Install(binary string) (string, error) {
	if !filepath.IsAbs(binary) {
		return "", fmt.Errorf("binary path must be absolute, got %q", binary)
	}
	path, err := UnitPath()
	if err != nil {
		return "", err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return "", fmt.Errorf("create unit directory: %w", err)
	}
	if err := os.WriteFile(path, []byte(Render(binary)), 0o644); err != nil {
		return "", fmt.Errorf("write unit file: %w", err)
	}
	if err := systemctl("daemon-reload"); err != nil {
		return path, err
	}
	if err := systemctl("enable", "--now", UnitName); err != nil {
		return path, err
	}
	return path, nil
}

// Remove uninstalls the unit (spec §8): `systemctl --user disable --now`
// while the unit file still exists, then the file is unlinked. A missing
// file is a no-op (no systemctl call) — re-running `--remove` is safe.
func Remove() (string, error) {
	path, err := UnitPath()
	if err != nil {
		return "", err
	}
	if _, err := os.Stat(path); err != nil {
		if os.IsNotExist(err) {
			return path, nil
		}
		return path, fmt.Errorf("stat unit file: %w", err)
	}
	if err := systemctl("disable", "--now", UnitName); err != nil {
		return path, err
	}
	if err := os.Remove(path); err != nil && !os.IsNotExist(err) {
		return path, fmt.Errorf("remove unit file: %w", err)
	}
	return path, nil
}
