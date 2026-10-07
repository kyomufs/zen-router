// Package gateway is the OpenAI-facing client surface of the daemon. It
// accepts OpenAI-style requests under /v1/*, shapes them onto the Zen wire
// (plan Task 12; spec §4 error envelopes and §6 staged rotation), and runs
// every upstream attempt through a Rotator implemented by internal/router.
// Pure protocol logic lives in internal/zen; this package owns the HTTP
// plumbing, the bounded re-issue attempt loop, the watchdog handoff, and
// the client-facing error envelopes.
package gateway

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strconv"
	"strings"
	"time"

	"zen-router/internal/config"
	"zen-router/internal/proxy"
	"zen-router/internal/router"
	"zen-router/internal/zen"
)

// Rotator is the staged-rotation seam: Attempt returns the first attempt
// (stage 0), NextAttempt receives the report of one FAILED attempt and
// returns the next attempt (or false when the stage machine is exhausted).
// internal/router.Router implements it.
type Rotator interface {
	Attempt() router.Attempt
	NextAttempt(rep router.Report) (router.Attempt, bool)
}

// Recorder is the optional dashboard-data seam (plan Task 1, spec §7): after
// an attempt receives a 2xx upstream response, the handler reports the
// egress, the API key and the response latency (time to upstream headers).
// internal/router.Router implements it — success counters per egress AND per
// key plus the per-egress latency view. The Handler field defaults to nil:
// a nil Recorder makes no calls, so behavior is unchanged for every existing
// caller and for Rotator fakes that do not implement it.
type Recorder interface {
	RecordSuccess(egress proxy.Egress, key string, latencyMS int64)
}

const (
	// maxBodyBytes bounds the buffered client request body (plan Task 12).
	maxBodyBytes = 4 << 20
	// maxErrorBody bounds how much of a failed upstream response is read
	// for classification (envelopes stop at 20 KB; this is the raw cap).
	maxErrorBody = 1 << 20
	// maxAttempts is D1: at most 3 EXECUTED upstream attempts per client
	// request (spec §6). Counted separately from Attempt.Step, which is the
	// stage id and may skip (single-key pool: 0,2,3).
	maxAttempts = 3
)

// Handler serves the OpenAI surface: POST /v1/chat/completions (chat and
// Responses lanes) and GET /v1/models. The control API (/_zenctl/*) and the
// legacy reverse proxy (/zen/v1/*) are mounted by main.go OUTSIDE this
// handler (Task 13).
type Handler struct {
	// Rot drives staged rotation for every attempt.
	Rot Rotator
	// Cfg supplies upstream base URL and watchdog budgets.
	Cfg *config.Config
	// Upstream is the Zen gateway base URL (test seam). It shadows
	// Cfg.Upstream when non-empty.
	Upstream string
	// Recorder optionally receives every 2xx upstream attempt (plan Task 1).
	// Nil — the default and the state of every existing test fake — records
	// nothing.
	Recorder Recorder
}

// New builds the gateway handler for one rotator + config pair.
func New(rot Rotator, cfg *config.Config) *Handler {
	return &Handler{Rot: rot, Cfg: cfg, Upstream: cfg.Upstream}
}

// Mux mounts ONLY the OpenAI surface (/v1/*) for the client-facing
// listener. Task 13 mounts /_zenctl/* and /zen/v1/* outside this mux.
func Mux(rot Rotator, cfg *config.Config) *http.ServeMux {
	mux := http.NewServeMux()
	mux.Handle("/v1/", New(rot, cfg))
	return mux
}

// ServeHTTP routes the two OpenAI endpoints; anything else is a 404
// envelope.
func (h *Handler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	switch r.URL.Path {
	case "/v1/models":
		if r.Method != http.MethodGet {
			writeError(w, http.StatusMethodNotAllowed, "MethodNotAllowedError",
				"zen: /v1/models is GET-only", nil)
			return
		}
		h.serveModels(w)
	case "/v1/chat/completions":
		if r.Method != http.MethodPost {
			writeError(w, http.StatusMethodNotAllowed, "MethodNotAllowedError",
				"zen: /v1/chat/completions is POST-only", nil)
			return
		}
		h.serveChat(w, r)
	default:
		writeError(w, http.StatusNotFound, "NotFoundError",
			"zen: no route for "+r.URL.Path, nil)
	}
}

// modelsCreated is a fixed "created" stamp for the model list (the daemon
// serves a static table, so a constant is honest).
const modelsCreated = 1759718400

// serveModels answers GET /v1/models from the daemon's models table —
// no upstream call (spec §5).
func (h *Handler) serveModels(w http.ResponseWriter) {
	type model struct {
		ID      string `json:"id"`
		Object  string `json:"object"`
		Created int    `json:"created"`
		OwnedBy string `json:"owned_by"`
	}
	data := make([]model, 0, len(zen.MODELS))
	for _, m := range zen.MODELS {
		data = append(data, model{ID: m.ID, Object: "model", Created: modelsCreated, OwnedBy: "opencode"})
	}
	writeJSON(w, http.StatusOK, map[string]any{"object": "list", "data": data})
}

// serveChat is the attempt loop: buffer ≤4 MiB, resolve the model, clamp
// effort, shape the lane body, then execute attempts until the stream ends
// or the failure path surfaces an envelope (plan Task 12 line 185). The
// client's stream flag selects the sink only: SSE, or a buffered single
// JSON completion when the client asked for no streaming (spec §5:179).
func (h *Handler) serveChat(w http.ResponseWriter, r *http.Request) {
	raw, err := io.ReadAll(http.MaxBytesReader(w, r.Body, maxBodyBytes))
	if err != nil {
		var mbe *http.MaxBytesError
		if errors.As(err, &mbe) {
			writeError(w, http.StatusRequestEntityTooLarge, "PayloadTooLargeError",
				"zen: request body exceeds 4 MiB", nil)
			return
		}
		writeError(w, http.StatusBadRequest, "InvalidRequestError",
			"zen: reading request body: "+err.Error(), nil)
		return
	}
	var body map[string]any
	if err := json.Unmarshal(raw, &body); err != nil {
		writeError(w, http.StatusBadRequest, "InvalidRequestError",
			"zen: malformed JSON body: "+err.Error(), nil)
		return
	}

	// Client stream flag: it decides ONLY the local rendering (spec §5:179
	// standard OpenAI chat, whose default is false — only an explicit true
	// gets SSE). Captured here, before lane shaping forces stream:true
	// upstream: the upstream ALWAYS streams (relay framing, first-event and
	// stall watchdogs, staged rotation and translation are unchanged, and
	// the watchdogs still govern the UPSTREAM read below) — the flag only
	// switches the client sink between SSE and the buffered JSON completion
	// (buffer.go: frames accumulate in memory; ONE application/json body is
	// written on a clean end, so only the CLIENT write is deferred).
	streamReq, _ := body["stream"].(bool)

	// Model first: spec §4 parity says unknown/gated model ids are a 401
	// ModelError (not 400), mirrored locally since the daemon resolves the
	// table itself.
	modelID, _ := body["model"].(string)
	model, mErr := zen.ResolveModel(modelID)
	if mErr != nil {
		writeError(w, http.StatusUnauthorized, "ModelError", "zen: "+mErr.Error(), nil)
		return
	}

	// Request ids BEFORE shaping: conversation seed → canonical session,
	// fresh req_ id per attempt (rolled inside the loop), stable project.
	ids := zen.DeriveRequestIDs(toMessages(body["messages"]))

	// Effort clamping happens here, pre-translation (plan Task 12 carry-over
	// bullet): chat wire keeps "off"→"none"; ChatToResponses then omits the
	// reasoning key entirely for the Responses wire.
	effort, _ := body["reasoning_effort"].(string)
	if effort = zen.ClampEffort(model, effort); effort != "" {
		body["reasoning_effort"] = zen.ToChatWire(effort)
	} else {
		delete(body, "reasoning_effort")
	}

	// Lane selection: Responses-capable models translate the chat body onto
	// the Responses wire; everything else stays on the chat wire.
	var (
		useResponses = model.Responses
		upPath       string
		payload      []byte
	)
	if useResponses {
		// No EnsureFreeLaneShape on this lane: ChatToResponses injects the
		// gate tools itself and pins tool_choice to "auto".
		respBody, err := zen.ChatToResponses(body)
		if err != nil {
			// Input error: the client, not the upstream, must hear about it.
			writeError(w, http.StatusBadRequest, "InvalidRequestError",
				"zen: "+err.Error(), nil)
			return
		}
		respBody["stream"] = true // upstream always streams (carry-over bullet: pinned)
		if payload, err = json.Marshal(respBody); err != nil {
			writeError(w, http.StatusInternalServerError, "InternalError",
				"zen: encoding responses body: "+err.Error(), nil)
			return
		}
		upPath = "/zen/v1/responses"
	} else {
		body["stream"] = true // upstream always streams, whatever the client flag said
		body["stream_options"] = map[string]any{"include_usage": true}
		body = zen.EnsureFreeLaneShape(body)
		var err error
		if payload, err = json.Marshal(body); err != nil {
			writeError(w, http.StatusInternalServerError, "InternalError",
				"zen: encoding chat body: "+err.Error(), nil)
			return
		}
		upPath = "/zen/v1/chat/completions"
	}

	base := h.Upstream
	if base == "" {
		base = h.Cfg.Upstream
	}
	upURL := strings.TrimRight(base, "/") + upPath

	firstEvent := h.Cfg.FirstEventTimeout
	idle := h.Cfg.IdleTimeout
	if useResponses {
		idle = h.Cfg.ResponsesIdleTimeout
	}

	// The stream latch lives for the whole request: a failure after the
	// first byte may never re-issue, even if rotation offers an attempt.
	// Sink selection: SSE goes straight to the clientStream; a client that
	// did not ask for streaming gets a completionBuffer — nothing is
	// written to the client before its single JSON flush, so the latch
	// stays false there and the FULL rotation budget applies pre-flush
	// (an exhausted budget or terminal failure then surfaces as a standard
	// error envelope instead of a partial body).
	stream := newClientStream(w)
	var (
		buf  *completionBuffer
		sink relaySink = stream
	)
	if !streamReq {
		buf = newCompletionBuffer(model.ID)
		sink = buf
	}
	ctx := r.Context()
	executed := 0
	att := h.Rot.Attempt()

	for {
		executed++
		if buf != nil {
			buf.reset() // a failed attempt's frames must not leak into a re-issue
		}

		// One attempt: fresh req_ id, per-attempt transport, watchdog-wrapped
		// body, lane relay with flush-per-frame.
		done, ue, workspace, metadata := func(att router.Attempt) (
			bool, *zen.UpstreamError, string, json.RawMessage) {
			attemptCtx, cancel := context.WithCancel(ctx)
			defer cancel() // watchdog handoff: cancel on any attempt exit

			req, err := http.NewRequestWithContext(attemptCtx, http.MethodPost,
				upURL, bytes.NewReader(payload))
			if err != nil {
				return false, &zen.UpstreamError{
					Kind:    zen.KindTransport,
					Status:  http.StatusBadGateway,
					Message: "zen: building upstream request: " + err.Error(),
				}, "", nil
			}
			req.Header.Set("Content-Type", "application/json")
			req.Header.Set("Authorization", "Bearer "+att.Key)
			for k, vs := range zen.BuildHeaders(zen.HeaderInput{
				Session:                ids.Session,
				RequestID:              zen.RandomID("req", 16), // fresh per attempt
				Project:                ids.Project,
				IncludeSessionAffinity: true,
			}) {
				for _, v := range vs {
					req.Header.Add(k, v)
				}
			}

			// First-event budget, phase 1 (connect + response headers): one
			// timer armed AROUND Do and stopped the instant Do returns — the
			// port arms FIRST_EVENT before fetch (one AbortController owns
			// connect + headers + body), so a stalled-headers upstream must
			// not pin this goroutine. Budget <= 0 disables the phase (same
			// convention as zen.NewWatchdog). Full policy: streamFailure doc.
			var headerTimer *time.Timer
			if firstEvent > 0 {
				headerTimer = time.AfterFunc(firstEvent, cancel)
			}
			started := time.Now()
			resp, err := (&http.Client{Transport: att.Transport}).Do(req)
			// Stop() reports false iff the timer already expired (or its
			// callback is running): the budget was blown. Re-issue cancel —
			// it is idempotent — to close the window where Stop lost the
			// race against the timer proc.
			headersTimedOut := headerTimer != nil && !headerTimer.Stop()
			if headersTimedOut {
				cancel()
			}
			if err != nil {
				if ctx.Err() != nil {
					return true, nil, "", nil // client went away: nothing to write
				}
				if headersTimedOut {
					return false, firstEventTimeoutError(firstEvent), "", nil
				}
				return false, &zen.UpstreamError{
					Kind:    zen.KindTransport,
					Status:  http.StatusBadGateway,
					Message: "zen: upstream transport: " + err.Error(),
				}, "", nil
			}
			if headersTimedOut {
				// Timer expired in the very instant Do returned a response:
				// the attempt context is dead, so the body could never be
				// relayed reliably. Drop the response — no byte reached the
				// client (the status line commits on the first frame), so
				// the loop can still surface an envelope under its normal
				// rules. Benign cancel of a completed attempt, by design.
				_ = resp.Body.Close()
				return false, firstEventTimeoutError(firstEvent), "", nil
			}

			if resp.StatusCode >= http.StatusBadRequest {
				failed := resp.Body
				defer failed.Close()
				fb, _ := io.ReadAll(io.LimitReader(failed, maxErrorBody))
				ws, meta := probeEnvelope(fb)
				return false,
					zen.Classify(resp.StatusCode, fb, resp.Header.Get("Retry-After")),
					ws, meta
			}

			// 2xx ONLY: record the dashboard data (plan Task 1): the
			// egress/key success counters and this attempt's response
			// latency (headers). A nil Recorder (default; existing Rotator
			// fakes) records nothing, so behavior without the seam is
			// unchanged. The explicit < 300 gate keeps a pass-through 3xx
			// (e.g. 304, which http.Client does not follow) out of the
			// counters — parity with router.OnResult (review F3).
			if h.Recorder != nil && resp.StatusCode >= http.StatusOK && resp.StatusCode < http.StatusMultipleChoices {
				h.Recorder.RecordSuccess(att.Egress, att.Key, time.Since(started).Milliseconds())
			}

			// 2xx: stream the lane. The watchdog wraps the body itself;
			// ErrWatchdogTimeout surfaces through the relay error below.
			defer resp.Body.Close()
			wrapped := zen.NewWatchdog(attemptCtx, resp.Body, firstEvent, idle)
			if useResponses {
				st := newStampWriter(sink, zen.RandomID("chatcmpl", 16),
					model.ID, time.Now().Unix())
				if err := zen.TranslateResponsesStream(wrapped, st); err != nil {
					return false, streamFailure(err), "", nil
				}
				if err := st.close(); err != nil {
					return false, streamFailure(err), "", nil
				}
			} else if err := zen.FilterChatStream(wrapped, sink); err != nil {
				return false, streamFailure(err), "", nil
			}
			if err := sink.close(); err != nil {
				return false, streamFailure(err), "", nil
			}
			return true, nil, "", nil
		}(att)

		if done {
			// Clean stream end. Buffered client: write the ONE
			// chat.completion body (application/json, no SSE markers).
			// Skipped when the client vanished (ctx.Err): nothing to write.
			if buf != nil && ctx.Err() == nil {
				buf.flush(w)
			}
			return // (or the client vanished)
		}
		if ctx.Err() != nil {
			return
		}

		// Failure: hand EVERY report to the rotator first — that call is
		// what records the per-egress and per-key counters (spec §6.1) —
		// then decide whether a re-issue is allowed at all.
		rep := router.Report{
			Kind:       ue.Kind,
			Egress:     att.Egress,
			Key:        att.Key,
			RetryAfter: ue.RetryAfter,
			Step:       att.Step, // the FAILED attempt's stage
			Type:       ue.Type,
			Workspace:  workspace,
		}
		next, nextOK := h.Rot.NextAttempt(rep)

		// Bounded re-issue: rotator agrees, the class is retryable, D1 has
		// budget left, and no byte reached the client yet.
		if !(nextOK && ue.IsRetryable() && executed < maxAttempts && !stream.started) {
			if stream.started {
				// First-byte guard: the status line is committed, so the
				// stream just ends here — without a terminator, because the
				// relay did not complete.
				return
			}
			status := ue.Status
			if status < 400 || status > 599 {
				status = http.StatusInternalServerError
			}
			if status == http.StatusTooManyRequests && ue.RetryAfter > 0 {
				w.Header().Set("Retry-After", formatRetryAfter(ue.RetryAfter))
			}
			writeError(w, status, errorClass(ue), errorMessage(ue), metadata)
			return
		}
		att = next
	}
}

// streamFailure classifies a mid-stream relay error. All of these are
// transport-class: the attempt answered 2xx but the body died (dial is not
// involved, the frame or the watchdog said stop). The first-byte guard in
// the main loop decides whether anything can still be surfaced.
//
// Watchdog handoff (zen/watchdog.go caller contract): on ErrWatchdogTimeout
// the attempt context is cancelled and the upstream body closed — both
// happen via the deferred cancel/Close in the attempt closure as soon as
// this error propagates out of the relay.
//
// Port-fidelity note: the plugin arms its first-event watchdog until the
// first PARSED SSE data line, while zen.NewWatchdog switches
// first-event→idle at the first BYTE. This layer deliberately keeps the
// byte-level semantics — the wrapper is byte-oriented, and enforcing
// line-level arming would mean parsing the stream above the wrapper for no
// practical gain (an upstream that sends bytes but no data line for
// FirstEventTimeout, 30s by default, is already pathological, and we are
// the stricter side of that race).
//
// First-event budget policy (two phases, one budget value): the plugin
// (lib/index.js:1382 armFirst() immediately before fetch; index.js:837-840
// "One AbortController owns connect + headers + body for the whole
// attempt") arms FIRST_EVENT before the attempt starts, so
// FirstEventTimeout must bound the CONNECT + RESPONSE-HEADERS phase, not
// only the body. The attempt closure enforces phase 1 with
// time.AfterFunc(firstEvent, attemptCancel) armed around http.Client.Do
// and stopped the moment Do returns (a timer that expires in the same
// instant Do succeeds is treated as a timeout: the cancelled response is
// closed and reported — no byte reached the client, so no relay state is
// corrupted, and cancel/Stop never double-fire a healthy attempt).
// Phase 2 — the body up to its first byte — re-arms the full firstEvent
// budget through zen.NewWatchdog after Do returns, then hands over to the
// idle budget. Deliberate divergence from the plugin's single shared
// deadline: each phase gets the full budget, so this layer is never LESS
// strict than the port (worst case headers + first byte = 2×
// FirstEventTimeout); sharing one deadline would need a second timer
// racing the watchdog for no gain in strictness.
func streamFailure(err error) *zen.UpstreamError {
	return &zen.UpstreamError{
		Kind:    zen.KindTransport,
		Status:  http.StatusBadGateway,
		Message: "zen: stream relay: " + err.Error(),
	}
}

// firstEventTimeoutError classifies an attempt cut off by phase 1 of the
// first-event budget (connect + response headers); see the streamFailure
// doc for the full policy. KindTransport keeps it inside the normal
// bounded re-issue rules (report to the rotator first, then decide).
func firstEventTimeoutError(firstEvent time.Duration) *zen.UpstreamError {
	return &zen.UpstreamError{
		Kind:   zen.KindTransport,
		Status: http.StatusBadGateway,
		Message: "zen: first event timeout: no response headers within " +
			firstEvent.String() + " (connect + response-headers phase)",
	}
}

// toMessages flattens a decoded JSON "messages" value into the shape
// zen.DeriveRequestIDs expects; a missing or malformed field yields an
// empty slice (ids then fall back to a random conversation seed).
func toMessages(v any) []map[string]any {
	list, _ := v.([]any)
	out := make([]map[string]any, 0, len(list))
	for _, entry := range list {
		if m, ok := entry.(map[string]any); ok {
			out = append(out, m)
		}
	}
	return out
}

// probeEnvelope extracts (workspace, metadata) from a raw upstream failure
// body. Workspace rides on the error object for account limits (spec §4) or
// inside metadata; metadata is passed through to the client envelope for
// 429s only.
func probeEnvelope(raw []byte) (string, json.RawMessage) {
	var top struct {
		Error    json.RawMessage `json:"error"`
		Metadata json.RawMessage `json:"metadata"`
	}
	if err := json.Unmarshal(raw, &top); err != nil {
		return "", nil
	}
	var inner struct {
		Workspace string `json:"workspace"`
	}
	_ = json.Unmarshal(top.Error, &inner)
	workspace := inner.Workspace
	if workspace == "" {
		var md struct {
			Workspace string `json:"workspace"`
		}
		if err := json.Unmarshal(top.Metadata, &md); err == nil {
			workspace = md.Workspace
		}
	}
	// DM-13: JSON null is absent, not a value — an upstream
	// "metadata":null must not pass json.Valid as a real payload.
	if !json.Valid(top.Metadata) || bytes.Equal(top.Metadata, []byte("null")) {
		top.Metadata = nil
	}
	return workspace, top.Metadata
}

// errorClass picks the envelope's error.type: the raw upstream class when
// classification recovered one (FreeUsageLimitError, …), otherwise the
// stable class for the Kind.
func errorClass(ue *zen.UpstreamError) string {
	if ue.Type != "" {
		return ue.Type
	}
	switch ue.Kind {
	case zen.KindTransport:
		return "TransportError"
	case zen.KindDailyLimit:
		return "FreeUsageLimitError"
	case zen.KindKeyRateLimit:
		return "RateLimitError"
	case zen.KindAuth:
		return "AuthError"
	case zen.KindModel:
		return "ModelError"
	case zen.KindRegion:
		return "RegionError"
	case zen.KindProviderRelay:
		return "ProviderRelayError"
	case zen.KindServer:
		return "ServerError"
	default:
		return "InvalidRequestError"
	}
}

func errorMessage(ue *zen.UpstreamError) string {
	if ue.Message != "" {
		return ue.Message
	}
	return ue.Error()
}

// formatRetryAfter renders the classified window as whole seconds — for a
// daily limit this is the synthesized distance to the next UTC midnight
// (spec §4).
func formatRetryAfter(d time.Duration) string {
	if d <= 0 {
		return "1"
	}
	return strconv.Itoa(int((d + 500*time.Millisecond) / time.Second))
}

// writeJSON answers with a JSON body.
func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

// writeError emits the CLIENT-facing envelope of spec §5:184-187 — the
// OpenAI error shape:
//
//	{"error":{"message":…,"type":…,"code":…}}
//
// with "metadata" as a TOP-LEVEL SIBLING of "error" and only on 429s
// (round-tripped from the upstream failure when present, {} otherwise;
// the metadata-only-on-429 rule is spec §4:140). Note this is NOT the
// upstream Zen envelope
// {"type":"error","error":{...}} the daemon parses in internal/zen — the
// wrapper "type" key never reaches a client.
//
// error.type keeps the raw classification path (errorClass), identical to
// the previous envelope, so Plan 3 mapping by error.type stays compatible.
// error.code MIRRORS type: the daemon exposes exactly one stable
// machine-readable class string per error, and duplicating it in "code"
// satisfies spec §5:192-193 (machine-readable code) without inventing a
// second code vocabulary the DSH plugin would have to keep in sync.
// Callers set Retry-After for 429s before invoking this.
func writeError(w http.ResponseWriter, status int, typ, msg string, metadata json.RawMessage) {
	env := map[string]any{
		"error": map[string]any{"message": msg, "type": typ, "code": typ},
	}
	if status == http.StatusTooManyRequests {
		// DM-13: JSON null metadata is treated as absent — it falls back to
		// the {} else-branch instead of reaching the client as null.
		if len(metadata) > 0 && json.Valid(metadata) && !bytes.Equal(metadata, []byte("null")) {
			env["metadata"] = metadata
		} else {
			env["metadata"] = map[string]any{}
		}
	}
	writeJSON(w, status, env)
}
