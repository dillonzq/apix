// Non-stream response conversion for the Responses adapter: an upstream
// /v1/responses HTTP result is translated back into
// the client protocol — validated passthrough for Responses clients,
// Chat Completions / Messages synthesis otherwise.
package responses

import (
	"encoding/json"
	"fmt"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/dillonzq/apix/internal/errclass"
	"github.com/dillonzq/apix/internal/llm/adapter/shared"
)

// ---- upstream Responses result shapes (decode-only) ----

type respTextPart struct {
	Type        string                     `json:"type"` // output_text | ...
	Text        string                     `json:"text"`
	Refusal     string                     `json:"refusal"`
	Raw         map[string]json.RawMessage `json:"-"`
	Annotations json.RawMessage            `json:"annotations"`
}

func (part *respTextPart) UnmarshalJSON(data []byte) error {
	type plain respTextPart
	if err := json.Unmarshal(data, (*plain)(part)); err != nil {
		return err
	}
	return json.Unmarshal(data, &part.Raw)
}
func (part respTextPart) unsupported() *errclass.Error {
	if part.Type == "output_text" || part.Type == "refusal" {
		return nil
	}
	for key, raw := range part.Raw {
		if key != "type" && string(raw) != "null" && string(raw) != `""` && string(raw) != "[]" && string(raw) != "{}" {
			return errclass.Translation("target protocol cannot represent Responses message content")
		}
	}
	return nil
}

type respOutputItem struct {
	Summary   []respTextPart             `json:"summary"`
	Type      string                     `json:"type"` // message | function_call | ...
	Raw       map[string]json.RawMessage `json:"-"`
	CallID    string                     `json:"call_id"`
	Name      string                     `json:"name"`
	Arguments string                     `json:"arguments"`
	Input     string                     `json:"input"`
	Content   []respTextPart             `json:"content"`
}

func (item *respOutputItem) UnmarshalJSON(data []byte) error {
	type plain respOutputItem
	if err := json.Unmarshal(data, (*plain)(item)); err != nil {
		return err
	}
	return json.Unmarshal(data, &item.Raw)
}

type respUsageIn struct {
	InputTokens  int64 `json:"input_tokens"`
	OutputTokens int64 `json:"output_tokens"`
	InputDetails *struct {
		CachedTokens     *int64 `json:"cached_tokens"`
		CacheWriteTokens *int64 `json:"cache_write_tokens"`
	} `json:"input_tokens_details"`
	OutputDetails *struct {
		ReasoningTokens *int64 `json:"reasoning_tokens"`
	} `json:"output_tokens_details"`
}

type respResultIn struct {
	ID     string                `json:"id"`
	Model  string                `json:"model"`
	Status string                `json:"status"`
	Error  *shared.ProtocolError `json:"error"`
	Output []respOutputItem      `json:"output"`
	Usage  respUsageIn           `json:"usage"`
}

// ConvertNonStreamResponse translates an upstream /v1/responses HTTP
// result back into the client protocol outputFormat.
// Statuses >= 400 become classified redacted errors; "responses"
// passes through; "chat-completions" and "messages" are converted, preserving text,
// function calls, terminal status, and usage. Unknown formats are
// ClassUnsupported; malformed upstream bodies are ClassTranslation.
func ConvertNonStreamResponse(outputFormat string, status int, upstreamBody []byte, tools ...*shared.ConversionContext) ([]byte, *errclass.Error) {
	if status >= 400 {
		return nil, shared.UpstreamStatusError(status, upstreamBody)
	}
	if err := shared.CheckProtocolFailure(upstreamBody, "responses"); err != nil {
		return nil, err
	}
	if outputFormat != "responses" {
		shared.ContextOrNew(tools).RecordResponseLosses("responses", outputFormat, upstreamBody)
	}
	switch outputFormat {
	case "responses":
		if !json.Valid(upstreamBody) {
			return nil, errclass.Translation("malformed Responses response JSON")
		}
		if err := shared.ValidateShellResponse(upstreamBody, "responses", shared.ContextOrNew(tools)); err != nil {
			return nil, err
		}
		return restoreCustomPassthrough(upstreamBody, shared.ContextOrNew(tools)), nil
	case "chat-completions":
		return responsesToChat(upstreamBody, tools...)
	case "messages":
		return responsesToClaude(upstreamBody, tools...)
	default:
		return nil, shared.UnsupportedFormat(outputFormat, EndpointPath)
	}
}

// restoreCustomPassthrough rewrites function_call output items for custom
// tools into their custom_tool_call shape on the native passthrough path;
// bodies without output arrays or custom tools pass through untouched.
func restoreCustomPassthrough(body []byte, rt *shared.ConversionContext) []byte {
	return shared.RestoreResponseTools(body, rt)
}

// decodeResp validates and decodes an upstream non-stream Responses body;
// a missing output array is a translation failure, never silently empty.
func decodeResp(body []byte) (*respResultIn, *errclass.Error) {
	var resp respResultIn
	if err := json.Unmarshal(body, &resp); err != nil {
		return nil, errclass.Translation("malformed Responses response JSON: " + err.Error())
	}
	if resp.Status == "failed" || resp.Status == "cancelled" || resp.Error != nil {
		return nil, shared.ProtocolFailure(resp.Error, "Responses response "+resp.Status)
	}
	if resp.Status != "" && resp.Status != "completed" && resp.Status != "incomplete" {
		return nil, errclass.Translation("Responses response has no terminal status")
	}
	if resp.Output == nil {
		return nil, errclass.Translation("Responses result carries no output items")
	}
	return &resp, nil
}

// ccFinishFromStatus derives a Chat Completions finish reason from the
// result status and emitted items via the shared precedence rule: any
// function_call means tool_calls (incomplete cannot downgrade it);
// otherwise incomplete maps to length, else stop.
func ccFinishFromStatus(status string, sawToolCall bool) string {
	statusFinish := shared.CCFinishFromResponseStatus(status)
	return shared.TerminalReason(sawToolCall, "tool_calls", statusFinish)
}

// respItemCarriesPayload reports whether an unrecognized output item
// carries real content the target protocol cannot represent; purely
// informational items are omittable per the compatibility policy.
func respItemCarriesPayload(item respOutputItem) bool {
	for key, raw := range item.Raw {
		switch key {
		case "type", "id", "status":
			continue
		}
		if string(raw) != "null" && string(raw) != `""` && string(raw) != "[]" && string(raw) != "{}" {
			return true
		}
	}
	return false
}

// responsesToChat converts a Responses result into a Chat Completions
// response: output_text parts concatenate into message.content,
// function_call items become indexed tool_calls with their arguments
// verbatim, status maps to finish_reason, and input/output tokens map to
// prompt/completion usage. Visible reasoning maps to
// reasoning_content. Unsupported items carrying payload fail descriptively.
func responsesToChat(body []byte, tools ...*shared.ConversionContext) ([]byte, *errclass.Error) {
	resp, eErr := decodeResp(body)
	if eErr != nil {
		return nil, eErr
	}
	msg := shared.CCResponseMessage{CCMessage: shared.CCMessage{Role: "assistant"}}
	var text strings.Builder
	sawToolCall := false
	ctx := shared.ContextOrNew(tools)
	offset := 0
	for _, item := range resp.Output {
		switch item.Type {
		case "message":
			for _, part := range item.Content {
				if err := part.unsupported(); err != nil {
					return nil, err
				}
				if part.Type == "output_text" {
					msg.Annotations = append(msg.Annotations, shared.CitationValues(shared.DecodeCitations(part.Annotations, "responses", "chat-completions", "output.content.annotations", part.Text, offset, ctx), "chat-completions")...)
					offset += utf8.RuneCountInString(part.Text)
					text.WriteString(part.Text)
				} else if part.Type == "refusal" {
					msg.Refusal += part.Refusal
				}
			}
		case "web_search_call":
			ctx.Report("responses", "chat-completions", "output.web_search_call", true)
		case "function_call", "custom_tool_call":
			tc := shared.CCToolCall{ID: item.CallID, Type: "function"}
			tc.Function.Name = item.Name
			tc.Function.Arguments = shared.DefaultArgs(item.Arguments)
			if item.Type == "custom_tool_call" {
				raw, _ := json.Marshal(map[string]string{"input": item.Input})
				tc.Function.Arguments = string(raw)
			}
			msg.ToolCalls = append(msg.ToolCalls, tc)
			sawToolCall = true
		case "reasoning":
			msg.ReasoningContent += item.reasoningText()
		default:
			if respItemCarriesPayload(item) {
				return nil, errclass.Translation(fmt.Sprintf(
					"%s output cannot represent %q output items", EndpointPath, item.Type))
			}
		}
	}
	if t := text.String(); t != "" {
		raw, _ := json.Marshal(t)
		msg.Content = raw
	}
	// Shared kernel computes total_tokens as prompt+completion so the
	// majority rule cannot diverge from the sibling mappers.
	out := shared.CompletionEnvelope(resp.ID, resp.Model, time.Now().Unix(),
		[]map[string]any{{
			"index":         0,
			"message":       msg,
			"finish_reason": shared.FinishWithRefusal(ccFinishFromStatus(resp.Status, sawToolCall), msg.Refusal != ""),
		}},
		shared.CCUsageFrom(resp.Usage.InputTokens, resp.Usage.OutputTokens, shared.UsageDetails{
			CachedTokens: func() *int64 {
				if resp.Usage.InputDetails != nil {
					return resp.Usage.InputDetails.CachedTokens
				}
				return nil
			}(),
			CacheWriteTokens: func() *int64 {
				if resp.Usage.InputDetails != nil {
					return resp.Usage.InputDetails.CacheWriteTokens
				}
				return nil
			}(),
			ReasoningTokens: func() *int64 {
				if resp.Usage.OutputDetails != nil {
					return resp.Usage.OutputDetails.ReasoningTokens
				}
				return nil
			}(),
		}))
	b, _ := json.Marshal(out) // composed marshallable types only; cannot fail
	return b, nil
}

// responsesToClaude converts a Responses result into a Messages response
// via the shared Claude-result kernel: output_text parts aggregate into
// text blocks (empty parts dropped, matching the Chat Completions
// route's block builder), function_call items become tool_use blocks with
// arguments decoded as input, completed status maps to stop_reason
// end_turn (tool_use when tools were called, max_tokens when incomplete),
// and input/output tokens map to input/output usage. Visible
// reasoning maps to thinking blocks without fabricated signatures.
// Unsupported items carrying payload fail descriptively.
func responsesToClaude(body []byte, tools ...*shared.ConversionContext) ([]byte, *errclass.Error) {
	resp, eErr := decodeResp(body)
	if eErr != nil {
		return nil, eErr
	}
	sawToolCall := false
	refused := false
	blocks := make([]map[string]any, 0, len(resp.Output))
	ctx := shared.ContextOrNew(tools)
	for _, item := range resp.Output {
		switch item.Type {
		case "message":
			for _, part := range item.Content {
				if err := part.unsupported(); err != nil {
					return nil, err
				}
				// Uniform DROP policy (route parity): empty output_text
				// parts ship no dead text block.
				if part.Type == "refusal" {
					refused = true
					if part.Refusal != "" {
						blocks = append(blocks, map[string]any{"type": "text", "text": part.Refusal})
					}
				}
				if part.Type == "output_text" && part.Text != "" {
					textBlock := map[string]any{"type": "text", "text": part.Text}
					if citations := shared.DecodeCitations(part.Annotations, "responses", "messages", "output.content.annotations", part.Text, 0, ctx); len(citations) > 0 {
						textBlock["citations"] = shared.CitationValues(citations, "messages")
					}
					blocks = append(blocks, textBlock)
				}
			}
		case "web_search_call":
			search, ok, err := shared.SearchFromResponse(item.Raw)
			if err != nil {
				return nil, err
			}
			if ok {
				blocks = append(blocks, search.MessageBlocks()...)
			} else {
				ctx.Report("responses", "messages", "output.web_search_call.action", true)
			}
		case "function_call", "custom_tool_call":
			args := item.Arguments
			if item.Type == "custom_tool_call" {
				raw, _ := json.Marshal(map[string]string{"input": item.Input})
				args = string(raw)
			}
			input, eErr := shared.DecodeArgs(args)
			if eErr != nil {
				return nil, eErr
			}
			blocks = append(blocks, map[string]any{
				"type": "tool_use", "id": item.CallID, "name": item.Name, "input": input,
			})
			sawToolCall = true
		case "reasoning":
			if text := item.reasoningText(); text != "" {
				blocks = append(blocks, map[string]any{"type": "thinking", "thinking": text})
			}
		default:
			if respItemCarriesPayload(item) {
				return nil, errclass.Translation(fmt.Sprintf(
					"%s output cannot represent %q output items", EndpointPath, item.Type))
			}
		}
	}
	statusStop := shared.FinishToClaudeStop(shared.FinishWithRefusal(shared.CCFinishFromResponseStatus(resp.Status), refused))
	stop := shared.TerminalReason(sawToolCall, "tool_use", statusStop)
	var cacheRead, cacheWrite *int64
	if resp.Usage.InputDetails != nil {
		cacheRead = resp.Usage.InputDetails.CachedTokens
		cacheWrite = resp.Usage.InputDetails.CacheWriteTokens
	}
	b, _ := json.Marshal(shared.NewClaudeResult(resp.ID, resp.Model, stop,
		shared.ClampSubtract(resp.Usage.InputTokens, cacheRead, cacheWrite), resp.Usage.OutputTokens,
		cacheRead, cacheWrite, blocks))
	return b, nil // composed marshallable types only; cannot fail
}

// reasoningText retains summaries and visible reasoning_text/text content
// emitted by compatible providers.
func (item respOutputItem) reasoningText() string {
	var summary, content strings.Builder
	for _, part := range item.Summary {
		if part.Type == "summary_text" {
			summary.WriteString(part.Text)
		}
	}
	for _, part := range item.Content {
		if part.Type == "reasoning_text" || part.Type == "text" {
			content.WriteString(part.Text)
		}
	}
	if summary.String() == content.String() {
		return content.String()
	}
	return summary.String() + content.String()
}
