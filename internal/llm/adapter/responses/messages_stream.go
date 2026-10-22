package responses

import (
	"encoding/json"
	"strings"
	"unicode/utf8"

	"github.com/dillonzq/apix/internal/errclass"
	"github.com/dillonzq/apix/internal/llm/adapter/shared"
)

// Messages requires sequential content blocks. A tool remains at the head
// of the queue until its arguments are complete; later content waits rather
// than closing a block that may still receive argument fragments.
type messagesOutput struct {
	blocks   shared.MessagesBlocks
	tools    map[int]*messagesTool
	queue    []messagesEmission
	text     strings.Builder
	search   map[string]*messagesSearch
	draining bool
}

type messagesEmission struct {
	kind     string
	text     string
	tool     *messagesTool
	origin   string
	citation *shared.Citation
	search   *messagesSearch
}

type messagesTool struct {
	id       string
	name     string
	args     strings.Builder
	index    int
	sent     int
	opened   bool
	closed   bool
	complete bool
}

func (sc *StreamConverter) queueMessagesTool(key int, id, name, delta string, complete bool) ([][]byte, *errclass.Error) {
	if sc.messages.tools == nil {
		sc.messages.tools = map[int]*messagesTool{}
	}
	tool := sc.messages.tools[key]
	if tool == nil {
		tool = &messagesTool{id: id, name: name, index: -1}
		sc.messages.tools[key] = tool
		sc.messages.queue = append(sc.messages.queue, messagesEmission{kind: "tool", tool: tool})
	}
	if !tool.opened {
		// Identity can arrive across several items after parameter deltas/done.
		if id != "" {
			tool.id = id
		}
		if name != "" {
			tool.name = name
		}
	}
	if tool.closed && delta != "" {
		return nil, errclass.Translation("Responses tool arguments changed after tool completion")
	}
	tool.args.WriteString(delta)
	tool.complete = tool.complete || complete
	return sc.drainMessages(), nil
}

func (sc *StreamConverter) queueMessagesContent(kind, text string) [][]byte {
	sc.messages.queue = append(sc.messages.queue, messagesEmission{kind: kind, text: text})
	return sc.drainMessages()
}

func (sc *StreamConverter) drainMessages() [][]byte {
	if sc.messages.draining {
		return nil
	}
	sc.messages.draining = true
	defer func() { sc.messages.draining = false }()
	var events [][]byte
	for len(sc.messages.queue) > 0 {
		entry := sc.messages.queue[0]
		if entry.kind == "tool" {
			tool := entry.tool
			if tool.id == "" || tool.name == "" {
				break
			}
			if !tool.opened {
				events = append(events, sc.closeOpenBlocks()...)
				var opening [][]byte
				tool.index, opening = sc.messages.blocks.Start(sc.claudeChunks(), "tool_use", map[string]any{"id": tool.id, "name": tool.name, "input": map[string]any{}})
				tool.opened = true
				events = append(events, opening...)
			}
			args := tool.args.String()
			if tool.sent < len(args) {
				events = append(events, sc.argsDelta(tool.index, args[tool.sent:]))
				tool.sent = len(args)
			}
			if !tool.complete {
				break
			}
			events = append(events, sc.closeOpenBlocks()...)
			tool.closed = true
		} else if entry.kind == "search" {
			if !entry.search.complete {
				break
			}
			if !entry.search.emitted {
				events = append(events, sc.closeOpenBlocks()...)
				for _, block := range entry.search.call.MessageBlocks() {
					_, opening := sc.messages.blocks.Start(sc.claudeChunks(), block["type"].(string), block)
					events = append(events, opening...)
					events = append(events, sc.messages.blocks.Close(sc.claudeChunks())...)
				}
				entry.search.emitted = true
			}
		} else if entry.kind == "citation" {
			events = append(events, sc.messageCitation(entry)...)

		} else {
			switch entry.kind {
			case "text":
				wasOpen := sc.messages.blocks.Active("text")
				more, _, _ := sc.emitTextDelta(entry.text)
				if !wasOpen {
					sc.messages.text.Reset()
				}
				sc.messages.text.WriteString(entry.text)
				if entry.origin != "" {
					owner := sc.textPart(entry.origin)
					owner.block = sc.messages.blocks.Index()
					owner.closed = false
					owner.emittedRunes += utf8.RuneCountInString(entry.text)
					pending := owner.pending
					owner.pending = nil
					for _, raw := range pending {
						more = append(more, sc.annotationPart(entry.origin, raw)...)
					}
				}
				events = append(events, more...)
			case "reasoning":
				events = append(events, sc.emitReasoningDelta(entry.text)...)
			case "reasoning_stop":
				if sc.messages.blocks.Active("thinking") {
					events = append(events, sc.closeOpenBlocks()...)
				}
			}
		}
		sc.messages.queue[0] = messagesEmission{}
		sc.messages.queue = sc.messages.queue[1:]
	}
	return events
}

func (sc *StreamConverter) textDelta(delta string) ([][]byte, bool, *errclass.Error) {
	if sc.outputFormat == "messages" {
		return sc.queueMessagesContent("text", delta), false, nil
	}
	return sc.emitTextDelta(delta)
}

func (sc *StreamConverter) emitReasoningDelta(text string) [][]byte {
	var events [][]byte
	if !sc.messages.blocks.Active("thinking") {
		events = append(events, sc.closeOpenBlocks()...)
		_, opening := sc.messages.blocks.Start(sc.claudeChunks(), "thinking", map[string]any{"thinking": ""})
		events = append(events, opening...)
	}
	return append(events, sc.messages.blocks.Delta(sc.claudeChunks(), map[string]any{"type": "thinking_delta", "thinking": text})...)
}

type messagesSearch struct {
	call     shared.HostedSearch
	complete bool
	emitted  bool
}

func (sc *StreamConverter) searchItem(item functionCallItem, complete bool) ([][]byte, bool, *errclass.Error) {
	search, ok, err := shared.SearchFromResponse(item.Raw)
	if err != nil {
		return nil, false, err
	}
	if !ok {
		sc.context.Report("responses", sc.outputFormat, "output.web_search_call.action", true)
		return nil, false, nil
	}
	if sc.outputFormat == "chat-completions" {
		sc.context.Report("responses", sc.outputFormat, "output.web_search_call", true)
		return nil, false, nil
	}
	if search.ID == "" {
		return nil, false, errclass.Translation("web search item has no identity")
	}
	if sc.messages.search == nil {
		sc.messages.search = map[string]*messagesSearch{}
	}
	state := sc.messages.search[search.ID]
	if state == nil {
		state = &messagesSearch{call: search}
		sc.messages.search[search.ID] = state
		sc.messages.queue = append(sc.messages.queue, messagesEmission{kind: "search", search: state})
	}
	if state.emitted {
		before, _ := json.Marshal(state.call.ResponseItem())
		after, _ := json.Marshal(search.ResponseItem())
		if string(before) != string(after) {
			sc.context.Report("responses", "messages", "output.web_search_call.snapshot", false)
		}
		return nil, false, nil
	}
	if search.Query == "" {
		search.Query = state.call.Query
	}
	if len(search.Sources) == 0 {
		search.Sources = state.call.Sources
	}
	state.call = search
	state.complete = state.complete || complete
	return sc.drainMessages(), false, nil
}

// Citation delivery is shared by queued events and annotations received before
// their text. Pending annotations are emitted immediately after that text,
// before the queue can open a later tool or content block.
func (sc *StreamConverter) messageCitation(entry messagesEmission) [][]byte {
	owner := sc.textPart(entry.origin)
	if !sc.messages.blocks.Active("text") || owner.block != sc.messages.blocks.Index() || owner.closed || entry.citation.Text != "" && !strings.Contains(sc.messages.text.String(), entry.citation.Text) {
		sc.context.Report("responses", "messages", "output."+entry.origin+".annotations.closed_block", false)
		return nil
	}
	return [][]byte{sc.claudeChunks().ContentBlockDelta(owner.block, map[string]any{"type": "citations_delta", "citation": entry.citation.Wire("messages")})}
}
