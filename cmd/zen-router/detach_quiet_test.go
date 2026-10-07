package main

// Hermetic tests for detachUp's success-line behavior (whole-branch review
// F1): a successful spawn through the TUI Spawner path must be SILENT (the
// model's altscreen is live — raw text garbles it until the next ≤1s poll
// repaint), while `up --detach` must keep its "started (pid N)" line —
// TestUpDetach parses `pid (\d+)` from that output.
//
// Recipe: these tests re-execute THIS test binary as detachUp's forked
// daemon. TestMain detects fakeDetachEnv + the `up --listen <addr>` argv and
// serves a canned GET /_zenctl/status on exactly <addr> (up=true,
// listen=<addr>, pid=<own pid>) until SIGTERM — so detachUp's child-bound
// readiness poll goes green hermetically: no real daemon, no live port, no
// network beyond loopback, no config files (HOME/XDG_*/ZEN_ROUTER_STATE/
// DSH_HOME all point into t.TempDir()s). Teardown always SIGTERMs the
// forked child (recipe (d) of detach_test.go), even on assertion failure.

import (
	"bytes"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"regexp"
	"strconv"
	"syscall"
	"testing"
	"time"
)

// fakeDetachEnv arms the TestMain fake-daemon branch in the forked child.
const fakeDetachEnv = "ZEN_ROUTER_TEST_FAKE_DETACH"

// TestMain doubles as the fake daemon for the detachUp tests: when the
// detachUp parent re-executes this test binary as `up --listen <addr>` with
// fakeDetachEnv set, the child serves the canned status until SIGTERM
// instead of running tests (whole-branch F1 hermetic recipe).
func TestMain(m *testing.M) {
	if os.Getenv(fakeDetachEnv) == "1" {
		runFakeDetachChild() // never returns
	}
	os.Exit(m.Run())
}

// runFakeDetachChild binds the requested address, answers a green status,
// and exits 0 on SIGTERM. Any problem exits 3 so the parent's readiness
// poll fails with a diagnosable child exit instead of hanging.
func runFakeDetachChild() {
	if len(os.Args) != 4 || os.Args[1] != "up" || os.Args[2] != "--listen" {
		fmt.Fprintf(os.Stderr, "fake detach child: unexpected argv %q\n", os.Args)
		os.Exit(3)
	}
	addr := os.Args[3]
	ln, err := net.Listen("tcp", addr)
	if err != nil {
		fmt.Fprintf(os.Stderr, "fake detach child: listen %s: %v\n", addr, err)
		os.Exit(3)
	}
	mux := http.NewServeMux()
	mux.HandleFunc("/_zenctl/status", func(w http.ResponseWriter, _ *http.Request) {
		// pid MUST be this process's own pid: detachUp only turns green on
		// status.pid == the pid IT forked (Task 3 fix F1).
		fmt.Fprintf(w, `{"up":true,"listen":%q,"pid":%d}`, addr, os.Getpid())
	})
	srv := &http.Server{Handler: mux}
	go func() { _ = srv.Serve(ln) }()
	ch := make(chan os.Signal, 1)
	signal.Notify(ch, syscall.SIGTERM)
	<-ch
	_ = srv.Close()
	os.Exit(0)
}

// fakeDetachSetup pins the hermetic env (fresh HOME/XDG roots, no live
// state), arms the fake-child marker, picks a free loopback port, and
// registers teardown of the forked child. Returns the listen address.
func fakeDetachSetup(t *testing.T) string {
	t.Helper()
	tmp := t.TempDir()
	t.Setenv("HOME", tmp)
	t.Setenv("XDG_CONFIG_HOME", tmp)
	t.Setenv("XDG_STATE_HOME", tmp)
	t.Setenv("ZEN_ROUTER_STATE", filepath.Join(tmp, "state.json"))
	t.Setenv("DSH_HOME", filepath.Join(t.TempDir(), "dsh"))
	t.Setenv("OPENCODE_ZEN_API_KEY", "")
	t.Setenv("OPENCODE_GO_API_KEY", "")
	t.Setenv(fakeDetachEnv, "1")
	listen := freeListen(t) // recipe (a) of detach_test.go: never a live port
	t.Cleanup(func() { killFakeDetachChild(t, listen) })
	return listen
}

// captureStdout runs fn with os.Stdout replaced by a pipe and returns
// everything fn wrote to it.
func captureStdout(t *testing.T, fn func()) []byte {
	t.Helper()
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatalf("os.Pipe: %v", err)
	}
	old := os.Stdout
	os.Stdout = w
	defer func() { os.Stdout = old }() // covers t.Fatalf unwinding from fn
	fn()
	os.Stdout = old
	if err := w.Close(); err != nil {
		t.Fatalf("close stdout pipe: %v", err)
	}
	data, rerr := io.ReadAll(r)
	_ = r.Close()
	if rerr != nil {
		t.Fatalf("read stdout pipe: %v", rerr)
	}
	return data
}

// killFakeDetachChild SIGTERMs (then SIGKILLs, bounded) every re-executed
// copy of this test binary serving `up --listen <addr>`: detachUp leaves
// its child running after a successful start, so the test owns teardown.
// A copy is identified by exe == this test binary AND the argv needle AND
// pid != our own — the parent test process shares the exe but not the argv.
func killFakeDetachChild(t *testing.T, addr string) {
	t.Helper()
	self, err := os.Executable()
	if err != nil {
		t.Logf("resolve own executable for teardown: %v", err)
		return
	}
	want, err := filepath.EvalSymlinks(self)
	if err != nil {
		t.Logf("eval symlinks %s: %v", self, err)
		return
	}
	needle := "--listen\x00" + addr
	entries, err := os.ReadDir("/proc")
	if err != nil {
		t.Logf("read /proc: %v", err)
		return
	}
	var pids []int
	for _, e := range entries {
		pid, perr := strconv.Atoi(e.Name())
		if perr != nil || pid == os.Getpid() {
			continue
		}
		exe, lerr := os.Readlink(filepath.Join("/proc", e.Name(), "exe"))
		if lerr != nil {
			continue // gone, or not ours (kernel threads have no exe)
		}
		resolved, verr := filepath.EvalSymlinks(exe)
		if verr != nil || resolved != want {
			continue
		}
		cl, cerr := os.ReadFile(filepath.Join("/proc", e.Name(), "cmdline"))
		if cerr != nil || !bytes.Contains(cl, []byte(needle)) {
			continue
		}
		pids = append(pids, pid)
	}
	for _, pid := range pids {
		_ = syscall.Kill(pid, syscall.SIGTERM)
	}
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		alive := false
		for _, pid := range pids {
			if syscall.Kill(pid, 0) == nil {
				alive = true
			}
		}
		if !alive {
			return
		}
		time.Sleep(50 * time.Millisecond)
	}
	for _, pid := range pids {
		_ = syscall.Kill(pid, syscall.SIGKILL)
	}
	t.Logf("had to SIGKILL fake detach child pids %v", pids)
}

// TestDetachUpQuietWritesNothingToStdout: the TUI Spawner path (`s` on a
// down daemon → cmdTui's detachUp closure) must write NOTHING to stdout —
// cmdTui runs the Bubble Tea altscreen (view.go: AltScreen = true), and a
// raw success line would garble it until the next poll repaint
// (whole-branch F1). The `up --detach` path keeps its line through the
// writer variant — TestDetachUpPrintsPIDLine and TestUpDetach.
func TestDetachUpQuietWritesNothingToStdout(t *testing.T) {
	listen := fakeDetachSetup(t)

	out := captureStdout(t, func() {
		if err := detachUp(listen, nil); err != nil {
			t.Errorf("detachUp(%q, nil): %v", listen, err)
		}
	})
	if len(out) != 0 {
		t.Errorf("quiet detach wrote %q to stdout — garbles the live TUI altscreen (whole-branch F1)", out)
	}
}

// TestDetachUpPrintsPIDLine: the `up --detach` success line must keep its
// "started (pid N)" shape on the WRITER it is given — TestUpDetach parses
// `pid (\d+)` (and TestUpDetachRefusesDuplicate parses
// `started \(pid (\d+)\)`) from the os.Stdout variant of that same line.
func TestDetachUpPrintsPIDLine(t *testing.T) {
	listen := fakeDetachSetup(t)

	var buf bytes.Buffer
	if err := detachUp(listen, &buf); err != nil {
		t.Fatalf("detachUp(%q, &buf): %v", listen, err)
	}
	out := buf.String()
	m := regexp.MustCompile(`started \(pid (\d+)\)`).FindStringSubmatch(out)
	if m == nil {
		t.Fatalf("up --detach success output carries no \"started (pid N)\" line (breaks TestUpDetach):\n%s", out)
	}
	if pid, _ := strconv.Atoi(m[1]); pid <= 0 {
		t.Fatalf("bad pid %q in success line:\n%s", m[1], out)
	}
}
