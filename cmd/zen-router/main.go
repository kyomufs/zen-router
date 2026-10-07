// Command zen-router runs a local reverse proxy in front of the OpenCode Zen
// gateway, keeping a warm keep-alive pool (the latency fix) and rotating the
// egress IP through Cloudflare WARP when the anonymous daily quota is spent.
package main

import (
	"context"
	"encoding/base64"
	"flag"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"strconv"
	"syscall"
	"time"

	"zen-router/internal/cli"
	"zen-router/internal/config"
	"zen-router/internal/gateway"
	"zen-router/internal/keys"
	"zen-router/internal/proxy"
	"zen-router/internal/quota"
	"zen-router/internal/router"
	"zen-router/internal/systemd"
	"zen-router/internal/warp"

	"golang.zx2c4.com/wireguard/wgctrl/wgtypes"
)

func main() {
	if len(os.Args) < 2 {
		usage()
		os.Exit(2)
	}
	cmd := os.Args[1]
	args := os.Args[2:]

	var err error
	switch cmd {
	case "up":
		err = cmdUp(args)
	case "status":
		err = cmdStatus(args)
	case "rotate":
		err = cmdRotate(args)
	case "use":
		err = cmdUse(args)
	case "stop":
		err = cmdStop(args)
	case "install-systemd":
		err = cmdInstallSystemd(args)
	case "__wgcfg":
		err = cmdWGConfig(args)
	case "help", "-h", "--help":
		usage()
	default:
		fmt.Fprintf(os.Stderr, "unknown command %q\n\n", cmd)
		usage()
		os.Exit(2)
	}
	if err != nil {
		fmt.Fprintln(os.Stderr, "error:", err)
		os.Exit(1)
	}
}

func usage() {
	fmt.Fprintf(os.Stderr, `zen-router — local reverse proxy for OpenCode Zen with WARP IP rotation

Usage:
  zen-router up      [--listen ADDR] [--detach]  start the proxy daemon (foreground; --detach forks it
                                                 into the background and returns once it is ready)
  zen-router status  [--listen ADDR]     show egress, mode and quota counters
  zen-router rotate  [--listen ADDR]     force an egress IP rotation now
  zen-router use     <direct|warp>       force the active egress path
  zen-router stop    [--listen ADDR]     gracefully stop the daemon
  zen-router install-systemd [--remove]  write $XDG_CONFIG_HOME/systemd/user/zen-router.service for this
                                         executable's foreground "up", then daemon-reload + enable --now;
                                         --remove disables (--now) and deletes the unit file

OpenAI surface (on the same listener):
  GET /v1/models, POST /v1/chat/completions — OpenAI-compatible endpoints

Environment:
  ZEN_ROUTER_LISTEN   default listen address (%s)
  ZEN_ROUTER_STATE    state file path
`, config.Default().Listen)
}

// parseListen parses the shared --listen flag of every foreground/control
// subcommand. `up` parses its own flag set (parseUp) because it also owns
// --detach.
func parseListen(args []string) (string, error) {
	fs := flag.NewFlagSet("zen-router", flag.ContinueOnError)
	listen := fs.String("listen", "", "listen address (default "+config.Default().Listen+")")
	if err := fs.Parse(args); err != nil {
		return "", err
	}
	return cli.Listen(*listen)
}

// parseUp parses the `up` flag set: --listen like every other subcommand,
// plus --detach (plan Task 3, spec §11: `zen-router up [--detach]`).
func parseUp(args []string) (listen string, detach bool, err error) {
	fs := flag.NewFlagSet("zen-router up", flag.ContinueOnError)
	listenFlag := fs.String("listen", "", "listen address (default "+config.Default().Listen+")")
	detachFlag := fs.Bool("detach", false, "start the daemon in the background; exit once it is ready")
	if err := fs.Parse(args); err != nil {
		return "", false, err
	}
	addr, err := cli.Listen(*listenFlag)
	if err != nil {
		return "", false, err
	}
	return addr, *detachFlag, nil
}

// fileLog attaches the XDG file log (plan Task 3, spec §10: the state dir
// holds state.json + zen.log) to the daemon logger: it returns the writer
// the logger should write to and a closer for the file. Failure to open the
// log is reported but NOT fatal — the daemon still serves, only the file
// copy of its output is missing.
//
// A detached child (`up --detach`) reopens its stderr onto this same file so
// main's pre-logger "error:" lines stay inspectable after the parent is
// gone. In that case the tee collapses to a single destination (stderr IS
// the log file), so every line is still written exactly once.
func fileLog(path string) (io.Writer, func(), error) {
	noop := func() {}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return os.Stderr, noop, err
	}
	f, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644)
	if err != nil {
		return os.Stderr, noop, err
	}
	if st, serr := os.Stderr.Stat(); serr == nil {
		if fst, ferr := f.Stat(); ferr == nil && os.SameFile(st, fst) {
			f.Close()
			return os.Stderr, noop, nil
		}
	}
	return io.MultiWriter(os.Stderr, f), func() { f.Close() }, nil
}

// detachReadyTimeout bounds how long `up --detach` waits for the forked
// daemon's control API to answer before giving up.
const detachReadyTimeout = 30 * time.Second

// detachUp forks a background copy of THIS executable — same environment
// (the caller's prepared HOME/XDG/ZEN_ROUTER_* values travel verbatim),
// pinned to the resolved listen address — reopens the child's console onto
// stable files, and polls /_zenctl/status until it answers. On readiness it
// prints the child pid to stdout and returns nil (main exits 0). If the
// child dies first, or readiness does not arrive within
// detachReadyTimeout, it returns an error (main prints it to stderr and
// exits non-zero); a readiness timeout also terminates the forked child, so
// a failed start never leaves a half-running daemon behind.
//
// The child's stdio is never inherited: once this parent exits, a pipe's
// read end dies with it, and the daemon's next write to fd 1/2 would raise
// SIGPIPE and kill it on the spot. stdout goes to /dev/null and stderr to
// the XDG file log (which the child's own logger tees into anyway — fileLog
// collapses the duplicate — preserving pre-logger "error:" lines too).
//
// The status-wait also demands status.listen == listen AND
// status.pid == <our child's pid> (fix F1): an answer from any other
// process can never turn the poll green, and a pre-existing daemon on the
// same address is reported instead of being forked over.
func detachUp(listen string) error {
	exe, err := os.Executable()
	if err != nil {
		return fmt.Errorf("resolve own executable: %w", err)
	}
	paths, err := config.DefaultPaths()
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(paths.LogFile), 0o755); err != nil {
		return fmt.Errorf("create log dir: %w", err)
	}
	devnull, err := os.OpenFile(os.DevNull, os.O_WRONLY, 0)
	if err != nil {
		return fmt.Errorf("open %s: %w", os.DevNull, err)
	}
	defer devnull.Close()
	logf, err := os.OpenFile(paths.LogFile, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644)
	if err != nil {
		return fmt.Errorf("open file log %s: %w", paths.LogFile, err)
	}
	defer logf.Close()

	child := exec.Command(exe, "up", "--listen", listen)
	child.Env = os.Environ()
	child.Stdout = devnull
	child.Stderr = logf
	// child.Stdin stays nil → /dev/null (exec.Cmd default).
	if err := child.Start(); err != nil {
		return fmt.Errorf("start daemon: %w", err)
	}

	// Reap the child if it dies before we do. The channel is buffered so
	// this goroutine never blocks when we return on the success path and
	// exit without consuming the status; the send publishes ProcessState
	// to the poll loop.
	exited := make(chan *os.ProcessState, 1)
	go func() {
		_ = child.Wait() // reaps the child; ProcessState is set by then
		exited <- child.ProcessState
	}()
	earlyExit := func(st *os.ProcessState) error {
		desc := "unknown status"
		if st != nil {
			desc = st.String()
		}
		return fmt.Errorf("daemon exited before becoming ready (%s); see %s or run `zen-router up --listen %s` in the foreground",
			desc, paths.LogFile, listen)
	}
	// stopChild terminates the forked child and reaps it (SIGTERM, 5s
	// grace, SIGKILL) — used whenever the parent refuses to go green, so no
	// doomed child is ever left behind.
	stopChild := func() {
		_ = child.Process.Signal(syscall.SIGTERM)
		select {
		case <-exited:
		case <-time.After(5 * time.Second):
			_ = child.Process.Kill()
			<-exited
		}
	}

	client := cli.NewControlClient(listen)
	deadline := time.Now().Add(detachReadyTimeout)
	var lastErr error
	for {
		select {
		case st := <-exited:
			return earlyExit(st)
		default:
		}
		// Short per-attempt read: connection refused while the child boots
		// is the normal case, not an error.
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		st, serr := client.Status(ctx)
		cancel()
		switch {
		case serr != nil:
			lastErr = serr
		case !st.Up:
			lastErr = fmt.Errorf("status up=false")
		case st.Listen != listen:
			lastErr = fmt.Errorf("status.listen=%q, want %q", st.Listen, listen)
		case st.Pid != child.Process.Pid:
			// Readiness must come from OUR forked child: an answer with the
			// right listen address but a DIFFERENT pid is a pre-existing
			// daemon (or a squatter) — never green on it, and take our
			// doomed child down before refusing (fix F1).
			stopChild()
			return fmt.Errorf("zen-router already running (pid %d) — not starting a duplicate", st.Pid)
		default:
			fmt.Printf("zen-router started (pid %d) on http://%s (log: %s)\n",
				child.Process.Pid, listen, paths.LogFile)
			return nil
		}
		if time.Now().After(deadline) {
			stopChild()
			return fmt.Errorf("daemon not ready within %s (%v); see %s", detachReadyTimeout, lastErr, paths.LogFile)
		}
		select {
		case st := <-exited:
			return earlyExit(st)
		case <-time.After(200 * time.Millisecond):
		}
	}
}

// cmdUp starts the daemon: control API + OpenAI gateway + legacy reverse
// proxy on one listener. Foreground by default; with --detach it forks that
// foreground run into the background and returns once it is ready.
func cmdUp(args []string) error {
	// Daemon start stamp for status.uptime_seconds (plan Task 1), taken
	// before any setup so uptime covers init time too.
	started := time.Now()
	listen, detach, err := parseUp(args)
	if err != nil {
		return err
	}
	if detach {
		return detachUp(listen)
	}

	// File log (plan Task 3, spec §10: the XDG state dir holds state.json +
	// zen.log — the TUI tails it). paths is needed before the logger exists,
	// so it resolves here, ahead of config.Load.
	paths, err := config.DefaultPaths()
	if err != nil {
		return err
	}
	logw, closeLog, logErr := fileLog(paths.LogFile)
	defer closeLog()
	logger := log.New(logw, "zen-router ", log.LstdFlags|log.Lmsgprefix)
	if logErr != nil {
		// Non-fatal: stderr-only logging, the daemon keeps serving.
		logger.Printf("warn: file log disabled: %v", logErr)
	}

	// Full config: the gateway needs the upstream base URL and watchdog
	// budgets; the rotator needs the key pool, identity-pool sizing,
	// cooldown and address family. parseUp already resolved Listen through
	// the same loader — config.json is a tiny read-only file, so reading it
	// again here beats duplicating flag-parsing logic.
	cfg, err := config.Load()
	if err != nil {
		return err
	}
	// One-shot legacy-state migration, BEFORE the state file is opened.
	if _, err := config.MigrateLegacyState(paths); err != nil {
		return fmt.Errorf("migrate legacy state: %w", err)
	}

	store, err := quota.Open("")
	if err != nil {
		return err
	}
	r, err := router.New(router.Options{
		Store:            store,
		Logger:           logger,
		Pool:             keys.New(cfg.KeyPoolFile),
		RotationCooldown: cfg.RotationCooldown,
		Family:           cfg.Family,
		PoolSize:         cfg.PoolSize,
		PoolSpare:        cfg.PoolSpare,
	})
	if err != nil {
		return err
	}

	// Egress IP observation (plan Task 2): the production echo re-reads the
	// ACTIVE transport on every attempt (so it follows the active path) and
	// is built ONLY when the user opted in via config.EgressIPEcho — the §12
	// gate, default false: no live echo call exists without that explicit
	// opt-in. Every effective active-egress change (rotation, its direct
	// fallback, manual mode switch) marks the observation stale via
	// OnRotated (debounced by egressIPMinInterval), status reads refresh it
	// lazily.
	echoer := cli.NewEgressIPEchoer(cfg.EgressIPEcho, func() http.RoundTripper {
		_, rt := r.Egress()
		return rt
	})
	egressIP := cli.NewEgressIPTracker(echoer)
	r.OnRotated = egressIP.Refresh

	// Warm the WARP path up front if state says we were on it, so the first
	// agent request does not pay the registration latency.
	if r.Current() == proxy.EgressWarp {
		ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
		if err := r.Use(ctx, proxy.EgressWarp); err != nil {
			logger.Printf("warn: could not restore warp egress: %v (falling back to direct)", err)
			_ = r.Use(ctx, proxy.EgressDirect)
		}
		cancel()
	}

	srv, err := proxy.New(proxy.Config{
		Listen:   listen,
		OnResult: r.OnResult,
		Logger:   logger,
		Egress:   r.Egress,
	})
	if err != nil {
		return err
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	ctrl := &cli.Control{
		Router:    r,
		Shutdown:  stop,
		Listen:    listen,
		StartedAt: started,
		IPTracker: egressIP,
	}
	// Three surfaces, ONE listener (plan Task 13): control stays outermost
	// and unchanged (it intercepts /_zenctl/* by prefix), the OpenAI
	// gateway takes /v1/*, and everything else — including the legacy
	// /zen/v1/* the plugin targets — falls through to the reverse proxy.
	// The gateway must never see /zen/* (it would 404-envelope the
	// plugin's traffic) and /v1/* must never reach the path-preserving
	// proxy (the OpenAI path would leak straight to the upstream).
	root := http.NewServeMux()
	gw := gateway.New(r, cfg)
	gw.Recorder = r // dashboard seam (plan Task 1): 2xx counters + latency
	root.Handle("/v1/", gw)
	root.Handle("/", srv.Handler())
	handler := ctrl.Handler(root)

	httpSrv := &http.Server{
		Addr:              listen,
		Handler:           handler,
		ReadHeaderTimeout: 15 * time.Second,
	}
	go func() {
		<-ctx.Done()
		shutCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		httpSrv.Shutdown(shutCtx)
		r.Stop()
	}()

	ln, err := net.Listen("tcp", listen)
	if err != nil {
		return fmt.Errorf("listen %s: %w", listen, err)
	}
	logger.Printf("zen-router up on http://%s (OpenAI /v1 gateway + %scontrol + legacy proxy), egress=%s mode=%s",
		listen, cli.ControlPrefix, r.Current(), store.Mode())
	if err := httpSrv.Serve(ln); err != nil && err != http.ErrServerClosed {
		return err
	}
	logger.Printf("zen-router stopped")
	return nil
}

// cmdStatus prints the daemon status, falling back to the state file when the
// daemon is not running.
func cmdStatus(args []string) error {
	listen, err := parseListen(args)
	if err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	st, err := cli.NewControlClient(listen).Status(ctx)
	if err != nil {
		// Daemon down: report from persisted state so the command still works.
		store, serr := quota.Open("")
		if serr != nil {
			return fmt.Errorf("%v (and state unreadable: %v)", err, serr)
		}
		snap := store.Snapshot()
		fmt.Printf("daemon: down (reporting persisted state)\n")
		fmt.Printf("mode:   %s\n", snap.Mode)
		fmt.Printf("last:   %s\n", snap.Current)
		printEgress(snap)
		return nil
	}
	fmt.Printf("daemon: up\n")
	fmt.Printf("mode:   %s\n", st.Mode)
	fmt.Printf("egress: %s\n", st.Current)
	printEgress(st.State)
	return nil
}

func printEgress(s quota.State) {
	fmt.Printf("\negress counters (updated %s):\n", time.UnixMilli(s.UpdatedAt).UTC().Format(time.RFC3339))
	for _, name := range []string{"direct", "warp"} {
		b := s.Egress[name]
		if b == nil {
			continue
		}
		suffix := ""
		if b.SpentUntil > time.Now().UnixMilli() {
			suffix = fmt.Sprintf("  [spent until %s]", time.UnixMilli(b.SpentUntil).UTC().Format(time.RFC3339))
		}
		fmt.Printf("  %-7s ok=%-6d daily429=%-5d%s\n", name, b.OK, b.Daily429, suffix)
	}
	if w := s.Warp; w != nil {
		fmt.Printf("\nwarp device: %s (registered %s)\n", w.DeviceID,
			time.UnixMilli(w.RegisteredAt).UTC().Format(time.RFC3339))
	}
}

// cmdRotate forces an IP rotation through the running daemon.
func cmdRotate(args []string) error {
	listen, err := parseListen(args)
	if err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()
	to, err := cli.NewControlClient(listen).Rotate(ctx)
	if err != nil {
		return err
	}
	fmt.Printf("rotated to egress: %s\n", to)
	return nil
}

// cmdUse forces the active egress path.
func cmdUse(args []string) error {
	if len(args) < 1 {
		return fmt.Errorf("usage: zen-router use <direct|warp>")
	}
	mode := args[0]
	listen, err := parseListen(args[1:])
	if err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()
	cur, err := cli.NewControlClient(listen).Use(ctx, mode)
	if err != nil {
		return err
	}
	fmt.Printf("active egress: %s\n", cur)
	return nil
}

// cmdStop asks the daemon to shut down.
func cmdStop(args []string) error {
	listen, err := parseListen(args)
	if err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	if err := cli.NewControlClient(listen).Stop(ctx); err != nil {
		return err
	}
	fmt.Println("stop requested")
	return nil
}

// cmdInstallSystemd manages the systemd user unit (plan Task 8, spec §8,
// §11). By default it renders the unit for THIS executable, writes
// $XDG_CONFIG_HOME/systemd/user/zen-router.service and runs
// `systemctl --user daemon-reload` + `enable --now`. With --remove it runs
// `systemctl --user disable --now` and deletes the file.
//
// It never installs or overwrites the binary itself (~/.local/bin stays an
// отмашка-time user action), and running it against the live environment is
// gated behind the отмашка (spec §12) — this function must not be invoked
// outside hermetic tests until then.
func cmdInstallSystemd(args []string) error {
	fs := flag.NewFlagSet("zen-router install-systemd", flag.ContinueOnError)
	remove := fs.Bool("remove", false, "uninstall the user unit (disable --now and delete the file)")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if *remove {
		path, err := systemd.Remove()
		if err != nil {
			return err
		}
		fmt.Printf("removed %s\n", path)
		return nil
	}
	exe, err := os.Executable()
	if err != nil {
		return fmt.Errorf("resolve own executable: %w", err)
	}
	path, err := systemd.Install(exe)
	if err != nil {
		return err
	}
	fmt.Printf("installed %s (systemctl --user daemon-reload + enable --now)\n", path)
	return nil
}

// cmdWGConfig is the privileged helper: it runs as root (via sudo) and applies
// one WireGuard netlink configuration. It is internal — the daemon invokes it
// through applyDeviceConfigPrivileged — and never appears in usage.
func cmdWGConfig(args []string) error {
	if len(args) != 7 {
		return fmt.Errorf("__wgcfg expects 7 args, got %d", len(args))
	}
	name := args[0]
	privRaw, err := base64.StdEncoding.DecodeString(args[1])
	if err != nil || len(privRaw) != 32 {
		return fmt.Errorf("bad private key: %v", err)
	}
	peerRaw, err := base64.StdEncoding.DecodeString(args[2])
	if err != nil || len(peerRaw) != 32 {
		return fmt.Errorf("bad peer key: %v", err)
	}
	ip := net.ParseIP(args[3])
	if ip == nil {
		return fmt.Errorf("bad endpoint ip %q", args[3])
	}
	port, err := strconv.Atoi(args[4])
	if err != nil {
		return fmt.Errorf("bad endpoint port: %v", err)
	}
	fwmark, err := strconv.Atoi(args[5])
	if err != nil {
		return fmt.Errorf("bad fwmark: %v", err)
	}
	keepalive, err := strconv.Atoi(args[6])
	if err != nil {
		return fmt.Errorf("bad keepalive: %v", err)
	}
	var privKey, peerKey wgtypes.Key
	copy(privKey[:], privRaw)
	copy(peerKey[:], peerRaw)
	return warp.ApplyDeviceConfig(warp.DeviceParams{
		Name:       name,
		PrivateKey: privKey,
		Peer:       peerKey,
		Endpoint:   &net.UDPAddr{IP: ip, Port: port},
		FWMark:     fwmark,
		Keepalive:  keepalive,
	})
}
