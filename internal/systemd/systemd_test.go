package systemd

// Task 8 (Phase C): the systemd user unit renderer/installer (spec §8, §11).
//
// Hermeticity contract (task brief): a fake `systemctl` stub sits at the
// FRONT of PATH and records every invocation's argv into a log file; HOME and
// XDG_CONFIG_HOME point into fresh t.TempDir()s. No real systemctl ever
// runs, no live ~/.config/systemd/user is ever touched — the live install
// path is gated behind the отмашка (spec §12).

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// stubSystemctl prepends a fake `systemctl` to PATH. The stub appends each
// invocation's argv (space-joined, like "$*" in sh) to the returned log file
// and exits with the given status, so tests can both assert the exact argv
// sequence and inject failures — without any real systemd within a hundred
// miles.
func stubSystemctl(t *testing.T, exit int) (logPath string) {
	t.Helper()
	dir := t.TempDir()
	logPath = filepath.Join(dir, "systemctl.argv")
	stub := filepath.Join(dir, "systemctl")
	script := fmt.Sprintf("#!/bin/sh\nprintf '%%s\\n' \"$*\" >> %q\nexit %d\n", logPath, exit)
	if err := os.WriteFile(stub, []byte(script), 0o755); err != nil {
		t.Fatalf("write systemctl stub: %v", err)
	}
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))
	return logPath
}

// hermeticHome points HOME and XDG_CONFIG_HOME into a fresh temp dir before
// any call resolves a path, so nothing can escape toward the real
// ~/.config/systemd/user.
func hermeticHome(t *testing.T) {
	t.Helper()
	tmp := t.TempDir()
	t.Setenv("HOME", tmp)
	t.Setenv("XDG_CONFIG_HOME", filepath.Join(tmp, ".config"))
}

// wantUnitPath is where the unit MUST land under the hermetic env.
func wantUnitPath(t *testing.T) string {
	t.Helper()
	return filepath.Join(os.Getenv("XDG_CONFIG_HOME"), "systemd", "user", UnitName)
}

// argvLog returns the recorded systemctl invocations, one line each.
func argvLog(t *testing.T, logPath string) []string {
	t.Helper()
	data, err := os.ReadFile(logPath)
	if err != nil {
		t.Fatalf("read systemctl argv log: %v", err)
	}
	return strings.Split(strings.TrimRight(string(data), "\n"), "\n")
}

// TestRenderSpecUnitContent pins the rendered unit to spec §8: foreground
// `up` (NOT --detach — journald owns stderr, Task 3 ruling), Restart=on-failure
// (spec:228), RestartSec, the NixOS PATH the daemon needs to exec sudo/ip
// (spec §14:352-353) and WantedBy=default.target.
func TestRenderSpecUnitContent(t *testing.T) {
	const bin = "/home/u/.local/bin/zen-router"
	unit := Render(bin)

	for _, want := range []string{
		"ExecStart=" + bin + " up\n",
		"Restart=on-failure\n",
		"RestartSec=",
		"Environment=PATH=/run/wrappers/bin:",
		"/run/current-system/sw/bin",
		"WantedBy=default.target",
	} {
		if !strings.Contains(unit, want) {
			t.Errorf("rendered unit missing %q:\n%s", want, unit)
		}
	}
	if strings.Contains(unit, "--detach") {
		t.Errorf("unit must run foreground `up` for journald, found --detach:\n%s", unit)
	}
	if !strings.HasSuffix(unit, "\n") {
		t.Errorf("unit must end with a newline:\n%q", unit)
	}
}

// TestInstallWritesUnitAndRecordsSystemctl is the core brief scenario: the
// unit file lands at $XDG_CONFIG_HOME/systemd/user/zen-router.service with
// the rendered content, and the exec seam records `systemctl --user
// daemon-reload` first, then `systemctl --user enable --now`, in order.
func TestInstallWritesUnitAndRecordsSystemctl(t *testing.T) {
	hermeticHome(t)
	logPath := stubSystemctl(t, 0)
	const bin = "/opt/zen-router/zen-router"

	path, err := Install(bin)
	if err != nil {
		t.Fatalf("Install: %v", err)
	}
	if path != wantUnitPath(t) {
		t.Errorf("Install path = %q, want %q", path, wantUnitPath(t))
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read installed unit: %v", err)
	}
	if string(data) != Render(bin) {
		t.Errorf("installed unit content mismatch:\ngot:\n%s\nwant:\n%s", data, Render(bin))
	}

	lines := argvLog(t, logPath)
	want := []string{"--user daemon-reload", "--user enable --now " + UnitName}
	if len(lines) != len(want) {
		t.Fatalf("systemctl argv = %q, want %q", lines, want)
	}
	for i := range want {
		if lines[i] != want[i] {
			t.Errorf("systemctl argv[%d] = %q, want %q", i, lines[i], want[i])
		}
	}
}

// TestInstallIdempotent: re-running install overwrites the same file and
// repeats the reload+enable cycle without error (brief: re-run safe).
func TestInstallIdempotent(t *testing.T) {
	hermeticHome(t)
	logPath := stubSystemctl(t, 0)
	const bin = "/opt/zen-router/zen-router"

	if _, err := Install(bin); err != nil {
		t.Fatalf("first Install: %v", err)
	}
	if _, err := Install(bin); err != nil {
		t.Fatalf("second Install: %v", err)
	}
	data, err := os.ReadFile(wantUnitPath(t))
	if err != nil {
		t.Fatalf("read unit after re-run: %v", err)
	}
	if string(data) != Render(bin) {
		t.Errorf("unit content changed after re-run:\n%s", data)
	}
	lines := argvLog(t, logPath)
	want := []string{
		"--user daemon-reload", "--user enable --now " + UnitName,
		"--user daemon-reload", "--user enable --now " + UnitName,
	}
	if len(lines) != len(want) {
		t.Fatalf("systemctl argv = %q, want %q", lines, want)
	}
	for i := range want {
		if lines[i] != want[i] {
			t.Errorf("systemctl argv[%d] = %q, want %q", i, lines[i], want[i])
		}
	}
}

// TestInstallRejectsRelativeBinary: ExecStart must be an absolute path —
// systemd cannot resolve a bare name, and os.Executable always yields an
// absolute one.
func TestInstallRejectsRelativeBinary(t *testing.T) {
	hermeticHome(t)
	_ = stubSystemctl(t, 0)

	if _, err := Install("zen-router"); err == nil {
		t.Fatal("Install with a relative binary path: want error, got nil")
	}
}

// TestInstallPropagatesSystemctlFailure: a failing systemctl must surface as
// an error, never a silent green (enable never runs after a failed reload).
func TestInstallPropagatesSystemctlFailure(t *testing.T) {
	hermeticHome(t)
	_ = stubSystemctl(t, 1)

	_, err := Install("/opt/zen-router/zen-router")
	if err == nil {
		t.Fatal("Install with failing systemctl: want error, got nil")
	}
	if !strings.Contains(err.Error(), "systemctl") {
		t.Errorf("error should name systemctl, got: %v", err)
	}
}

// TestRemoveDisablesAndUnlinks: `--remove` runs `systemctl --user disable
// --now` while the unit file still exists, then unlinks it (brief).
func TestRemoveDisablesAndUnlinks(t *testing.T) {
	hermeticHome(t)
	logPath := stubSystemctl(t, 0)
	const bin = "/opt/zen-router/zen-router"
	if _, err := Install(bin); err != nil {
		t.Fatalf("Install: %v", err)
	}

	path, err := Remove()
	if err != nil {
		t.Fatalf("Remove: %v", err)
	}
	if path != wantUnitPath(t) {
		t.Errorf("Remove path = %q, want %q", path, wantUnitPath(t))
	}
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Errorf("unit file still present after Remove (stat err = %v)", err)
	}

	lines := argvLog(t, logPath)
	want := []string{
		"--user daemon-reload", "--user enable --now " + UnitName,
		"--user disable --now " + UnitName,
	}
	if len(lines) != len(want) {
		t.Fatalf("systemctl argv = %q, want %q", lines, want)
	}
	if lines[len(lines)-1] != want[len(want)-1] {
		t.Errorf("last systemctl argv = %q, want %q", lines[len(lines)-1], want[len(want)-1])
	}
}

// TestRemoveMissingUnitIsNoop: re-running --remove with no unit file present
// is safe — nothing to disable, no systemctl invocation, nil error.
func TestRemoveMissingUnitIsNoop(t *testing.T) {
	hermeticHome(t)
	logPath := stubSystemctl(t, 0)

	if _, err := Remove(); err != nil {
		t.Fatalf("Remove with no unit installed: %v", err)
	}
	if _, err := os.Stat(logPath); err == nil {
		lines, _ := os.ReadFile(logPath)
		if len(strings.TrimSpace(string(lines))) > 0 {
			t.Errorf("systemctl invoked although no unit file exists: %s", lines)
		}
	}
}
