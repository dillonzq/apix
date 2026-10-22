package messages

import (
	"encoding/json"
	"fmt"
	"sort"
	"strings"
	"unicode/utf8"

	"github.com/dillonzq/apix/internal/errclass"
	"github.com/dillonzq/apix/internal/llm/adapter/shared"
)

// dispatchResponses synthesizes Responses SSE events.
func (sc *StreamConverter) dispatchResponses(etype string, ev *sseEvent, events *[][]byte) (bool, *errclass.Error) {
	switch etype {
	case "message_start":
		sc.msgID = ev.Message.ID
		sc.model = ev.Message.Model
		sc.promptTokens = int64(ev.Message.Usage.InputTokens)
		sc.captureCache(ev.Message.Usage)
		// Lifecycle parity with the Chat Completions route's synthesizer:
		// announce the response before any deltas reference it.
		*events = append(*events, sc.responsesEm().Created())
	case "content_block_start":
		bs := &blockState{kind: ev.ContentBlock.Type, id: ev.ContentBlock.ID, name: ev.ContentBlock.Name, textOffset: sc.textRunes}
		if bs.kind == "text" {
			bs.text.WriteString(ev.ContentBlock.Text)
			sc.textRunes += utf8.RuneCountInString(ev.ContentBlock.Text)
		}
		if bs.kind == "server_tool_use" && bs.name == "web_search" {
			var input struct {
				Query string `json:"query"`
			}
			if json.Unmarshal(ev.ContentBlock.Input, &input) != nil {
				return false, errclass.Translation("invalid web search input")
			}
			bs.search = &shared.HostedSearch{ID: bs.id, Query: input.Query, Status: "in_progress"}
			sc.search[bs.id] = bs
		}
		sc.blocks[int(ev.Index)] = bs
		// Announce each translatable item before its first delta,
		// including visible thinking blocks. Indexes come
		// from the SAME compacted
		// space outputItems() renders, so streamed output_index values
		// always match terminal response.completed.output positions.
		switch bs.kind {
		case "thinking":
			bs.id = fmt.Sprintf("%s_reasoning_%d", sc.msgID, int(ev.Index))
			bs.outIdx = sc.responses.outCount
			sc.responses.outCount++
			*events = append(*events, sc.responsesEm().ReasoningAdded(bs.id, bs.outIdx)...)
			bs.text.WriteString(ev.ContentBlock.Thinking)
			if ev.ContentBlock.Thinking != "" {
				*events = append(*events, sc.responsesEm().ReasoningDelta(bs.id, bs.outIdx, ev.ContentBlock.Thinking))
			}
		case "text":
			if sc.responses.msgIdx >= 0 {
				break // one message item aggregates every text block (canonical shape)
			}
			sc.responses.msgIdx = sc.responses.outCount
			sc.responses.outCount++
			// Native-compatible shape: the announced item carries "id" so
			// downstream output_text.delta item_ids resolve (item identity parity).
			*events = append(*events, sc.responsesEm().ItemAdded(sc.responses.msgIdx, map[string]any{
				"type": "message", "id": sc.msgID, "role": "assistant", "content": []any{},
			}))
		case "server_tool_use":
			if bs.search == nil {
				return false, errclass.Translation("unsupported server tool")
			}
			bs.outIdx = sc.responses.outCount
			sc.responses.outCount++
			*events = append(*events, sc.responsesEm().ItemAdded(bs.outIdx, bs.search.ResponseItem()))
		case "web_search_tool_result":
			if owner := sc.search[ev.ContentBlock.ToolUseID]; owner != nil {
				shared.ReadSearchResults(owner.search, ev.ContentBlock.Content, "messages", "responses", "content.web_search_tool_result", sc.context)
				*events = append(*events, sc.responsesEm().SearchDone(owner.outIdx, *owner.search)...)
				owner.searchDone = true
			} else {
				sc.context.Report("messages", "responses", "content.web_search_tool_result", true)
			}
		case "tool_use":
			if sc.context.IsShell(bs.name) {
				if err := shared.ValidateShellCallID(bs.id); err != nil {
					return false, err
				}
			}
			if sc.context.IsShell(bs.name) && shared.HasContent(ev.ContentBlock.Input) {
				var input map[string]json.RawMessage
				if json.Unmarshal(ev.ContentBlock.Input, &input) != nil {
					return false, errclass.Translation("shell tool input must be an object")
				}
				if len(input) > 0 {
					bs.args.Write(ev.ContentBlock.Input)
				}
			}
			sc.toolsSeen = true
			bs.outIdx = sc.responses.outCount
			sc.responses.outCount++
			*events = append(*events, sc.responsesEm().ItemAdded(bs.outIdx, map[string]any{
				"type": "function_call", "call_id": bs.id, "name": bs.name, "arguments": "",
			}))
		}
		if bs.kind == "text" && ev.ContentBlock.Text != "" {
			*events = append(*events, sc.responsesEm().TextDelta(sc.msgID, sc.responses.msgIdx, ev.ContentBlock.Text))
		}
		sc.emitCitations(bs, ev.ContentBlock.Citations, events)

	case "content_block_delta":
		idx := int(ev.Index)
		bs := sc.blocks[idx]
		switch ev.Delta.Type {
		case "text_delta":
			if bs != nil {
				bs.text.WriteString(ev.Delta.Text)
				sc.textRunes += utf8.RuneCountInString(ev.Delta.Text)
			}
			// Deltas target the single aggregated message item, so later
			// text blocks reference its announced position, never their own.
			dIdx := idx
			if sc.responses.msgIdx >= 0 {
				dIdx = sc.responses.msgIdx
			}
			*events = append(*events, sc.responsesEm().TextDelta(sc.msgID, dIdx, ev.Delta.Text))
		case "citations_delta":
			sc.emitCitations(bs, json.RawMessage("["+string(ev.Delta.Citation)+"]"), events)
		case "input_json_delta":
			if bs != nil && bs.search != nil {
				bs.args.WriteString(ev.Delta.PartialJSON)
				break
			}
			if bs == nil || bs.kind != "tool_use" {
				return false, errclass.Translation("tool arguments fragment without a tool_use block start")
			}
			bs.args.WriteString(ev.Delta.PartialJSON)
			if sc.context.IsShell(bs.name) {
				// Buffer JSON until a complete shell action is available.
			} else if sc.context.IsCustom(bs.name) {
				// Same wrapped-JSON accumulation as the Chat
				// Completions route: raw fragments never reach the
				// client, only newly-unwrapped input tails.
				if tail := shared.CustomInputTail(bs.args.String(), &bs.customEmitted); tail != "" {
					*events = append(*events, sc.responsesEm().InputDelta(bs.id, bs.outIdx, tail))
				}
			} else {
				*events = append(*events, sc.responsesEm().ArgsDelta(bs.id, bs.outIdx, ev.Delta.PartialJSON))
			}
		case "thinking_delta":
			if bs != nil && bs.kind == "thinking" {
				bs.text.WriteString(ev.Delta.Thinking)
				*events = append(*events, sc.responsesEm().ReasoningDelta(bs.id, bs.outIdx, ev.Delta.Thinking))
			}
		}
	case "content_block_stop":
		if bs := sc.blocks[int(ev.Index)]; bs != nil {
			bs.closed = true
			if bs.search != nil && bs.args.Len() > 0 {
				var input struct {
					Query string `json:"query"`
				}
				if json.Unmarshal([]byte(bs.args.String()), &input) != nil {
					return false, errclass.Translation("invalid web search input")
				}
				bs.search.Query = input.Query
				bs.search.Status = "searching"
			}
		}
	case "message_delta":
		if ev.Usage.InputTokens != 0 {
			sc.promptTokens = int64(ev.Usage.InputTokens)
		}
		sc.completionTokens = int64(ev.Usage.OutputTokens) // accumulated, reported on completion
		sc.captureCache(ev.Usage)
		if ev.Delta.StopReason != "" {
			sc.stopReason = ev.Delta.StopReason
		}
	case "message_stop":
		if err := sc.validateShellTools(); err != nil {
			return false, err
		}
		*events = append(*events, sc.responsesCompleted()...)
		return true, nil
	case "error":
		return false, sseError(ev.Error)
	default: // ping and unknown event types are ignored (forward compat)
	}
	return false, nil
}

// responsesCompleted builds the terminal event sequence: item lifecycle
// completion events for every rendered output item followed by
// response.completed — shape parity with the non-stream
// claudeToResponses converter for the terminal payload, with status from
// the Responses vocabulary {completed, incomplete} via the observed
// stop_reason (max_tokens truncation → incomplete); tool calls are
// represented by their function_call output items and never override the
// status. Shared by message_stop and Flush so an early upstream
// close cannot diverge from the normal-path shape.
func (sc *StreamConverter) responsesCompleted() [][]byte {
	status := shared.ResponseStatusFromClaudeStop(sc.stopReason)
	usage := shared.NewResponsesUsageFrom(sc.promptTokens+valueOrZero(sc.cacheRead)+valueOrZero(sc.cacheCreation), sc.completionTokens,
		shared.UsageDetails{CachedTokens: sc.cacheRead, CacheWriteTokens: sc.cacheCreation})
	items := sc.outputItems()
	em := sc.responsesEm()
	out := make([][]byte, 0, len(items)*2+1)
	for idx, item := range items {
		if hosted, ok := item.(map[string]any); ok && hosted["type"] == "web_search_call" {
			id, _ := hosted["id"].(string)
			if bs := sc.search[id]; bs != nil && !bs.searchDone {
				out = append(out, em.SearchDone(idx, *bs.search)...)
				bs.searchDone = true
			}
			continue
		}
		v, ok := item.(shared.RespItem)
		if !ok {
			continue
		}
		switch v.Type {
		case "reasoning":
			out = append(out, em.ReasoningDone(v, idx)...)
		case "message":
			text := responsesMessageText(v.Content)
			_ = text
			out = append(out, em.MessageDone(v, idx)...)
		case "function_call":
			out = append(out, em.ArgsDone(v.CallID, idx, v.Name, v.Arguments), em.ItemDone(idx, v))
		case "custom_tool_call":
			out = append(out, em.InputDone(v.CallID, idx, v.Input), em.ItemDone(idx, v))
		case "shell_call":
			out = append(out, em.ItemDone(idx, v))
		}
	}
	return append(out, em.Completed(status, usage, items))
}

// responsesMessageText extracts the aggregated output_text from a rendered
// message item's content parts.
func responsesMessageText(content []byte) string {
	var parts []struct {
		Text string `json:"text"`
	}
	if err := json.Unmarshal(content, &parts); err != nil {
		return ""
	}
	var b strings.Builder
	for _, p := range parts {
		b.WriteString(p.Text)
	}
	return b.String()
}

// responsesEm binds the shared Responses emitter kernel to the captured
// upstream message identity so this route's frames cannot diverge from the
// sibling Chat-Completions-route synthesizer.
func (sc *StreamConverter) responsesEm() shared.ResponsesEventEmitter {
	return shared.ResponsesEventEmitter{ID: sc.msgID, Model: sc.model, Tools: sc.context}
}

// outputItems materializes the Responses output array from observed
// content blocks in upstream index order through the shared
// OutputAssembler: ALL text blocks aggregate into ONE message item
// positioned at the first text block's slot while tool_use blocks become
// separate function_call items — the canonical native shape, equal to the
// non-stream claudeToResponses converter (mode parity); thinking
// blocks become reasoning summaries.
func (sc *StreamConverter) outputItems() []any {
	indexes := make([]int, 0, len(sc.blocks))
	for i := range sc.blocks {
		indexes = append(indexes, i)
	}
	sort.Ints(indexes)
	oa := shared.NewOutputAssembler(sc.msgID, sc.context)
	for _, i := range indexes {
		bs := sc.blocks[i]
		switch bs.kind {
		case "thinking":
			oa.AppendReasoning(bs.id, bs.text.String())
		case "text":
			oa.ReserveTextSlot()
			oa.AddText(bs.text.String())
		case "server_tool_use":
			if bs.search != nil {
				shared.FinalizeSearch(bs.search, sc.context)
				oa.AppendItem(bs.search.ResponseItem())
			}
		case "tool_use":
			oa.AppendFunctionCall(bs.id, bs.name, shared.DefaultArgs(bs.args.String()))
		}
	}
	oa.AddCitations(sc.citations)
	return oa.Render()
}

// responsesOutput tracks compact item indexes independently of source blocks.
type responsesOutput struct {
	msgIdx   int
	outCount int
}
