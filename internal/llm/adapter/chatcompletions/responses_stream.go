package chatcompletions

import (
	"github.com/dillonzq/apix/internal/errclass"
	"github.com/dillonzq/apix/internal/llm/adapter/shared"
)

// ---- Chat Completions -> Responses (Responses) stream ----
// responsesLine synthesizes Responses events: delta.content becomes
// response.output_text.delta, tool_calls argument fragments become
// response.function_call_arguments.delta keyed by item_id, and
// finish_reason holds response.completed — emitted at the next data
// line or [DONE] with prompt/completion tokens mapped to input/output
// usage (the standard include_usage trailer follows the finish_reason
// chunk); [DONE] terminates the stream.
func (sc *StreamConverter) responsesLine(line string) ([][]byte, *errclass.Error) {
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
		if err := sc.validateTools(); err != nil {
			return nil, err
		}
		sc.done = true
		return sc.responsesTerminal(), nil
	}
	chunk, eErr := sc.decodeChunk(data)
	if eErr != nil {
		return nil, eErr
	}
	var events [][]byte
	if sc.heldFinish != "" {
		// A post-finish chunk (typically the include_usage trailer)
		// flushes the deferred terminal so it carries the final counts.
		events = append(events, sc.responsesTerminal()...)
	}
	if !sc.started && len(chunk.Choices) > 0 {
		sc.started = true
		sc.id = chunk.ID
		sc.model = chunk.Model
		// Lifecycle parity with the Responses protocol: announce the
		// response before any deltas reference it.
		events = append(events, sc.responsesEm().Created())
	}
	if len(chunk.Choices) == 0 {
		return events, nil
	}
	choice := chunk.Choices[0]
	if reasoning := choice.Delta.Text(); reasoning != "" {
		if sc.responses.reasonIndex < 0 {
			sc.responses.reasonIndex = sc.responses.nextIndex
			sc.responses.nextIndex++
			events = append(events, sc.responsesEm().ReasoningAdded(sc.id+"_reasoning", sc.responses.reasonIndex)...)
		}
		sc.responses.reasoning.WriteString(reasoning)
		events = append(events, sc.responsesEm().ReasoningDelta(sc.id+"_reasoning", sc.responses.reasonIndex, reasoning))
	}
	if choice.Delta.Content != "" || choice.Delta.Refusal != "" {
		if sc.responses.msgIndex < 0 {
			sc.responses.msgIndex = sc.responses.nextIndex
			sc.responses.nextIndex++
			events = append(events, sc.responsesEm().ItemAdded(sc.responses.msgIndex, map[string]any{
				"type": "message", "role": "assistant", "id": sc.id, "content": []any{},
			}))
		}
		if choice.Delta.Content != "" {
			sc.responses.text.WriteString(choice.Delta.Content)
			partIndex := 0
			if sc.responses.refusalFirst {
				partIndex = 1
			}
			events = append(events, sc.responsesEm().TextDeltaAt(sc.id, sc.responses.msgIndex, partIndex, choice.Delta.Content))
		}
		if choice.Delta.Refusal != "" {
			if sc.responses.text.Len() == 0 {
				sc.responses.refusalFirst = true
			}
			sc.responses.refusal.WriteString(choice.Delta.Refusal)
			partIndex := 0
			if !sc.responses.refusalFirst {
				partIndex = 1
			}
			events = append(events, sc.responsesEm().RefusalDelta(sc.id, sc.responses.msgIndex, partIndex, choice.Delta.Refusal))
		}
	}
	sc.citationText.WriteString(choice.Delta.Content)
	sc.responses.citations = append(sc.responses.citations, sc.decodeCitations(choice.Delta.Annotations, "responses")...)
	if sc.responses.msgIndex >= 0 {
		for sc.responses.citationSent < len(sc.responses.citations) {
			i := sc.responses.citationSent
			sc.responses.citationSent++
			contentIndex := 0
			if sc.responses.refusalFirst {
				contentIndex = 1
			}
			events = append(events, sc.responsesEm().AnnotationAdded(sc.id, sc.responses.msgIndex, contentIndex, i, sc.responses.citations[i]))
		}
	}

	for _, tc := range choice.Delta.ToolCalls {
		t := sc.tools[tc.Index]
		if t == nil {
			t = &streamTool{blockIndex: sc.responses.nextIndex}
			sc.responses.nextIndex++
			sc.tools[tc.Index] = t
			sc.toolsSeen = true
			sc.toolOrder = append(sc.toolOrder, tc.Index)
		}
		if !t.announced {
			if tc.ID != "" {
				t.id = tc.ID
			}
			if tc.Function.Name != "" {
				t.name = mergeToolName(t.name, tc.Function.Name)
			}
		}
		t.args.WriteString(tc.Function.Arguments)
		if t.id == "" || t.name == "" {
			continue
		}
		if sc.context.IsShell(t.name) {
			if err := shared.ValidateShellCallID(t.id); err != nil {
				return events, err
			}
		}
		if !t.announced {
			t.announced = true
			events = append(events, sc.responsesEm().ItemAdded(t.blockIndex, map[string]any{"type": "function_call", "call_id": t.id, "name": t.name, "arguments": ""}))
		}
		if sc.context.IsShell(t.name) {
			// Shell actions are emitted as a complete output item.
		} else if sc.context.IsCustom(t.name) {
			if tail := shared.CustomInputTail(t.args.String(), &t.customEmitted); tail != "" {
				events = append(events, sc.responsesEm().InputDelta(t.id, t.blockIndex, tail))
			}
		} else if tail := t.args.String()[t.sent:]; tail != "" {
			events = append(events, sc.responsesEm().ArgsDelta(t.id, t.blockIndex, tail))
			t.sent = t.args.Len()
		}
	}
	if choice.FinishReason != "" && !sc.finished {
		if err := sc.validateTools(); err != nil {
			return events, err
		}
		sc.finished = true
		sc.heldFinish = choice.FinishReason
	}
	return events, nil
}

// responsesEm binds the shared Responses emitter kernel to the captured
// upstream chunk identity so this route's frames cannot diverge from the
// sibling Messages-route synthesizer.
func (sc *StreamConverter) responsesEm() shared.ResponsesEventEmitter {
	return shared.ResponsesEventEmitter{ID: sc.id, Model: sc.model, Tools: sc.context}
}

// responsesTerminal renders the held response.completed exactly once,
// carrying the captured usage when any arrived before emission.
func (sc *StreamConverter) responsesTerminal() [][]byte {
	finish := sc.heldFinish
	sc.heldFinish = ""
	if finish == "" {
		return nil
	}
	// Status derives positionally from finish alone: length→incomplete,
	// else completed. Tool calls are represented by output items, not
	// status vocabulary (Messages-route parity).
	status := shared.ResponseStatusFromCCFinish(finish)
	// Shared assembler (sibling parity): the terminal payload
	// carries the output items fed in ARRIVAL order so Render() reproduces
	// the streamed output_item.added indexes exactly — the message slot is
	// pinned at sc.responses.msgIndex (assigned at the first text delta, after any
	// tools-first announcements), never displacing already-announced
	// function_call items; this mirrors the Messages-route invariant that
	// terminal output order equals announcement order (W4 pin).
	oa := shared.NewOutputAssembler(sc.id, sc.context)
	reserved := false
	for _, idx := range sc.toolOrder {
		t := sc.tools[idx]
		if sc.responses.msgIndex >= 0 && !reserved && t.blockIndex > sc.responses.msgIndex {
			oa.ReserveTextSlot()
			reserved = true
		}
		oa.AppendFunctionCall(t.id, t.name, shared.DefaultArgs(t.args.String()))
	}
	if sc.responses.msgIndex >= 0 && !reserved {
		oa.ReserveTextSlot()
	}
	if sc.responses.refusalFirst {
		oa.AddRefusal(sc.responses.refusal.String())
		oa.AddText(sc.responses.text.String())
	} else {
		oa.AddText(sc.responses.text.String())
		oa.AddRefusal(sc.responses.refusal.String())
	}
	// Always attach: zero-valued fields when upstream sent none.
	input, outputTokens := int64(0), int64(0)
	if sc.usage != nil {
		input = sc.usage.PromptTokens
		outputTokens = sc.usage.CompletionTokens
	}
	var details shared.UsageDetails
	if sc.usage != nil {
		if sc.usage.PromptDetails != nil {
			details.CachedTokens = sc.usage.PromptDetails.CachedTokens
			details.CacheWriteTokens = sc.usage.PromptDetails.CacheWriteTokens
		}
		if sc.usage.CompletionDetails != nil {
			details.ReasoningTokens = sc.usage.CompletionDetails.ReasoningTokens
		}
	}
	usage := shared.NewResponsesUsageFrom(input, outputTokens, details)
	oa.AddCitations(sc.responses.citations)
	items := oa.Render()
	if sc.responses.reasonIndex >= 0 {
		item := shared.ReasoningItem(sc.id+"_reasoning", sc.responses.reasoning.String())
		items = append(items, nil)
		copy(items[sc.responses.reasonIndex+1:], items[sc.responses.reasonIndex:])
		items[sc.responses.reasonIndex] = item
	}
	em := sc.responsesEm()
	var events [][]byte
	for idx, item := range items {
		v, ok := item.(shared.RespItem)
		if !ok {
			continue
		}
		switch v.Type {
		case "reasoning":
			events = append(events, em.ReasoningDone(v, idx)...)
		case "message":
			events = append(events, em.MessageDone(v, idx)...)
		case "function_call":
			events = append(events, em.ArgsDone(v.CallID, idx, v.Name, v.Arguments))
			events = append(events, em.ItemDone(idx, v))
		case "custom_tool_call":
			events = append(events, em.InputDone(v.CallID, idx, v.Input))
			events = append(events, em.ItemDone(idx, v))
		case "shell_call":
			events = append(events, em.ItemDone(idx, v))
		}
	}
	return append(events, em.Completed(status, usage, items))
}
