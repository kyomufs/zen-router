package cli

import (
	"context"
	"encoding/json"
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

// Rotate asks the daemon to force an IP rotation now.
func (c *ControlClient) Rotate(ctx context.Context) (string, error) {
	data, err := c.do(ctx, http.MethodPost, "rotate", nil)
	if err != nil {
		return "", err
	}
	var out struct {
		RotatedTo string `json:"rotated_to"`
		Error     string `json:"error"`
	}
	if err := json.Unmarshal(data, &out); err != nil {
		return "", fmt.Errorf("decode rotate: %w", err)
	}
	if out.Error != "" {
		return "", fmt.Errorf("%s", out.Error)
	}
	return out.RotatedTo, nil
}

// Use forces the daemon onto direct or warp egress.
func (c *ControlClient) Use(ctx context.Context, mode string) (string, error) {
	data, err := c.do(ctx, http.MethodPost, "use?mode="+mode, nil)
	if err != nil {
		return "", err
	}
	var out struct {
		Current string `json:"current"`
		Error   string `json:"error"`
	}
	if err := json.Unmarshal(data, &out); err != nil {
		return "", fmt.Errorf("decode use: %w", err)
	}
	if out.Error != "" {
		return "", fmt.Errorf("%s", out.Error)
	}
	return out.Current, nil
}

// Stop asks the daemon to shut down gracefully.
func (c *ControlClient) Stop(ctx context.Context) error {
	_, err := c.do(ctx, http.MethodPost, "stop", nil)
	return err
}
