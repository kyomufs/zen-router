package zen

import (
	"encoding/json"
	"reflect"
	"testing"
)

// wantGateDescription is the literal plugin description for reserved gate
// tools (dsh-opencode-zen lib/index.js:250).
const wantGateDescription = "Reserved for the host runtime; do not call it."

// requireFreeLaneGateTool asserts v is exactly the gate tool object the
// plugin's freeLaneGateTool builds (lib/index.js:245-254).
func requireFreeLaneGateTool(t *testing.T, v any, name string) {
	t.Helper()
	tool, ok := v.(map[string]any)
	if !ok {
		t.Fatalf("gate tool %q: got %T (%v), want object", name, v, v)
	}
	if got := tool["type"]; got != "function" {
		t.Errorf("gate tool %q: type = %v, want \"function\"", name, got)
	}
	fn, ok := tool["function"].(map[string]any)
	if !ok {
		t.Fatalf("gate tool %q: function = %T (%v), want object", name, tool["function"], tool["function"])
	}
	if got := fn["name"]; got != name {
		t.Errorf("gate tool function.name = %v, want %q", got, name)
	}
	if got := fn["description"]; got != wantGateDescription {
		t.Errorf("gate tool %q: description = %v, want %q", name, got, wantGateDescription)
	}
	params, ok := fn["parameters"].(map[string]any)
	if !ok {
		t.Fatalf("gate tool %q: parameters = %T (%v), want object", name, fn["parameters"], fn["parameters"])
	}
	if got := params["type"]; got != "object" {
		t.Errorf("gate tool %q: parameters.type = %v, want \"object\"", name, got)
	}
	props, ok := params["properties"].(map[string]any)
	if !ok {
		t.Fatalf("gate tool %q: parameters.properties = %T (%v), want object", name, params["properties"], params["properties"])
	}
	if len(props) != 0 {
		t.Errorf("gate tool %q: parameters.properties = %v, want empty object", name, props)
	}
}

// callerTool builds a non-reserved caller tool used across subtests.
func callerTool(name string) map[string]any {
	return map[string]any{
		"type": "function",
		"function": map[string]any{
			"name":        name,
			"description": "a caller tool",
			"parameters":  map[string]any{"type": "object", "properties": map[string]any{}},
		},
	}
}

// toolList extracts the tools array from a shaped body.
func toolList(t *testing.T, body map[string]any) []any {
	t.Helper()
	tools, ok := body["tools"].([]any)
	if !ok {
		t.Fatalf("tools = %T (%v), want []any", body["tools"], body["tools"])
	}
	return tools
}

// TestGateTools asserts ensureFreeLaneShape ported from the plugin
// (dsh-opencode-zen lib/index.js:256-273): reserved bash/read gate tools
// are appended when missing, tool_choice "none" only for zero caller tools,
// and the input body is never mutated.
func TestGateTools(t *testing.T) {
	base := func() map[string]any {
		return map[string]any{
			"model":    "mimo-v2.6-flash-free",
			"messages": []any{map[string]any{"role": "user", "content": "hi"}},
		}
	}

	t.Run("zero tools gets both gates and tool_choice none", func(t *testing.T) {
		body := base()
		got := EnsureFreeLaneShape(body)

		tools := toolList(t, got)
		if len(tools) != 2 {
			t.Fatalf("tools length = %d, want 2; got %v", len(tools), tools)
		}
		requireFreeLaneGateTool(t, tools[0], "bash")
		requireFreeLaneGateTool(t, tools[1], "read")
		if got["tool_choice"] != "none" {
			t.Errorf("tool_choice = %v, want \"none\"", got["tool_choice"])
		}
		if got["model"] != "mimo-v2.6-flash-free" {
			t.Errorf("model = %v, want preserved value", got["model"])
		}
	})

	t.Run("empty tools array gets gates and tool_choice none", func(t *testing.T) {
		body := base()
		body["tools"] = []any{}
		got := EnsureFreeLaneShape(body)

		tools := toolList(t, got)
		if len(tools) != 2 {
			t.Fatalf("tools length = %d, want 2", len(tools))
		}
		requireFreeLaneGateTool(t, tools[0], "bash")
		requireFreeLaneGateTool(t, tools[1], "read")
		if got["tool_choice"] != "none" {
			t.Errorf("tool_choice = %v, want \"none\"", got["tool_choice"])
		}
	})

	t.Run("caller tools kept in order, gates appended, tool_choice untouched", func(t *testing.T) {
		caller := callerTool("web_search")
		body := base()
		body["tools"] = []any{caller}
		body["tool_choice"] = "auto"
		got := EnsureFreeLaneShape(body)

		tools := toolList(t, got)
		if len(tools) != 3 {
			t.Fatalf("tools length = %d, want 3; got %v", len(tools), tools)
		}
		if !reflect.DeepEqual(tools[0], map[string]any(caller)) {
			t.Errorf("tools[0] = %v, want caller tool first", tools[0])
		}
		requireFreeLaneGateTool(t, tools[1], "bash")
		requireFreeLaneGateTool(t, tools[2], "read")
		if got["tool_choice"] != "auto" {
			t.Errorf("tool_choice = %v, want caller value \"auto\" untouched", got["tool_choice"])
		}
	})

	t.Run("caller tools without tool_choice never gain one", func(t *testing.T) {
		body := base()
		body["tools"] = []any{callerTool("web_search")}
		got := EnsureFreeLaneShape(body)

		if _, ok := got["tool_choice"]; ok {
			t.Errorf("tool_choice = %v, want absent", got["tool_choice"])
		}
		if tools := toolList(t, got); len(tools) != 3 {
			t.Errorf("tools length = %d, want 3", len(tools))
		}
	})

	t.Run("caller already has bash, only read added", func(t *testing.T) {
		body := base()
		body["tools"] = []any{callerTool("bash")}
		got := EnsureFreeLaneShape(body)

		tools := toolList(t, got)
		if len(tools) != 2 {
			t.Fatalf("tools length = %d, want 2 (caller bash + read); got %v", len(tools), tools)
		}
		requireFreeLaneGateTool(t, tools[1], "read")
		if _, ok := got["tool_choice"]; ok {
			t.Errorf("tool_choice = %v, want absent for caller with tools", got["tool_choice"])
		}
	})

	t.Run("both gates already present returns body unchanged", func(t *testing.T) {
		body := base()
		body["tools"] = []any{callerTool("bash"), callerTool("read")}
		got := EnsureFreeLaneShape(body)

		if reflect.ValueOf(got).Pointer() != reflect.ValueOf(body).Pointer() {
			t.Errorf("want identical (unmodified) map returned when nothing missing")
		}
		if _, ok := got["tool_choice"]; ok {
			t.Errorf("tool_choice = %v, want absent", got["tool_choice"])
		}
	})

	t.Run("missing messages returns body unchanged", func(t *testing.T) {
		body := map[string]any{"model": "mimo-v2.6-flash-free"}
		got := EnsureFreeLaneShape(body)

		if reflect.ValueOf(got).Pointer() != reflect.ValueOf(body).Pointer() {
			t.Errorf("want identical map returned when messages missing")
		}
		if _, ok := got["tools"]; ok {
			t.Errorf("tools = %v, want absent", got["tools"])
		}
		if _, ok := got["tool_choice"]; ok {
			t.Errorf("tool_choice = %v, want absent", got["tool_choice"])
		}
	})

	t.Run("non-array messages returns body unchanged", func(t *testing.T) {
		body := map[string]any{"messages": "not-an-array"}
		got := EnsureFreeLaneShape(body)

		if reflect.ValueOf(got).Pointer() != reflect.ValueOf(body).Pointer() {
			t.Errorf("want identical map returned for non-array messages")
		}
		if _, ok := got["tools"]; ok {
			t.Errorf("tools = %v, want absent", got["tools"])
		}
	})

	t.Run("nil body returns nil", func(t *testing.T) {
		if got := EnsureFreeLaneShape(nil); got != nil {
			t.Errorf("got %v, want nil", got)
		}
	})

	t.Run("input map is not mutated", func(t *testing.T) {
		caller := callerTool("web_search")
		inputTools := make([]any, 1, 4) // spare capacity: catch backing-array writes
		inputTools[0] = caller
		body := base()
		body["tools"] = inputTools

		before, err := json.Marshal(body)
		if err != nil {
			t.Fatalf("marshal input: %v", err)
		}
		got := EnsureFreeLaneShape(body)
		after, err := json.Marshal(body)
		if err != nil {
			t.Fatalf("marshal input: %v", err)
		}
		if string(before) != string(after) {
			t.Errorf("input mutated:\nbefore = %s\nafter  = %s", before, after)
		}
		tools := toolList(t, got)
		if len(tools) != 3 {
			t.Fatalf("shaped tools length = %d, want 3", len(tools))
		}
		if &tools[0] == &inputTools[0] {
			t.Errorf("shaped tools share the caller's backing array")
		}
	})
}

// TestGateToolsNonObjectTools asserts malformed tool entries are skipped
// defensively (the plugin reads tool.function.name only from objects,
// lib/index.js:259-265) and never crash the shaping.
func TestGateToolsNonObjectTools(t *testing.T) {
	t.Run("malformed entries yield no names, both gates appended", func(t *testing.T) {
		body := map[string]any{
			"messages": []any{map[string]any{"role": "user", "content": "hi"}},
			"tools": []any{
				nil,
				"bash",
				42,
				map[string]any{"name": "bash"},     // no function
				map[string]any{"function": nil},    // function not an object
				map[string]any{"function": "bash"}, // function not an object
				map[string]any{"function": map[string]any{"name": 123}}, // name not a string
			},
		}
		got := EnsureFreeLaneShape(body)

		tools := toolList(t, got)
		if len(tools) != 9 {
			t.Fatalf("tools length = %d, want 9 (7 malformed + 2 gates)", len(tools))
		}
		requireFreeLaneGateTool(t, tools[7], "bash")
		requireFreeLaneGateTool(t, tools[8], "read")
		// Non-empty caller tools slice: tool_choice must stay untouched.
		if _, ok := got["tool_choice"]; ok {
			t.Errorf("tool_choice = %v, want absent for non-empty caller tools", got["tool_choice"])
		}
	})

	t.Run("valid name among malformed entries suppresses that gate", func(t *testing.T) {
		body := map[string]any{
			"messages": []any{map[string]any{"role": "user", "content": "hi"}},
			"tools": []any{
				42,
				map[string]any{"function": map[string]any{"name": "bash"}},
			},
		}
		got := EnsureFreeLaneShape(body)

		tools := toolList(t, got)
		if len(tools) != 3 {
			t.Fatalf("tools length = %d, want 3 (2 mixed + read gate)", len(tools))
		}
		requireFreeLaneGateTool(t, tools[2], "read")
	})

	t.Run("non-array tools replaced by gates with tool_choice none", func(t *testing.T) {
		// The plugin treats a non-array tools value as an empty list
		// (Array.isArray check, lib/index.js:258) and therefore as
		// "zero caller tools"; mirror that exactly.
		body := map[string]any{
			"messages": []any{map[string]any{"role": "user", "content": "hi"}},
			"tools":    "not-an-array",
		}
		got := EnsureFreeLaneShape(body)

		tools := toolList(t, got)
		if len(tools) != 2 {
			t.Fatalf("tools length = %d, want 2", len(tools))
		}
		requireFreeLaneGateTool(t, tools[0], "bash")
		requireFreeLaneGateTool(t, tools[1], "read")
		if got["tool_choice"] != "none" {
			t.Errorf("tool_choice = %v, want \"none\"", got["tool_choice"])
		}
	})
}
