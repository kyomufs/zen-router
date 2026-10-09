package cli

// Keys-management endpoints (Phase 3): GET/POST /_zenctl/keys and POST
// /_zenctl/keys/delete against a temp pool file. Hermetic: httptest
// handlers only, no daemon, no network.

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"

	"zen-router/internal/keys"
)

func keysControl(t *testing.T) (*Control, http.Handler) {
	t.Helper()
	pool := filepath.Join(t.TempDir(), "pool-config.json")
	c := &Control{
		Router:      newTestRouter(t),
		Listen:      "127.0.0.1:8787",
		KeyPoolFile: pool,
	}
	return c, c.Handler(http.NewServeMux())
}

func postKeys(t *testing.T, h http.Handler, route, body string) *httptest.ResponseRecorder {
	t.Helper()
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, ControlPrefix+route, strings.NewReader(body)))
	return rec
}

func TestKeysEndpointsRoundTrip(t *testing.T) {
	_, h := keysControl(t)

	// Add → fingerprint; raw key never in the response.
	rec := postKeys(t, h, "keys", `{"key":"sk-secret"}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("add status = %d, body %s", rec.Code, rec.Body)
	}
	if strings.Contains(rec.Body.String(), "sk-secret") {
		t.Fatalf("add response leaks the raw key: %s", rec.Body)
	}
	var added struct {
		Fingerprint string `json:"fingerprint"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &added); err != nil {
		t.Fatal(err)
	}
	if added.Fingerprint != keys.Fingerprint("sk-secret") {
		t.Fatalf("fingerprint = %q, want %q", added.Fingerprint, keys.Fingerprint("sk-secret"))
	}

	// List contains the fingerprint only.
	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, ControlPrefix+"keys", nil))
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), added.Fingerprint) {
		t.Fatalf("list status=%d body=%s", rec.Code, rec.Body)
	}
	if strings.Contains(rec.Body.String(), "sk-secret") {
		t.Fatalf("list leaks the raw key: %s", rec.Body)
	}

	// Delete → 200, then 404 for the stale fingerprint.
	rec = postKeys(t, h, "keys/delete", `{"fingerprint":"`+added.Fingerprint+`"}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("delete status = %d, body %s", rec.Code, rec.Body)
	}
	rec = postKeys(t, h, "keys/delete", `{"fingerprint":"`+added.Fingerprint+`"}`)
	if rec.Code != http.StatusNotFound {
		t.Fatalf("stale delete status = %d, want 404", rec.Code)
	}

	// The file now holds an empty key list for the selected sub-pool.
	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, ControlPrefix+"keys", nil))
	if !strings.Contains(rec.Body.String(), `"keys": []`) {
		t.Fatalf("list after delete should be empty: %s", rec.Body)
	}
}

func TestKeysEndpointsRejectBadInput(t *testing.T) {
	_, h := keysControl(t)

	if rec := postKeys(t, h, "keys", `not json`); rec.Code != http.StatusBadRequest {
		t.Fatalf("bad JSON status = %d, want 400", rec.Code)
	}
	if rec := postKeys(t, h, "keys", `{"key":""}`); rec.Code != http.StatusBadRequest {
		t.Fatalf("empty key status = %d, want 400", rec.Code)
	}
	if rec := postKeys(t, h, "keys/delete", `{}`); rec.Code != http.StatusBadRequest {
		t.Fatalf("missing fingerprint status = %d, want 400", rec.Code)
	}
}

func TestKeysEndpointsUnconfigured(t *testing.T) {
	c := &Control{Router: newTestRouter(t), Listen: "127.0.0.1:8787"}
	h := c.Handler(http.NewServeMux())

	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, ControlPrefix+"keys", nil))
	if rec.Code != http.StatusMethodNotAllowed {
		t.Fatalf("unconfigured list status = %d, want 405", rec.Code)
	}
	if rec := postKeys(t, h, "keys", `{"key":"x"}`); rec.Code != http.StatusMethodNotAllowed {
		t.Fatalf("unconfigured add status = %d, want 405", rec.Code)
	}
}
