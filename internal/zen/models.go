// Package zen holds pure, side-effect-free protocol helpers for the Zen
// gateway shim: model resolution, effort clamping, session ids, headers,
// body shaping, error classification and stream translation.
package zen

import (
	"fmt"
	"slices"
)

// Model describes one Zen model id served by opencode.ai.
// All literal values are ported from dsh-opencode-zen lib/index.js v0.15.1
// (MODELS table) and cross-checked against spec §4 of the gateway design.
type Model struct {
	ID                string
	Name              string
	Description       string
	ContextWindow     int
	MaxOutput         int
	Vision            bool
	Responses         bool
	ReasoningRequired bool
	// Efforts is the declared reasoning ladder; nil (as in the plugin
	// MODELS rows without an efforts field) means defaultEfforts applies.
	Efforts []string
}

// defaultEfforts mirrors the plugin DEFAULT_EFFORT_IDS: the ladder used for
// models that declare no efforts field.
var defaultEfforts = []string{"off", "low", "high", "max"}

// effortOrder mirrors the plugin EFFORT_ORDER: the canonical reasoning
// vocabulary across all Zen wires, ordered. Out-of-ladder requests are
// located in this order and clamped into the model ladder's bounds.
var effortOrder = []string{"off", "minimal", "low", "medium", "high", "xhigh", "max"}

// MODELS is the static 9-model table (spec §4), verbatim from the plugin.
var MODELS = []Model{
	{ID: "big-pickle", Name: "Big Pickle (Free)", Description: "OpenCode Zen free",
		ContextWindow: 200000, MaxOutput: 32000, Vision: true},
	{ID: "jev-1.13-free", Name: "Jev 1.13 (Free)", Description: "OpenCode Zen free (limits unpublished; conservative budget)",
		ContextWindow: 200000, MaxOutput: 32000},
	{ID: "ling-3.0-flash-fin-free", Name: "Ling 3.0 Flash Fin (Free)", Description: "OpenCode Zen free: reasoning + tool calls, daily driver",
		ContextWindow: 262144, MaxOutput: 32768},
	{ID: "mimo-v2.5-free", Name: "MiMo 2.5 (Free)", Description: "OpenCode Zen free",
		ContextWindow: 200000, MaxOutput: 32000, Vision: true,
		Efforts: []string{"off", "low", "medium", "high"}},
	{ID: "mimo-v2.6-flash-free", Name: "MiMo 2.6 Flash (Free)", Description: "OpenCode Zen free",
		ContextWindow: 200000, MaxOutput: 32000, Vision: true,
		Efforts: []string{"off", "low", "medium", "high"}},
	{ID: "muse-spark-1.3-contributor-free", Name: "Muse Spark 1.3 Contributor (Free)", Description: "OpenCode Zen free · Responses wire (auto-routed via /responses)",
		ContextWindow: 1048576, MaxOutput: 131072, Responses: true,
		Efforts: []string{"minimal", "low", "medium", "high", "xhigh"}},
	{ID: "nemotron-3.5-lightning-free", Name: "Nemotron 3.5 Lightning (Free)", Description: "OpenCode Zen free (NVIDIA)",
		ContextWindow: 262144, MaxOutput: 262144},
	{ID: "nemotron-3-ultra-free", Name: "Nemotron 3 Ultra (Free)", Description: "OpenCode Zen free (NVIDIA)",
		ContextWindow: 1000000, MaxOutput: 128000},
	{ID: "space-bunny-free", Name: "Space Bunny (Free)", Description: "OpenCode Zen free · OpenRouter-backed, reasoning always on",
		ContextWindow: 1048576, MaxOutput: 524288, Vision: true, ReasoningRequired: true,
		Efforts: []string{"low", "medium", "high", "xhigh", "max"}},
}

// ResolveModel looks up a model by id; an unknown id returns an error.
func ResolveModel(id string) (Model, error) {
	if i := slices.IndexFunc(MODELS, func(m Model) bool { return m.ID == id }); i >= 0 {
		return MODELS[i], nil
	}
	return Model{}, fmt.Errorf("zen: unknown model %q", id)
}

// Ladder returns the effort ladder the model accepts, falling back to the
// default ladder for models that declare none. The returned slice is a copy
// and safe for the caller to mutate.
func (m Model) Ladder() []string {
	src := m.Efforts
	if len(src) == 0 {
		src = defaultEfforts
	}
	return slices.Clone(src)
}

// ClampEffort maps a client-requested reasoning effort onto the model's
// declared ladder.
//
// Algorithm:
//  1. Absent effort: reasoningRequired models must always receive an effort
//     (Zen 400s otherwise) and default to "high"; everything else returns
//     "" so the caller decides whether to send the field at all.
//  2. The exact request "off" short-circuits to "off" unconditionally —
//     never clamped into the ladder (mirrors plugin resolveReasoningEffort:
//     disabling reasoning must survive even on ladders that start higher).
//  3. An effort already on the ladder passes through unchanged.
//  4. Otherwise both the request and every ladder entry are located in the
//     canonical global order (effortOrder: off minimal low medium high xhigh
//     max); the request's global index is clamped into the ladder's
//     [first, last] global-index bounds, and the ladder entry nearest to
//     the clamped index wins — ties go to the earlier ladder entry (e.g.
//     mimo "minimal" → "off", mimo "xhigh" → "high", default-ladder
//     "medium" → "low").
//  5. A word outside the global vocabulary falls back to "high" when the
//     ladder declares it, else the ladder's first entry — mirroring the
//     plugin clampEffort fallback.
//
// Composition contract (spec §4): ClampEffort("off") → "off" on every
// model; the chat lane sends ToChatWire(result) ("off" → "none") at call
// time; the Responses lane omits the reasoning_effort key entirely when the
// result is "off". Absent-effort handling above is unchanged by this rule.
func ClampEffort(m Model, effort string) string {
	if effort == "" {
		if m.ReasoningRequired {
			return "high"
		}
		return ""
	}
	if effort == "off" {
		return "off"
	}
	ladder := m.Ladder()
	if slices.Contains(ladder, effort) {
		return effort
	}
	g := slices.Index(effortOrder, effort)
	if g < 0 {
		if slices.Contains(ladder, "high") {
			return "high"
		}
		return ladder[0]
	}
	// Ladder entries are all drawn from effortOrder, so both bounds are
	// valid global indices.
	lo := slices.Index(effortOrder, ladder[0])
	hi := slices.Index(effortOrder, ladder[len(ladder)-1])
	if g < lo {
		return ladder[0]
	}
	if g > hi {
		return ladder[len(ladder)-1]
	}
	best := ladder[0]
	bestDist := abs(slices.Index(effortOrder, best) - g)
	for _, id := range ladder[1:] {
		if d := abs(slices.Index(effortOrder, id) - g); d < bestDist {
			best, bestDist = id, d
		}
	}
	return best
}

// ToChatWire converts a clamped effort for the chat wire: "off" becomes
// "none" (the chat endpoint's disable value); everything else is unchanged.
func ToChatWire(effort string) string {
	if effort == "off" {
		return "none"
	}
	return effort
}

func abs(x int) int {
	if x < 0 {
		return -x
	}
	return x
}
