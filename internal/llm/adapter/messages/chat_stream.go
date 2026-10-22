package messages

import (
	"encoding/json"
	"unicode/utf8"

	"github.com/dillonzq/apix/internal/errclass"
	"github.com/dillonzq/apix/internal/llm/adapter/shared"
)

// dispatchOpenAI synthesizes chat.completion.chunk frames.
func (sc *StreamConverter) dispatchOpenAI(etype string, ev *sseEvent, events *[][]byte) (bool, *errclass.Error) {
	switch etype {
	case "message_start":
		sc.msgID = ev.Message.ID
		sc.model = ev.Message.Model
		sc.promptTokens = int64(ev.Message.Usage.InputTokens)
		sc.captureCache(ev.Message.Usage)
		sc.emitRole(events)
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
			shared.DecodeProbe(ev.ContentBlock.Input, &input)
			bs.search = &shared.HostedSearch{ID: bs.id, Query: input.Query, Status: "in_progress"}
			sc.search[bs.id] = bs
		}
		if bs.kind == "tool_use" {
			bs.ordinal = sc.chat.toolOrdinal
			sc.chat.toolOrdinal++
			sc.toolsSeen = true
		}
		sc.blocks[int(ev.Index)] = bs
		if bs.search != nil || bs.kind == "web_search_tool_result" {
			sc.context.Report("messages", "chat-completions", "content."+bs.kind, true)
		}
		if bs.kind == "text" && ev.ContentBlock.Text != "" {
			sc.emitRole(events)
			sc.emitChunk(events, map[string]any{"content": ev.ContentBlock.Text}, "", false)
		}
		sc.emitCitations(bs, ev.ContentBlock.Citations, events)

		if bs.kind == "thinking" && ev.ContentBlock.Thinking != "" {
			sc.emitRole(events)
			sc.emitChunk(events, map[string]any{"reasoning_content": ev.ContentBlock.Thinking}, "", false)
		}
	case "content_block_delta":
		bs := sc.blocks[int(ev.Index)]
		switch ev.Delta.Type {
		case "text_delta":
			if bs != nil {
				bs.text.WriteString(ev.Delta.Text)
				sc.textRunes += utf8.RuneCountInString(ev.Delta.Text)
			}
			sc.emitRole(events)
			sc.emitChunk(events, map[string]any{"content": ev.Delta.Text}, "", false)
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
			var tc map[string]any
			if !bs.emitted {
				tc = shared.CCToolCallOpeningEntry(bs.ordinal, bs.id, bs.name, ev.Delta.PartialJSON)
				bs.emitted = true
			} else {
				tc = map[string]any{
					"index":    bs.ordinal,
					"function": map[string]any{"arguments": ev.Delta.PartialJSON},
				}
			}
			sc.emitRole(events)
			sc.emitChunk(events, map[string]any{"tool_calls": []any{tc}}, "", false)
		case "thinking_delta":
			if bs != nil && bs.kind == "thinking" {
				sc.emitRole(events)
				sc.emitChunk(events, map[string]any{"reasoning_content": ev.Delta.Thinking}, "", false)
			}
		}
	case "content_block_stop":
		if bs := sc.blocks[int(ev.Index)]; bs != nil {
			bs.closed = true
		}
	case "message_delta":
		if ev.Usage.InputTokens != 0 {
			sc.promptTokens = int64(ev.Usage.InputTokens)
		}
		sc.completionTokens = int64(ev.Usage.OutputTokens)
		sc.captureCache(ev.Usage)
		// Observed tool calls outrank the status-derived reason: a
		// terminal max_tokens cannot downgrade tool_calls to length.
		if sc.stopReason == "" || sc.terminalSent {
			break
		}
		sc.terminalSent = true
		sc.emitChunk(events, map[string]any{},
			shared.TerminalReason(sc.toolsSeen, "tool_calls",
				shared.ClaudeStopToFinish(ev.Delta.StopReason)), true)
	case "message_stop":
		return true, nil
	case "error":
		return false, sseError(ev.Error)
	default: // ping and unknown event types are ignored (forward compat)
	}
	return false, nil
}

// chatChunks binds the shared chunk kernel to the captured upstream
// message identity so this route's frames cannot diverge from the
// sibling Responses-route synthesizer.
func (sc *StreamConverter) chatChunks() shared.ChatChunkBuilder {
	return shared.ChatChunkBuilder{ID: sc.msgID, Model: sc.model, Created: sc.created}
}

// emitRole guarantees the leading assistant-role chunk openai clients expect.
func (sc *StreamConverter) emitRole(events *[][]byte) {
	if sc.chat.roleSent {
		return
	}
	sc.chat.roleSent = true
	*events = append(*events, sc.chatChunks().RoleChunk())
}

// emitChunk appends one synthesized chat.completion.chunk frame via the
// shared kernel; an empty finish renders null (intermediate chunks) and
// withUsage attaches the accumulated token usage to the terminal frame.
func (sc *StreamConverter) emitChunk(events *[][]byte, delta map[string]any, finish string, withUsage bool) {
	b := sc.chatChunks()
	if finish == "" {
		*events = append(*events, b.Delta(delta))
		return
	}
	var usage map[string]any
	if withUsage {
		usage = shared.CCUsageFrom(sc.promptTokens+valueOrZero(sc.cacheRead)+valueOrZero(sc.cacheCreation), sc.completionTokens,
			shared.UsageDetails{CachedTokens: sc.cacheRead, CacheWriteTokens: sc.cacheCreation})
	}
	*events = append(*events, b.Finish(finish, usage))
}

// chatOutput tracks the target role announcement and compact tool indexes.
type chatOutput struct {
	roleSent    bool
	toolOrdinal int
}
