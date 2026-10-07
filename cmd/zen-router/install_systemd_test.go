package main

// Task 8 (Phase C): the `zen-router install-systemd` subcommand wiring —
// registry dispatch, the `--remove` flag and the usage line (spec §8, §11).
//
// Hermeticity contract (task brief): fake `systemctl` at the FRONT of PATH
// recording argv, HOME/XDG_CONFIG_HOME in fresh t.TempDir()s. These tests
// never reach the live ~/.config/systemd/user unit, the live daemon on
// 127.0.0.1:8787, or ~/.local/bin — the live install is gated behind the
// отмашка (spec §12).

import (
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// stubSystemctl prepends a fake `systemctl` to PATH; it appends each
// invocation's argv (space-joined) to the returned log and exits 0.
func stubSystemctl(t *testing.T) (logPath string) {
	t.Helper()
	dir := t.TempDir()
	logPath = filepath.Join(dir, "systemctl.argv")
	stub := filepath.Join(dir, "systemctl")
	script := fmt.Sprintf("#!/bin/sh\nprintf '%%s\\n' \"$*\" >> %q\nexit 0\n", logPath)
	if err := os.WriteFile(stub, []byte(script), 0o755); err != nil {
		t.Fatalf("write systemctl stub: %v", err)
	}
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))
	return logPath
}

// hermeticEnv points HOME and XDG_CONFIG_HOME into a fresh temp dir.
func hermeticEnv(t *testing.T) {
	t.Helper()
	tmp := t.TempDir()
	t.Setenv("HOME", tmp)
	t.Setenv("XDG_CONFIG_HOME", filepath.Join(tmp, ".config"))
}

// TestInstallSystemdRegistryDispatch runs the real `main()` dispatch on
// `zen-router install-systemd` (os.Args swapped) and proves the registry
// case exists: the unit lands under the temp XDG config home with
// ExecStart=<this executable> up, and the fake systemctl records reload +
// enable --now. A missing registry case would exit(1) and fail the run.
func TestInstallSystemdRegistryDispatch(t *testing.T) {
	hermeticEnv(t)
	logPath := stubSystemctl(t)

	oldArgs := os.Args
	os.Args = []string{"zen-router", "install-systemd"}
	defer func() { os.Args = oldArgs }()
	main()

	exe, err := os.Executable()
	if err != nil {
		t.Fatalf("os.Executable: %v", err)
	}
	unitPath := filepath.Join(os.Getenv("XDG_CONFIG_HOME"), "systemd", "user", "zen-router.service")
	data, err := os.ReadFile(unitPath)
	if err != nil {
		t.Fatalf("unit file not written by registry dispatch: %v", err)
	}
	if !strings.Contains(string(data), "ExecStart="+exe+" up\n") {
		t.Errorf("unit must run THIS executable's foreground `up`, got:\n%s", data)
	}
	if strings.Contains(string(data), "--detach") {
		t.Errorf("unit must not contain --detach (journald path):\n%s", data)
	}

	raw, err := os.ReadFile(logPath)
	if err != nil {
		t.Fatalf("read systemctl argv log: %v", err)
	}
	want := "--user daemon-reload\n--user enable --now zen-router.service\n"
	if string(raw) != want {
		t.Errorf("systemctl argv log =\n%q\nwant\n%q", raw, want)
	}
}

// TestInstallSystemdRemoveFlag: `install-systemd --remove` unlinks the unit
// file and records `systemctl --user disable --now zen-router.service`.
func TestInstallSystemdRemoveFlag(t *testing.T) {
	hermeticEnv(t)
	logPath := stubSystemctl(t)

	unitPath := filepath.Join(os.Getenv("XDG_CONFIG_HOME"), "systemd", "user", "zen-router.service")
	if err := os.MkdirAll(filepath.Dir(unitPath), 0o755); err != nil {
		t.Fatalf("pre-create unit dir: %v", err)
	}
	if err := os.WriteFile(unitPath, []byte("stale unit\n"), 0o644); err != nil {
		t.Fatalf("pre-write unit: %v", err)
	}

	if err := cmdInstallSystemd([]string{"--remove"}); err != nil {
		t.Fatalf("cmdInstallSystemd --remove: %v", err)
	}
	if _, err := os.Stat(unitPath); !os.IsNotExist(err) {
		t.Errorf("unit file still present after --remove (stat err = %v)", err)
	}
	raw, err := os.ReadFile(logPath)
	if err != nil {
		t.Fatalf("read systemctl argv log: %v", err)
	}
	want := "--user disable --now zen-router.service\n"
	if string(raw) != want {
		t.Errorf("systemctl argv log =\n%q\nwant\n%q", raw, want)
	}
}

// TestUsageListsInstallSystemd: the usage() text advertises the new
// subcommand per spec §11 (`install-systemd [--remove]`) so the chained
// registry and the help stay in sync (Task 4 appends `tui` to the same list).
func TestUsageListsInstallSystemd(t *testing.T) {
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatalf("pipe: %v", err)
	}
	old := os.Stderr
	os.Stderr = w
	usage()
	os.Stderr = old
	w.Close()
	data, err := io.ReadAll(r)
	if err != nil {
		t.Fatalf("read usage output: %v", err)
	}
	if !strings.Contains(string(data), "install-systemd [--remove]") {
		t.Errorf("usage() must list install-systemd [--remove], got:\n%s", data)
	}
}
