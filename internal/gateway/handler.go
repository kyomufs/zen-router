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
// or the failure path surfaces an envelope (plan Task 12 line 185).
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
		respBody["stream"] = true // force SSE (carry-over bullet: pinned)
		if payload, err = json.Marshal(respBody); err != nil {
			writeError(w, http.StatusInternalServerError, "InternalError",
				"zen: encoding responses body: "+err.Error(), nil)
			return
		}
		upPath = "/zen/v1/responses"
	} else {
		body["stream"] = true // force SSE so the relay always sees frames
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
	stream := newClientStream(w)
	ctx := r.Context()
	executed := 0
	att := h.Rot.Attempt()

	for {
		executed++

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

			resp, err := (&http.Client{Transport: att.Transport}).Do(req)
			if err != nil {
				if ctx.Err() != nil {
					return true, nil, "", nil // client went away: nothing to write
				}
				return false, &zen.UpstreamError{
					Kind:    zen.KindTransport,
					Status:  http.StatusBadGateway,
					Message: "zen: upstream transport: " + err.Error(),
				}, "", nil
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

			// 2xx: stream the lane. The watchdog wraps the body itself;
			// ErrWatchdogTimeout surfaces through the relay error below.
			defer resp.Body.Close()
			wrapped := zen.NewWatchdog(attemptCtx, resp.Body, firstEvent, idle)
			if useResponses {
				st := newStampWriter(stream, zen.RandomID("chatcmpl", 16),
					model.ID, time.Now().Unix())
				if err := zen.TranslateResponsesStream(wrapped, st); err != nil {
					return false, streamFailure(err), "", nil
				}
				if err := st.close(); err != nil {
					return false, streamFailure(err), "", nil
				}
			} else if err := zen.FilterChatStream(wrapped, stream); err != nil {
				return false, streamFailure(err), "", nil
			}
			if err := stream.close(); err != nil {
				return false, streamFailure(err), "", nil
			}
			return true, nil, "", nil
		}(att)

		if done {
			return // clean stream end (or the client vanished)
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
func streamFailure(err error) *zen.UpstreamError {
	return &zen.UpstreamError{
		Kind:    zen.KindTransport,
		Status:  http.StatusBadGateway,
		Message: "zen: stream relay: " + err.Error(),
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
	if !json.Valid(top.Metadata) {
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

// writeError emits the spec §4 gateway envelope:
//
//	{"type":"error","error":{"type":…,"message":…},"metadata":…}
//
// metadata appears ONLY on 429s (round-tripped from the upstream failure
// when present, {} otherwise). Callers set Retry-After for 429s before
// invoking this.
func writeError(w http.ResponseWriter, status int, typ, msg string, metadata json.RawMessage) {
	env := map[string]any{
		"type":  "error",
		"error": map[string]any{"type": typ, "message": msg},
	}
	if status == http.StatusTooManyRequests {
		if len(metadata) > 0 && json.Valid(metadata) {
			env["metadata"] = metadata
		} else {
			env["metadata"] = map[string]any{}
		}
	}
	writeJSON(w, status, env)
}
