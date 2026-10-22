// Non-stream response conversion for the Messages adapter: an upstream Anthropic
// Messages HTTP response is translated back
// into the client protocol — validated passthrough for claude clients,
// Chat Completions / Responses synthesis otherwise. Visible thinking maps
// to reasoning_content or reasoning summaries; opaque metadata is omitted.
package messages

import (
	"bytes"
	"encoding/json"
	"fmt"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/dillonzq/apix/internal/errclass"
	"github.com/dillonzq/apix/internal/llm/adapter/shared"
)

// ---- upstream Anthropic Messages response shapes (decode-only) ----

type claudeBlockIn struct {
	Thinking  string          `json:"thinking"`
	Type      string          `json:"type"` // text | thinking | tool_use | ...
	Text      string          `json:"text"`
	ID        string          `json:"id"`
	Name      string          `json:"name"`
	Input     json.RawMessage `json:"input"`
	Citations json.RawMessage `json:"citations"`
	Content   json.RawMessage `json:"content"`
	ToolUseID string          `json:"tool_use_id"`
}

type claudeUsageIn struct {
	InputTokens   int64  `json:"input_tokens"`
	OutputTokens  int64  `json:"output_tokens"`
	CacheRead     *int64 `json:"cache_read_input_tokens"`
	CacheCreation *int64 `json:"cache_creation_input_tokens"`
}

type claudeResponseIn struct {
	ID         string          `json:"id"`
	Model      string          `json:"model"`
	Content    []claudeBlockIn `json:"content"`
	StopReason string          `json:"stop_reason"`
	Usage      claudeUsageIn   `json:"usage"`
}

// ConvertNonStreamResponse translates an upstream Anthropic Messages HTTP
// response back into the client protocol outputFormat.
// Statuses >= 400 become classified redacted errors; "messages"
// passes through; "chat-completions" and "responses" are converted, preserving
// text, thinking, tool_use, stop reason, and usage. Unknown formats are ClassUnsupported;
// malformed upstream bodies are ClassTranslation.
func ConvertNonStreamResponse(outputFormat string, status int, upstreamBody []byte, tools ...*shared.ConversionContext) ([]byte, *errclass.Error) {
	if status >= 400 {
		return nil, shared.UpstreamStatusError(status, upstreamBody)
	}
	if err := shared.CheckProtocolFailure(upstreamBody, "messages"); err != nil {
		return nil, err
	}
	if outputFormat != "messages" {
		shared.ContextOrNew(tools).RecordResponseLosses("messages", outputFormat, upstreamBody)
	}
	switch outputFormat {
	case "messages":
		if !json.Valid(upstreamBody) {
			return nil, errclass.Translation("malformed Messages response JSON")
		}
		return upstreamBody, nil
	case "chat-completions":
		return claudeToChat(upstreamBody, tools...)
	case "responses":
		if err := shared.ValidateShellResponse(upstreamBody, "messages", shared.ContextOrNew(tools)); err != nil {
			return nil, err
		}
		return claudeToResponses(upstreamBody, tools...)
	default:
		return nil, shared.UnsupportedFormat(outputFormat, EndpointPath)
	}
}

// decodeClaude validates and decodes an upstream non-stream Messages body;
// a missing content array is a translation failure, never silently empty.
func decodeClaude(body []byte) (*claudeResponseIn, *errclass.Error) {
	var resp claudeResponseIn
	if err := json.Unmarshal(body, &resp); err != nil {
		return nil, errclass.Translation("malformed Messages response JSON: " + err.Error())
	}
	if resp.Content == nil {
		return nil, errclass.Translation("Messages response carries no content blocks")
	}
	return &resp, nil
}

// claudeToChat converts a Messages response into a Chat Completions
// response: text blocks concatenate into message.content, tool_use blocks
// become indexed tool_calls with JSON-encoded arguments, stop_reason maps
// to finish_reason, and input/output tokens map to prompt/completion
// usage. Thinking maps to reasoning_content;
// redacted_thinking and signatures have no compatible representation.
func claudeToChat(body []byte, tools ...*shared.ConversionContext) ([]byte, *errclass.Error) {
	resp, eErr := decodeClaude(body)
	if eErr != nil {
		return nil, eErr
	}
	var text strings.Builder
	msg := shared.CCResponseMessage{CCMessage: shared.CCMessage{Role: "assistant"}}
	ctx := shared.ContextOrNew(tools)
	offset := 0
	for _, blk := range resp.Content {
		switch blk.Type {
		case "text":
			msg.Annotations = append(msg.Annotations, shared.CitationValues(shared.DecodeCitations(blk.Citations, "messages", "chat-completions", "content.citations", blk.Text, offset, ctx), "chat-completions")...)
			offset += utf8.RuneCountInString(blk.Text)
			text.WriteString(blk.Text)
		case "tool_use":
			args, _ := json.Marshal(decodeBlockInput(blk.Input))
			tc := shared.CCToolCall{ID: blk.ID, Type: "function"}
			tc.Function.Name = blk.Name
			tc.Function.Arguments = string(args)
			msg.ToolCalls = append(msg.ToolCalls, tc)
		case "thinking":
			msg.ReasoningContent += blk.Thinking
		case "server_tool_use", "web_search_tool_result":
			ctx.Report("messages", "chat-completions", "content."+blk.Type, true)
		case "redacted_thinking":
			// Opaque provider metadata cannot be mapped to readable reasoning.
		default:
			return nil, errclass.Translation(fmt.Sprintf(
				"%s output cannot represent %q content blocks", EndpointPath, blk.Type))
		}
	}
	// Aggregated text ships verbatim (repo-wide verbatim policy, matching
	// OutputAssembler.Render and the streaming legs): no trimming here.
	if text.Len() > 0 {
		msg.Content, _ = json.Marshal(text.String())
	}
	// Observed tool calls outrank the status-derived reason: a terminal
	// max_tokens cannot downgrade tool_calls to length.
	finish := shared.TerminalReason(len(msg.ToolCalls) > 0, "tool_calls",
		shared.ClaudeStopToFinish(resp.StopReason))
	out := shared.CompletionEnvelope(resp.ID, resp.Model, time.Now().Unix(),
		[]map[string]any{{
			"index":         0,
			"message":       msg,
			"finish_reason": finish,
		}},
		shared.CCUsageFrom(resp.Usage.InputTokens+valueOrZero(resp.Usage.CacheRead)+valueOrZero(resp.Usage.CacheCreation), resp.Usage.OutputTokens, shared.UsageDetails{CachedTokens: resp.Usage.CacheRead, CacheWriteTokens: resp.Usage.CacheCreation}))
	b, _ := json.Marshal(out) // composed marshallable types only; cannot fail
	return b, nil
}

// decodeBlockInput normalizes a tool_use input payload: absent/empty
// decodes to an empty object so every tool_call carries valid arguments.
// The raw bytes come from an already-parsed document, so they are always
// valid JSON; UseNumber keeps large integers and literals outside the
// float64 range byte-exact instead of degrading them to null.
func decodeBlockInput(raw json.RawMessage) any {
	if !shared.HasContent(raw) {
		return map[string]any{}
	}
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.UseNumber()
	var v any
	if dec.Decode(&v) != nil {
		// Unreachable for a valid JSON value; preserve the bytes rather than
		// inventing an empty object if it ever happens.
		return raw
	}
	return v
}

// ---- Messages -> Responses (Responses) ----
// claudeToResponses converts a Messages response into a Responses result:
// text blocks aggregate into an assistant output_text message item,
// tool_use blocks become function_call items, stop_reason end_turn maps to
// status completed, and input/output tokens map to input/output usage. Thinking
// becomes reasoning summary items; redacted_thinking
// and signatures have no compatible representation.
func claudeToResponses(body []byte, tools ...*shared.ConversionContext) ([]byte, *errclass.Error) {
	resp, eErr := decodeClaude(body)
	if eErr != nil {
		return nil, eErr
	}
	// Stop reason maps straight onto the Responses status via the shared
	// table (max_tokens -> incomplete, end_turn -> completed); function_call
	// items already carry the tool signal.
	status := shared.ResponseStatusFromClaudeStop(resp.StopReason)
	out := shared.ResponsesResult{
		ID:     resp.ID,
		Object: "response",
		Model:  resp.Model,
		Status: status,
		Output: []any{},
		Usage:  shared.NewResponsesUsageFrom(resp.Usage.InputTokens+valueOrZero(resp.Usage.CacheRead)+valueOrZero(resp.Usage.CacheCreation), resp.Usage.OutputTokens, shared.UsageDetails{CachedTokens: resp.Usage.CacheRead, CacheWriteTokens: resp.Usage.CacheCreation}),
	}
	oa := shared.NewOutputAssembler(resp.ID, tools...)
	ctx := shared.ContextOrNew(tools)
	offset := 0
	search := map[string]*shared.HostedSearch{}
	searchItems := map[string]map[string]any{}

	for idx, blk := range resp.Content {
		switch blk.Type {
		case "text":
			// The shared assembler reserves the first text block's slot so
			// the output array matches the streaming synthesizer's
			// upstream-order emission (stream.go outputItems): identical
			// input must not reorder by mode.
			oa.ReserveTextSlot()
			oa.AddText(blk.Text)
			oa.AddCitations(shared.DecodeCitations(blk.Citations, "messages", "responses", fmt.Sprintf("content[%d].citations", idx), blk.Text, offset, ctx))
			offset += utf8.RuneCountInString(blk.Text)
		case "server_tool_use":
			if blk.Name != "web_search" {
				return nil, errclass.Translation("unsupported server tool")
			}
			var input struct {
				Query string `json:"query"`
			}
			if json.Unmarshal(blk.Input, &input) != nil {
				return nil, errclass.Translation("invalid web search input")
			}
			call := &shared.HostedSearch{ID: blk.ID, Query: input.Query, Status: "in_progress"}
			search[blk.ID] = call
			item := call.ResponseItem()
			searchItems[blk.ID] = item
			oa.AppendItem(item)
		case "web_search_tool_result":
			if call := search[blk.ToolUseID]; call != nil {
				shared.ReadSearchResults(call, blk.Content, "messages", "responses", fmt.Sprintf("content[%d].content", idx), ctx)
				for key, value := range call.ResponseItem() {
					searchItems[blk.ToolUseID][key] = value
				}
			} else {
				ctx.Report("messages", "responses", fmt.Sprintf("content[%d]", idx), true)
			}
		case "tool_use":
			args, _ := json.Marshal(decodeBlockInput(blk.Input))
			if ctx.IsShell(blk.Name) {
				args = blk.Input
			}
			oa.AppendFunctionCall(blk.ID, blk.Name, string(args))
		case "thinking":
			oa.AppendReasoning(fmt.Sprintf("%s_reasoning_%d", resp.ID, idx), blk.Thinking)
		case "redacted_thinking":
			// Opaque provider metadata has no compatible representation.
		default:
			return nil, errclass.Translation(fmt.Sprintf(
				"%s output cannot represent %q content blocks", EndpointPath, blk.Type))
		}
	}
	for id, call := range search {
		shared.FinalizeSearch(call, ctx)
		for key, value := range call.ResponseItem() {
			searchItems[id][key] = value
		}
	}

	out.Output = oa.Render()
	b, _ := json.Marshal(out) // only marshallable composed types; cannot fail
	return b, nil
}

func valueOrZero(v *int64) int64 {
	if v != nil {
		return *v
	}
	return 0
}
