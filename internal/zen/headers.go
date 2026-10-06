package zen

import "net/http"

// DefaultUserAgent is the exact user agent the opencode CLI puts on Zen
// requests: the plain `opencode/${InstallationVersion}` form used by the
// opencode provider lane (dsh-opencode-zen lib/index.js:217-219,
// opencodeUserAgent).
const DefaultUserAgent = "opencode/1.18.34"

// HeaderInput carries the values BuildHeaders needs to assemble the
// disguise header union.
type HeaderInput struct {
	// Session is the canonical ses_ session id. Empty omits every
	// session-derived header (x-opencode-session, x-opencode-session-id,
	// and the affinity pair).
	Session string
	// RequestID is the per-attempt req_ id; empty omits x-opencode-request.
	RequestID string
	// Project is the stable prj_ project id; empty omits x-opencode-project.
	Project string
	// UserAgent overrides DefaultUserAgent when non-empty.
	UserAgent string
	// IncludeSessionAffinity adds the x-session-affinity / X-Session-Id
	// pair. The plugin's disguiseHeaders always includes the pair
	// (lib/index.js:234-235); the gateway passes true to mirror that
	// exact union, false only when a caller opts out deliberately.
	IncludeSessionAffinity bool
}

// BuildHeaders assembles the disguise header union exactly as the plugin's
// disguiseHeaders (dsh-opencode-zen lib/index.js:223-240) sends it on every
// upstream attempt. x-opencode-session-id and x-opencode-session carry the
// SAME canonical session id. Content-Type and Authorization are deliberately
// NOT included: the request loop adds them at the call site
// (lib/index.js:1372-1374), so the handler/rotator own them here.
func BuildHeaders(in HeaderInput) http.Header {
	h := make(http.Header)
	userAgent := in.UserAgent
	if userAgent == "" {
		userAgent = DefaultUserAgent
	}
	h.Set("user-agent", userAgent)
	h.Set("x-opencode-client", "cli")
	if in.Session != "" {
		h.Set("x-opencode-session-id", in.Session)
		h.Set("x-opencode-session", in.Session)
		if in.IncludeSessionAffinity {
			h.Set("x-session-affinity", in.Session)
			h.Set("X-Session-Id", in.Session)
		}
	}
	if in.RequestID != "" {
		h.Set("x-opencode-request", in.RequestID)
	}
	if in.Project != "" {
		h.Set("x-opencode-project", in.Project)
	}
	return h
}
