package zen

import "testing"

// TestBuildHeaders asserts the literal disguise header union the daemon must
// send upstream. Ported from the plugin disguiseHeaders
// (dsh-opencode-zen lib/index.js:223-240), which the request loop spreads
// into every upstream attempt alongside Content-Type/Authorization
// (lib/index.js:1372-1374). Content-Type and Authorization are deliberately
// NOT built here — the handler and rotator set them.
func TestBuildHeaders(t *testing.T) {
	const (
		session   = "ses_018f4b3c9a2d4e5f8b6c1d0e3f"
		requestID = "req_5f2c9a7b1d4e8f03a6c2b9d5e7f10483"
		project   = "prj_6b86b273ff34fce19d6b804e"
	)

	t.Run("full union with all ids present", func(t *testing.T) {
		h := BuildHeaders(HeaderInput{
			Session:                session,
			RequestID:              requestID,
			Project:                project,
			IncludeSessionAffinity: true,
		})

		want := map[string]string{
			"user-agent":            "opencode/1.18.34",
			"x-opencode-session-id": session,
			"x-opencode-client":     "cli",
			"x-opencode-session":    session,
			"x-session-affinity":    session,
			"X-Session-Id":          session,
			"x-opencode-request":    requestID,
			"x-opencode-project":    project,
		}
		if len(h) != len(want) {
			t.Errorf("header count = %d, want %d; got %v", len(h), len(want), h)
		}
		for k, v := range want {
			if got := h.Get(k); got != v {
				t.Errorf("header %q = %q, want %q", k, got, v)
			}
		}
		if got := h.Get("Content-Type"); got != "" {
			t.Errorf("Content-Type = %q, want unset (handler sets it)", got)
		}
		if got := h.Get("Authorization"); got != "" {
			t.Errorf("Authorization = %q, want unset (rotator sets it)", got)
		}
	})

	t.Run("explicit user agent overrides the default literal", func(t *testing.T) {
		h := BuildHeaders(HeaderInput{UserAgent: "opencode/9.9.9"})
		if got := h.Get("user-agent"); got != "opencode/9.9.9" {
			t.Errorf("user-agent = %q, want %q", got, "opencode/9.9.9")
		}
	})

	t.Run("project header omitted when project empty", func(t *testing.T) {
		h := BuildHeaders(HeaderInput{
			Session:                session,
			RequestID:              requestID,
			IncludeSessionAffinity: true,
		})
		if got := h.Get("x-opencode-project"); got != "" {
			t.Errorf("x-opencode-project = %q, want unset", got)
		}
		if len(h) != 7 {
			t.Errorf("header count = %d, want 7; got %v", len(h), h)
		}
	})

	t.Run("request header omitted when request id empty", func(t *testing.T) {
		h := BuildHeaders(HeaderInput{Session: session, IncludeSessionAffinity: true})
		if got := h.Get("x-opencode-request"); got != "" {
			t.Errorf("x-opencode-request = %q, want unset", got)
		}
		// user-agent + client + 4 session-derived headers.
		if len(h) != 6 {
			t.Errorf("header count = %d, want 6; got %v", len(h), h)
		}
	})

	t.Run("session derived headers omitted when session empty", func(t *testing.T) {
		h := BuildHeaders(HeaderInput{
			RequestID:              requestID,
			Project:                project,
			IncludeSessionAffinity: true,
		})
		for _, k := range []string{"x-opencode-session", "x-opencode-session-id", "x-session-affinity", "X-Session-Id"} {
			if got := h.Get(k); got != "" {
				t.Errorf("header %q = %q, want unset", k, got)
			}
		}
		if len(h) != 4 {
			t.Errorf("header count = %d, want 4; got %v", len(h), h)
		}
	})

	t.Run("affinity pair omitted when not requested", func(t *testing.T) {
		h := BuildHeaders(HeaderInput{
			Session:   session,
			RequestID: requestID,
			Project:   project,
		})
		for _, k := range []string{"x-session-affinity", "X-Session-Id"} {
			if got := h.Get(k); got != "" {
				t.Errorf("header %q = %q, want unset", k, got)
			}
		}
		if len(h) != 6 {
			t.Errorf("header count = %d, want 6; got %v", len(h), h)
		}
	})
}
