package zen

import (
	"regexp"
	"testing"
)

// Golden values below are produced by the plugin's own algorithm
// (dsh-opencode-zen lib/index.js:157-205), executed in node during
// implementation, and pin the Go port against regressions.

func TestCanonicalSessionID(t *testing.T) {
	canonical := regexp.MustCompile(`^ses_[0-9a-f]{12}[0-9A-Za-z]{14}$`)

	id := CanonicalSessionID("hello")
	if !canonical.MatchString(id) {
		t.Errorf("CanonicalSessionID(\"hello\") = %q, does not match %s", id, canonical)
	}
	if len(id) != 30 {
		t.Errorf("CanonicalSessionID(\"hello\") length = %d, want 30", len(id))
	}
	// Pinned against the plugin algorithm's node output.
	if want := "ses_3ae2ec8551c52ss37XmkOuk1FJ"; id != want {
		t.Errorf("CanonicalSessionID(\"hello\") = %q, want %q", id, want)
	}

	// Same seed yields the identical id.
	if again := CanonicalSessionID("hello"); again != id {
		t.Errorf("CanonicalSessionID not deterministic: %q then %q", id, again)
	}

	// Different seed yields a different id.
	other := CanonicalSessionID("world")
	if other == id {
		t.Errorf("CanonicalSessionID(\"world\") = %q, same as \"hello\" id", other)
	}
	if !canonical.MatchString(other) {
		t.Errorf("CanonicalSessionID(\"world\") = %q, does not match %s", other, canonical)
	}

	// An already-canonical id passes through unchanged.
	if got := CanonicalSessionID(id); got != id {
		t.Errorf("CanonicalSessionID passthrough = %q, want %q", got, id)
	}
}

func TestProjectIDStable(t *testing.T) {
	// Pinned against stableID('prj', 'dsh-opencode-zen:default-project')
	// from the plugin (sha256 of prefix+NUL+value, first 12 bytes hex).
	const want = "prj_f698f87ea99d390458196363"

	got := StableID("prj", "dsh-opencode-zen:default-project")
	if got != want {
		t.Errorf("StableID(prj, default project) = %q, want %q", got, want)
	}
	// Fixed input yields a fixed output.
	if again := StableID("prj", "dsh-opencode-zen:default-project"); again != got {
		t.Errorf("StableID not stable: %q then %q", got, again)
	}
	// 12 hash bytes rendered as 24 lowercase hex chars.
	pattern := regexp.MustCompile(`^prj_[0-9a-f]{24}$`)
	if !pattern.MatchString(got) {
		t.Errorf("StableID = %q, does not match %s", got, pattern)
	}
	// A different value under the same prefix yields a different id.
	if other := StableID("prj", "other"); other == got {
		t.Errorf("StableID collapsed distinct values to %q", got)
	}
}

func TestRequestIDFresh(t *testing.T) {
	pattern := regexp.MustCompile(`^req_[0-9a-f]{32}$`)

	a := RandomID("req", 16)
	b := RandomID("req", 16)
	if !pattern.MatchString(a) {
		t.Errorf("RandomID(\"req\", 16) = %q, does not match %s", a, pattern)
	}
	if !pattern.MatchString(b) {
		t.Errorf("RandomID(\"req\", 16) = %q, does not match %s", b, pattern)
	}
	if a == b {
		t.Errorf("RandomID(\"req\", 16) not fresh: both calls returned %q", a)
	}
}

func TestConversationSeed(t *testing.T) {
	// Normal user message: content JSON-encoded (JSON.stringify equivalent).
	msgs := []map[string]any{{"role": "user", "content": "hello"}}
	if got, want := ConversationSeed(msgs), `"hello"`; got != want {
		t.Errorf("ConversationSeed(normal) = %q, want %q", got, want)
	}

	// First user message wins; assistant messages are ignored.
	msgs = []map[string]any{
		{"role": "user", "content": "first"},
		{"role": "assistant", "content": "second"},
		{"role": "user", "content": "third"},
	}
	if got, want := ConversationSeed(msgs), `"first"`; got != want {
		t.Errorf("ConversationSeed(first user wins) = %q, want %q", got, want)
	}

	// A user message whose content encodes to "null" is skipped; the loop
	// continues to the next user message.
	msgs = []map[string]any{
		{"role": "user", "content": nil},
		{"role": "user", "content": "next"},
	}
	if got, want := ConversationSeed(msgs), `"next"`; got != want {
		t.Errorf("ConversationSeed(null content skipped) = %q, want %q", got, want)
	}

	// Empty conversation yields an empty signal.
	if got := ConversationSeed(nil); got != "" {
		t.Errorf("ConversationSeed(nil) = %q, want %q", got, "")
	}
	if got := ConversationSeed([]map[string]any{}); got != "" {
		t.Errorf("ConversationSeed(empty) = %q, want %q", got, "")
	}

	// Empty-object content encodes to "{}" and is kept as the signal;
	// DeriveRequestIDs must then replace it with a random fallback seed.
	msgs = []map[string]any{{"role": "user", "content": map[string]any{}}}
	if got, want := ConversationSeed(msgs), "{}"; got != want {
		t.Errorf("ConversationSeed(object content) = %q, want %q", got, want)
	}

	canonical := regexp.MustCompile(`^ses_[0-9a-f]{12}[0-9A-Za-z]{14}$`)
	request := regexp.MustCompile(`^req_[0-9a-f]{32}$`)
	const wantProject = "prj_f698f87ea99d390458196363"

	// Exported fallback path: "{}" signal -> session id derived from a
	// per-call random fallback seed, so two calls must differ.
	idsA := DeriveRequestIDs(msgs)
	idsB := DeriveRequestIDs(msgs)
	for name, ids := range map[string]RequestIDs{"A": idsA, "B": idsB} {
		if !canonical.MatchString(ids.Session) {
			t.Errorf("DeriveRequestIDs({}).%s.Session = %q, does not match %s", name, ids.Session, canonical)
		}
		if !request.MatchString(ids.Request) {
			t.Errorf("DeriveRequestIDs({}).%s.Request = %q, does not match %s", name, ids.Request, request)
		}
		if ids.Project != wantProject {
			t.Errorf("DeriveRequestIDs({}).%s.Project = %q, want %q", name, ids.Project, wantProject)
		}
		if ids.ParentSession != "" {
			t.Errorf("DeriveRequestIDs({}).%s.ParentSession = %q, want empty", name, ids.ParentSession)
		}
	}
	if idsA.Session == idsB.Session {
		t.Errorf("DeriveRequestIDs({}) did not fall back to randomness: both sessions %q", idsA.Session)
	}

	// Exported fallback path: empty conversation -> well-formed session,
	// stable project id, empty parent, fresh random request id.
	emptyA := DeriveRequestIDs(nil)
	emptyB := DeriveRequestIDs(nil)
	if !canonical.MatchString(emptyA.Session) {
		t.Errorf("DeriveRequestIDs(nil).Session = %q, does not match %s", emptyA.Session, canonical)
	}
	if emptyA.Session == emptyB.Session {
		t.Errorf("DeriveRequestIDs(nil) did not fall back to randomness: both sessions %q", emptyA.Session)
	}
	if !request.MatchString(emptyA.Request) {
		t.Errorf("DeriveRequestIDs(nil).Request = %q, does not match %s", emptyA.Request, request)
	}
	if emptyA.Project != wantProject {
		t.Errorf("DeriveRequestIDs(nil).Project = %q, want %q", emptyA.Project, wantProject)
	}
	if emptyA.ParentSession != "" {
		t.Errorf("DeriveRequestIDs(nil).ParentSession = %q, want empty", emptyA.ParentSession)
	}
}
