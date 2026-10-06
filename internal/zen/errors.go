// Upstream error classification: ported from dsh-opencode-zen lib/index.js
// v0.15.1 (DAILY_LIMIT_RE at line 405, gatewayError/httpFailure at lines
// 700-770, parseRetryAfter, DAILY_LIMIT_RE raw-body match at line 1405)
// onto the Kind taxonomy of spec §4 of the gateway design ("Error
// envelopes", design doc lines 139-150).
package zen

import (
	"bytes"
	"encoding/json"
	"fmt"
	"regexp"
	"strconv"
	"strings"
	"time"
)

// Kind classifies one upstream (opencode.ai) failure for the rotation
// state machine and the client-facing error mapping.
type Kind int

const (
	// KindNone is the zero value: no error (2xx responses classify to nil,
	// not to a *UpstreamError).
	KindNone Kind = iota
	// KindDailyLimit is a free/Go/black usage-window 429
	// (FreeUsageLimitError, GoUsageLimitError, BlackUsageLimitError).
	KindDailyLimit
	// KindKeyRateLimit is the per-key request-rate 429 (RateLimitError).
	KindKeyRateLimit
	// KindAuth is a credential/account rejection
	// (AuthError, CreditsError, MonthlyLimitError, UserLimitError, bare 401).
	KindAuth
	// KindModel is a model-served problem — notably the spec §4 trap of a
	// 401 carrying error.type ModelError ("unknown model" is 401, not 400).
	KindModel
	// KindRegion is a 403 region/policy gate (RegionError, DataPolicyError).
	KindRegion
	// KindProviderRelay is a relayed provider body without a gateway
	// envelope ("Error from provider (Name): …").
	KindProviderRelay
	// KindServer is a 5xx, or a malformed/empty body behind a 5xx status.
	KindServer
	// KindClient is the catch-all for other 4xx (and generic 403s).
	KindClient
	// KindTransport is a local transport failure — dial error, broken
	// response body, or stream watchdog timeout — where the attempt never
	// received a classifiable HTTP answer (Task 12).
	KindTransport
)

// String returns a stable snake_case name for logs and client-facing codes.
func (k Kind) String() string {
	switch k {
	case KindNone:
		return "none"
	case KindDailyLimit:
		return "daily_limit"
	case KindKeyRateLimit:
		return "key_rate_limit"
	case KindAuth:
		return "auth"
	case KindModel:
		return "model"
	case KindRegion:
		return "region"
	case KindProviderRelay:
		return "provider_relay"
	case KindServer:
		return "server"
	case KindClient:
		return "client"
	case KindTransport:
		return "transport"
	default:
		return fmt.Sprintf("kind(%d)", int(k))
	}
}

// dailyLimitRe mirrors the plugin's DAILY_LIMIT_RE (lib/index.js:405)
// verbatim. It runs against the RAW body text: relayed provider bodies keep
// the gateway class names inside free text (lib/index.js:1405 tests raw).
var dailyLimitRe = regexp.MustCompile(`FreeUsageLimitError|GoUsageLimitError|BlackUsageLimitError`)

// relayMarker is the spec §4 relay prefix ("Error from provider (Name): ").
const relayMarker = "Error from provider"

// kindByErrorType maps gateway error.type values onto Kinds (spec §4
// status map). Types outside this table (e.g. OpenAI's "invalid_api_key")
// still populate UpstreamError.Type but classify by status instead.
var kindByErrorType = map[string]Kind{
	"FreeUsageLimitError":  KindDailyLimit,
	"GoUsageLimitError":    KindDailyLimit,
	"BlackUsageLimitError": KindDailyLimit,
	"RateLimitError":       KindKeyRateLimit,
	"AuthError":            KindAuth,
	"CreditsError":         KindAuth,
	"MonthlyLimitError":    KindAuth,
	"UserLimitError":       KindAuth,
	"ModelError":           KindModel,
	"RegionError":          KindRegion,
	"DataPolicyError":      KindRegion,
}

// UpstreamError is the classified result of one failed upstream response.
// A RetryAfter of 0 means "no window known": the header was absent and the
// Kind carries no default.
type UpstreamError struct {
	Kind       Kind
	Status     int
	Type       string // raw error.type (e.g. "FreeUsageLimitError"), may be ""
	Message    string // error.message when parseable, else a body snippet
	RetryAfter time.Duration
}

// Error implements the error interface.
func (e *UpstreamError) Error() string {
	if e == nil {
		return "upstream zen error: <nil>"
	}
	s := fmt.Sprintf("upstream zen error: kind=%s status=%d", e.Kind, e.Status)
	if e.Type != "" {
		s += " type=" + e.Type
	}
	if e.Message != "" {
		s += ": " + e.Message
	}
	return s
}

// IsRetryable reports whether the request may be re-issued (after rotation
// or backoff) rather than surfaced as terminal. Daily/key/server/relay
// failures rotate or wait; auth/model/region/client failures are terminal
// for this attempt. Safe on a nil receiver.
func (e *UpstreamError) IsRetryable() bool {
	if e == nil {
		return false
	}
	switch e.Kind {
	case KindDailyLimit, KindKeyRateLimit, KindServer, KindProviderRelay, KindTransport:
		return true
	default:
		return false
	}
}

// Classify maps an upstream HTTP failure onto a Kind. It is deliberately
// pure: the gateway handler reads the Retry-After header itself and passes
// the raw value as retryAfterHeader ("" = absent), so no *http.Response is
// needed and tests stay trivial. body is the raw response body.
//
// Precedence (ported from the plugin):
//  1. daily-limit regex on the RAW body text first — relayed provider
//     bodies carry the gateway class names in free text (index.js:1405);
//  2. parsed error.type — gateway envelope
//     {"type":"error","error":{"type","message"}}, tolerated OpenAI-style
//     {"error":{...}} and bare {"message":...}, also parsed from inside a
//     relayed "Error from provider (Name): {…}" prefix so Type carries the
//     raw upstream class even for relays;
//  3. relay marker "Error from provider" → KindProviderRelay (status kept);
//  4. status fallback: 5xx → KindServer, 401 → KindAuth, other → KindClient
//     (a generic 403 is NOT a region gate — KindRegion requires the
//     RegionError/DataPolicyError type).
//
// 1xx/2xx returns nil (KindNone: not an error). The spec §4 trap holds by
// construction: a 401 with error.type ModelError classifies in step 2 as
// KindModel, never KindAuth.
//
// RetryAfter: a present header always wins (decimal seconds or RFC1123
// HTTP-date, both clamped to a minimum of 1s like the plugin's
// parseRetryAfter). When the header is absent or unparsable: KindDailyLimit
// synthesizes seconds to the next UTC midnight (spec §4 line 161), a
// KindKeyRateLimit defaults to 60s (key lane, 1000 req/min), everything
// else stays 0.
func Classify(status int, body []byte, retryAfterHeader string) *UpstreamError {
	return classify(time.Now(), status, body, retryAfterHeader)
}

// classify is the testable core of Classify: now is injected so the
// UTC-midnight synthesis and HTTP-date parsing are deterministic.
func classify(now time.Time, status int, body []byte, retryAfterHeader string) *UpstreamError {
	if status >= 100 && status < 300 {
		return nil // 1xx/2xx: not an error.
	}
	typ, msg := parseEnvelope(body)

	var kind Kind
	switch {
	case dailyLimitRe.Match(body):
		kind = KindDailyLimit
	default:
		if k, known := kindByErrorType[typ]; known {
			kind = k
		} else if bytes.Contains(body, []byte(relayMarker)) {
			kind = KindProviderRelay
		} else {
			kind = kindByStatus(status)
		}
	}
	if msg == "" {
		msg = snippet(body)
	}

	retry, ok := parseRetryAfter(retryAfterHeader, now)
	if !ok {
		switch kind {
		case KindDailyLimit:
			retry = secondsToUTCMidnight(now)
		case KindKeyRateLimit:
			retry = 60 * time.Second
		}
	}
	return &UpstreamError{
		Kind:       kind,
		Status:     status,
		Type:       typ,
		Message:    msg,
		RetryAfter: retry,
	}
}

// kindByStatus is the fallback used when neither the daily regex nor a
// known error.type applied: 5xx → server, 401 → auth (bare 401 = real auth
// rejection, mirroring the plugin's INVALID_CREDENTIAL), everything else →
// client catch-all. A bare 429 lands in the catch-all: the gateway always
// types its 429s (spec §4), so an untyped one is anomalous — and guessing
// key-lane there would fabricate a window the daemon does not know.
func kindByStatus(status int) Kind {
	switch {
	case status >= 500:
		return KindServer
	case status == 401:
		return KindAuth
	default:
		return KindClient
	}
}

// maxEnvelopeBytes mirrors the plugin's gatewayError guard
// (lib/index.js:703): bodies beyond this are not gateway envelopes.
const maxEnvelopeBytes = 20000

type envelopeTop struct {
	Error   json.RawMessage `json:"error"`
	Message string          `json:"message"`
}

type envelopeInner struct {
	Type    string `json:"type"`
	Message string `json:"message"`
}

// parseEnvelope extracts (error.type, error.message) from a body. It
// accepts, in order: the spec §4 gateway envelope, an OpenAI-style
// {"error":{...}} object, a bare {"message":...}, and — when the full body
// is not JSON — the first embedded JSON object behind a relay prefix.
// Both values are "" when nothing parseable is found.
func parseEnvelope(body []byte) (typ, msg string) {
	if len(body) == 0 || len(body) > maxEnvelopeBytes {
		return "", ""
	}
	if t, m, ok := decodeEnvelope(body); ok {
		return t, m
	}
	if i := bytes.IndexByte(body, '{'); i > 0 {
		// Relayed bodies prefix provider text before a JSON payload
		// ("Error from provider (Name): {...}"): recover the raw
		// error.type/message from the embedded object when parseable.
		if t, m, ok := decodeEnvelope(body[i:]); ok {
			return t, m
		}
	}
	return "", ""
}

// decodeEnvelope tries one JSON object; ok reports whether an error shape
// with a type or message was recovered.
func decodeEnvelope(b []byte) (typ, msg string, ok bool) {
	var top envelopeTop
	if err := json.Unmarshal(b, &top); err != nil {
		return "", "", false
	}
	if len(top.Error) > 0 {
		var inner envelopeInner
		if err := json.Unmarshal(top.Error, &inner); err == nil && (inner.Type != "" || inner.Message != "") {
			return inner.Type, inner.Message, true
		}
	}
	if top.Message != "" {
		return "", top.Message, true
	}
	return "", "", false
}

// parseRetryAfter mirrors the plugin's parseRetryAfter (lib/index.js:768):
// decimal seconds first, RFC1123 HTTP-date otherwise; both clamp to a
// minimum of 1s so RetryAfter == 0 unambiguously means "absent". ok=false
// means the header is absent or unparsable — the caller then applies the
// per-kind defaults.
func parseRetryAfter(header string, now time.Time) (time.Duration, bool) {
	h := strings.TrimSpace(header)
	if h == "" || !isDigits(h) {
		if h == "" {
			return 0, false
		}
		if when, err := time.Parse(time.RFC1123, h); err == nil {
			d := when.Sub(now).Round(time.Second)
			if d < time.Second {
				d = time.Second
			}
			return d, true
		}
		return 0, false
	}
	n, err := strconv.ParseInt(h, 10, 64)
	if err != nil {
		return 0, false // beyond int64: treat as absent rather than overflow
	}
	if n < 1 {
		n = 1
	}
	const maxRetrySeconds = int64(1<<63-1) / int64(time.Second)
	if n > maxRetrySeconds {
		n = maxRetrySeconds
	}
	return time.Duration(n) * time.Second, true
}

func isDigits(s string) bool {
	for _, r := range s {
		if r < '0' || r > '9' {
			return false
		}
	}
	return s != ""
}

// secondsToUTCMidnight returns whole seconds from now until the next UTC
// midnight — the IP-lane daily-window rule (spec §4 line 161).
func secondsToUTCMidnight(now time.Time) time.Duration {
	u := now.UTC()
	next := time.Date(u.Year(), u.Month(), u.Day()+1, 0, 0, 0, 0, time.UTC)
	return next.Sub(u).Round(time.Second)
}

// snippet bounds a body for Message: the plugin slices failures to 300
// chars (lib/index.js httpFailure).
const maxSnippet = 300

func snippet(body []byte) string {
	s := strings.TrimSpace(string(body))
	if len(s) <= maxSnippet {
		return s
	}
	return strings.TrimSpace(s[:maxSnippet])
}
