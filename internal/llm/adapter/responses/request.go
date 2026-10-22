// Package responses implements Responses protocol conversion.

package responses

import (
	"encoding/json"
	"fmt"
	"strings"

	"github.com/dillonzq/apix/internal/errclass"
	"github.com/dillonzq/apix/internal/llm/adapter/shared"
)

// EndpointPath is the upstream endpoint path for the Responses route.
const EndpointPath = "/v1/responses"

// BuildRequest translates an inbound request body from sourceFormat
// ("chat-completions" Chat Completions, "messages" Anthropic Messages,
// "responses" native Responses passthrough) into a Responses body
// for upstreamModel. upstreamModel replaces whatever
// client-facing model ID the request carried. Reasoning controls
// are not filtered or clamped.
// Unknown formats are ClassUnsupported; malformed input is
// ClassTranslation; messages are descriptive and redacted.
func BuildRequest(upstreamModel string, sourceFormat string, sourceBody []byte, tools ...*shared.ConversionContext) ([]byte, *errclass.Error) {
	switch sourceFormat {
	case "responses":
		return shared.RewriteModelID(upstreamModel, sourceBody, "responses")
	case "chat-completions":
		return fromChatCompletions(upstreamModel, sourceBody, shared.ContextOrNew(tools))
	case "messages":
		return fromClaudeMessages(upstreamModel, sourceBody, shared.ContextOrNew(tools))
	default:
		return nil, shared.UnsupportedFormat(sourceFormat, EndpointPath)
	}
}

type responsesEnvelope struct {
	Model             string            `json:"model"`
	Input             []any             `json:"input,omitempty"`
	Instructions      string            `json:"instructions,omitempty"`
	Tools             []shared.RespTool `json:"tools,omitempty"`
	ToolChoice        json.RawMessage   `json:"tool_choice,omitempty"`
	ParallelToolCalls *bool             `json:"parallel_tool_calls,omitempty"`
	Reasoning         map[string]any    `json:"reasoning,omitempty"`
	MaxOutputTokens   *int64            `json:"max_output_tokens,omitempty"`
	Stream            bool              `json:"stream,omitempty"`
	Temperature       *float64          `json:"temperature,omitempty"`
	TopP              *float64          `json:"top_p,omitempty"`
	Text              json.RawMessage   `json:"text,omitempty"`
}

func msgItem(role string, parts []map[string]any) map[string]any {
	return map[string]any{"type": "message", "role": role, "content": parts}
}

// textPart renders message text; assistant history replays with
// output_text, every other role uses input_text.
func textPart(role, text string) map[string]any {
	if role == "assistant" {
		return map[string]any{"type": "output_text", "text": text}
	}
	return map[string]any{"type": "input_text", "text": text}
}

func imagePart(url string) map[string]any {
	return map[string]any{"type": "input_image", "image_url": url}
}

// respToolChoice renders a shared.DecodeToolChoice kind into the Responses
// wire value; absent yields nil so omitempty drops the field. It is
// infallible: shared.DecodeToolChoice emits exactly the closed set below,
// so the switch covers every possible kind (the exhaustiveness test in
// request_test.go enforces this against shared.AllToolChoiceKinds).
func respToolChoice(kind, name string) json.RawMessage {
	switch kind {
	case shared.ToolChoiceAuto:
		return json.RawMessage(`"auto"`)
	case shared.ToolChoiceAny:
		return json.RawMessage(`"required"`)
	case shared.ToolChoiceNone:
		return json.RawMessage(`"none"`)
	case shared.ToolChoiceNamed:
		b, _ := json.Marshal(map[string]any{"type": "function", "name": name})
		return b
	}
	return nil
}

// ---- Chat Completions source -------------------------------------------
// contentParts normalizes an OpenAI-style content field (JSON string or
// typed parts) into Responses input parts for the given role. The shared kernel owns
// validation and the
// empty-input policy: empty-text parts decode to zero parts so empty
// messages drop identically on every route.
func contentParts(raw json.RawMessage, role string) ([]map[string]any, *errclass.Error) {
	parts, eErr := shared.DecodeStringOrParts(raw, EndpointPath)
	if eErr != nil {
		return nil, eErr
	}
	out := make([]map[string]any, 0, len(parts))
	for _, p := range parts {
		if p.Media != nil {
			if role != "user" {
				return nil, shared.UnsupportedPartType(p.Media.Kind, EndpointPath)
			}
			part, err := p.Media.OpenAI(true)
			if err != nil {
				return nil, err
			}
			out = append(out, part)
			continue
		}
		if p.ImageURL != "" {
			out = append(out, imagePart(p.ImageURL))
			continue
		}
		out = append(out, textPart(role, p.Text))
	}
	return out, nil
}

// fromChatCompletions translates a Chat Completions request into a
// Responses request: system/developer messages become
// instructions, user/assistant turns become message items (assistant
// replaying output_text), tool_calls history becomes function_call items,
// tool results become function_call_output items, max_tokens/
// max_completion_tokens become max_output_tokens, and reasoning_effort is
// forwarded as reasoning.effort without model capability validation. Auto
// uses upstream defaults because Responses has no auto effort value.
// tool_choice is decoded by the shared classifier and rendered into the
// Responses wire values ("auto"/"none"/"required", flat
// {type:function,name} for a forced tool); parallel_tool_calls passes
// through as-is.
//
// Explicit omission policy ("where compatible"): `stop` has no
// Responses equivalent and is dropped, as are sampling/extras without an
// equivalent (logprobs, frequency_penalty, presence_penalty, n, seed,
// response_format, logit_bias) via struct selection. Tool calls and
// reasoning controls are never dropped silently.
func fromChatCompletions(upstreamModel string, body []byte, ctx ...*shared.ConversionContext) ([]byte, *errclass.Error) {
	var src shared.ChatCompletionsRequest
	if err := json.Unmarshal(body, &src); err != nil {
		return nil, errclass.Translation("malformed openai request JSON: " + err.Error())
	}
	req := &responsesEnvelope{
		Model:             upstreamModel,
		Stream:            src.Stream,
		Temperature:       src.Temperature,
		TopP:              src.TopP,
		ParallelToolCalls: src.ParallelToolCalls,
	}
	kind, tcName, eErr := shared.DecodeToolChoice(src.ToolChoice)
	if eErr != nil {
		return nil, eErr
	}
	req.ToolChoice = respToolChoice(kind, tcName)
	if src.MaxTokens != nil && *src.MaxTokens > 0 {
		req.MaxOutputTokens = src.MaxTokens
	} else if src.MaxCompletionTokens != nil && *src.MaxCompletionTokens > 0 {
		req.MaxOutputTokens = src.MaxCompletionTokens
	}
	if src.ReasoningEffort != "" {
		norm := strings.ToLower(strings.TrimSpace(src.ReasoningEffort))
		if norm != "auto" {
			req.Reasoning = map[string]any{"effort": norm}
		}
	}

	var instr strings.Builder
	for i := range src.Messages {
		m := &src.Messages[i]
		switch m.Role {
		case "system", "developer":
			parts, eErr := contentParts(m.Content, m.Role)
			if eErr != nil {
				return nil, eErr
			}
			// Instructions have no image representation: reject like the
			// sibling routes instead of dropping silently.
			for _, p := range parts {
				if typ, _ := p["type"].(string); typ != "input_text" && typ != "output_text" {
					return nil, shared.SystemImageRejected()
				}
			}
			if text := shared.JoinTexts(parts); text != "" {
				if instr.Len() > 0 {
					instr.WriteString("\n\n")
				}
				instr.WriteString(text)
			}
		case "user":
			parts, eErr := contentParts(m.Content, m.Role)
			if eErr != nil {
				return nil, eErr
			}
			if len(parts) > 0 {
				req.Input = append(req.Input, msgItem(m.Role, parts))
			}
		case "assistant":
			if text := m.ReasoningFields.ReplayText(); text != "" {
				req.Input = append(req.Input, shared.ReasoningItem("", text))
			}
			parts, eErr := contentParts(m.Content, m.Role)
			if eErr != nil {
				return nil, eErr
			}
			if len(parts) > 0 {
				req.Input = append(req.Input, msgItem(m.Role, parts))
			}
			for _, tc := range m.ToolCalls {
				req.Input = append(req.Input, map[string]any{
					"type":      "function_call",
					"call_id":   tc.ID,
					"name":      tc.Function.Name,
					"arguments": shared.DefaultArgs(tc.Function.Arguments),
				})
			}
		case "tool":
			parts, eErr := contentParts(m.Content, m.Role)
			if eErr != nil {
				return nil, eErr
			}
			for _, p := range parts {
				if typ, _ := p["type"].(string); typ != "input_text" && typ != "output_text" {
					return nil, errclass.Translation(fmt.Sprintf(
						"unsupported tool content part type %q; function_call_output carries text only", typ))
				}
			}
			req.Input = append(req.Input, map[string]any{
				"type":    "function_call_output",
				"call_id": m.ToolCallID,
				"output":  shared.JoinTexts(parts),
			})
		default:
			return nil, shared.ValidateRole(m.Role, EndpointPath)
		}
	}
	req.Instructions = instr.String()

	for _, t := range src.Tools {
		if eErr := shared.FunctionTool(t.Type, EndpointPath); eErr != nil {
			return nil, eErr
		}
		req.Tools = append(req.Tools, shared.RespTool{
			Type:        "function",
			Name:        t.Function.Name,
			Description: t.Function.Description,
			Parameters:  shared.ObjectSchema(t.Function.Parameters),
			Strict:      t.Function.Strict,
		})
	}
	format, err := shared.ConvertOutputFormat("chat-completions", "responses", src.ResponseFormat, shared.ContextOrNew(ctx))
	if err != nil {
		return nil, err
	}
	req.Text = format
	b, _ := json.Marshal(req) // only marshallable composed types; cannot fail
	return b, nil
}

// ---- Claude Messages source --------------------------------------------
// fromClaudeMessages translates an Anthropic Messages request into a
// Responses request: system becomes instructions, text/
// image blocks become input parts, tool_use/tool_result become
// function_call/function_call_output items, explicit thinking controls
// map to reasoning.effort without capability filtering, and max_tokens
// becomes max_output_tokens (defaulted by the
// shared kernel). Decoding is owned entirely by the shared
// Claude-request kernel; only target-shape rendering stays local.
//
// Explicit omission policy ("where compatible"): stop_sequences is
// dropped (no Responses equivalent); readable thinking becomes reasoning
// content while signatures and redacted_thinking remain unrepresentable; images
// nested inside tool_result content are rejected descriptively, never
// silently lost. The tool_result is_error flag has no Responses
// field and is preserved as an "[error] " marker in the output text rather
// than lost silently.
func fromClaudeMessages(upstreamModel string, body []byte, ctx ...*shared.ConversionContext) ([]byte, *errclass.Error) {
	src, eErr := shared.DecodeClaudeMessages(body)
	if eErr != nil {
		return nil, eErr
	}
	req := &responsesEnvelope{
		Model:           upstreamModel,
		MaxOutputTokens: &src.MaxTokens,
		Stream:          src.Stream,
		Temperature:     src.Temperature,
		TopP:            src.TopP,
	}
	req.ToolChoice = respToolChoice(src.ToolChoiceKind, src.ToolChoiceName)
	effort, eErr := shared.ClaudeReasoningEffort(src)
	if eErr != nil {
		return nil, eErr
	}
	if effort != "" && effort != "auto" {
		req.Reasoning = map[string]any{"effort": effort}
	}
	req.Instructions = src.System

	for _, m := range src.Messages {
		if m.Role == "system" {
			text := m.Content
			for _, blk := range m.Blocks {
				switch blk.Kind {
				case "text":
					if text != "" {
						text += "\n\n"
					}
					text += blk.Text
				case "image":
					return nil, shared.SystemImageRejected()
				case "thinking", "redacted_thinking":
					// omitted per the policy in the doc comment
				default:
					return nil, shared.UnsupportedPartType(blk.Kind, EndpointPath)
				}
			}
			if text != "" {
				if req.Instructions != "" {
					req.Instructions += "\n\n"
				}
				req.Instructions += text
			}
			continue
		}
		switch m.Role {
		case "user", "assistant":
		default:
			return nil, shared.ValidateRole(m.Role, EndpointPath)
		}
		var parts []map[string]any
		flush := func() {
			if len(parts) > 0 {
				req.Input = append(req.Input, msgItem(m.Role, parts))
				parts = nil
			}
		}
		if m.Content != "" {
			parts = append(parts, textPart(m.Role, m.Content))
		}
		for _, blk := range m.Blocks {
			switch blk.Kind {
			case "text":
				parts = append(parts, textPart(m.Role, blk.Text))
			case "document":
				if m.Role != "user" {
					return nil, shared.UnsupportedPartType(blk.Kind, EndpointPath)
				}
				part, err := blk.Media.OpenAI(true)
				if err != nil {
					return nil, err
				}
				parts = append(parts, part)
			case "image":
				parts = append(parts, imagePart(blk.URL))
			case "tool_use":
				flush()
				args := shared.DefaultArgs(string(blk.Input))
				req.Input = append(req.Input, map[string]any{
					"type":      "function_call",
					"call_id":   blk.CallID,
					"name":      blk.Name,
					"arguments": args,
				})
			case "tool_result":
				flush()
				output, eErr := shared.ToolResultText(blk.Result, "function_call_output carries text only")
				if eErr != nil {
					return nil, eErr
				}
				if blk.IsError {
					output = shared.ToolResultErrorPrefix + output
				}
				req.Input = append(req.Input, map[string]any{
					"type":    "function_call_output",
					"call_id": blk.CallID,
					"output":  output,
				})
			case "thinking":
				if m.Role != "assistant" {
					return nil, shared.UnsupportedPartType(blk.Kind, EndpointPath)
				}
				if blk.Thinking != "" {
					flush()
					req.Input = append(req.Input, shared.ReasoningItem("", blk.Thinking))
				}
			case "redacted_thinking":
				// Opaque provider data has no cross-protocol representation.
			default:
				flush()
				return nil, shared.UnsupportedPartType(blk.Kind, EndpointPath)
			}
		}
		flush()
	}

	for _, t := range src.Tools {
		if shared.IsSearchTool(t.Type) {
			req.Tools = append(req.Tools, shared.MessageSearchTool(t))
			if src.ToolChoiceKind == shared.ToolChoiceNamed && src.ToolChoiceName == t.Name {
				req.ToolChoice = json.RawMessage(`{"type":"web_search"}`)
			}
			continue
		}
		if t.Type != "" && t.Type != "custom" {
			continue
		}
		// Claude tools carry no type field (validation cannot reject here).
		req.Tools = append(req.Tools, shared.RespTool{
			Type:        "function",
			Name:        t.Name,
			Description: t.Description,
			Parameters:  shared.ObjectSchema(t.InputSchema),
			Strict:      t.Strict,
		})
	}
	format, err := shared.ConvertOutputFormat("messages", "responses", src.OutputConfig, shared.ContextOrNew(ctx))
	if err != nil {
		return nil, err
	}
	req.Text = format
	b, _ := json.Marshal(req) // only marshallable composed types; cannot fail
	return b, nil
}
