package zen

import (
	"bytes"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"math/big"
	"regexp"
	"strings"
)

// canonicalSessionPattern mirrors the plugin CANONICAL_SESSION_PATTERN
// (lib/index.js:157): 12 lowercase hex chars + 14 base62 chars after "ses_".
var canonicalSessionPattern = regexp.MustCompile(`^ses_[0-9a-f]{12}[0-9A-Za-z]{14}$`)

// base62Alphabet mirrors the plugin BASE62_ALPHABET (lib/index.js:158),
// copied literally: 0-9, then A-Z, then a-z.
const base62Alphabet = "0123456789ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmnopqrstuvwxyz"

// base62Fixed mirrors the plugin base62Fixed (lib/index.js:160-169): renders
// value as exactly width base62 digits, least-significant digit last, by
// repeated `n % 62` indexing into the alphabet with truncating division.
func base62Fixed(value *big.Int, width int) string {
	base := big.NewInt(62)
	n := new(big.Int).Set(value)
	out := make([]byte, width)
	for i := width - 1; i >= 0; i-- {
		rem := new(big.Int).Mod(n, base)
		out[i] = base62Alphabet[rem.Int64()]
		n.Div(n, base)
	}
	return string(out)
}

// CanonicalSessionID mirrors the plugin canonicalSessionID (lib/index.js:
// 171-177). An already-canonical id passes through unchanged; otherwise the
// seed is hashed with sha256("ses\x00"+seed): the first 6 bytes form the
// 12-hex time part and the next 10 bytes form the 14-char base62 random
// part, giving "ses_" + 26 chars = 30 chars total.
func CanonicalSessionID(seed string) string {
	if canonicalSessionPattern.MatchString(seed) {
		return seed
	}
	sum := sha256.Sum256([]byte("ses\x00" + seed))
	timePart := hex.EncodeToString(sum[:6])
	randomPart := base62Fixed(new(big.Int).SetBytes(sum[6:16]), 14)
	return "ses_" + timePart + randomPart
}

// StableID mirrors the plugin stableID (lib/index.js:179-182): sha256 of
// prefix+NUL+value, first 12 bytes rendered as 24 lowercase hex chars, as
// prefix_hex.
func StableID(prefix, value string) string {
	sum := sha256.Sum256([]byte(prefix + "\x00" + value))
	return prefix + "_" + hex.EncodeToString(sum[:12])
}

// RandomID mirrors the plugin randomID (lib/index.js:184-186): nbytes of
// crypto/rand entropy rendered as hex, as prefix_hex. crypto/rand.Read never
// returns an error on Go >= 1.24.
func RandomID(prefix string, nbytes int) string {
	buf := make([]byte, nbytes)
	_, _ = rand.Read(buf)
	return prefix + "_" + hex.EncodeToString(buf)
}

// encodeJSON renders v the way plugin JSON.stringify does for conversation
// content: compact, without HTML escaping. Values that cannot be encoded
// yield "" (the plugin's non-string stringify result), which the caller
// treats like an unusable encoding and skips.
func encodeJSON(v any) string {
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	if err := enc.Encode(v); err != nil {
		return ""
	}
	return strings.TrimSuffix(buf.String(), "\n")
}

// ConversationSeed mirrors the plugin conversationSeed (lib/index.js:
// 188-195): the JSON encoding of the first user message whose encoding is
// neither "null" nor empty; "" when no such message exists. Messages with a
// missing (nil) content encode to "null" and are skipped.
func ConversationSeed(messages []map[string]any) string {
	for _, message := range messages {
		if role, _ := message["role"].(string); role != "user" {
			continue
		}
		encoded := encodeJSON(message["content"])
		if encoded != "null" && len(encoded) > 0 {
			return encoded
		}
	}
	return ""
}

// RequestIDs mirrors the object returned by the plugin deriveRequestIDs
// (lib/index.js:197-206).
type RequestIDs struct {
	Session       string
	Request       string
	Project       string
	ParentSession string
}

// defaultProject mirrors the plugin's fixed project value passed to
// stableID in deriveRequestIDs.
const defaultProject = "dsh-opencode-zen:default-project"

// DeriveRequestIDs mirrors the plugin deriveRequestIDs (lib/index.js:
// 197-206): the conversation seed (or a per-call random "fallback" seed when
// it is "" or "{}") becomes the canonical session id; the request id is
// fresh random entropy per call; the project id is stable; the parent
// session is always empty.
func DeriveRequestIDs(messages []map[string]any) RequestIDs {
	signal := ConversationSeed(messages)
	if signal == "" || signal == "{}" {
		signal = RandomID("fallback", 16)
	}
	return RequestIDs{
		Session:       CanonicalSessionID(signal),
		Request:       RandomID("req", 16),
		Project:       StableID("prj", defaultProject),
		ParentSession: "",
	}
}
