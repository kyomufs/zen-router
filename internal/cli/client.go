package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"zen-router/internal/config"
)

// Listen resolves the daemon address from the flag, or — when the flag is
// empty — from config.Load (config.json + ZEN_ROUTER_LISTEN env + built-in
// default, in that precedence). The config package is the single source of
// truth for listen addresses: the duplicated DefaultListen constant and
// ZEN_ROUTER_LISTEN parsing that lived here until Task 13 are gone.
func Listen(flagVal string) (string, error) {
	if flagVal != "" {
		return flagVal, nil
	}
	cfg, err := config.Load()
	if err != nil {
		return "", err
	}
	return cfg.Listen, nil
}

// controlBase builds the control API base URL for a listen address.
func controlBase(listen string) string {
	host := listen
	if strings.HasPrefix(host, ":") {
		host = "127.0.0.1" + host
	}
	return "http://" + host + ControlPrefix
}

// ControlClient talks to a running daemon's control API.
type ControlClient struct {
	base   string
	client *http.Client
}

// NewControlClient builds a client for the given listen address.
func NewControlClient(listen string) *ControlClient {
	return &ControlClient{
		base:   controlBase(listen),
		client: &http.Client{Timeout: 90 * time.Second},
	}
}

func (c *ControlClient) do(ctx context.Context, method, route string, body io.Reader) ([]byte, error) {
	req, err := http.NewRequestWithContext(ctx, method, c.base+route, body)
	if err != nil {
		return nil, err
	}
	resp, err := c.client.Do(req)
	if err != nil {
		// A request killed by OUR context is a wedged (or overloaded)
		// daemon, not a dead one: mapping it to ErrNotRunning would render
		// "daemon is not running (start it with `zen-router up`)" against a
		// daemon that is up — exactly what the TUI's 60s action timeout can
		// hit below the client's own 90s timeout (review F2).
		if ctx.Err() != nil || errors.Is(err, context.DeadlineExceeded) || errors.Is(err, context.Canceled) {
			return nil, fmt.Errorf("control %s %s: %w", method, route, err)
		}
		return nil, ErrNotRunning
	}
	defer resp.Body.Close()
	data, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return nil, err
	}
	if resp.StatusCode >= 400 {
		return data, fmt.Errorf("control %s %s: HTTP %d: %s", method, route, resp.StatusCode, strings.TrimSpace(string(data)))
	}
	return data, nil
}

// Status fetches the daemon status snapshot.
func (c *ControlClient) Status(ctx context.Context) (*Status, error) {
	data, err := c.do(ctx, http.MethodGet, "status", nil)
	if err != nil {
		return nil, err
	}
	var st Status
	if err := json.Unmarshal(data, &st); err != nil {
		return nil, fmt.Errorf("decode status: %w", err)
	}
	return &st, nil
}

// Stats fetches the daemon's request-history rollups (GET /_zenctl/stats).
func (c *ControlClient) Stats(ctx context.Context) (*Stats, error) {
	data, err := c.do(ctx, http.MethodGet, "stats", nil)
	if err != nil {
		return nil, err
	}
	var st Stats
	if err := json.Unmarshal(data, &st); err != nil {
		return nil, fmt.Errorf("decode stats: %w", err)
	}
	return &st, nil
}

// Keys lists the pool file's key fingerprints (GET /_zenctl/keys).
func (c *ControlClient) Keys(ctx context.Context) ([]string, error) {
	data, err := c.do(ctx, http.MethodGet, "keys", nil)
	if err != nil {
		return nil, err
	}
	var resp struct {
		Keys []string `json:"keys"`
	}
	if err := json.Unmarshal(data, &resp); err != nil {
		return nil, fmt.Errorf("decode keys: %w", err)
	}
	return resp.Keys, nil
}

// AddKey appends one raw key to the pool file (POST /_zenctl/keys) and
// returns its fingerprint. The raw key exists only in the request body.
func (c *ControlClient) AddKey(ctx context.Context, raw string) (string, error) {
	body, err := json.Marshal(map[string]string{"key": raw})
	if err != nil {
		return "", err
	}
	data, err := c.do(ctx, http.MethodPost, "keys", bytes.NewReader(body))
	if err != nil {
		return "", err
	}
	var resp struct {
		Fingerprint string `json:"fingerprint"`
	}
	if err := json.Unmarshal(data, &resp); err != nil {
		return "", fmt.Errorf("decode add key: %w", err)
	}
	return resp.Fingerprint, nil
}

// DeleteKey removes the pool-file key matching fp (POST
// /_zenctl/keys/delete).
func (c *ControlClient) DeleteKey(ctx context.Context, fingerprint string) error {
	body, err := json.Marshal(map[string]string{"fingerprint": fingerprint})
	if err != nil {
		return err
	}
	_, err = c.do(ctx, http.MethodPost, "keys/delete", bytes.NewReader(body))
	return err
}

// Stop asks the daemon to shut down gracefully.
func (c *ControlClient) Stop(ctx context.Context) error {
	_, err := c.do(ctx, http.MethodPost, "stop", nil)
	return err
}
