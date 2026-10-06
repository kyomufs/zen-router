package zen

import (
	"encoding/json"
	"fmt"
	"math"
	"strings"
)

// defaultMaxTokens mirrors DEFAULT_MAX_TOKENS (dsh-opencode-zen
// lib/index.js:93): the output budget for a body whose model declares no
// maxOutput (unknown id — the gateway normally rejects those earlier).
const defaultMaxTokens = 32000

// ChatToResponses rewrites a parsed chat.completions request body into a
// Responses API request body. It mirrors buildResponsesBody
// (dsh-opencode-zen lib/index.js:1036-1081) plus the tool serializers it
// calls (serializeResponsesTools/ensureResponsesGateTools, index.js:1010-1027)
// and the effort/token resolvers the plugin runs before the call
// (resolveReasoningEffort/clampEffort index.js:1253-1276, resolveMaxTokens
// index.js:1231-1235).
//
// Semantics, ported from the plugin:
//
//   - errors on a nil body, a missing "messages" key, a non-array
//     "messages", or an empty messages array (malformed input);
//   - ALL system messages are concatenated into "instructions" with "\n\n"
//     (index.js:1041); a non-string system content is flattened like the
//     plugin's flattenText (index.js:635-640: text parts joined with "");
//     the key is omitted when the result is the empty string (JS truthiness);
//   - "input" is one item per non-system message, in order: user (and any
//     unknown role) → one {role:"user", content:[parts]} item with
//     input_text/input_image parts; assistant → an output_text message item
//     (when content is a non-empty string) plus one function_call item per
//     tool_call; tool → one function_call_output item with the
//     "(no output)" fallback;
//   - the response object is built from a FIXED key set (the plugin does
//     not spread the chat body): model, stream, input, tools, tool_choice,
//     instructions, max_output_tokens, reasoning, temperature — everything
//     else (stream_options, top_p, n, stop, response_format, ...) is
//     dropped;
//   - tools are re-shaped from the chat wire's nested {function:{...}}
//     objects into the Responses flat {type,name,description,parameters}
//     form (missing parameters → {type:"object",properties:{}}), missing
//     reserved gate tools are appended after the caller's tools, and
//     tool_choice is always "auto" (the Responses gateway accepts auto
//     only — index.js:1073-1075);
//   - max_completion_tokens wins over max_tokens; the value is capped at
//     the model's maxOutput and defaults to it when absent/invalid
//     (resolveMaxTokens); unknown model → DEFAULT_MAX_TOKENS;
//   - reasoning_effort maps onto the model's ladder via ClampEffort (the
//     plugin's clampEffort); "off" and the chat disable word "none" omit
//     the reasoning key entirely (the Responses wire has no disable word —
//     resolveReasoningEffort returns undefined for "off" on responses
//     models, and the emit guard is `effort && effort !== 'none'`);
//   - "stream" passes through when present and is omitted when absent —
//     the plugin hardcodes stream:true because the plugin always streams;
//     forcing it belongs to the gateway caller;
//   - "model" passes through verbatim; picking the endpoint path is the
//     caller's job.
//
// The input is never mutated: a fresh map is returned.
func ChatToResponses(chatBody map[string]any) (map[string]any, error) {
	if chatBody == nil {
		return nil, fmt.Errorf("zen: chat body is not an object")
	}
	rawMessages, ok := chatBody["messages"]
	if !ok {
		return nil, fmt.Errorf("zen: chat body has no messages")
	}
	messages, ok := rawMessages.([]any)
	if !ok {
		return nil, fmt.Errorf("zen: messages is not an array")
	}
	if len(messages) == 0 {
		return nil, fmt.Errorf("zen: messages array is empty")
	}

	var instructions string
	haveInstructions := false
	input := make([]any, 0, len(messages))
	for _, rawMessage := range messages {
		message, ok := rawMessage.(map[string]any)
		if !ok {
			// The plugin's fall-through: non-object entries contribute
			// nothing instead of failing the translation.
			continue
		}
		role, _ := message["role"].(string)
		switch role {
		case "system":
			text := flattenText(message["content"])
			if !haveInstructions {
				instructions, haveInstructions = text, true
			} else {
				instructions += "\n\n" + text
			}
		case "assistant":
			input = append(input, assistantItems(message)...)
		case "tool":
			input = append(input, functionCallOutput(message))
		default:
			// user plus every unknown role lands in the plugin's default
			// branch (index.js:1057-1066).
			if parts := userParts(message["content"]); len(parts) > 0 {
				input = append(input, map[string]any{
					"role":    "user",
					"content": parts,
				})
			}
		}
	}

	modelID, _ := chatBody["model"].(string)
	model, modelErr := ResolveModel(modelID)

	out := map[string]any{
		"input":             input,
		"tools":             responsesTools(chatBody),
		"tool_choice":       "auto",
		"max_output_tokens": maxOutputTokens(chatBody, model, modelErr == nil),
	}
	if v, ok := chatBody["model"]; ok {
		out["model"] = v
	}
	if v, ok := chatBody["stream"]; ok {
		out["stream"] = v
	}
	if v, ok := chatBody["temperature"]; ok {
		out["temperature"] = v
	}
	if haveInstructions && instructions != "" {
		out["instructions"] = instructions
	}
	// Unknown model → zero Model: the default effort ladder and no
	// reasoningRequired default, mirroring the plugin's findModel → undefined
	// path (effortLevels falls back to DEFAULT_EFFORT_IDS, index.js:1221-1222).
	if effort := responsesEffort(chatBody, model); effort != "" {
		out["reasoning"] = map[string]any{"effort": effort}
	}
	return out, nil
}

// flattenText mirrors flattenText (index.js:635-640): a string passes
// through; an array contributes only its type:"text" parts joined with "";
// anything else becomes "".
func flattenText(content any) string {
	switch c := content.(type) {
	case string:
		return c
	case []any:
		var b strings.Builder
		for _, rawPart := range c {
			part, ok := rawPart.(map[string]any)
			if !ok {
				continue
			}
			if part["type"] != "text" {
				continue
			}
			if text, ok := part["text"].(string); ok {
				b.WriteString(text)
			}
		}
		return b.String()
	default:
		return ""
	}
}

// userParts builds the content parts of one user (or unknown-role) input
// item (index.js:1057-1066): a non-empty string → one input_text; an array
// → input_text per truthy text part and input_image per image_url part,
// forwarding only image_url.url (the plugin's `part.image_url?.url ?? ”`,
// so a bare string image_url yields "").
func userParts(content any) []any {
	switch c := content.(type) {
	case string:
		if c == "" {
			return nil
		}
		return []any{map[string]any{"type": "input_text", "text": c}}
	case []any:
		parts := make([]any, 0, len(c))
		for _, rawPart := range c {
			part, ok := rawPart.(map[string]any)
			if !ok {
				continue
			}
			switch part["type"] {
			case "image_url":
				imageURL := ""
				if image, ok := part["image_url"].(map[string]any); ok {
					imageURL, _ = image["url"].(string)
				}
				parts = append(parts, map[string]any{
					"type":      "input_image",
					"image_url": imageURL,
				})
			case "text":
				if text, ok := part["text"].(string); ok && text != "" {
					parts = append(parts, map[string]any{"type": "input_text", "text": text})
				}
			}
		}
		return parts
	default:
		return nil
	}
}

// assistantItems mirrors the plugin's assistant branch (index.js:1044-1052):
// non-empty string content → one output_text message item (the Responses
// gateway wants encrypted_content for reasoning round-trips, so reasoning
// history is not replayed — see the plugin comment at index.js:1029-1035),
// and each well-formed tool_call → one function_call item.
func assistantItems(message map[string]any) []any {
	var items []any
	if text, ok := message["content"].(string); ok && text != "" {
		items = append(items, map[string]any{
			"type":    "message",
			"role":    "assistant",
			"content": []any{map[string]any{"type": "output_text", "text": text}},
		})
	}
	calls, ok := message["tool_calls"].([]any)
	if !ok {
		return items
	}
	for _, rawCall := range calls {
		call, ok := rawCall.(map[string]any)
		if !ok {
			continue
		}
		function, ok := call["function"].(map[string]any)
		if !ok {
			continue
		}
		name, ok := function["name"].(string)
		if !ok {
			continue
		}
		item := map[string]any{
			"type": "function_call",
			"name": name,
		}
		if id, ok := call["id"]; ok {
			item["call_id"] = id
		}
		if arguments, ok := function["arguments"]; ok && arguments != nil {
			if s, isString := arguments.(string); isString {
				item["arguments"] = s
			} else if encoded, err := json.Marshal(arguments); err == nil {
				item["arguments"] = string(encoded)
			}
		}
		items = append(items, item)
	}
	return items
}

// functionCallOutput mirrors the plugin's tool branch (index.js:1053-1056):
// `output: m.content || '(no output)'` — a non-empty string passes through,
// array content passes through as-is (JS arrays are truthy), everything
// else (absent, "", null) falls back to "(no output)".
func functionCallOutput(message map[string]any) map[string]any {
	item := map[string]any{"type": "function_call_output"}
	if id, ok := message["tool_call_id"]; ok {
		item["call_id"] = id
	}
	output := any("(no output)")
	switch content := message["content"].(type) {
	case string:
		if content != "" {
			output = content
		}
	case []any:
		output = content
	}
	item["output"] = output
	return item
}

// responsesTools mirrors serializeResponsesTools + ensureResponsesGateTools
// (index.js:1010-1027) for chat-wire input: each nested
// {type:"function",function:{...}} tool becomes a flat Responses tool,
// parameters fall back to an empty object schema, and the reserved gate
// tools are appended (in plugin order) when missing.
func responsesTools(chatBody map[string]any) []any {
	tools := []any{}
	names := make(map[string]struct{}, len(freeLaneGateToolNames))
	if raw, ok := chatBody["tools"].([]any); ok {
		for _, rawTool := range raw {
			tool, ok := rawTool.(map[string]any)
			if !ok {
				continue
			}
			// The chat wire nests the schema under "function"; flat tools
			// (the plugin's harness shape) are accepted as-is.
			source := tool
			if function, ok := tool["function"].(map[string]any); ok {
				source = function
			}
			name, ok := source["name"].(string)
			if !ok {
				continue
			}
			entry := map[string]any{"type": "function", "name": name}
			if description, ok := source["description"].(string); ok {
				entry["description"] = description
			}
			parameters := map[string]any{"type": "object", "properties": map[string]any{}}
			if p, ok := source["parameters"].(map[string]any); ok {
				parameters = p
			}
			entry["parameters"] = parameters
			tools = append(tools, entry)
			names[name] = struct{}{}
		}
	}
	for _, name := range freeLaneGateToolNames {
		if _, present := names[name]; present {
			continue
		}
		gate, _ := freeLaneGateTool(name)["function"].(map[string]any)
		tools = append(tools, map[string]any{
			"type":        "function",
			"name":        name,
			"description": gate["description"],
			"parameters":  gate["parameters"],
		})
		names[name] = struct{}{}
	}
	return tools
}

// responsesEffort resolves reasoning_effort for the Responses wire, porting
// resolveReasoningEffort + clampEffort (index.js:1253-1276):
//
//   - "off" and "none" → "" (the caller omits the reasoning key): the
//     Responses wire exposes no disable word, so resolveReasoningEffort
//     returns undefined for "off" on responses models, and the emit guard
//     is `effort && effort !== 'none'`. "none" is also what ToChatWire
//     produces for "off" on the chat wire, so a body pre-converted for the
//     chat lane still translates correctly;
//   - absent → "" unless the model is reasoningRequired (default effort
//     "high", index.js:1258);
//   - otherwise ClampEffort pins the word onto the model ladder — the
//     chat→Responses ladder mapping ("max" → "xhigh" for muse);
//   - a clamped result of "off"/"none" omits the key (models.go documents
//     the Responses wire omits the field when the result is "off").
func responsesEffort(chatBody map[string]any, model Model) string {
	requested, _ := chatBody["reasoning_effort"].(string)
	switch requested {
	case "off", "none":
		return ""
	case "":
		return ClampEffort(model, "")
	default:
		effort := ClampEffort(model, requested)
		if effort == "off" || effort == "none" {
			return ""
		}
		return effort
	}
}

// maxOutputTokens mirrors resolveMaxTokens (index.js:1231-1235): the
// request's output budget capped at the model's maxOutput, defaulting to
// the cap when absent or not a positive safe integer — and to
// DEFAULT_MAX_TOKENS when the model id is unknown (meta?.maxOutput ?? ...).
// max_completion_tokens wins over max_tokens (OpenAI precedence); the
// plugin receives a single pre-resolved value from its harness.
func maxOutputTokens(chatBody map[string]any, model Model, modelKnown bool) int {
	budget := defaultMaxTokens
	if modelKnown {
		budget = model.MaxOutput
	}
	requested, ok := chatBody["max_completion_tokens"]
	if !ok {
		requested, ok = chatBody["max_tokens"]
	}
	if !ok {
		return budget
	}
	switch v := requested.(type) {
	case float64:
		if !math.IsNaN(v) && v >= 1 && v <= float64(1<<53-1) && v == math.Trunc(v) {
			if n := int(v); n < budget {
				return n
			}
		}
	case int:
		if v >= 1 && v < budget {
			return v
		}
	}
	return budget
}
