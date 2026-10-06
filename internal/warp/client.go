package warp

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"strconv"
	"strings"
	"time"
)

// Cloudflare WARP client API, mirroring the contract wgcf uses against the
// official 1.1.1.1 Android app backend (version 6.38.9, build 5641).
const (
	apiURL     = "https://api.cloudflareclient.com"
	apiVersion = "v0a5641"

	userAgent      = "1.1.1.1/6.38.9-5641 (Android 16.0.0)"
	cfClientVer    = "a-6.38.9-5641"
	defaultWGPort  = 2408
	requestTimeout = 30 * time.Second
)

// Device is the registration record returned by POST /reg.
type Device struct {
	ID      string `json:"id"`
	Token   string `json:"token"`
	Account struct {
		License     string `json:"license"`
		AccountType string `json:"account_type"`
	} `json:"account"`
}

// Endpoint is a WireGuard server endpoint as advertised by the API.
type Endpoint struct {
	Host  string  `json:"host"`
	V4    string  `json:"v4"`
	V6    string  `json:"v6"`
	Ports []int32 `json:"ports"`
}

// Peer is a WireGuard peer (server) in the device config.
type Peer struct {
	Endpoint  Endpoint `json:"endpoint"`
	PublicKey string   `json:"public_key"`
}

// Client talks to the WARP control API. One Client per device identity.
type Client struct {
	http    *http.Client
	baseURL string
	version string

	// Device identity, populated by Register or Restore.
	Device *Device
}

// NewClient builds a WARP API client. baseURL/version are overridable for tests.
func NewClient() *Client {
	return &Client{
		http:    &http.Client{Timeout: requestTimeout},
		baseURL: apiURL,
		version: apiVersion,
	}
}

func (c *Client) defaultHeaders(auth bool) map[string]string {
	h := map[string]string{
		"User-Agent":        userAgent,
		"CF-Client-Version": cfClientVer,
		"Content-Type":      "application/json; charset=UTF-8",
		"Accept":            "application/json",
	}
	if auth && c.Device != nil && c.Device.Token != "" {
		h["Authorization"] = "Bearer " + c.Device.Token
	}
	return h
}

func (c *Client) do(ctx context.Context, method, path string, body any, auth bool) ([]byte, error) {
	var reader io.Reader
	if body != nil {
		raw, err := json.Marshal(body)
		if err != nil {
			return nil, fmt.Errorf("marshal request: %w", err)
		}
		reader = bytes.NewReader(raw)
	}
	url := strings.TrimRight(c.baseURL, "/") + path
	req, err := http.NewRequestWithContext(ctx, method, url, reader)
	if err != nil {
		return nil, err
	}
	for k, v := range c.defaultHeaders(auth) {
		req.Header.Set(k, v)
	}
	resp, err := c.http.Do(req)
	if err != nil {
		return nil, fmt.Errorf("%s %s: %w", method, path, err)
	}
	defer resp.Body.Close()
	data, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return nil, fmt.Errorf("read %s %s: %w", method, path, err)
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return nil, fmt.Errorf("%s %s: HTTP %d: %s", method, path, resp.StatusCode, truncate(string(data), 300))
	}
	return data, nil
}

// Register creates a brand-new WARP device + account around publicKey and
// stores the returned identity on the client. This is the IP-rotation primitive:
// every call mints a fresh device, and a fresh device gets a fresh egress IP.
func (c *Client) Register(ctx context.Context, publicKey string) (*Device, error) {
	payload := map[string]string{
		"fcm_token":     "",
		"install_id":    "",
		"key":           publicKey,
		"locale":        "en_US",
		"model":         "PC",
		"tos":           time.Now().UTC().Format(time.RFC3339),
		"serial_number": "",
		"os_version":    "16.0.0",
		"key_type":      "curve25519",
		"tunnel_type":   "wireguard",
	}
	data, err := c.do(ctx, http.MethodPost, "/"+c.version+"/reg", payload, false)
	if err != nil {
		return nil, err
	}
	var device Device
	if err := json.Unmarshal(data, &device); err != nil {
		return nil, fmt.Errorf("decode register response: %w", err)
	}
	if device.ID == "" || device.Token == "" {
		return nil, fmt.Errorf("register response missing id/token: %s", truncate(string(data), 300))
	}
	c.Device = &device
	return &device, nil
}

// GetDevice fetches the current device record including its WireGuard config.
func (c *Client) GetDevice(ctx context.Context) (*Device, error) {
	data, err := c.do(ctx, http.MethodGet, "/"+c.version+"/reg/"+c.Device.ID, nil, true)
	if err != nil {
		return nil, err
	}
	var device Device
	if err := json.Unmarshal(data, &device); err != nil {
		return nil, fmt.Errorf("decode device response: %w", err)
	}
	return &device, nil
}

// WireGuardProfile is the fully-resolved tunnel configuration for a device.
type WireGuardProfile struct {
	PrivateKey string
	AddressV4  string
	AddressV6  string
	ServerPub  string
	Endpoint   string // host:port
}

// GetWireGuardProfile resolves the WireGuard config for the current device:
// interface addresses plus the first peer's endpoint/public key.
func (c *Client) GetWireGuardProfile(ctx context.Context) (*WireGuardProfile, error) {
	data, err := c.do(ctx, http.MethodGet, "/"+c.version+"/reg/"+c.Device.ID, nil, true)
	if err != nil {
		return nil, err
	}
	var payload struct {
		Config struct {
			Interface struct {
				Addresses struct {
					V4 string `json:"v4"`
					V6 string `json:"v6"`
				} `json:"addresses"`
			} `json:"interface"`
			Peers []Peer `json:"peers"`
		} `json:"config"`
	}
	if err := json.Unmarshal(data, &payload); err != nil {
		return nil, fmt.Errorf("decode device config: %w", err)
	}
	if len(payload.Config.Peers) == 0 {
		return nil, fmt.Errorf("device config contains no WireGuard peer")
	}
	peer := payload.Config.Peers[0]
	endpoint, err := wireGuardEndpoint(peer.Endpoint)
	if err != nil {
		return nil, err
	}
	return &WireGuardProfile{
		AddressV4:  payload.Config.Interface.Addresses.V4,
		AddressV6:  payload.Config.Interface.Addresses.V6,
		ServerPub:  peer.PublicKey,
		Endpoint:   endpoint,
	}, nil
}

// wireGuardEndpoint mirrors wgcf's resolution: prefer the explicit host, then
// IPv4, then IPv6; default port 2408 unless the API advertised one.
func wireGuardEndpoint(e Endpoint) (string, error) {
	host := e.Host
	if host == "" {
		host = e.V4
	}
	if host == "" {
		host = e.V6
	}
	if host == "" {
		return "", fmt.Errorf("WARP response contained no WireGuard endpoint")
	}
	// WARP sometimes returns a bare host, sometimes host:port; strip any port.
	if h, _, err := net.SplitHostPort(host); err == nil {
		host = h
	}
	port := int32(defaultWGPort)
	if len(e.Ports) > 0 && e.Ports[0] > 0 {
		port = e.Ports[0]
	}
	return net.JoinHostPort(host, strconv.Itoa(int(port))), nil
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "…"
}
