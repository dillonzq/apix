// Package chatcompletions implements Chat Completions protocol conversion.

package chatcompletions

import (
	"encoding/json"
	"fmt"
	"strings"

	"github.com/dillonzq/apix/internal/errclass"
	"github.com/dillonzq/apix/internal/llm/adapter/shared"
)

const EndpointPath = "/v1/chat/completions"

// BuildRequest translates an inbound request body from sourceFormat
// ("chat-completions" Chat Completions, "responses" Responses, "messages"
// Messages) into a Chat Completions body for upstreamModel.
// Reasoning controls are not filtered or clamped. Unknown formats are ClassUnsupported; malformed
// input is ClassTranslation. Errors are descriptive and redacted — no
// silent loss of tools or reasoning controls.
func BuildRequest(upstreamModel, sourceFormat string, sourceBody []byte, tools ...*shared.ConversionContext) ([]byte, *errclass.Error) {
	switch sourceFormat {
	case "chat-completions":
		return buildOpenAIRequest(upstreamModel, sourceBody)
	case "messages":
		return claudeToChat(upstreamModel, sourceBody, shared.ContextOrNew(tools))
	case "responses":
		return responsesToChat(upstreamModel, sourceBody, shared.ContextOrNew(tools))
	default:
		return nil, shared.UnsupportedFormat(sourceFormat, EndpointPath)
	}
}

// buildOpenAIRequest preserves native fields and only rewrites the model.
func buildOpenAIRequest(upstreamModel string, body []byte) ([]byte, *errclass.Error) {
	return shared.RewriteModelID(upstreamModel, body, "chat-completions")
}

type imageURLField struct {
	URL string `json:"url"`
}

type ccContentPart struct {
	Type       string         `json:"type"`
	Text       string         `json:"text,omitempty"`
	ImageURL   *imageURLField `json:"image_url,omitempty"`
	File       any            `json:"file,omitempty"`
	InputAudio any            `json:"input_audio,omitempty"`
	VideoURL   any            `json:"video_url,omitempty"`
}

type ccMessage struct {
	ReasoningContent string              `json:"reasoning_content,omitempty"`
	Role             string              `json:"role"`
	Content          any                 `json:"content"` // string, []ccContentPart, or nil
	ToolCalls        []shared.CCToolCall `json:"tool_calls,omitempty"`
	ToolCallID       string              `json:"tool_call_id,omitempty"`
}

type ccRequest struct {
	Model             string          `json:"model"`
	Messages          []ccMessage     `json:"messages"`
	MaxTokens         *int64          `json:"max_tokens,omitempty"`
	Stop              any             `json:"stop,omitempty"`
	Stream            bool            `json:"stream,omitempty"`
	Temperature       *float64        `json:"temperature,omitempty"`
	TopP              *float64        `json:"top_p,omitempty"`
	ReasoningEffort   string          `json:"reasoning_effort,omitempty"`
	Tools             []shared.CCTool `json:"tools,omitempty"`
	ToolChoice        json.RawMessage `json:"tool_choice,omitempty"`
	ParallelToolCalls *bool           `json:"parallel_tool_calls,omitempty"`
	ResponseFormat    json.RawMessage `json:"response_format,omitempty"`
}

// encode finalizes and encodes a translated request; the composed types
// are always marshallable.
func encode(req *ccRequest) []byte {
	if req.Messages == nil {
		req.Messages = []ccMessage{}
	}
	b, _ := json.Marshal(req)
	return b
}

// ---- claude (Anthropic Messages) -> Chat Completions ----

type claudeBlock = map[string]any

// claudeToChat translates an Anthropic Messages request into a Chat
// Completions request: top-level system becomes a system
// message, text blocks become string content, tool_use blocks become
// assistant tool_calls, tool_result blocks become role:"tool" messages,
// max_tokens/stop_sequences/tools map to their CC equivalents
// (max_tokens defaulted by the shared kernel), and the thinking
// control maps to reasoning_effort through fixed protocol conversion. The
// shared Claude-request kernel owns decoding; target-shape rendering stays local.
func claudeToChat(upstreamModel string, body []byte, ctx ...*shared.ConversionContext) ([]byte, *errclass.Error) {
	src, eErr := shared.DecodeClaudeMessages(body)
	if eErr != nil {
		return nil, eErr
	}
	out := &ccRequest{
		Model:       upstreamModel,
		Stream:      src.Stream,
		Temperature: src.Temperature,
		TopP:        src.TopP,
	}
	out.MaxTokens = &src.MaxTokens
	if len(src.StopSequences) > 0 {
		out.Stop = src.StopSequences
	}
	effort, eErr := shared.ClaudeReasoningEffort(src)
	if eErr != nil {
		return nil, eErr
	}
	if effort != "auto" {
		out.ReasoningEffort = effort
	}
	applyToolChoiceCC(out, src.ToolChoiceKind, src.ToolChoiceName)
	if src.System != "" {
		out.Messages = append(out.Messages, ccMessage{Role: "system", Content: src.System})
	}
	for i := range src.Messages {
		m := &src.Messages[i]
		switch m.Role {
		case "user":
			msgs, eErr := claudeUserMessages(m)
			if eErr != nil {
				return nil, eErr
			}
			out.Messages = append(out.Messages, msgs...)
		case "assistant":
			msg, eErr := claudeAssistantMessage(m)
			if eErr != nil {
				return nil, eErr
			}
			if msg != nil {
				out.Messages = append(out.Messages, *msg)
			}
		case "system":
			var sb strings.Builder
			sb.WriteString(m.Content)
			for i := range m.Blocks {
				blk := &m.Blocks[i]
				switch blk.Kind {
				case "text":
					sb.WriteString(blk.Text)
				case "image":
					return nil, shared.SystemImageRejected()
				default:
					return nil, shared.UnsupportedPartType(blk.Kind, EndpointPath)
				}
			}
			if sb.Len() > 0 {
				out.Messages = append(out.Messages, ccMessage{Role: "system", Content: sb.String()})
			}
		default:
			return nil, shared.ValidateRole(m.Role, EndpointPath)
		}
	}
	for _, t := range src.Tools {
		if t.Type != "" && t.Type != "custom" {
			if shared.IsSearchTool(t.Type) && src.ToolChoiceKind == shared.ToolChoiceNamed && src.ToolChoiceName == t.Name {
				return nil, shared.UnsupportedHostedChoice()
			}
			continue
		}
		// Claude tools carry no type field; validation cannot reject here.
		schema := shared.ObjectSchema(t.InputSchema)
		out.Tools = append(out.Tools, shared.CCTool{
			Type:     "function",
			Function: shared.CCFunction{Name: t.Name, Description: t.Description, Parameters: schema, Strict: t.Strict},
		})
	}
	format, err := shared.ConvertOutputFormat("messages", "chat-completions", src.OutputConfig, shared.ContextOrNew(ctx))
	if err != nil {
		return nil, err
	}
	out.ResponseFormat = format
	return encode(out), nil
}

// claudeResultText flattens tool_result content (string or text blocks)
// into a tool-message string via the shared kernel; non-text blocks have
// no tool-message equivalent and are rejected descriptively rather than
// dropped.
func claudeResultText(content json.RawMessage) (string, *errclass.Error) {
	return shared.ToolResultText(content, "tool messages carry text only")
}

// claudeUserMessages converts a user turn into Chat Completions messages:
// text/image blocks become one user message (plain string when only
// text), each tool_result block becomes a separate role:"tool" message
// carrying its tool_use_id.
func claudeUserMessages(m *shared.ClaudeMessageRecord) ([]ccMessage, *errclass.Error) {
	var msgs []ccMessage
	var parts []ccContentPart
	flush := func() {
		if len(parts) == 0 {
			return
		}
		var c any = parts
		if len(parts) == 1 && parts[0].Type == "text" {
			c = parts[0].Text
		}
		msgs = append(msgs, ccMessage{Role: "user", Content: c})
		parts = nil
	}
	if m.Content != "" {
		parts = append(parts, ccContentPart{Type: "text", Text: m.Content})
	}
	for i := range m.Blocks {
		blk := &m.Blocks[i]
		switch blk.Kind {
		case "text":
			parts = append(parts, ccContentPart{Type: "text", Text: blk.Text})
		case "document":
			part, err := mediaContentPart(blk.Media)
			if err != nil {
				return nil, err
			}
			parts = append(parts, part)
		case "image":
			parts = append(parts, ccContentPart{Type: "image_url", ImageURL: &imageURLField{URL: blk.URL}})
		case "tool_result":
			text, eErr := claudeResultText(blk.Result)
			if eErr != nil {
				return nil, eErr
			}
			if blk.IsError {
				// Same convention as the Responses-route twin so the
				// flag is never silently erased.
				text = shared.ToolResultErrorPrefix + text
			}
			flush()
			msgs = append(msgs, ccMessage{
				Role: "tool", Content: text, ToolCallID: blk.CallID,
			})
		default:
			return nil, shared.UnsupportedPartType(blk.Kind, EndpointPath)
		}
	}
	flush()
	return msgs, nil
}

// claudeAssistantMessage converts an assistant turn: text blocks join
// the message content, tool_use blocks become tool_calls, and readable
// thinking becomes reasoning_content. Opaque redacted blocks are omitted.
// Returns nil for empty turns.
func claudeAssistantMessage(m *shared.ClaudeMessageRecord) (*ccMessage, *errclass.Error) {
	msg := &ccMessage{Role: "assistant"}
	var sb strings.Builder
	sb.WriteString(m.Content)
	for i := range m.Blocks {
		blk := &m.Blocks[i]
		switch blk.Kind {
		case "text":
			sb.WriteString(blk.Text)
		case "thinking":
			msg.ReasoningContent += blk.Thinking
		case "redacted_thinking":
			// Provider-specific opaque data cannot be replayed as text.
		case "tool_use":
			tc := shared.CCToolCall{ID: blk.CallID, Type: "function"}
			tc.Function.Name = blk.Name
			tc.Function.Arguments = shared.DefaultArgs(string(blk.Input))
			msg.ToolCalls = append(msg.ToolCalls, tc)
		default:
			return nil, shared.UnsupportedPartType(blk.Kind, EndpointPath)
		}
	}
	if sb.Len() > 0 {
		msg.Content = sb.String()
	}
	if msg.Content == nil && len(msg.ToolCalls) == 0 && msg.ReasoningContent == "" {
		return nil, nil
	}
	return msg, nil
}

// ---- Responses (Responses) -> Chat Completions ----
// responsesToChat translates an OpenAI Responses request into a Chat
// Completions request: instructions and system items become
// system messages, message items map by role, function_call items merge
// into assistant tool_calls, function_call_output items become role:"tool"
// messages, reasoning.effort maps to reasoning_effort,
// parallel_tool_calls passes through as-is (the reverse leg forwards the
// same field), and max_output_tokens maps to max_tokens. Original reasoning
// content joins the assistant message containing the following text and calls.
func responsesToChat(upstreamModel string, body []byte, tools *shared.ConversionContext) ([]byte, *errclass.Error) {
	var src shared.ResponsesRequest
	if err := json.Unmarshal(body, &src); err != nil {
		return nil, errclass.Translation("malformed Responses request JSON: " + err.Error())
	}
	out := &ccRequest{
		Model:             upstreamModel,
		Stream:            src.Stream,
		Temperature:       src.Temperature,
		TopP:              src.TopP,
		ParallelToolCalls: src.ParallelToolCalls,
	}
	if src.MaxOutputTokens != nil && *src.MaxOutputTokens > 0 {
		out.MaxTokens = src.MaxOutputTokens
	}
	if src.Reasoning != nil && src.Reasoning.Effort != "" {
		effort := strings.ToLower(strings.TrimSpace(src.Reasoning.Effort))
		if effort != "auto" {
			out.ReasoningEffort = effort
		}
	}
	addSystem := func(text string) {
		if text != "" {
			out.Messages = append(out.Messages, ccMessage{Role: "system", Content: text})
		}
	}
	instr, eErr := src.DecodeInstructions()
	if eErr != nil {
		return nil, eErr
	}
	addSystem(instr)

	items, eErr := tools.Normalize(&src, EndpointPath)
	if eErr != nil {
		return nil, eErr
	}
	choice, choiceErr := shared.HostedChoice(src.ToolChoice, "chat-completions")
	if choiceErr != nil {
		return nil, choiceErr
	}
	kind, tcName, eErr := shared.DecodeToolChoice(choice)
	if eErr != nil {
		return nil, eErr
	}
	applyToolChoiceCC(out, kind, tcName)
	// Chat Completions tool messages cannot carry images. Emit them as a
	// user message after all consecutive tool replies, so parallel calls
	// receive their results before any user message interrupts the turn.
	// Keep each mixed result in order and label its call ID so images remain
	// associated with their source tool and accompanying text.
	var toolImages []ccContentPart
	flushToolImages := func() {
		if len(toolImages) > 0 {
			out.Messages = append(out.Messages, ccMessage{Role: "user", Content: toolImages})
			toolImages = nil
		}
	}
	// Reasoning begins an assistant segment; text and parallel tool calls
	// belong to that segment until a user/tool/system boundary or new reasoning.
	assistant := -1
	for _, item := range items {
		if item.Type == "function_call_output" || item.Type == "custom_tool_call_output" || (item.Type == "message" && item.Role != "assistant") {
			assistant = -1
		}
		switch item.Type {
		case "message":
			if item.Role != "user" {
				parts, err := shared.DecodeStringOrParts(item.Content, EndpointPath)
				if err != nil {
					return nil, err
				}
				for _, p := range parts {
					if p.Media != nil {
						return nil, shared.UnsupportedPartType(p.Media.Kind, EndpointPath)
					}
				}
			}
			content, eErr := respContent(item.Content)
			if eErr != nil {
				return nil, eErr
			}
			switch item.Role {
			case "system", "developer":
				// Flatten to text; images have no system-message
				// equivalent and are rejected descriptively.
				text := ""
				if s, ok := content.(string); ok {
					text = s
				} else if parts, ok := content.([]ccContentPart); ok {
					var sb strings.Builder
					for _, p := range parts {
						if p.Type != "text" {
							return nil, shared.SystemImageRejected()
						}
						sb.WriteString(p.Text)
					}
					text = sb.String()
				}
				if text != "" {
					flushToolImages()
					addSystem(text)
				}
			case "user", "assistant":
				if content != nil {
					flushToolImages()
					if item.Role == "assistant" && assistant >= 0 && out.Messages[assistant].Content == nil {
						out.Messages[assistant].Content = content
					} else {
						out.Messages = append(out.Messages, ccMessage{Role: item.Role, Content: content})
						if item.Role == "assistant" {
							assistant = len(out.Messages) - 1
						}
					}
				}
			default:
				return nil, shared.ValidateRole(item.Role, EndpointPath)
			}
		case "function_call", "custom_tool_call":
			flushToolImages()
			args := item.Arguments
			if args == "" && item.Input != "" {
				args = item.Input
			}
			tc := shared.CCToolCall{ID: item.CallID, Type: "function"}
			tc.Function.Name = item.Name
			tc.Function.Arguments = shared.DefaultArgs(args)
			// Merge consecutive function_call items into one
			// assistant message so multi-call turns round-trip.
			if assistant >= 0 {
				out.Messages[assistant].ToolCalls = append(out.Messages[assistant].ToolCalls, tc)
				continue
			}
			out.Messages = append(out.Messages, ccMessage{
				Role: "assistant", ToolCalls: []shared.CCToolCall{tc},
			})
			assistant = len(out.Messages) - 1
		case "function_call_output", "custom_tool_call_output":
			parts, eErr := shared.DecodeStringOrParts(item.Output, EndpointPath)
			if eErr != nil {
				return nil, eErr
			}
			var text strings.Builder
			var mixed []ccContentPart
			imageCount := 0
			for _, p := range parts {
				if p.Media != nil {
					return nil, shared.UnsupportedPartType(p.Media.Kind, "tool output")
				}
				if p.ImageURL != "" {
					imageCount++
					mixed = append(mixed, ccContentPart{Type: "image_url", ImageURL: &imageURLField{URL: p.ImageURL}})
				} else {
					text.WriteString(p.Text)
					mixed = append(mixed, ccContentPart{Type: "text", Text: p.Text})
				}
			}
			if imageCount > 0 {
				toolImages = append(toolImages, ccContentPart{Type: "text", Text: fmt.Sprintf("Tool output for call_id %q (%d images):", item.CallID, imageCount)})
				toolImages = append(toolImages, mixed...)
			}
			out.Messages = append(out.Messages, ccMessage{
				Role: "tool", Content: text.String(), ToolCallID: item.CallID,
			})
		case "reasoning":
			if assistant >= 0 && (out.Messages[assistant].Content != nil || len(out.Messages[assistant].ToolCalls) > 0) {
				assistant = -1
			}
			if text := shared.ReasoningContent(item.Content); text != "" {
				flushToolImages()
				if assistant >= 0 && out.Messages[assistant].Content == nil && len(out.Messages[assistant].ToolCalls) == 0 {
					out.Messages[assistant].ReasoningContent += text
				} else {
					out.Messages = append(out.Messages, ccMessage{Role: "assistant", ReasoningContent: text})
					assistant = len(out.Messages) - 1
				}
			}
		default:
			return nil, shared.UnsupportedInputItemType(item.Type)
		}
	}
	flushToolImages()

	for _, t := range src.Tools {
		if eErr := shared.FunctionTool(t.Type, EndpointPath); eErr != nil {
			return nil, eErr
		}
		schema := shared.ObjectSchema(t.Parameters)
		out.Tools = append(out.Tools, shared.CCTool{
			Type:     "function",
			Function: shared.CCFunction{Name: t.Name, Description: t.Description, Parameters: schema, Strict: t.Strict},
		})
	}
	format, err := shared.ConvertOutputFormat("responses", "chat-completions", src.Text, tools)
	if err != nil {
		return nil, err
	}
	out.ResponseFormat = format
	return encode(out), nil
}

// respContent normalizes a Responses content field (JSON string or typed
// parts) into Chat Completions content: a plain string, content parts,
// or nil when empty (multimodal preservation). The shared kernel
// owns validation and the empty-input policy — empty-text messages drop
// identically on every route.
func respContent(raw json.RawMessage) (any, *errclass.Error) {
	parts, eErr := shared.DecodeStringOrParts(raw, EndpointPath)
	if eErr != nil {
		return nil, eErr
	}
	switch {
	case len(parts) == 0:
		return nil, nil
	case len(parts) == 1 && parts[0].ImageURL == "" && parts[0].Media == nil:
		return parts[0].Text, nil
	}
	out := make([]ccContentPart, 0, len(parts))
	for _, p := range parts {
		if p.Media != nil {
			part, err := mediaContentPart(p.Media)
			if err != nil {
				return nil, err
			}
			out = append(out, part)
			continue
		}
		if p.ImageURL != "" {
			out = append(out, ccContentPart{Type: "image_url", ImageURL: &imageURLField{URL: p.ImageURL}})
			continue
		}
		out = append(out, ccContentPart{Type: "text", Text: p.Text})
	}
	return out, nil
}

// applyToolChoiceCC maps a normalized tool choice onto the outbound Chat
// Completions request, which natively represents every normalized kind. It is
// infallible: shared.DecodeToolChoice emits exactly the
// closed set below, so the switch covers every possible kind (the
// exhaustiveness test in request_test.go enforces this against
// shared.AllToolChoiceKinds); absent omits the field.
func applyToolChoiceCC(req *ccRequest, kind, name string) {
	switch kind {
	case shared.ToolChoiceAuto:
		req.ToolChoice = json.RawMessage(`"auto"`)
	case shared.ToolChoiceAny:
		req.ToolChoice = json.RawMessage(`"required"`)
	case shared.ToolChoiceNone:
		req.ToolChoice = json.RawMessage(`"none"`)
	case shared.ToolChoiceNamed:
		b, _ := json.Marshal(map[string]any{
			"type": "function", "function": map[string]any{"name": name},
		})
		req.ToolChoice = b
	}
}

func mediaContentPart(m *shared.MediaPart) (ccContentPart, *errclass.Error) {
	part, err := m.OpenAI(false)
	if err != nil {
		return ccContentPart{}, err
	}
	return ccContentPart{Type: part["type"].(string), File: part["file"], InputAudio: part["input_audio"], VideoURL: part["video_url"]}, nil
}
