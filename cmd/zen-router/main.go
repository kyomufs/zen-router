// Command zen-router runs a local reverse proxy in front of the OpenCode Zen
// gateway, keeping a warm keep-alive pool (the latency fix) and rotating the
// egress IP through Cloudflare WARP when the anonymous daily quota is spent.
package main

import (
	"context"
	"encoding/base64"
	"flag"
	"fmt"
	"log"
	"net"
	"net/http"
	"os"
	"os/signal"
	"strconv"
	"syscall"
	"time"

	"zen-router/internal/cli"
	"zen-router/internal/proxy"
	"zen-router/internal/quota"
	"zen-router/internal/router"
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
	fmt.Fprint(os.Stderr, `zen-router — local reverse proxy for OpenCode Zen with WARP IP rotation

Usage:
  zen-router up      [--listen ADDR]     start the proxy daemon (foreground)
  zen-router status  [--listen ADDR]     show egress, mode and quota counters
  zen-router rotate  [--listen ADDR]     force an egress IP rotation now
  zen-router use     <direct|warp>       force the active egress path
  zen-router stop    [--listen ADDR]     gracefully stop the daemon

Environment:
  ZEN_ROUTER_LISTEN   default listen address (127.0.0.1:8787)
  ZEN_ROUTER_STATE    state file path
`)
}

func parseListen(args []string) (string, error) {
	fs := flag.NewFlagSet("zen-router", flag.ContinueOnError)
	listen := fs.String("listen", "", "listen address (default 127.0.0.1:8787)")
	if err := fs.Parse(args); err != nil {
		return "", err
	}
	return cli.Listen(*listen), nil
}

// cmdUp starts the daemon: router + proxy + control API, in the foreground.
func cmdUp(args []string) error {
	listen, err := parseListen(args)
	if err != nil {
		return err
	}
	logger := log.New(os.Stderr, "zen-router ", log.LstdFlags|log.Lmsgprefix)

	store, err := quota.Open("")
	if err != nil {
		return err
	}
	r, err := router.New(router.Options{Store: store, Logger: logger})
	if err != nil {
		return err
	}

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

	ctrl := &cli.Control{Router: r, Shutdown: stop}
	handler := ctrl.Handler(srv.Handler())

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
	logger.Printf("zen-router up on http://%s (proxy + %scontrol), egress=%s mode=%s",
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
