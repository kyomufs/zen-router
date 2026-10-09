package main

// Tests for plan Task 3: XDG file log + `up --detach`.
//
// Hermeticity contract (task brief recipe (a)-(d)) — these tests must NEVER
// reach the live daemon on 127.0.0.1:8787:
//   (a) a free port is pre-allocated (net.Listen on 127.0.0.1:0, then
//       closed) and pinned with --listen, so the LIVE daemon cannot answer
//       the readiness poll and produce a false green;
//   (b) HOME, XDG_CONFIG_HOME, XDG_STATE_HOME, ZEN_ROUTER_STATE and DSH_HOME
//       all point into fresh t.TempDir()s BEFORE the child is spawned (the
//       explicit DSH_HOME keeps config.LegacyStatePath away from the real
//       ~/.dsh/state/zen-router/state.json — fix F2);
//   (c) the child binary is built into a fresh t.TempDir()
//       (`go build -o <tmp>/zen-router ./cmd/zen-router`) — never go
//       install, never ~/.local/bin;
//   (d) teardown ALWAYS SIGTERMs the spawned daemon (and waits for it),
//       even when an assertion fails.
// Network beyond loopback and the spawn of the built child does not happen:
// the daemon makes no outbound call at startup with a fresh state file.

import (
	"bytes"
	"context"
	"fmt"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"zen-router/internal/cli"
)

// TestUpDetach is the mandatory hermetic recipe for `up --detach`: the
// parent forks the daemon into the prepared env/pinned port, polls
// /_zenctl/status until it answers (bounded), exits 0; the daemon tees its
// logger to Paths.LogFile under the temp XDG state dir; SIGTERM shuts the
// child down cleanly.
func TestUpDetach(t *testing.T) {
	// (c) Build BEFORE the hermetic t.Setenv calls: the child build must
	// reuse the warm GOCACHE under the real HOME (a HOME-scoped cache would
	// cold-rebuild every dependency on every run).
	bin := buildZenRouter(t)

	// (b) Fresh temp roots for every path-sensitive env var.
	tmp := t.TempDir()
	t.Setenv("HOME", tmp)
	t.Setenv("XDG_CONFIG_HOME", tmp)
	t.Setenv("XDG_STATE_HOME", tmp)
	// ZEN_ROUTER_STATE resolves to a FILE inside the fresh temp root: the
	// env var is a state FILE path, and quota.Open rejects a directory
	// (EISDIR), so the directory itself cannot be the value.
	t.Setenv("ZEN_ROUTER_STATE", filepath.Join(tmp, "state.json"))
	// (b) DSH_HOME too: LegacyStatePath must never read the real
	// ~/.dsh/state/zen-router/state.json (fix F2).
	dshHome := filepath.Join(t.TempDir(), "dsh")
	t.Setenv("DSH_HOME", dshHome)
	t.Setenv("OPENCODE_ZEN_API_KEY", "")
	t.Setenv("OPENCODE_GO_API_KEY", "")

	// Plant a legacy state file at the ISOLATED DSH_HOME with a marker
	// timestamp: the daemon's one-shot migration must copy THIS file into
	// the XDG state path — cheap proof the legacy path stayed inside the
	// temp root (fix F2 assertion).
	const legacyMarker = 1700000001
	legacyDir := filepath.Join(dshHome, "state", "zen-router")
	if err := os.MkdirAll(legacyDir, 0o755); err != nil {
		t.Fatalf("plant legacy dir: %v", err)
	}
	legacyJSON := fmt.Sprintf(`{"version":2,"mode":"auto","current":"direct","updatedAt": %d,"egress":{"direct":{},"warp":{}},"keys":{},"active":0}`+"\n", legacyMarker)
	if err := os.WriteFile(filepath.Join(legacyDir, "state.json"), []byte(legacyJSON), 0o644); err != nil {
		t.Fatalf("plant legacy state: %v", err)
	}

	// (a) Pre-allocated free port — the only address the child may serve.
	listen := freeListen(t)

	// (d) Teardown ALWAYS signals the daemon, even on t.Fatal paths below.
	var daemonPid int
	t.Cleanup(func() { stopDaemons(t, bin, &daemonPid) })

	// The `up --detach` parent: must exit 0 within a bounded timeout once
	// the forked daemon answers /_zenctl/status.
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	parent := exec.CommandContext(ctx, bin, "up", "--detach", "--listen", listen)
	var out bytes.Buffer
	parent.Stdout = &out
	parent.Stderr = &out
	err := parent.Run()
	if ctx.Err() != nil {
		t.Fatalf("up --detach did not exit within 60s (want: bounded readiness poll, exit 0)\noutput:\n%s", out.String())
	}
	if err != nil {
		t.Fatalf("up --detach exited non-zero: %v\noutput:\n%s", err, out.String())
	}

	// The parent reports the forked daemon's pid so callers (and this
	// teardown) can signal exactly that process.
	m := regexp.MustCompile(`pid (\d+)`).FindStringSubmatch(out.String())
	if m == nil {
		t.Fatalf("up --detach output carries no pid line\noutput:\n%s", out.String())
	}
	daemonPid, _ = strconv.Atoi(m[1])
	if daemonPid <= 0 {
		t.Fatalf("bad pid %q in output:\n%s", m[1], out.String())
	}

	// Bounded readiness poll against OUR port (proof the endpoint answers
	// the forked child, not some ambient daemon).
	st := waitStatus(t, listen, 15*time.Second)
	if st.Listen != listen {
		t.Fatalf("status.listen = %q, want the pinned %q", st.Listen, listen)
	}

	// The daemon tees its logger to $XDG_STATE_HOME/zen-router/zen.log.
	logFile := filepath.Join(tmp, "zen-router", "zen.log")
	data := waitFileContains(t, logFile, "zen-router up on http://"+listen, 15*time.Second)
	t.Logf("file log %s (%d bytes):\n%s", logFile, len(data), data)

	// Fix F2 hermeticity proof: the one-shot migration copied the PLANTED
	// legacy file from the isolated DSH_HOME into the XDG state path
	// ($XDG_STATE_HOME/zen-router/state.json — MigrateLegacyState's target;
	// the ZEN_ROUTER_STATE override is the quota layer's separate path), so
	// the real ~/.dsh state can never have been read: its timestamp would
	// not match the marker.
	migrated, err := os.ReadFile(filepath.Join(tmp, "zen-router", "state.json"))
	if err != nil {
		t.Fatalf("read migrated state: %v", err)
	}
	if want := fmt.Sprintf(`"updatedAt": %d`, legacyMarker); !strings.Contains(string(migrated), want) {
		t.Fatalf("migrated state does not carry the planted DSH_HOME marker %q (real ~/.dsh read instead?):\n%s", want, migrated)
	}

	// SIGTERM shuts the child down cleanly (recipe teardown): the explicit
	// signal here asserts graceful exit; t.Cleanup repeats it defensively.
	if err := syscall.Kill(daemonPid, syscall.SIGTERM); err != nil {
		t.Fatalf("SIGTERM %d: %v", daemonPid, err)
	}
	waitGone(t, daemonPid, 15*time.Second)
}

// TestUpForegroundTeesLogFile pins the daemon-side logger tee itself: the
// foreground child's stderr goes to the TEST, so the only way content can
// reach $XDG_STATE_HOME/zen-router/zen.log is the logger tee in cmdUp. The
// detach test alone cannot prove the tee — `up --detach` reopens the child's
// stderr onto that same file.
func TestUpForegroundTeesLogFile(t *testing.T) {
	bin := buildZenRouter(t)

	tmp := t.TempDir()
	t.Setenv("HOME", tmp)
	t.Setenv("XDG_CONFIG_HOME", tmp)
	t.Setenv("XDG_STATE_HOME", tmp)
	t.Setenv("ZEN_ROUTER_STATE", filepath.Join(tmp, "state.json"))
	t.Setenv("DSH_HOME", filepath.Join(t.TempDir(), "dsh")) // fix F2: keep LegacyStatePath inside the temp root
	t.Setenv("OPENCODE_ZEN_API_KEY", "")
	t.Setenv("OPENCODE_GO_API_KEY", "")

	listen := freeListen(t)

	child := exec.Command(bin, "up", "--listen", listen)
	var out bytes.Buffer
	child.Stdout = &out
	child.Stderr = &out
	if err := child.Start(); err != nil {
		t.Fatalf("start foreground up: %v", err)
	}
	pid := child.Process.Pid

	// Reap exactly once: the body consumes `done` on its success path;
	// cleanup covers every early t.Fatal path (SIGTERM, then SIGKILL).
	done := make(chan error, 1)
	go func() { done <- child.Wait() }()
	reaped := false
	t.Cleanup(func() {
		if reaped {
			return
		}
		_ = child.Process.Signal(syscall.SIGTERM)
		select {
		case <-done:
		case <-time.After(10 * time.Second):
			_ = child.Process.Kill()
			<-done
		}
	})

	waitStatus(t, listen, 15*time.Second)
	logFile := filepath.Join(tmp, "zen-router", "zen.log")
	data := waitFileContains(t, logFile, "zen-router up on http://"+listen, 15*time.Second)
	t.Logf("file log %s (%d bytes):\n%s", logFile, len(data), data)

	// SIGTERM shuts the foreground child down cleanly and it exits 0.
	if err := child.Process.Signal(syscall.SIGTERM); err != nil {
		t.Fatalf("SIGTERM %d: %v", pid, err)
	}
	select {
	case err := <-done:
		reaped = true
		if err != nil {
			t.Fatalf("foreground up exited %v after SIGTERM; output:\n%s", err, out.String())
		}
	case <-time.After(15 * time.Second):
		t.Fatalf("foreground up (pid %d) still alive 15s after SIGTERM; output:\n%s", pid, out.String())
	}
}

// TestUpDetachRefusesDuplicate: `up --detach` must never declare readiness
// from a daemon that was ALREADY serving the pinned port (fix F1). A status
// answer whose pid is not the spawned child's cannot turn the poll green:
// the parent refuses with a non-zero exit and an "already running (pid N)"
// message, terminates its own child, and leaves daemon #1 untouched.
func TestUpDetachRefusesDuplicate(t *testing.T) {
	bin := buildZenRouter(t)

	tmp := t.TempDir()
	t.Setenv("HOME", tmp)
	t.Setenv("XDG_CONFIG_HOME", tmp)
	t.Setenv("XDG_STATE_HOME", tmp)
	t.Setenv("ZEN_ROUTER_STATE", filepath.Join(tmp, "state.json"))
	t.Setenv("DSH_HOME", filepath.Join(t.TempDir(), "dsh")) // fix F2
	t.Setenv("OPENCODE_ZEN_API_KEY", "")
	t.Setenv("OPENCODE_GO_API_KEY", "")

	listen := freeListen(t)

	// Always stop every process running bin (daemon #1 and any stray child);
	// pid2 stays 0 — the refused detach must not leave a second child.
	var pid2 int
	t.Cleanup(func() { stopDaemons(t, bin, &pid2) })

	// Daemon #1: foreground child of THIS test, owns the port first.
	d1 := exec.Command(bin, "up", "--listen", listen)
	var out1 bytes.Buffer
	d1.Stdout = &out1
	d1.Stderr = &out1
	if err := d1.Start(); err != nil {
		t.Fatalf("start daemon #1: %v", err)
	}
	pid1 := d1.Process.Pid
	done1 := make(chan error, 1)
	go func() { done1 <- d1.Wait() }()
	t.Cleanup(func() {
		_ = d1.Process.Signal(syscall.SIGTERM)
		select {
		case <-done1:
		case <-time.After(10 * time.Second):
			_ = d1.Process.Kill()
			<-done1
		}
	})
	st1 := waitStatus(t, listen, 15*time.Second)
	if st1.Pid != pid1 {
		t.Fatalf("status.pid = %d, want daemon #1's %d — the pid field must report the process serving /_zenctl/status", st1.Pid, pid1)
	}

	// Second detach for the SAME port must be refused.
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	parent := exec.CommandContext(ctx, bin, "up", "--detach", "--listen", listen)
	var out bytes.Buffer
	parent.Stdout = &out
	parent.Stderr = &out
	err := parent.Run()
	if ctx.Err() != nil {
		t.Fatalf("duplicate `up --detach` did not exit within 60s\noutput:\n%s", out.String())
	}
	// Capture a SUCCESS pid line (buggy green) BEFORE asserting, so a wrong
	// exit 0 still gets its doomed child torn down by stopDaemons; the
	// refusal message's pid belongs to daemon #1, not to a spawned child.
	if m := regexp.MustCompile(`started \(pid (\d+)\)`).FindStringSubmatch(out.String()); m != nil {
		pid2, _ = strconv.Atoi(m[1])
	}
	if err == nil {
		t.Fatalf("second `up --detach` went green against the already-running daemon (pid %d); want non-zero exit\noutput:\n%s", pid1, out.String())
	}
	if want := fmt.Sprintf("already running (pid %d)", pid1); !strings.Contains(out.String(), want) {
		t.Fatalf("detach error does not identify the running daemon (%q)\noutput:\n%s", want, out.String())
	}

	// No second child may be left behind, and daemon #1 keeps serving.
	alive := daemonPIDs(t, bin)
	if len(alive) != 1 || alive[0] != pid1 {
		t.Fatalf("after the refused detach, processes running %s = %v, want only daemon #1 (pid %d)", bin, alive, pid1)
	}
	if err := syscall.Kill(pid1, 0); err != nil {
		t.Fatalf("daemon #1 (pid %d) must survive the refused detach: %v", pid1, err)
	}
	if pid2 > 0 {
		if err := syscall.Kill(pid2, 0); err == nil {
			t.Fatalf("doomed second child (pid %d) still alive after refusal", pid2)
		}
	}
}

// buildZenRouter builds the zen-router binary into a fresh t.TempDir()
// (recipe (c): never go install, never ~/.local/bin) and returns its path.
// It runs BEFORE the hermetic t.Setenv calls so `go build` keeps the warm
// GOCACHE of the real HOME.
func buildZenRouter(t *testing.T) string {
	t.Helper()
	bin := filepath.Join(t.TempDir(), "zen-router")
	wd, err := os.Getwd() // go test runs the binary in the package source dir
	if err != nil {
		t.Fatalf("getwd: %v", err)
	}
	build := exec.Command("go", "build", "-o", bin, "./cmd/zen-router")
	// wd = <repo>/cmd/zen-router → repo root is two levels up.
	build.Dir = filepath.Dir(filepath.Dir(wd))
	if out, err := build.CombinedOutput(); err != nil {
		t.Fatalf("go build ./cmd/zen-router: %v\n%s", err, out)
	}
	return bin
}

// freeListen pre-allocates and immediately releases a loopback port
// (recipe (a)) and returns its "127.0.0.1:<port>" address.
func freeListen(t *testing.T) string {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("allocate free port: %v", err)
	}
	addr := ln.Addr().String()
	if err := ln.Close(); err != nil {
		t.Fatalf("release free port: %v", err)
	}
	return addr
}

// waitStatus polls GET /_zenctl/status on the pinned address until it
// answers with up=true, bounded by timeout.
func waitStatus(t *testing.T, listen string, timeout time.Duration) *cli.Status {
	t.Helper()
	client := cli.NewControlClient(listen)
	deadline := time.Now().Add(timeout)
	var last error
	for time.Now().Before(deadline) {
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		st, err := client.Status(ctx)
		cancel()
		if err == nil && st.Up {
			return st
		}
		// Keep a REAL reason for the final Fatal — a bare `err` would print
		// `: <nil>` whenever the endpoint answered with up=false (fix F4).
		if err != nil {
			last = err
		} else {
			last = fmt.Errorf("status up=false (listen=%q, pid=%d)", st.Listen, st.Pid)
		}
		time.Sleep(200 * time.Millisecond)
	}
	t.Fatalf("status on %s not ready within %s: %v", listen, timeout, last)
	return nil
}

// waitFileContains polls path until it contains want or the timeout elapses.
func waitFileContains(t *testing.T, path, want string, timeout time.Duration) []byte {
	t.Helper()
	deadline := time.Now().Add(timeout)
	var last []byte
	for time.Now().Before(deadline) {
		data, err := os.ReadFile(path)
		if err == nil {
			last = data
			if strings.Contains(string(data), want) {
				return data
			}
		}
		time.Sleep(150 * time.Millisecond)
	}
	t.Fatalf("file %s never contained %q within %s (last contents: %q)", path, want, timeout, last)
	return nil
}

// waitGone waits until pid no longer exists (SIGKILL also counts: the zombie
// window of an init-reaped orphan is milliseconds).
func waitGone(t *testing.T, pid int, timeout time.Duration) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if err := syscall.Kill(pid, 0); err != nil {
			return
		}
		time.Sleep(100 * time.Millisecond)
	}
	_ = syscall.Kill(pid, syscall.SIGKILL)
	t.Fatalf("pid %d survived SIGTERM for %s", pid, timeout)
}

// stopDaemons SIGTERMs every live process executing bin and waits (bounded)
// for the pids to disappear, escalating to SIGKILL. The detach test's daemon
// is a GRANDCHILD (reparented to init when the detach parent exits), so this
// /proc scan — not cmd.Process — is the teardown's source of truth; it also
// covers the case where the test failed before parsing the pid line (recipe
// (d): killed even on failure).
func stopDaemons(t *testing.T, bin string, pid *int) {
	t.Helper()
	pids := daemonPIDs(t, bin)
	// The raw pid came from parsed stdout and may already be a recycled id
	// if the daemon died — never signal it without re-verifying that
	// /proc/<pid>/exe still IS the built binary (fix F3). On the normal path
	// the daemon is already gone after SIGTERM+waitGone, so this appends
	// nothing.
	if *pid > 0 && !containsInt(pids, *pid) && pidRunsBinary(*pid, bin) {
		pids = append(pids, *pid)
	}
	for _, p := range pids {
		_ = syscall.Kill(p, syscall.SIGTERM)
	}
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		alive := false
		for _, p := range pids {
			if syscall.Kill(p, 0) == nil {
				alive = true
			}
		}
		if !alive {
			return
		}
		time.Sleep(100 * time.Millisecond)
	}
	for _, p := range pids {
		_ = syscall.Kill(p, syscall.SIGKILL)
	}
	t.Logf("had to SIGKILL pids %v executing %s", pids, bin)
}

// daemonPIDs returns the pids of every live process whose /proc/<pid>/exe
// resolves to bin.
func daemonPIDs(t *testing.T, bin string) []int {
	t.Helper()
	want, err := filepath.EvalSymlinks(bin)
	if err != nil {
		t.Logf("eval symlinks %s: %v", bin, err)
		return nil
	}
	entries, err := os.ReadDir("/proc")
	if err != nil {
		t.Logf("read /proc: %v", err)
		return nil
	}
	var pids []int
	for _, e := range entries {
		pid, err := strconv.Atoi(e.Name())
		if err != nil {
			continue
		}
		exe, err := os.Readlink(filepath.Join("/proc", e.Name(), "exe"))
		if err != nil {
			continue // gone, or not ours (kernel threads have no exe)
		}
		if resolved, err := filepath.EvalSymlinks(exe); err == nil && resolved == want {
			pids = append(pids, pid)
		}
	}
	return pids
}

// pidRunsBinary reports whether pid is alive and /proc/<pid>/exe resolves to
// bin (fix F3): parsed pids are only ever signalled after this re-check.
func pidRunsBinary(pid int, bin string) bool {
	exe, err := os.Readlink(filepath.Join("/proc", strconv.Itoa(pid), "exe"))
	if err != nil {
		return false
	}
	want, err := filepath.EvalSymlinks(bin)
	if err != nil {
		return false
	}
	got, err := filepath.EvalSymlinks(exe)
	return err == nil && got == want
}

func containsInt(haystack []int, needle int) bool {
	for _, v := range haystack {
		if v == needle {
			return true
		}
	}
	return false
}
