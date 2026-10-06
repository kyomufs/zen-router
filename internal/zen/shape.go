package zen

// freeLaneGateToolNames mirrors FREE_LANE_GATE_TOOL_NAMES
// (dsh-opencode-zen lib/index.js:243), in plugin order.
var freeLaneGateToolNames = []string{"bash", "read"}

// freeLaneGateTool mirrors freeLaneGateTool (dsh-opencode-zen
// lib/index.js:245-254) verbatim.
func freeLaneGateTool(name string) map[string]any {
	return map[string]any{
		"type": "function",
		"function": map[string]any{
			"name":        name,
			"description": "Reserved for the host runtime; do not call it.",
			"parameters": map[string]any{
				"type":       "object",
				"properties": map[string]any{},
			},
		},
	}
}

// EnsureFreeLaneShape mirrors ensureFreeLaneShape (dsh-opencode-zen
// lib/index.js:256-273):
//
//   - a nil body or a body without a JSON array under "messages" is
//     returned unchanged (the plugin's Array.isArray(body.messages) guard);
//   - existing tool names are collected from body["tools"] entries that are
//     objects with an object "function" carrying a string "name" — anything
//     else is defensively treated as no name;
//   - missing reserved gate tools (bash, read) are appended after the
//     caller's tools, preserving caller order;
//   - tool_choice "none" is set only when the caller had ZERO tools (an
//     absent or non-array tools counts as zero, as in the plugin); with one
//     or more caller tools the caller's tool_choice is left untouched and
//     never added;
//   - when nothing is missing the input is returned unchanged.
//
// The input is never mutated: changes go into a shallow copy with a fresh
// tools slice (the plugin spreads {...body, tools: [...]}).
// Bodies are expected to be JSON-decoded (map[string]any / []any).
func EnsureFreeLaneShape(body map[string]any) map[string]any {
	if body == nil {
		return body
	}
	messages, ok := body["messages"]
	if !ok {
		return body
	}
	if _, ok := messages.([]any); !ok {
		return body
	}

	var tools []any
	if raw, ok := body["tools"].([]any); ok {
		tools = raw
	}
	names := make(map[string]struct{}, len(tools))
	for _, tool := range tools {
		obj, ok := tool.(map[string]any)
		if !ok {
			continue
		}
		fn, ok := obj["function"].(map[string]any)
		if !ok {
			continue
		}
		if name, ok := fn["name"].(string); ok {
			names[name] = struct{}{}
		}
	}

	missing := make([]string, 0, len(freeLaneGateToolNames))
	for _, name := range freeLaneGateToolNames {
		if _, ok := names[name]; !ok {
			missing = append(missing, name)
		}
	}
	if len(missing) == 0 {
		return body
	}

	out := make(map[string]any, len(body)+2)
	for k, v := range body {
		out[k] = v
	}
	next := make([]any, 0, len(tools)+len(missing))
	next = append(next, tools...)
	for _, name := range missing {
		next = append(next, freeLaneGateTool(name))
	}
	out["tools"] = next
	if len(tools) == 0 {
		out["tool_choice"] = "none"
	}
	return out
}
