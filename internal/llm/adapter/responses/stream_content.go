package responses

import (
	"encoding/json"
	"fmt"
	"strings"
	"unicode/utf8"

	"github.com/dillonzq/apix/internal/errclass"
	"github.com/dillonzq/apix/internal/llm/adapter/shared"
)

// Complete part/item/terminal snapshots fill missing tails without replaying
// text already delivered as deltas. Summary and content use separate keys.
func (sc *StreamConverter) reasoningSnapshot(item functionCallItem) [][]byte {
	var events [][]byte
	var summary, content strings.Builder
	contentSent := false
	for _, p := range item.Summary {
		if p.Type == "summary_text" {
			summary.WriteString(p.Text)
		}
	}
	for i, p := range item.Content {
		if p.Type == "reasoning_text" || p.Type == "text" {
			content.WriteString(p.Text)
			if sent := sc.reasoningParts[fmt.Sprintf("%s/content/%d", item.ID, i)]; sent != nil && sent.Len() > 0 {
				contentSent = true
			}
		}
	}
	// Synthesized Responses items expose the same text for display and replay.
	// Reconcile just one channel so the terminal content does not echo summary
	// deltas back into Chat reasoning_content.
	mirrored := summary.Len() > 0 && summary.String() == content.String()
	for i, p := range item.Summary {
		if p.Type == "summary_text" && !(mirrored && contentSent) {
			events = append(events, sc.reasoningPart(fmt.Sprintf("%s/summary/%d", item.ID, i), p.Text, true)...)
		}
	}
	for i, p := range item.Content {
		if (p.Type == "reasoning_text" || p.Type == "text") && !(mirrored && !contentSent) {
			events = append(events, sc.reasoningPart(fmt.Sprintf("%s/content/%d", item.ID, i), p.Text, true)...)
		}
	}
	return events
}

func (sc *StreamConverter) reasoningPart(key, text string, complete bool) [][]byte {
	if sc.reasoningParts == nil {
		sc.reasoningParts = map[string]*strings.Builder{}
	}
	part := sc.reasoningParts[key]
	if part == nil {
		part = &strings.Builder{}
		sc.reasoningParts[key] = part
	}
	if complete {
		previous := part.String()
		if !strings.HasPrefix(text, previous) {
			return nil
		}
		text = text[len(previous):]
	}
	if text == "" {
		return nil
	}
	part.WriteString(text)
	if sc.outputFormat == "chat-completions" {
		return [][]byte{sc.chatChunks().Delta(map[string]any{"reasoning_content": text})}
	}
	return sc.queueMessagesContent("reasoning", text)
}

// Text/refusal done and item/terminal snapshots can carry the full content
// without deltas. Separate per-part accumulation prevents duplicate output.
func (sc *StreamConverter) visiblePart(id string, index int, refused bool, text string, complete bool) ([][]byte, bool, *errclass.Error) {
	key := fmt.Sprintf("%s/%d/%t", id, index, refused)
	owner := sc.textPart(key)
	part := &owner.text
	if complete {
		previous := part.String()
		if !strings.HasPrefix(text, previous) {
			return nil, false, nil
		}
		text = text[len(previous):]
	}
	if text == "" {
		return nil, false, nil
	}
	if owner.base < 0 {
		owner.base = sc.textRunes
	}
	if owner.text.Len() > 0 && sc.lastTextPart != "" && sc.lastTextPart != key {
		owner.interleaved = true
	}
	// Chat keeps refusals separate from content; only Messages renders them
	// in its visible text and therefore includes them in citation offsets.
	if !refused || sc.outputFormat == "messages" {
		sc.textRunes += utf8.RuneCountInString(text)
		sc.lastTextPart = key
	}

	part.WriteString(text)
	if refused {
		sc.refused = true
		if sc.outputFormat == "chat-completions" {
			return [][]byte{sc.chatChunks().Delta(map[string]any{"refusal": text})}, false, nil
		}
	}
	if sc.outputFormat == "messages" {
		sc.messages.queue = append(sc.messages.queue, messagesEmission{kind: "text", text: text, origin: key})
		return sc.drainMessages(), false, nil
	}
	return sc.textDelta(text)
}

func (sc *StreamConverter) messageSnapshot(item functionCallItem) ([][]byte, bool, *errclass.Error) {
	var events [][]byte
	for i, part := range item.Content {
		text := part.Text
		if part.Type == "refusal" {
			text = part.Refusal
		} else if part.Type != "output_text" {
			continue
		}
		more, _, eErr := sc.visiblePart(item.ID, i, part.Type == "refusal", text, true)
		if eErr != nil {
			return nil, false, eErr
		}
		events = append(events, more...)
		var annotations []json.RawMessage
		if json.Unmarshal(part.Annotations, &annotations) != nil {
			if shared.HasContent(part.Annotations) {
				sc.context.Report("responses", sc.outputFormat, fmt.Sprintf("output.%s/%d/false.annotations", item.ID, i), false)
			}
			continue
		}
		for _, raw := range annotations {
			events = append(events, sc.annotation(item.ID, i, raw)...)
		}
	}
	return events, false, nil
}

func (sc *StreamConverter) customArguments(id, delta string) string {
	buffer := sc.native.customArgs[id]
	if buffer == nil {
		buffer = &strings.Builder{}
		sc.native.customArgs[id] = buffer
	}
	buffer.WriteString(delta)
	return buffer.String()
}

func (sc *StreamConverter) textPart(key string) *visibleTextPart {
	if sc.textParts == nil {
		sc.textParts = map[string]*visibleTextPart{}
	}
	part := sc.textParts[key]
	if part == nil {
		part = &visibleTextPart{base: -1, block: -1, citations: map[string]bool{}}
		sc.textParts[key] = part
	}
	return part
}

func (sc *StreamConverter) annotation(id string, index int, raw json.RawMessage) [][]byte {
	key := fmt.Sprintf("%s/%d/false", id, index)
	return sc.annotationPart(key, raw)
}

func (sc *StreamConverter) annotationPart(key string, raw json.RawMessage) [][]byte {

	part := sc.textPart(key)
	if sc.outputFormat == "messages" && part.block < 0 {
		part.pending = append(part.pending, append(json.RawMessage(nil), raw...))
		return nil
	}
	base := part.base
	if base < 0 {
		base = sc.textRunes
	}
	c, ok := shared.ReadCitation(raw, "responses", sc.outputFormat, "output."+key+".annotations", part.text.String(), base, sc.context)
	if !ok {
		return nil
	}
	if part.interleaved {
		c.Start = nil
		c.End = nil
		sc.context.Report("responses", sc.outputFormat, "output."+key+".annotations.offset", false)
	}

	// Source bytes may already include later queued fragments. Wait until the
	// quoted range is actually delivered to this target block before consuming
	// or deduplicating the annotation; retry it after every text fragment.
	if sc.outputFormat == "messages" && sc.messages.blocks.Active("text") && part.block == sc.messages.blocks.Index() && !part.closed {
		incompleteRange := c.End != nil && *c.End-base > part.emittedRunes
		incompleteText := c.Text != "" && !strings.Contains(sc.messages.text.String(), c.Text)
		if incompleteRange || incompleteText {
			part.pending = append(part.pending, append(json.RawMessage(nil), raw...))
			return nil
		}
	}
	encoded, _ := json.Marshal(c.Wire(sc.outputFormat))
	if part.citations[string(encoded)] {
		return nil
	}
	part.citations[string(encoded)] = true
	if sc.outputFormat == "chat-completions" {
		return [][]byte{sc.chatChunks().Delta(map[string]any{"annotations": shared.CitationValues([]shared.Citation{c}, sc.outputFormat)})}
	}
	copyCitation := c
	entry := messagesEmission{kind: "citation", origin: key, citation: &copyCitation}
	if sc.messages.draining {
		return sc.messageCitation(entry)
	}
	sc.messages.queue = append(sc.messages.queue, entry)
	return sc.drainMessages()
}
