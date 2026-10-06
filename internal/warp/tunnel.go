package warp

import (
	"context"
	"encoding/base64"
	"fmt"
	"net"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"time"

	"golang.zx2c4.com/wireguard/wgctrl"
	"golang.zx2c4.com/wireguard/wgctrl/wgtypes"
)

// FirewallMark tags the kernel WireGuard socket so its outer UDP packets can
// be routed around a competing TUN (FlClash) via a dedicated ip rule.
// 51820 is the conventional WireGuard fwmark; the value itself is arbitrary
// as long as the ip rule matches it.
const FirewallMark = 51820

// ipPath is the absolute path to iproute2; NixOS keeps it out of bare PATH
// for non-login shells.
const ipPath = "/run/current-system/sw/bin/ip"

// Tunnel owns one WireGuard interface used as a selective egress for the
// proxy. Only sockets bound to the device (SO_BINDTODEVICE) traverse it; the
// rest of the system keeps its existing routing.
type Tunnel struct {
	Name       string
	PrivateKey *Key
	PeerPub    string
	Endpoint   string // host:port
	AddressV4  string // e.g. 172.16.0.2/32
	AddressV6  string // e.g. 2606:4700:.../128
	Keepalive  int    // seconds
}

// NewTunnel builds a tunnel description from a resolved WARP profile.
func NewTunnel(name string, priv *Key, prof *WireGuardProfile) *Tunnel {
	return &Tunnel{
		Name:       name,
		PrivateKey: priv,
		PeerPub:    prof.ServerPub,
		Endpoint:   prof.Endpoint,
		AddressV4:  normalizeCIDR(prof.AddressV4, 32),
		AddressV6:  normalizeCIDR(prof.AddressV6, 128),
		Keepalive:  25,
	}
}

// normalizeCIDR ensures an address carries a prefix length.
func normalizeCIDR(addr string, bits int) string {
	if addr == "" {
		return ""
	}
	if strings.Contains(addr, "/") {
		return addr
	}
	return fmt.Sprintf("%s/%d", addr, bits)
}

func (t *Tunnel) run(name string, args ...string) (string, error) {
	cmd := exec.Command(name, args...)
	cmd.Env = os.Environ()
	out, err := cmd.CombinedOutput()
	if err != nil {
		return "", fmt.Errorf("%s %s: %v: %s", name, strings.Join(args, " "), err, strings.TrimSpace(string(out)))
	}
	return strings.TrimSpace(string(out)), nil
}

// sudoRun executes an ip(8) command, escalating to sudo when not root. The
// host grants passwordless sudo for these precise networking commands.
func (t *Tunnel) sudoRun(args ...string) (string, error) {
	if os.Geteuid() == 0 {
		return t.run(ipPath, args...)
	}
	full := append([]string{"-n", ipPath}, args...)
	return t.run("sudo", full...)
}

// DeviceExists reports whether the WireGuard interface is already present.
func (t *Tunnel) DeviceExists() bool {
	_, err := t.sudoRun("link", "show", "dev", t.Name)
	return err == nil
}

// ensureDevice creates the WireGuard interface if it does not exist.
func (t *Tunnel) ensureDevice() error {
	if t.DeviceExists() {
		return nil
	}
	if _, err := t.sudoRun("link", "add", "dev", t.Name, "type", "wireguard"); err != nil {
		return fmt.Errorf("create wireguard device: %w", err)
	}
	return nil
}

// DeviceParams carries everything the kernel needs to configure one WireGuard
// interface. It is the payload for the privileged netlink step.
type DeviceParams struct {
	Name       string
	PrivateKey wgtypes.Key
	Peer       wgtypes.Key
	Endpoint   *net.UDPAddr
	FWMark     int
	Keepalive  int
}

// ApplyDeviceConfig writes keys, peer, endpoint and fwmark into the device via
// netlink. This needs CAP_NET_ADMIN, so it is invoked either in-process (when
// already root) or through the privileged __wgcfg helper.
func ApplyDeviceConfig(p DeviceParams) error {
	client, err := wgctrl.New()
	if err != nil {
		return fmt.Errorf("open wgctrl: %w", err)
	}
	defer client.Close()

	fwmark := p.FWMark
	cfg := wgtypes.Config{
		PrivateKey:   &p.PrivateKey,
		FirewallMark: &fwmark,
		Peers: []wgtypes.PeerConfig{{
			PublicKey:                   p.Peer,
			Endpoint:                    p.Endpoint,
			PersistentKeepaliveInterval: durationPtr(p.Keepalive),
			ReplaceAllowedIPs:           true,
			AllowedIPs: []net.IPNet{
				{IP: net.IPv4zero, Mask: net.CIDRMask(0, 32)},
				{IP: net.IPv6zero, Mask: net.CIDRMask(0, 128)},
			},
		}},
	}
	if err := client.ConfigureDevice(p.Name, cfg); err != nil {
		return fmt.Errorf("configure device %s: %w", p.Name, err)
	}
	return nil
}

// configure writes keys, peer and fwmark into the device via netlink, then
// assigns addresses and brings the link up.
func (t *Tunnel) configure() error {
	priv, err := wgtypes.ParseKey(t.PrivateKey.Private)
	if err != nil {
		return fmt.Errorf("parse private key: %w", err)
	}
	peer, err := wgtypes.ParseKey(t.PeerPub)
	if err != nil {
		return fmt.Errorf("parse peer key: %w", err)
	}
	ip, port, err := net.SplitHostPort(t.Endpoint)
	if err != nil {
		return fmt.Errorf("parse endpoint: %w", err)
	}
	// The WARP API advertises a hostname, but the kernel tunnel needs a concrete
	// IP. Resolve it through DoH — the system resolver would return a fake-IP
	// under an active TUN, which is useless as a tunnel destination.
	if net.ParseIP(ip) == nil {
		resolved, rerr := ResolveHostIP(context.Background(), ip)
		if rerr != nil {
			return fmt.Errorf("resolve endpoint host %s: %w", ip, rerr)
		}
		ip = resolved
	}
	portNum, err := strconv.Atoi(port)
	if err != nil {
		return fmt.Errorf("parse endpoint port: %w", err)
	}
	udpAddr := &net.UDPAddr{IP: net.ParseIP(ip), Port: portNum}
	if udpAddr.IP == nil {
		return fmt.Errorf("endpoint host is not an IP: %q", ip)
	}

	params := DeviceParams{
		Name:       t.Name,
		PrivateKey: priv,
		Peer:       peer,
		Endpoint:   udpAddr,
		FWMark:     FirewallMark,
		Keepalive:  t.Keepalive,
	}
	// Netlink configuration needs CAP_NET_ADMIN. When not root, escalate the
	// single privileged call through the helper subcommand instead of running
	// the whole daemon as root.
	if os.Geteuid() == 0 {
		return ApplyDeviceConfig(params)
	}
	return applyDeviceConfigPrivileged(params)
}

// applyDeviceConfigPrivileged re-invokes this binary as root to run just the
// netlink configuration step.
func applyDeviceConfigPrivileged(p DeviceParams) error {
	exe, err := os.Executable()
	if err != nil {
		return fmt.Errorf("locate zen-router binary: %w", err)
	}
	args := []string{
		"-n", exe, "__wgcfg",
		p.Name,
		base64.StdEncoding.EncodeToString(p.PrivateKey[:]),
		base64.StdEncoding.EncodeToString(p.Peer[:]),
		p.Endpoint.IP.String(),
		strconv.Itoa(p.Endpoint.Port),
		strconv.Itoa(p.FWMark),
		strconv.Itoa(p.Keepalive),
	}
	cmd := exec.Command("sudo", args...)
	cmd.Env = os.Environ()
	out, err := cmd.CombinedOutput()
	if err != nil {
		return fmt.Errorf("privileged wg config: %v: %s", err, strings.TrimSpace(string(out)))
	}
	return nil
}

func durationPtr(secs int) *time.Duration {
	if secs <= 0 {
		return nil
	}
	d := time.Duration(secs) * time.Second
	return &d
}

// assignAddresses adds the tunnel addresses (idempotent) and brings the link up.
func (t *Tunnel) assignAddresses() error {
	if t.AddressV4 != "" {
		if _, err := t.sudoRun("addr", "replace", t.AddressV4, "dev", t.Name); err != nil {
			return fmt.Errorf("assign ipv4: %w", err)
		}
	}
	if t.AddressV6 != "" {
		if _, err := t.sudoRun("-6", "addr", "replace", t.AddressV6, "dev", t.Name); err != nil {
			return fmt.Errorf("assign ipv6: %w", err)
		}
	}
	if _, err := t.sudoRun("link", "set", "dev", t.Name, "up", "mtu", "1420"); err != nil {
		return fmt.Errorf("link up: %w", err)
	}
	return nil
}

// ensureRule was removed: see the note on Up(). No fwmark rule is installed.

// Up creates, configures and activates the tunnel end-to-end.
//
// Note on routing: no fwmark escape hatch is installed. On this host the
// competing TUN (FlClash) already routes the Cloudflare WARP endpoint directly,
// so the outer WireGuard packets simply ride the normal path through it. An
// fwmark/priority rule that bypasses the TUN actually breaks the tunnel — the
// packets fall back to the home router, where they never reach Cloudflare.
func (t *Tunnel) Up() error {
	if t.PrivateKey == nil || t.PrivateKey.Private == "" {
		return fmt.Errorf("tunnel has no private key")
	}
	if err := t.ensureDevice(); err != nil {
		return err
	}
	if err := t.configure(); err != nil {
		return err
	}
	return t.assignAddresses()
}

// Reconfigure swaps in a new peer/endpoint/addresses without tearing the
// interface down — this is the hot IP-rotation path.
func (t *Tunnel) Reconfigure() error {
	if !t.DeviceExists() {
		return t.Up()
	}
	if err := t.configure(); err != nil {
		return err
	}
	return t.assignAddresses()
}

// Down removes the tunnel interface.
func (t *Tunnel) Down() error {
	if !t.DeviceExists() {
		return nil
	}
	_, err := t.sudoRun("link", "del", "dev", t.Name)
	return err
}
