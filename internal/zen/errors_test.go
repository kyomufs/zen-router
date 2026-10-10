package zen

import (
	"fmt"
	"strings"
	"testing"
	"time"
	"unicode/utf8"
)

// Fixtures follow spec §4 "Error envelopes" (gateway design doc lines
// 139-150):
//
//	gateway envelope: {"type":"error","error":{"type":"<Class>","message":"..."}}
//	relay prefix:     "Error from provider (Name): "
//
// plus the OpenAI-style error shape {"error":{"message","type","code"}}
// documented at line 184 and the UTC-midnight retry-after rule at line 161.

// errEnvelope builds a spec-§4 gateway error envelope.
func errEnvelope(class, message string) []byte {
	return []byte(fmt.Sprintf(`{"type":"error","error":{"type":%q,"message":%q}}`, class, message))
}

// errNow is a fixed clock (15:00 UTC) so the midnight synthesis in
// Classify is exact: seconds until the next UTC midnight == 9h.
var errNow = time.Date(2026, 10, 6, 15, 0, 0, 0, time.UTC)

func TestClassifyDailyLimit(t *testing.T) {
	for _, class := range []string{"FreeUsageLimitError", "GoUsageLimitError", "BlackUsageLimitError"} {
		t.Run(class+" envelope", func(t *testing.T) {
			got := Classify(429, errEnvelope(class, "daily usage limit reached"), "")
			if got == nil {
				t.Fatal("Classify returned nil, want *UpstreamError")
			}
			if got.Kind != KindDailyLimit {
				t.Errorf("Kind = %v, want KindDailyLimit", got.Kind)
			}
			if got.Status != 429 {
				t.Errorf("Status = %d, want 429", got.Status)
			}
			if got.Type != class {
				t.Errorf("Type = %q, want %q", got.Type, class)
			}
			if got.Message != "daily usage limit reached" {
				t.Errorf("Message = %q, want %q", got.Message, "daily usage limit reached")
			}
		})
	}

	t.Run("raw-text relayed body", func(t *testing.T) {
		// Provider-relayed bodies keep gateway class names in free text;
		// the plugin matches DAILY_LIMIT_RE against the raw body
		// (lib/index.js:1583), so this must win over the relay marker.
		body := []byte(`Error from provider (OpenAI): {"error":{"message":"FreeUsageLimitError: daily usage limit exceeded","type":"rate_limit_exceeded"}}`)
		got := Classify(429, body, "")
		if got == nil {
			t.Fatal("Classify returned nil, want *UpstreamError")
		}
		if got.Kind != KindDailyLimit {
			t.Errorf("Kind = %v, want KindDailyLimit (daily regex runs before the relay marker)", got.Kind)
		}
		if got.Status != 429 {
			t.Errorf("Status = %d, want 429", got.Status)
		}
		if got.Type != "rate_limit_exceeded" {
			t.Errorf("Type = %q, want raw error.type %q carried through the relay prefix", got.Type, "rate_limit_exceeded")
		}
	})

	t.Run("retry-after header parsed", func(t *testing.T) {
		got := Classify(429, errEnvelope("FreeUsageLimitError", "limit"), "120")
		if got == nil {
			t.Fatal("Classify returned nil, want *UpstreamError")
		}
		if want := 120 * time.Second; got.RetryAfter != want {
			t.Errorf("RetryAfter = %v, want %v", got.RetryAfter, want)
		}
	})

	t.Run("absent header synthesizes seconds to UTC midnight", func(t *testing.T) {
		got := classify(errNow, 429, errEnvelope("FreeUsageLimitError", "limit"), "")
		if got == nil {
			t.Fatal("classify returned nil, want *UpstreamError")
		}
		if want := 9 * time.Hour; got.RetryAfter != want {
			t.Errorf("RetryAfter = %v, want exactly %v (15:00 UTC → next midnight)", got.RetryAfter, want)
		}
	})
}

// TestClassifyDailyRegexGatedOn429 pins the DM-7 corpus decision: the
// free-text quota regex may produce KindDailyLimit only behind a 429 —
// the plugin gates the same match on status (a416790 lib/index.js:1579
// returns before the regex at :1583). A non-429 body that merely quotes
// a class name must fall through to the typed/status precedence instead
// of marking the daily window exhausted.
func TestClassifyDailyRegexGatedOn429(t *testing.T) {
	t.Run("500 prose quoting a quota class", func(t *testing.T) {
		got := Classify(500, []byte("upstream said FreeUsageLimitError while failing"), "")
		if got == nil {
			t.Fatal("Classify returned nil, want *UpstreamError")
		}
		if got.Kind != KindServer {
			t.Errorf("Kind = %v, want KindServer (free-text regex must not fire without a 429)", got.Kind)
		}
	})

	t.Run("403 prose quoting a quota class", func(t *testing.T) {
		got := Classify(403, []byte("blocked: GoUsageLimitError mentioned in body"), "")
		if got == nil {
			t.Fatal("Classify returned nil, want *UpstreamError")
		}
		if got.Kind != KindClient {
			t.Errorf("Kind = %v, want KindClient (generic 403 stays a client catch-all)", got.Kind)
		}
	})

	t.Run("typed quota envelope still classifies without a 429", func(t *testing.T) {
		// The kindByErrorType path stays status-ungated on purpose: an
		// explicit error.type is the gateway's own verdict (spec §4
		// type-over-status, same principle as the ModelError trap).
		got := Classify(503, errEnvelope("FreeUsageLimitError", "daily usage limit exceeded"), "")
		if got == nil {
			t.Fatal("Classify returned nil, want *UpstreamError")
		}
		if got.Kind != KindDailyLimit {
			t.Errorf("Kind = %v, want KindDailyLimit (typed path ungated)", got.Kind)
		}
	})
}

func TestClassifyKeyRateLimit(t *testing.T) {
	body := errEnvelope("RateLimitError", "key rate limit exceeded")

	t.Run("retry-after 60 header", func(t *testing.T) {
		got := Classify(429, body, "60")
		if got == nil {
			t.Fatal("Classify returned nil, want *UpstreamError")
		}
		if got.Kind != KindKeyRateLimit {
			t.Errorf("Kind = %v, want KindKeyRateLimit", got.Kind)
		}
		if got.Status != 429 {
			t.Errorf("Status = %d, want 429", got.Status)
		}
		if got.Type != "RateLimitError" {
			t.Errorf("Type = %q, want RateLimitError", got.Type)
		}
		if want := 60 * time.Second; got.RetryAfter != want {
			t.Errorf("RetryAfter = %v, want %v", got.RetryAfter, want)
		}
	})

	t.Run("absent header defaults to 60s", func(t *testing.T) {
		got := Classify(429, body, "")
		if got == nil {
			t.Fatal("Classify returned nil, want *UpstreamError")
		}
		if want := 60 * time.Second; got.RetryAfter != want {
			t.Errorf("RetryAfter = %v, want key-lane default %v", got.RetryAfter, want)
		}
	})
}

func TestClassifyModel(t *testing.T) {
	// Spec §4 trap: "unknown model" is 401 with error.type ModelError.
	// It must classify as a model problem, never as a credential failure —
	// rotating pooled keys over it just burns one futile request per key.
	got := Classify(401, errEnvelope("ModelError", "model not found"), "")
	if got == nil {
		t.Fatal("Classify returned nil, want *UpstreamError")
	}
	if got.Kind != KindModel {
		t.Errorf("Kind = %v, want KindModel (401 ModelError must NOT classify as auth)", got.Kind)
	}
	if got.Status != 401 {
		t.Errorf("Status = %d, want 401 (upstream status preserved)", got.Status)
	}
	if got.Type != "ModelError" {
		t.Errorf("Type = %q, want ModelError", got.Type)
	}
}

func TestClassifyAuth(t *testing.T) {
	for _, class := range []string{"AuthError", "CreditsError", "MonthlyLimitError", "UserLimitError"} {
		t.Run(class+" envelope", func(t *testing.T) {
			got := Classify(401, errEnvelope(class, "credential rejected"), "")
			if got == nil {
				t.Fatal("Classify returned nil, want *UpstreamError")
			}
			if got.Kind != KindAuth {
				t.Errorf("Kind = %v, want KindAuth", got.Kind)
			}
			if got.Status != 401 {
				t.Errorf("Status = %d, want 401", got.Status)
			}
			if got.Type != class {
				t.Errorf("Type = %q, want %q", got.Type, class)
			}
		})
	}

	t.Run("bare 401 without envelope", func(t *testing.T) {
		got := Classify(401, nil, "")
		if got == nil {
			t.Fatal("Classify returned nil, want *UpstreamError")
		}
		if got.Kind != KindAuth {
			t.Errorf("Kind = %v, want KindAuth (status fallback: 401 → auth)", got.Kind)
		}
	})

	t.Run("OpenAI-style error object", func(t *testing.T) {
		// Tolerated shape (spec §5 line 184): {"error": {"message","type","code"}}.
		body := []byte(`{"error":{"message":"Incorrect API key provided","type":"invalid_api_key","code":401}}`)
		got := Classify(401, body, "")
		if got == nil {
			t.Fatal("Classify returned nil, want *UpstreamError")
		}
		if got.Kind != KindAuth {
			t.Errorf("Kind = %v, want KindAuth (unknown type falls back to status)", got.Kind)
		}
		if got.Type != "invalid_api_key" {
			t.Errorf("Type = %q, want raw error.type %q", got.Type, "invalid_api_key")
		}
		if got.Message != "Incorrect API key provided" {
			t.Errorf("Message = %q, want %q", got.Message, "Incorrect API key provided")
		}
	})
}

func TestClassifyRegion(t *testing.T) {
	for _, class := range []string{"RegionError", "DataPolicyError"} {
		t.Run(class+" envelope", func(t *testing.T) {
			got := Classify(403, errEnvelope(class, "not available in your region"), "")
			if got == nil {
				t.Fatal("Classify returned nil, want *UpstreamError")
			}
			if got.Kind != KindRegion {
				t.Errorf("Kind = %v, want KindRegion", got.Kind)
			}
			if got.Status != 403 {
				t.Errorf("Status = %d, want 403", got.Status)
			}
			if got.Type != class {
				t.Errorf("Type = %q, want %q", got.Type, class)
			}
		})
	}

	t.Run("generic 403 is not a region gate", func(t *testing.T) {
		// 403 qualifies as KindRegion only for RegionError/DataPolicyError;
		// any other 403 is the 4xx catch-all.
		got := Classify(403, []byte("forbidden"), "")
		if got == nil {
			t.Fatal("Classify returned nil, want *UpstreamError")
		}
		if got.Kind != KindClient {
			t.Errorf("Kind = %v, want KindClient for a generic 403", got.Kind)
		}
	})
}

func TestClassifyProviderRelay(t *testing.T) {
	// Relayed provider body, no gateway envelope (spec §4 lines 146-148).
	body := []byte(`Error from provider (OpenAI): {"error":{"message":"The server is busy","type":"server_error"}}`)
	got := Classify(429, body, "30")
	if got == nil {
		t.Fatal("Classify returned nil, want *UpstreamError")
	}
	if got.Kind != KindProviderRelay {
		t.Errorf("Kind = %v, want KindProviderRelay", got.Kind)
	}
	if got.Status != 429 {
		t.Errorf("Status = %d, want 429 (upstream status preserved)", got.Status)
	}
	if want := 30 * time.Second; got.RetryAfter != want {
		t.Errorf("RetryAfter = %v, want %v (header still honored for relays)", got.RetryAfter, want)
	}
	if got.Type != "server_error" {
		t.Errorf("Type = %q, want raw error.type %q carried through the relay prefix", got.Type, "server_error")
	}
	if got.Message != "The server is busy" {
		t.Errorf("Message = %q, want %q", got.Message, "The server is busy")
	}
}

func TestClassify5xx(t *testing.T) {
	t.Run("500 plain body", func(t *testing.T) {
		got := Classify(500, []byte("plain upstream failure"), "")
		if got == nil {
			t.Fatal("Classify returned nil, want *UpstreamError")
		}
		if got.Kind != KindServer {
			t.Errorf("Kind = %v, want KindServer", got.Kind)
		}
		if got.Status != 500 {
			t.Errorf("Status = %d, want 500", got.Status)
		}
	})

	t.Run("502 empty body", func(t *testing.T) {
		got := Classify(502, nil, "")
		if got == nil {
			t.Fatal("Classify returned nil, want *UpstreamError")
		}
		if got.Kind != KindServer {
			t.Errorf("Kind = %v, want KindServer for 5xx with empty body", got.Kind)
		}
	})
}

func TestClassifyMalformed(t *testing.T) {
	t.Run("400 garbage", func(t *testing.T) {
		got := Classify(400, []byte("not json at all"), "")
		if got == nil {
			t.Fatal("Classify returned nil, want *UpstreamError")
		}
		if got.Kind != KindClient {
			t.Errorf("Kind = %v, want KindClient (4xx catch-all)", got.Kind)
		}
	})

	t.Run("400 truncated envelope", func(t *testing.T) {
		got := Classify(400, []byte(`{"type":"error","error":`), "")
		if got == nil {
			t.Fatal("Classify returned nil, want *UpstreamError")
		}
		if got.Kind != KindClient {
			t.Errorf("Kind = %v, want KindClient", got.Kind)
		}
	})

	t.Run("bare message shape 400", func(t *testing.T) {
		// Tolerated shape: {"message": "..."} with no error object.
		got := Classify(400, []byte(`{"message":"bad request"}`), "")
		if got == nil {
			t.Fatal("Classify returned nil, want *UpstreamError")
		}
		if got.Kind != KindClient {
			t.Errorf("Kind = %v, want KindClient", got.Kind)
		}
		if got.Message != "bad request" {
			t.Errorf("Message = %q, want %q", got.Message, "bad request")
		}
	})

	t.Run("429 without gateway envelope", func(t *testing.T) {
		// A plain 429 carries no quota evidence: classified as the IP
		// lane limit (KindIPLimit), retryable so the warp lane may rotate
		// the egress identity — never guessed as a daily quota.
		got := Classify(429, nil, "")
		if got == nil {
			t.Fatal("Classify returned nil, want *UpstreamError")
		}
		if got.Kind != KindIPLimit {
			t.Errorf("Kind = %v, want KindIPLimit (bare 429 = ip lane)", got.Kind)
		}
		if !got.IsRetryable() {
			t.Error("IsRetryable = false, want true (ip lane may rotate)")
		}
		if got.RetryAfter != 60*time.Second {
			t.Errorf("RetryAfter = %v, want 60s default", got.RetryAfter)
		}
	})

	t.Run("200 is not an error", func(t *testing.T) {
		if got := Classify(200, []byte("ok"), ""); got != nil {
			t.Errorf("Classify(200) = %#v, want nil (KindNone: not an error)", got)
		}
	})
}

func TestClassifyRetryAfterHTTPDate(t *testing.T) {
	// The plugin accepts both decimal seconds and an HTTP-date
	// (index.js parseRetryAfter: Number() then Date.parse); our port
	// parses whole (integer) seconds or the HTTP-date — this test pins
	// the date path.
	header := errNow.Add(90 * time.Second).Format(time.RFC1123)
	got := classify(errNow, 429, errEnvelope("RateLimitError", "slow down"), header)
	if got == nil {
		t.Fatal("classify returned nil, want *UpstreamError")
	}
	if want := 90 * time.Second; got.RetryAfter != want {
		t.Errorf("RetryAfter = %v, want %v (HTTP-date header)", got.RetryAfter, want)
	}
}

func TestUpstreamErrorIsRetryable(t *testing.T) {
	cases := []struct {
		kind Kind
		want bool
	}{
		{KindDailyLimit, true},
		{KindKeyRateLimit, true},
		{KindServer, true},
		{KindProviderRelay, true},
		{KindAuth, false},
		{KindModel, false},
		{KindRegion, false},
		{KindClient, false},
		{KindNone, false},
	}
	for _, c := range cases {
		e := &UpstreamError{Kind: c.kind}
		if got := e.IsRetryable(); got != c.want {
			t.Errorf("IsRetryable(%v) = %v, want %v", c.kind, got, c.want)
		}
	}

	var nilErr *UpstreamError
	if nilErr.IsRetryable() {
		t.Error("nil.IsRetryable() = true, want false")
	}
}

func TestUpstreamErrorString(t *testing.T) {
	e := Classify(429, errEnvelope("RateLimitError", "slow down"), "60")
	if e == nil {
		t.Fatal("Classify returned nil, want *UpstreamError")
	}
	msg := e.Error()
	for _, want := range []string{"key_rate_limit", "429", "slow down"} {
		if !strings.Contains(msg, want) {
			t.Errorf("Error() = %q, want it to contain %q", msg, want)
		}
	}

	var nilErr *UpstreamError
	if msg := nilErr.Error(); !strings.Contains(msg, "nil") {
		t.Errorf("nil.Error() = %q, want a safe <nil> rendering", msg)
	}
}

// TestSnippetKeepsRuneBoundary: the Message snippet bounds a body at 300
// BYTES (lib/index.js httpFailure parity), but a naive byte slice can cut a
// multi-byte rune in half — a Cyrillic body straddling byte 300 would emit
// invalid UTF-8. The truncation must back off to a rune boundary while
// staying within the byte budget.
func TestSnippetKeepsRuneBoundary(t *testing.T) {
	var sb strings.Builder
	sb.WriteString(strings.Repeat("a", 299)) // byte 299 starts a 2-byte п
	sb.WriteString(strings.Repeat("привет мир ", 50))
	body := []byte(sb.String())
	if len(body) <= maxSnippet {
		t.Fatalf("fixture length = %d, want > maxSnippet=%d", len(body), maxSnippet)
	}

	got := snippet(body)
	if !utf8.ValidString(got) {
		t.Errorf("snippet split a multi-byte rune: invalid UTF-8 at the cut (% x)",
			got[len(got)-3:])
	}
	if len(got) > maxSnippet {
		t.Errorf("snippet length = %d, want <= %d (byte budget must hold)", len(got), maxSnippet)
	}
	if trimmed := strings.TrimSpace(string(body)); !strings.HasPrefix(trimmed, got) {
		t.Errorf("snippet is not a prefix of the body: cut altered content")
	}
	// The boundary must have backed off to a complete rune, not merely
	// happened to be valid: the byte right after the cut starts a rune.
	if len(got) < maxSnippet-1 {
		t.Errorf("snippet length = %d, expected maxSnippet minus at most one incomplete rune", len(got))
	}
}
