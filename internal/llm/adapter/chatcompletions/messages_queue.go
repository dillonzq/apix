package chatcompletions

import (
	"strings"
	"unicode/utf8"

	"github.com/dillonzq/apix/internal/errclass"
	"github.com/dillonzq/apix/internal/llm/adapter/shared"
)

// messagesOutput owns the target queue, visible block and citation window.
type messagesOutput struct {
	emitter          shared.ClaudeEventEmitter
	context          *shared.ConversionContext
	blocks           shared.MessagesBlocks
	pending          []messagesPending
	messagesText     strings.Builder
	messagesTextBase int
	messagesTextEnd  int
}

func (sc *messagesOutput) close() [][]byte {
	return sc.blocks.Close(sc.emitter)
}

type messagesPending struct {
	kind     string
	text     string
	tool     *streamTool
	offset   int
	citation *shared.Citation
}

// Keep later blocks queued until the current tool finishes. Chat Completions
// supplies one finish signal for all tools, so only the first can stream live.
func (sc *messagesOutput) drain(complete bool) ([][]byte, *errclass.Error) {
	var events [][]byte
	for len(sc.pending) > 0 {
		entry := sc.pending[0]
		if t := entry.tool; t != nil {
			if t.id == "" || t.name == "" {
				if complete {
					return events, errclass.Translation("Chat Completions tool ended without an id or name")
				}
				return events, nil
			}
			if !t.announced {
				events = append(events, sc.close()...)
				var opening [][]byte
				t.blockIndex, opening = sc.blocks.Start(sc.emitter, "tool_use", map[string]any{"id": t.id, "name": t.name, "input": map[string]any{}})
				events = append(events, opening...)
				t.announced = true
			}
			if complete && t.args.Len() == 0 {
				t.args.WriteString("{}")
			}
			if tail := t.args.String()[t.sent:]; tail != "" {
				events = append(events, sc.blocks.Delta(sc.emitter, map[string]any{"type": "input_json_delta", "partial_json": tail})...)
				t.sent = t.args.Len()
			}
			if !complete {
				return events, nil
			}
			events = append(events, sc.close()...)
		} else if entry.kind == "citation" {
			c := entry.citation
			belongs := sc.blocks.Active("text")
			if c.Start != nil && (*c.Start < sc.messagesTextBase || c.End != nil && *c.End > sc.messagesTextEnd) {
				belongs = false
			}
			if c.Text != "" && !strings.Contains(sc.messagesText.String(), c.Text) {
				belongs = false
			}
			if !belongs {
				sc.context.Report("chat-completions", "messages", "delta.annotations.closed_block", false)
			} else {
				events = append(events, sc.blocks.Delta(sc.emitter, map[string]any{"type": "citations_delta", "citation": c.Wire("messages")})...)
			}
		} else if entry.kind == "thinking" {
			if !sc.blocks.Active("thinking") {
				events = append(events, sc.close()...)
				_, opening := sc.blocks.Start(sc.emitter, "thinking", map[string]any{"thinking": ""})
				events = append(events, opening...)
			}
			events = append(events, sc.blocks.Delta(sc.emitter, map[string]any{"type": "thinking_delta", "thinking": entry.text})...)
		} else {
			if !sc.blocks.Active("text") {
				events = append(events, sc.close()...)
				_, opening := sc.blocks.Start(sc.emitter, "text", map[string]any{"text": ""})
				events = append(events, opening...)
				sc.messagesText.Reset()
				sc.messagesTextBase = entry.offset
				sc.messagesTextEnd = entry.offset
			}
			sc.messagesText.WriteString(entry.text)
			sc.messagesTextEnd = entry.offset + utf8.RuneCountInString(entry.text)
			events = append(events, sc.blocks.Delta(sc.emitter, map[string]any{"type": "text_delta", "text": entry.text})...)
		}
		sc.pending[0] = messagesPending{}
		sc.pending = sc.pending[1:]
	}
	return events, nil
}
