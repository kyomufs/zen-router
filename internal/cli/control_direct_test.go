package cli

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"testing"

	"zen-router/internal/quota"
	"zen-router/internal/router"
)

// newTestControl builds a Control around a fresh direct-only Router.
func newDirectTestControl(t *testing.T) *Control {
	t.Helper()
	st, err := quota.Open(filepath.Join(t.TempDir(), "state.json"))
	if err != nil {
		t.Fatalf("quota.Open: %v", err)
	}
	r, err := router.New(router.Options{Store: st})
	if err != nil {
		t.Fatalf("router.New: %v", err)
	}
	return &Control{Router: r}
}

// TestControlRotateUseRoutesRemoved pins that the manual rotation/mode
// endpoints are gone: they answer 404 like any unknown control route.
func TestControlRotateUseRoutesRemoved(t *testing.T) {
	c := newDirectTestControl(t)
	h := c.Handler(http.NotFoundHandler())

	for _, tc := range []struct {
		route string
		body  io.Reader
	}{
		{"/_zenctl/rotate", nil},
		{"/_zenctl/use?mode=direct", nil},
	} {
		req := httptest.NewRequest(http.MethodPost, tc.route, tc.body)
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		if rec.Code != http.StatusNotFound {
			t.Errorf("POST %s = %d, want 404", tc.route, rec.Code)
		}
	}
}

// TestStatusPayloadDropsWarpEraFields pins the excised dashboard fields:
// mode/current/last_rotate/rotating/registering/lastSpareError must be
// absent from the status JSON, while the core fields stay.
func TestStatusPayloadDropsWarpEraFields(t *testing.T) {
	c := newDirectTestControl(t)
	h := c.Handler(http.NotFoundHandler())

	req := httptest.NewRequest(http.MethodGet, "/_zenctl/status", nil)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("GET /_zenctl/status = %d, want 200", rec.Code)
	}

	var payload map[string]json.RawMessage
	if err := json.Unmarshal(rec.Body.Bytes(), &payload); err != nil {
		t.Fatalf("decode status: %v", err)
	}
	for _, banned := range []string{
		"mode", "current", "last_rotate", "rotating", "registering",
		"lastSpareError",
	} {
		if _, ok := payload[banned]; ok {
			t.Errorf("status payload carries removed field %q", banned)
		}
	}
	for _, keep := range []string{"up", "state", "listen", "pid", "uptime_seconds"} {
		if _, ok := payload[keep]; !ok {
			t.Errorf("status payload lost field %q", keep)
		}
	}
}
