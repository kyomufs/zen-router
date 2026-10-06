package zen

import (
	"reflect"
	"strings"
	"testing"
)

// responsesFixture builds a valid minimal chat body for the Responses-only
// model; tests override fields as needed.
func responsesFixture() map[string]any {
	return map[string]any{
		"model": "muse-spark-1.3-contributor-free",
		"messages": []any{
			map[string]any{"role": "user", "content": "Hello"},
		},
	}
}

func requireResponsesInput(t *testing.T, out map[string]any) []any {
	t.Helper()
	input, ok := out["input"].([]any)
	if !ok {
		t.Fatalf("input = %T (%v), want array", out["input"], out["input"])
	}
	return input
}

func requireNoKey(t *testing.T, out map[string]any, key string) {
	t.Helper()
	if v, ok := out[key]; ok {
		t.Errorf("unexpected key %q = %v", key, v)
	}
}

// requireMessageItem asserts one Responses input message item with content parts.
func requireMessageItem(t *testing.T, v any, role string, wantParts []any) {
	t.Helper()
	item, ok := v.(map[string]any)
	if !ok {
		t.Fatalf("input item = %T (%v), want object", v, v)
	}
	if item["role"] != role {
		t.Errorf("item role = %v, want %q", item["role"], role)
	}
	parts, ok := item["content"].([]any)
	if !ok {
		t.Fatalf("item content = %T (%v), want array", item["content"], item["content"])
	}
	if !reflect.DeepEqual(parts, wantParts) {
		t.Errorf("item content = %#v, want %#v", parts, wantParts)
	}
}

func textPart(text string) map[string]any {
	return map[string]any{"type": "input_text", "text": text}
}

// requireResponsesTool asserts one flattened Responses function tool
// (plugin serializeResponsesTools, index.js:1010-1018). A nil
// wantDescription asserts the key is absent — the plugin forwards
// `description: t.description`, which JSON.stringify drops when undefined.
func requireResponsesTool(t *testing.T, v any, name string, wantDescription any, wantParams map[string]any) {
	t.Helper()
	tool, ok := v.(map[string]any)
	if !ok {
		t.Fatalf("tool = %T (%v), want object", v, v)
	}
	if tool["type"] != "function" {
		t.Errorf("tool type = %v, want \"function\"", tool["type"])
	}
	if tool["name"] != name {
		t.Errorf("tool name = %v, want %q", tool["name"], name)
	}
	desc, hasDesc := tool["description"]
	if wantDescription == nil {
		if hasDesc {
			t.Errorf("tool description = %v, want key absent", desc)
		}
	} else if !hasDesc || desc != wantDescription {
		t.Errorf("tool description = %v (present=%t), want %q", desc, hasDesc, wantDescription)
	}
	if !reflect.DeepEqual(tool["parameters"], wantParams) {
		t.Errorf("tool parameters = %#v, want %#v", tool["parameters"], wantParams)
	}
	if _, nested := tool["function"]; nested {
		t.Errorf("tool carries nested function object; Responses tools are flattened")
	}
}

func emptyParams() map[string]any {
	return map[string]any{"type": "object", "properties": map[string]any{}}
}

// --- plan Task 7 tests ------------------------------------------------------

// TestChatToResponsesBody covers the plan's mandated assertions: system →
// instructions, user text → input_text item, image attachment part →
// input_image, max_tokens → max_output_tokens, reasoning_effort "off" →
// field absent, tool messages converted per plugin shape, tool_choice
// "auto" (the Responses gateway accepts auto only).
func TestChatToResponsesBody(t *testing.T) {
	chat := map[string]any{
		"model": "muse-spark-1.3-contributor-free",
		"messages": []any{
			map[string]any{"role": "system", "content": "You are terse."},
			map[string]any{"role": "user", "content": "Hello"},
			map[string]any{
				"role":    "assistant",
				"content": "Hi!",
				"tool_calls": []any{
					map[string]any{
						"id":   "call_1",
						"type": "function",
						"function": map[string]any{
							"name":      "get_weather",
							"arguments": "{\"city\":\"Oslo\"}",
						},
					},
				},
			},
			map[string]any{"role": "tool", "tool_call_id": "call_1", "content": "sunny"},
			map[string]any{
				"role": "user",
				"content": []any{
					map[string]any{"type": "text", "text": "what color?"},
					map[string]any{
						"type":      "image_url",
						"image_url": map[string]any{"url": "data:image/png;base64,AAAA", "detail": "high"},
					},
				},
			},
		},
		"max_tokens":       float64(500),
		"reasoning_effort": "off",
		"stream":           true,
		"stream_options":   map[string]any{"include_usage": true},
		"top_p":            float64(0.95),
		"temperature":      float64(0.4),
		"n":                float64(1),
		"response_format":  map[string]any{"type": "json_object"},
		"user":             "tester",
		"tool_choice":      "auto",
	}

	out, err := ChatToResponses(chat)
	if err != nil {
		t.Fatalf("ChatToResponses: %v", err)
	}

	if got := out["instructions"]; got != "You are terse." {
		t.Errorf("instructions = %v, want %q", got, "You are terse.")
	}
	if got := out["max_output_tokens"]; got != 500 {
		t.Errorf("max_output_tokens = %v (%T), want 500", got, got)
	}
	if got := out["stream"]; got != true {
		t.Errorf("stream = %v, want true", got)
	}
	if got := out["temperature"]; got != 0.4 {
		t.Errorf("temperature = %v, want 0.4", got)
	}
	requireNoKey(t, out, "stream_options")
	requireNoKey(t, out, "reasoning") // reasoning_effort "off" → omitted
	requireNoKey(t, out, "top_p")     // responses body carries no top_p
	requireNoKey(t, out, "n")
	requireNoKey(t, out, "response_format")
	requireNoKey(t, out, "user")
	requireNoKey(t, out, "messages")

	input := requireResponsesInput(t, out)
	want := []any{
		map[string]any{"role": "user", "content": []any{textPart("Hello")}},
		map[string]any{
			"type": "message", "role": "assistant",
			"content": []any{map[string]any{"type": "output_text", "text": "Hi!"}},
		},
		map[string]any{
			"type": "function_call", "call_id": "call_1",
			"name": "get_weather", "arguments": "{\"city\":\"Oslo\"}",
		},
		map[string]any{"type": "function_call_output", "call_id": "call_1", "output": "sunny"},
		map[string]any{
			"role": "user",
			"content": []any{
				textPart("what color?"),
				map[string]any{"type": "input_image", "image_url": "data:image/png;base64,AAAA"},
			},
		},
	}
	if !reflect.DeepEqual(input, want) {
		t.Errorf("input =\n%#v\nwant\n%#v", input, want)
	}

	tools, ok := out["tools"].([]any)
	if !ok || len(tools) != 2 {
		t.Fatalf("tools = %#v, want the two gate tools (caller sent none)", out["tools"])
	}
	requireResponsesTool(t, tools[0], "bash", wantGateDescription, emptyParams())
	requireResponsesTool(t, tools[1], "read", wantGateDescription, emptyParams())
	if got := out["tool_choice"]; got != "auto" {
		t.Errorf("tool_choice = %v, want \"auto\" (Responses gateway accepts auto only)", got)
	}
}

// TestChatToResponsesKeepsModel asserts the model id passes through verbatim;
// the endpoint path (chat vs responses) is the caller's choice, not this
// function's.
func TestChatToResponsesKeepsModel(t *testing.T) {
	chat := responsesFixture()
	out, err := ChatToResponses(chat)
	if err != nil {
		t.Fatalf("ChatToResponses: %v", err)
	}
	if got := out["model"]; got != "muse-spark-1.3-contributor-free" {
		t.Errorf("model = %v, want unchanged id", got)
	}
	if _, ok := out["messages"]; ok {
		t.Errorf("output still carries messages: %v", out["messages"])
	}
}

// --- parent task tests ------------------------------------------------------

// TestChatToResponsesBasic covers the core field mapping: ALL system
// messages concatenated with "\n\n" (plugin index.js:1041), user text →
// input_text, max_tokens → max_output_tokens, stream passthrough,
// stream_options dropped, unknown keys dropped (fixed-key body).
func TestChatToResponsesBasic(t *testing.T) {
	chat := map[string]any{
		"model": "muse-spark-1.3-contributor-free",
		"messages": []any{
			map[string]any{"role": "system", "content": "first rule"},
			map[string]any{"role": "system", "content": "second rule"},
			map[string]any{"role": "user", "content": "Hi"},
		},
		"max_tokens":     float64(123),
		"stream":         true,
		"stream_options": map[string]any{"include_usage": true},
		"stop":           "END",
	}
	out, err := ChatToResponses(chat)
	if err != nil {
		t.Fatalf("ChatToResponses: %v", err)
	}
	if got := out["instructions"]; got != "first rule\n\nsecond rule" {
		t.Errorf("instructions = %q, want %q", got, "first rule\n\nsecond rule")
	}
	if got := out["max_output_tokens"]; got != 123 {
		t.Errorf("max_output_tokens = %v, want 123", got)
	}
	if got := out["stream"]; got != true {
		t.Errorf("stream = %v, want true passthrough", got)
	}
	requireNoKey(t, out, "stream_options")
	requireNoKey(t, out, "stop")

	input := requireResponsesInput(t, out)
	want := []any{map[string]any{"role": "user", "content": []any{textPart("Hi")}}}
	if !reflect.DeepEqual(input, want) {
		t.Errorf("input = %#v, want %#v", input, want)
	}

	// stream absent → the key is absent (pure passthrough; the plugin
	// hardcodes stream:true because the plugin itself always streams).
	chat2 := responsesFixture()
	out2, err := ChatToResponses(chat2)
	if err != nil {
		t.Fatalf("ChatToResponses: %v", err)
	}
	requireNoKey(t, out2, "stream")
}

// TestChatToResponsesImages: chat image_url parts → input_image with the
// bare URL string; data: URIs forwarded; detail not forwarded (plugin
// index.js:1062 keeps only image_url.url).
func TestChatToResponsesImages(t *testing.T) {
	chat := map[string]any{
		"model": "muse-spark-1.3-contributor-free",
		"messages": []any{
			map[string]any{
				"role": "user",
				"content": []any{
					map[string]any{"type": "image_url", "image_url": map[string]any{"url": "data:image/png;base64,iVBORw0KGgo=", "detail": "high"}},
					map[string]any{"type": "text", "text": "describe"},
				},
			},
		},
	}
	out, err := ChatToResponses(chat)
	if err != nil {
		t.Fatalf("ChatToResponses: %v", err)
	}
	input := requireResponsesInput(t, out)
	if len(input) != 1 {
		t.Fatalf("input has %d items, want 1", len(input))
	}
	requireMessageItem(t, input[0], "user", []any{
		map[string]any{"type": "input_image", "image_url": "data:image/png;base64,iVBORw0KGgo="},
		textPart("describe"),
	})

	// Plugin quirk mirrored exactly: image_url given as a bare string has no
	// `.url`, so the plugin forwards "" (part.image_url?.url ?? '').
	chat2 := map[string]any{
		"model": "muse-spark-1.3-contributor-free",
		"messages": []any{
			map[string]any{
				"role":    "user",
				"content": []any{map[string]any{"type": "image_url", "image_url": "https://example.com/a.png"}},
			},
		},
	}
	out2, err := ChatToResponses(chat2)
	if err != nil {
		t.Fatalf("ChatToResponses: %v", err)
	}
	input2 := requireResponsesInput(t, out2)
	requireMessageItem(t, input2[0], "user", []any{
		map[string]any{"type": "input_image", "image_url": ""},
	})
}

// TestChatToResponsesEffort verifies the chat → Responses effort mapping
// against the plugin tables: "off" (and the chat disable value "none") →
// reasoning key omitted; every other word is clamped onto the model ladder
// (plugin resolveReasoningEffort + clampEffort, index.js:1253-1276) — for
// muse [minimal low medium high xhigh] the chat word "max" (EFFORT_ORDER
// index 6) maps to "xhigh" (ladder bound 5).
func TestChatToResponsesEffort(t *testing.T) {
	cases := []struct {
		name   string
		model  string
		effort any
		want   any // nil → reasoning key absent
	}{
		{"off omitted", "muse-spark-1.3-contributor-free", "off", nil},
		{"none omitted", "muse-spark-1.3-contributor-free", "none", nil},
		{"low passes ladder", "muse-spark-1.3-contributor-free", "low", "low"},
		{"medium passes ladder", "muse-spark-1.3-contributor-free", "medium", "medium"},
		{"high passes ladder", "muse-spark-1.3-contributor-free", "high", "high"},
		{"max clamps to xhigh", "muse-spark-1.3-contributor-free", "max", "xhigh"},
		{"minimal passes ladder", "muse-spark-1.3-contributor-free", "minimal", "minimal"},
		{"absent omitted", "muse-spark-1.3-contributor-free", nil, nil},
		// Unknown model: plugin findModel → undefined → DEFAULT_EFFORT_IDS
		// ladder [off low high max], so "max" passes unchanged.
		{"unknown model keeps word", "some-other-model", "max", "max"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			chat := map[string]any{"model": tc.model, "messages": []any{
				map[string]any{"role": "user", "content": "Hi"},
			}}
			if tc.effort != nil {
				chat["reasoning_effort"] = tc.effort
			}
			out, err := ChatToResponses(chat)
			if err != nil {
				t.Fatalf("ChatToResponses: %v", err)
			}
			got, present := out["reasoning"]
			if tc.want == nil {
				if present {
					t.Errorf("reasoning = %#v, want key absent", got)
				}
				return
			}
			if !present {
				t.Fatalf("reasoning key absent, want %#v", tc.want)
			}
			obj, ok := got.(map[string]any)
			if !ok {
				t.Fatalf("reasoning = %#v, want object", got)
			}
			if obj["effort"] != tc.want {
				t.Errorf("reasoning.effort = %v, want %v", obj["effort"], tc.want)
			}
		})
	}
}

// TestChatToResponsesEmptyMessages: malformed input errors.
func TestChatToResponsesEmptyMessages(t *testing.T) {
	cases := []struct {
		name string
		body map[string]any
	}{
		{"nil body", nil},
		{"missing messages", map[string]any{"model": "muse-spark-1.3-contributor-free"}},
		{"empty messages", map[string]any{"messages": []any{}}},
		{"messages not an array", map[string]any{"messages": "hi"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			out, err := ChatToResponses(tc.body)
			if err == nil {
				t.Fatalf("out = %#v, want error", out)
			}
			if out != nil {
				t.Errorf("out = %#v, want nil on error", out)
			}
			if !strings.Contains(err.Error(), "zen:") {
				t.Errorf("error %q does not carry the package prefix", err)
			}
		})
	}
}

// TestChatToResponsesAssistantMessages: roles and ordering — user items,
// assistant message + function_call items, function_call_output, empty tool
// output fallback, and assistant messages carrying only tool_calls.
func TestChatToResponsesAssistantMessages(t *testing.T) {
	chat := map[string]any{
		"model": "muse-spark-1.3-contributor-free",
		"messages": []any{
			map[string]any{"role": "assistant", "content": "thinking out loud"},
			map[string]any{
				"role": "assistant",
				"tool_calls": []any{
					map[string]any{"id": "call_a", "type": "function",
						"function": map[string]any{"name": "search", "arguments": "{}"}},
					map[string]any{"id": "call_b", "type": "function",
						"function": map[string]any{"name": "read", "arguments": "{\"path\":\"x\"}"}},
				},
			},
			map[string]any{"role": "tool", "tool_call_id": "call_a", "content": "results"},
			map[string]any{"role": "tool", "tool_call_id": "call_b", "content": ""},
			map[string]any{"role": "user", "content": "next question"},
		},
	}
	out, err := ChatToResponses(chat)
	if err != nil {
		t.Fatalf("ChatToResponses: %v", err)
	}
	input := requireResponsesInput(t, out)
	want := []any{
		map[string]any{"type": "message", "role": "assistant",
			"content": []any{map[string]any{"type": "output_text", "text": "thinking out loud"}}},
		map[string]any{"type": "function_call", "call_id": "call_a", "name": "search", "arguments": "{}"},
		map[string]any{"type": "function_call", "call_id": "call_b", "name": "read", "arguments": "{\"path\":\"x\"}"},
		map[string]any{"type": "function_call_output", "call_id": "call_a", "output": "results"},
		map[string]any{"type": "function_call_output", "call_id": "call_b", "output": "(no output)"},
		map[string]any{"role": "user", "content": []any{textPart("next question")}},
	}
	if !reflect.DeepEqual(input, want) {
		t.Errorf("input =\n%#v\nwant\n%#v", input, want)
	}
	// No instructions: there was no system message.
	requireNoKey(t, out, "instructions")
}

// TestChatToResponsesTools: chat tools (nested function objects) are
// flattened for the Responses wire, missing gate tools are appended after
// the caller's tools, and tool_choice is always "auto".
func TestChatToResponsesTools(t *testing.T) {
	callerWithBash := []any{
		map[string]any{"type": "function", "function": map[string]any{
			"name":        "get_weather",
			"description": "look up weather",
			"parameters":  map[string]any{"type": "object", "properties": map[string]any{"city": map[string]any{"type": "string"}}},
		}},
		map[string]any{"type": "function", "function": map[string]any{
			"name": "bash",
			// no description, no parameters → defaults apply
		}},
	}
	chat := map[string]any{
		"model":    "muse-spark-1.3-contributor-free",
		"messages": []any{map[string]any{"role": "user", "content": "Hi"}},
		"tools":    callerWithBash,
	}
	out, err := ChatToResponses(chat)
	if err != nil {
		t.Fatalf("ChatToResponses: %v", err)
	}
	tools, ok := out["tools"].([]any)
	if !ok {
		t.Fatalf("tools = %T, want array", out["tools"])
	}
	if len(tools) != 3 {
		t.Fatalf("len(tools) = %d, want 3 (2 caller + read gate)", len(tools))
	}
	requireResponsesTool(t, tools[0], "get_weather", "look up weather", map[string]any{
		"type": "object", "properties": map[string]any{"city": map[string]any{"type": "string"}},
	})
	requireResponsesTool(t, tools[1], "bash", nil, emptyParams())
	requireResponsesTool(t, tools[2], "read", wantGateDescription, emptyParams())
	if got := out["tool_choice"]; got != "auto" {
		t.Errorf("tool_choice = %v, want \"auto\"", got)
	}
}

// TestChatToResponsesMaxTokens mirrors resolveMaxTokens (index.js:1231-1235):
// max_completion_tokens wins over max_tokens, the value is capped at the
// model's maxOutput, and an absent/invalid value falls back to the model cap
// (DEFAULT_MAX_TOKENS = 32000 for an unknown model).
func TestChatToResponsesMaxTokens(t *testing.T) {
	cases := []struct {
		name string
		body map[string]any
		want int
	}{
		{
			"max_tokens maps",
			map[string]any{"max_tokens": float64(100)},
			100,
		},
		{
			"max_completion_tokens wins",
			map[string]any{"max_tokens": float64(1000), "max_completion_tokens": float64(200)},
			200,
		},
		{
			"capped at model maxOutput",
			map[string]any{"max_tokens": float64(999999)},
			131072,
		},
		{
			"absent uses model cap",
			map[string]any{},
			131072,
		},
		{
			"non-integer uses model cap",
			map[string]any{"max_tokens": 200.5},
			131072,
		},
		{
			"string uses model cap",
			map[string]any{"max_tokens": "lots"},
			131072,
		},
		{
			"unknown model falls back to DEFAULT_MAX_TOKENS",
			map[string]any{"model": "some-other-model"},
			32000,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			chat := map[string]any{"messages": []any{
				map[string]any{"role": "user", "content": "Hi"},
			}}
			for k, v := range tc.body {
				chat[k] = v
			}
			if _, ok := chat["model"]; !ok {
				chat["model"] = "muse-spark-1.3-contributor-free"
			}
			out, err := ChatToResponses(chat)
			if err != nil {
				t.Fatalf("ChatToResponses: %v", err)
			}
			if got := out["max_output_tokens"]; got != tc.want {
				t.Errorf("max_output_tokens = %v (%T), want %d", got, got, tc.want)
			}
		})
	}
}

// TestChatToResponsesSkipsNonObjectMessages mirrors the plugin's fall-through:
// a messages entry that is not an object contributes nothing instead of
// failing the whole translation.
func TestChatToResponsesSkipsNonObjectMessages(t *testing.T) {
	chat := map[string]any{
		"model": "muse-spark-1.3-contributor-free",
		"messages": []any{
			"junk",
			float64(42),
			nil,
			map[string]any{"role": "user", "content": "real"},
		},
	}
	out, err := ChatToResponses(chat)
	if err != nil {
		t.Fatalf("ChatToResponses: %v", err)
	}
	input := requireResponsesInput(t, out)
	want := []any{map[string]any{"role": "user", "content": []any{textPart("real")}}}
	if !reflect.DeepEqual(input, want) {
		t.Errorf("input = %#v, want %#v", input, want)
	}
}
