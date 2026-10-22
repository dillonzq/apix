package chatcompletions

import (
	"unicode/utf8"

	"github.com/dillonzq/apix/internal/errclass"
	"github.com/dillonzq/apix/internal/llm/adapter/shared"
)

// ---- Chat Completions -> claude (Anthropic Messages) stream ----
// claudeLine synthesizes Messages events from one chunk: the first
// chunk opens message_start, delta.content opens the text content block
// lazily on first non-empty delta (tool-only streams therefore carry
// tool_use from index 0 like non-stream chatToClaude) and becomes
// text_delta (reopening a new text block if tool_use starts
// closed it), tool_calls argument fragments open tool_use
// blocks and stream input_json_delta, finish_reason closes blocks and
// holds message_delta with the mapped stop reason until the next data
// line or [DONE] (OpenAI's standard include_usage trailer follows the
// finish_reason chunk and must land in the terminal event), and [DONE]
// emits message_stop.
func (sc *StreamConverter) claudeLine(line string) ([][]byte, *errclass.Error) {
	data, ok := sseData(line)
	if !ok || data == "" {
		return nil, nil
	}
	if shared.IsSSEDone(data) {
		if sc.started && !sc.finished {
			sc.finished = true
			sc.heldFinish = "stop"
			if sc.toolsSeen {
				sc.heldFinish = "tool_calls"
			}
		}
		events, eErr := sc.messages.drain(true)
		if eErr != nil {
			return events, eErr
		}
		sc.done = true
		events = append(events, sc.messages.close()...)
		events = append(events, sc.claudeTerminal()...)
		return append(events, sc.messages.emitter.MessageStop()), nil
	}
	chunk, eErr := sc.decodeChunk(data)
	if eErr != nil {
		return nil, eErr
	}
	var events [][]byte
	if sc.heldFinish != "" {
		// A post-finish chunk (typically the include_usage trailer)
		// flushes the deferred terminal so it carries the final counts.
		events = append(events, sc.claudeTerminal()...)
	}
	if !sc.started && len(chunk.Choices) > 0 {
		sc.started = true
		input := int64(0)
		if chunk.Usage != nil {
			input = chunk.Usage.PromptTokens
		}
		sc.messages.emitter = shared.NewClaudeEventEmitter(chunk.ID, chunk.Model)
		events = append(events, sc.messages.emitter.MessageStart(input))
	}
	if len(chunk.Choices) == 0 {
		return events, nil
	}
	choice := chunk.Choices[0]
	if reasoning := choice.Delta.Text(); reasoning != "" {
		sc.messages.pending = append(sc.messages.pending, messagesPending{kind: "thinking", text: reasoning})
	}
	if choice.Delta.Refusal != "" {
		sc.refused = true
	}
	if text := choice.Delta.Content + choice.Delta.Refusal; text != "" {
		sc.messages.pending = append(sc.messages.pending, messagesPending{kind: "text", text: text, offset: utf8.RuneCountInString(sc.citationText.String())})
	}
	for _, tc := range choice.Delta.ToolCalls {
		t := sc.tools[tc.Index]
		if t == nil {
			t = &streamTool{}
			sc.tools[tc.Index] = t
			sc.toolOrder = append(sc.toolOrder, tc.Index)
			sc.toolsSeen = true
			sc.messages.pending = append(sc.messages.pending, messagesPending{tool: t})
		}
		if tc.ID != "" && !t.announced {
			t.id = tc.ID
		}
		if tc.Function.Name != "" && !t.announced {
			t.name = mergeToolName(t.name, tc.Function.Name)
		}
		t.args.WriteString(tc.Function.Arguments)
	}
	sc.citationText.WriteString(choice.Delta.Content)
	for _, c := range sc.decodeCitations(choice.Delta.Annotations, "messages") {
		copyCitation := c
		sc.messages.pending = append(sc.messages.pending, messagesPending{kind: "citation", citation: &copyCitation})
	}

	if choice.FinishReason != "" && !sc.finished {
		sc.finished = true
		sc.heldFinish = choice.FinishReason
	}
	pending, eErr := sc.messages.drain(sc.finished)
	events = append(events, pending...)
	if eErr != nil {
		return events, eErr
	}
	if sc.finished {
		events = append(events, sc.messages.close()...)
	}
	return events, nil
}

// claudeTerminal renders the held message_delta exactly once, carrying
// the captured usage when any arrived before emission.
func (sc *StreamConverter) claudeTerminal() [][]byte {
	finish := sc.heldFinish
	sc.heldFinish = ""
	if finish == "" {
		return nil
	}
	sc.terminalSent = true
	// Observed tool calls outrank the status-derived reason, mirroring
	// non-stream chatToClaude.
	reason := shared.TerminalReason(sc.toolsSeen, "tool_use", shared.FinishToClaudeStop(shared.FinishWithRefusal(finish, sc.refused)))
	// Always attach: sibling terminals never omit usage; fields zero when
	// upstream reported none.
	input, output := int64(0), int64(0)
	var cacheRead, cacheWrite *int64
	if sc.usage != nil {
		input = sc.usage.PromptTokens
		output = sc.usage.CompletionTokens
		if sc.usage.PromptDetails != nil {
			cacheRead = sc.usage.PromptDetails.CachedTokens
			cacheWrite = sc.usage.PromptDetails.CacheWriteTokens
		}
	}
	return [][]byte{sc.messages.emitter.MessageDelta(&reason,
		shared.ClaudeUsage(shared.ClampSubtract(input, cacheRead, cacheWrite), output, cacheRead, cacheWrite))}
}
