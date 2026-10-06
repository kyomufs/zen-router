package zen

import (
	"reflect"
	"testing"
)

// TestResolveModel asserts the literal model facts ported from the plugin
// MODELS table (dsh-opencode-zen lib/index.js v0.15.1) and spec §4.
func TestResolveModel(t *testing.T) {
	defaultLadder := []string{"off", "low", "high", "max"}

	tests := []struct {
		id                string
		name              string
		description       string
		contextWindow     int
		maxOutput         int
		vision            bool
		responses         bool
		reasoningRequired bool
		ladder            []string
	}{
		{
			id:            "big-pickle",
			name:          "Big Pickle (Free)",
			description:   "OpenCode Zen free",
			contextWindow: 200000,
			maxOutput:     32000,
			vision:        true,
			ladder:        defaultLadder,
		},
		{
			id:            "jev-1.13-free",
			name:          "Jev 1.13 (Free)",
			description:   "OpenCode Zen free (limits unpublished; conservative budget)",
			contextWindow: 200000,
			maxOutput:     32000,
			ladder:        defaultLadder,
		},
		{
			id:            "ling-3.0-flash-fin-free",
			name:          "Ling 3.0 Flash Fin (Free)",
			description:   "OpenCode Zen free: reasoning + tool calls, daily driver",
			contextWindow: 262144,
			maxOutput:     32768,
			ladder:        defaultLadder,
		},
		{
			id:            "mimo-v2.5-free",
			name:          "MiMo 2.5 (Free)",
			description:   "OpenCode Zen free",
			contextWindow: 200000,
			maxOutput:     32000,
			vision:        true,
			ladder:        []string{"off", "low", "medium", "high"},
		},
		{
			id:            "mimo-v2.6-flash-free",
			name:          "MiMo 2.6 Flash (Free)",
			description:   "OpenCode Zen free",
			contextWindow: 200000,
			maxOutput:     32000,
			vision:        true,
			ladder:        []string{"off", "low", "medium", "high"},
		},
		{
			id:            "muse-spark-1.3-contributor-free",
			name:          "Muse Spark 1.3 Contributor (Free)",
			description:   "OpenCode Zen free · Responses wire (auto-routed via /responses)",
			contextWindow: 1048576,
			maxOutput:     131072,
			responses:     true,
			ladder:        []string{"minimal", "low", "medium", "high", "xhigh"},
		},
		{
			id:            "nemotron-3.5-lightning-free",
			name:          "Nemotron 3.5 Lightning (Free)",
			description:   "OpenCode Zen free (NVIDIA)",
			contextWindow: 262144,
			maxOutput:     262144,
			ladder:        defaultLadder,
		},
		{
			id:            "nemotron-3-ultra-free",
			name:          "Nemotron 3 Ultra (Free)",
			description:   "OpenCode Zen free (NVIDIA)",
			contextWindow: 1000000,
			maxOutput:     128000,
			ladder:        defaultLadder,
		},
		{
			id:                "space-bunny-free",
			name:              "Space Bunny (Free)",
			description:       "OpenCode Zen free · OpenRouter-backed, reasoning always on",
			contextWindow:     1048576,
			maxOutput:         524288,
			vision:            true,
			reasoningRequired: true,
			ladder:            []string{"low", "medium", "high", "xhigh", "max"},
		},
	}

	for _, tt := range tests {
		t.Run(tt.id, func(t *testing.T) {
			m, err := ResolveModel(tt.id)
			if err != nil {
				t.Fatalf("ResolveModel(%q) returned error: %v", tt.id, err)
			}
			if m.ID != tt.id {
				t.Errorf("ID = %q, want %q", m.ID, tt.id)
			}
			if m.Name != tt.name {
				t.Errorf("Name = %q, want %q", m.Name, tt.name)
			}
			if m.Description != tt.description {
				t.Errorf("Description = %q, want %q", m.Description, tt.description)
			}
			if m.ContextWindow != tt.contextWindow {
				t.Errorf("ContextWindow = %d, want %d", m.ContextWindow, tt.contextWindow)
			}
			if m.MaxOutput != tt.maxOutput {
				t.Errorf("MaxOutput = %d, want %d", m.MaxOutput, tt.maxOutput)
			}
			if m.Vision != tt.vision {
				t.Errorf("Vision = %v, want %v", m.Vision, tt.vision)
			}
			if m.Responses != tt.responses {
				t.Errorf("Responses = %v, want %v", m.Responses, tt.responses)
			}
			if m.ReasoningRequired != tt.reasoningRequired {
				t.Errorf("ReasoningRequired = %v, want %v", m.ReasoningRequired, tt.reasoningRequired)
			}
			if got := m.Ladder(); !reflect.DeepEqual(got, tt.ladder) {
				t.Errorf("Ladder() = %v, want %v", got, tt.ladder)
			}
		})
	}

	t.Run("unknown id errors", func(t *testing.T) {
		if _, err := ResolveModel("no-such-model"); err == nil {
			t.Fatal("ResolveModel(unknown id) = nil error, want error")
		}
	})
}

// TestClampEffort verifies clamping against every declared ladder: the exact
// request "off" short-circuits to "off" on every model (spec §4 — the chat
// lane maps it to "none" via ToChatWire, the Responses lane omits the key),
// other exact ladder members pass through, and out-of-ladder requests clamp
// to the nearest declared level (plugin resolveReasoningEffort semantics).
func TestClampEffort(t *testing.T) {
	tests := []struct {
		model string
		input string
		want  string
	}{
		// mimo ladder: [off low medium high].
		{model: "mimo-v2.6-flash-free", input: "off", want: "off"},
		{model: "mimo-v2.6-flash-free", input: "low", want: "low"},
		{model: "mimo-v2.6-flash-free", input: "medium", want: "medium"},
		{model: "mimo-v2.6-flash-free", input: "high", want: "high"},
		{model: "mimo-v2.6-flash-free", input: "minimal", want: "off"},
		{model: "mimo-v2.6-flash-free", input: "xhigh", want: "high"},
		{model: "mimo-v2.6-flash-free", input: "max", want: "high"},
		{model: "mimo-v2.5-free", input: "minimal", want: "off"},
		{model: "mimo-v2.5-free", input: "max", want: "high"},

		// muse ladder: [minimal low medium high xhigh].
		{model: "muse-spark-1.3-contributor-free", input: "off", want: "off"},
		{model: "muse-spark-1.3-contributor-free", input: "minimal", want: "minimal"},
		{model: "muse-spark-1.3-contributor-free", input: "low", want: "low"},
		{model: "muse-spark-1.3-contributor-free", input: "medium", want: "medium"},
		{model: "muse-spark-1.3-contributor-free", input: "high", want: "high"},
		{model: "muse-spark-1.3-contributor-free", input: "xhigh", want: "xhigh"},
		{model: "muse-spark-1.3-contributor-free", input: "max", want: "xhigh"},

		// space-bunny ladder: [low medium high xhigh max].
		{model: "space-bunny-free", input: "off", want: "off"},
		{model: "space-bunny-free", input: "minimal", want: "low"},
		{model: "space-bunny-free", input: "low", want: "low"},
		{model: "space-bunny-free", input: "medium", want: "medium"},
		{model: "space-bunny-free", input: "high", want: "high"},
		{model: "space-bunny-free", input: "xhigh", want: "xhigh"},
		{model: "space-bunny-free", input: "max", want: "max"},

		// Default ladder [off low high max] (models without an efforts field).
		{model: "big-pickle", input: "off", want: "off"},
		{model: "big-pickle", input: "low", want: "low"},
		{model: "big-pickle", input: "high", want: "high"},
		{model: "big-pickle", input: "max", want: "max"},
		{model: "big-pickle", input: "minimal", want: "off"},
		{model: "big-pickle", input: "medium", want: "low"},
		{model: "big-pickle", input: "xhigh", want: "high"},
		{model: "jev-1.13-free", input: "minimal", want: "off"},
		{model: "ling-3.0-flash-fin-free", input: "xhigh", want: "high"},
		{model: "nemotron-3.5-lightning-free", input: "medium", want: "low"},
		{model: "nemotron-3-ultra-free", input: "max", want: "max"},

		// Vocabulary the model never declares: plugin fallback = "high".
		{model: "mimo-v2.6-flash-free", input: "ultra", want: "high"},
	}

	for _, tt := range tests {
		t.Run(tt.model+"_"+tt.input, func(t *testing.T) {
			m, err := ResolveModel(tt.model)
			if err != nil {
				t.Fatalf("ResolveModel(%q): %v", tt.model, err)
			}
			if got := ClampEffort(m, tt.input); got != tt.want {
				t.Errorf("ClampEffort(%q, %q) = %q, want %q", tt.model, tt.input, got, tt.want)
			}
		})
	}
}

// TestClampEffortReasoningRequired covers absent-effort defaults,
// reasoningRequired fallback to "high", and the chat-wire "off" → "none"
// mapping (the Responses wire omits the field entirely — caller side).
func TestClampEffortReasoningRequired(t *testing.T) {
	absent := []struct {
		model string
		want  string
	}{
		{model: "space-bunny-free", want: "high"},
		{model: "big-pickle", want: ""},
		{model: "muse-spark-1.3-contributor-free", want: ""},
		{model: "mimo-v2.6-flash-free", want: ""},
	}
	for _, tt := range absent {
		t.Run(tt.model+"_absent", func(t *testing.T) {
			m, err := ResolveModel(tt.model)
			if err != nil {
				t.Fatalf("ResolveModel(%q): %v", tt.model, err)
			}
			if got := ClampEffort(m, ""); got != tt.want {
				t.Errorf("ClampEffort(%q, \"\") = %q, want %q", tt.model, got, tt.want)
			}
		})
	}

	chatWire := []struct {
		input string
		want  string
	}{
		{input: "off", want: "none"},
		{input: "high", want: "high"},
		{input: "minimal", want: "minimal"},
		{input: "", want: ""},
	}
	for _, tt := range chatWire {
		t.Run("ToChatWire_"+tt.input, func(t *testing.T) {
			if got := ToChatWire(tt.input); got != tt.want {
				t.Errorf("ToChatWire(%q) = %q, want %q", tt.input, got, tt.want)
			}
		})
	}
}

// TestClampEffortTieBreak pins the documented tie rule (models.go:97-99):
// when the clamped request sits equidistant from two ladder entries, the
// EARLIER (lower) ladder entry wins. Ruled over the plugin, which breaks
// ties UPWARD — ledger-plan1.md:13 ("minimal→off over plugin upward").
// The tie is unreachable in production traffic today (it needs an
// off-ladder word exactly halfway between two entries); this test exists
// so the ruling cannot regress silently. The clamp VALUES also appear as
// rows in TestClampEffort — this test names the rule itself.
func TestClampEffortTieBreak(t *testing.T) {
	cases := []struct {
		name  string
		model string
		input string
		want  string
	}{
		// mimo ladder [off low medium high]: "minimal" (global index 1)
		// is 1 step from both "off" and "low" → tie → earlier entry
		// "off" (the plugin would answer "low").
		{"mimo minimal ties off vs low", "mimo-v2.6-flash-free", "minimal", "off"},
		// Default ladder [off low high max]: "medium" (global index 3)
		// is 1 step from both "low" and "high" → tie → earlier entry
		// "low" (the plugin would answer "high").
		{"default ladder medium ties low vs high", "big-pickle", "medium", "low"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			m, err := ResolveModel(tc.model)
			if err != nil {
				t.Fatalf("ResolveModel(%q): %v", tc.model, err)
			}
			if got := ClampEffort(m, tc.input); got != tc.want {
				t.Errorf("ClampEffort(%q, %q) = %q, want %q (tie → earlier ladder entry)",
					tc.model, tc.input, got, tc.want)
			}
		})
	}
}
