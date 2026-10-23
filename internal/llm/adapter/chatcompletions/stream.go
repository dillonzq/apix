package chatcompletions

import (
	"bytes"
	"encoding/json"
	"strconv"
	"strings"

	"github.com/dillonzq/apix/internal/errclass"
	"github.com/dillonzq/apix/internal/llm/adapter/shared"
)

// StreamConverter incrementally converts an upstream Chat Completions
// SSE stream into the client protocol's stream: "chat-completions"
// frames pass through verbatim, "messages" synthesizes Anthropic Messages
// events, "responses" synthesizes Responses events. Partial SSE
// lines are buffered across Feed calls; tool-call argument fragments are
// accumulated per tool_calls index until the stream terminates.
//
// A converter is safe for SEQUENTIAL Feed calls only — it is not
// goroutine-safe; use one converter per upstream stream. Synthesis
// follows choices[0] (upstream serves n=1 routes).
type StreamConverter struct {
	messages     messagesOutput
	responses    responsesOutput
	outputFormat string
	lineBuf      []byte // partial SSE line carried across Feed calls
	done         bool
	ending       bool

	started      bool // message_start / first chunk seen
	id           string
	model        string
	tools        map[int64]*streamTool
	toolOrder    []int64 // upstream tool_call indices in first-arrival order
	toolsSeen    bool    // any tool_calls entry observed (terminal-reason precedence)
	usage        *ccUsage
	finished     bool   // finish_reason processed
	heldFinish   string // finish_reason awaiting terminal emission (flushed on the next data line or [DONE], so the standard include_usage trailer lands in the terminal event; Flush covers close-without-[DONE])
	terminalSent bool   // claudeTerminal already emitted message_delta (Flush must still close with message_stop)
	flushed      bool   // Flush already ran (one-shot guard)
	refused      bool
	context      *shared.ConversionContext
	citationText strings.Builder
	citationKeys map[string]bool
}

// streamTool accumulates one upstream tool_calls index; args collects
// argument fragments so the terminal response.completed output carries the
// complete call.
type streamTool struct {
	blockIndex    int
	id            string
	name          string
	args          strings.Builder
	announced     bool
	sent          int
	customEmitted int // custom_tool_call_input bytes already emitted as deltas
}

// NewStreamConverter returns a converter translating Chat Completions
// SSE into outputFormat's stream shape.
func NewStreamConverter(outputFormat string, tools ...*shared.ConversionContext) *StreamConverter {
	sc := &StreamConverter{
		outputFormat: outputFormat,
		tools:        map[int64]*streamTool{},
	}
	sc.responses.msgIndex = -1
	sc.responses.reasonIndex = -1
	if len(tools) > 0 && tools[0] != nil {
		sc.context = tools[0]
	}
	sc.messages.context = sc.context
	return sc
}

// Feed consumes one upstream chunk (any split of the byte stream),
// returning the client-protocol events completed by this chunk, whether
// the stream is done ([DONE] seen), and a classified error for
// malformed chunks. Errors carry at most an 80-character
// redacted snippet — never the full upstream body.
func (sc *StreamConverter) Feed(chunk []byte) (events [][]byte, done bool, eErr *errclass.Error) {
	if sc.done {
		return nil, true, nil
	}
	sc.lineBuf = append(sc.lineBuf, chunk...)
	for {
		i := bytes.IndexByte(sc.lineBuf, '\n')
		if i < 0 {
			break // keep trailing partial line buffered
		}
		line := strings.TrimSuffix(string(sc.lineBuf[:i]), "\r")
		sc.lineBuf = sc.lineBuf[i+1:]
		ev, eErr := sc.handleLine(line)
		events = append(events, ev...)
		if eErr != nil {
			return events, false, eErr
		}
		if sc.done {
			break
		}
	}
	return events, sc.done, nil
}

func (sc *StreamConverter) handleLine(line string) ([][]byte, *errclass.Error) {
	if sc.ending {
		if data, ok := sseData(line); ok && data != "" && !shared.IsSSEDone(data) && !shared.IsJSONObject(data) {
			return nil, errclass.Translation("Chat Completions stream ended with an incomplete event payload")
		}
	}
	switch sc.outputFormat {
	case "chat-completions":
		return sc.passthroughLine(line)
	case "messages":
		return sc.claudeLine(line)
	case "responses":
		return sc.responsesLine(line)
	default:
		return nil, shared.UnsupportedFormat(sc.outputFormat, EndpointPath)
	}
}

// Flush terminates a stream whose upstream closed without [DONE]: the
// finish_reason chunk may legally be the last data line before close, and
// without this the client never sees message_delta/message_stop or
// response.completed — nor usage stranded behind the deferred terminal. Claude
// synthesis appends message_stop after the held
// message_delta; Responses synthesis emits response.completed. One-shot:
// later calls return nothing, as does any call with no pending terminal.
func (sc *StreamConverter) Flush() [][]byte {
	if sc.flushed || sc.done || !sc.finished {
		return nil
	}
	sc.flushed = true
	switch sc.outputFormat {
	case "messages":
		events := sc.claudeTerminal()
		if len(events) == 0 {
			if !sc.terminalSent {
				return nil
			}
			// The post-finish include_usage trailer already delivered the
			// terminal via claudeLine; close-without-[DONE] must still emit
			// message_stop or Anthropic clients hang until timeout.
			return [][]byte{sc.messages.emitter.MessageStop()}
		}
		return append(events, sc.messages.emitter.MessageStop())
	case "responses":
		return sc.responsesTerminal()
	default:
		return nil
	}
}

// Finish processes a final unterminated line and rejects EOF before a
// finish_reason or [DONE]. Existing finish-without-[DONE] remains supported.
func (sc *StreamConverter) Finish() ([][]byte, *errclass.Error) {
	if sc.done || sc.flushed {
		return nil, nil
	}
	sc.ending = true
	events, done, eErr := sc.Feed([]byte("\n"))
	if eErr != nil {
		return events, eErr
	}
	if done {
		return events, nil
	}
	if !sc.finished {
		return events, errclass.Translation("Chat Completions stream ended before a terminal state")
	}
	return append(events, sc.Flush()...), nil
}

// sseData extracts the single canonical data line supplied by the public
// transport boundary, which parses multiline SSE frames before dispatch.
func sseData(line string) (payload string, ok bool) {
	rest, ok := strings.CutPrefix(line, "data:")
	if !ok {
		return "", false
	}
	return strings.TrimSpace(rest), true
}

// passthroughLine extracts the bare data payload for openai-target emissions;
// [DONE] sets the terminal state; the public stream wrapper emits its frame.
func (sc *StreamConverter) passthroughLine(line string) ([][]byte, *errclass.Error) {
	data, ok := sseData(line)
	if !ok {
		return nil, nil
	}
	if shared.IsSSEDone(data) {
		sc.done = true
		return nil, nil
	}
	if data == "" {
		return nil, nil
	}
	chunk, eErr := sc.decodeChunk(data)
	if eErr == nil {
		for _, choice := range chunk.Choices {
			if choice.FinishReason != "" {
				sc.finished = true
			}
		}
	}
	if eErr != nil {
		return nil, eErr
	}
	return [][]byte{[]byte(data)}, nil
}

// ---- upstream Chat Completions chunk shape ----

type ccToolCallDelta struct {
	Index    int64  `json:"index"`
	ID       string `json:"id"`
	Function struct {
		Name      string `json:"name"`
		Arguments string `json:"arguments"`
	} `json:"function"`
}

type ccDelta struct {
	shared.ReasoningFields
	Refusal     string            `json:"refusal"`
	Content     string            `json:"content"`
	ToolCalls   []ccToolCallDelta `json:"tool_calls"`
	Annotations json.RawMessage   `json:"annotations"`
}

type ccChunkChoice struct {
	Delta        ccDelta `json:"delta"`
	FinishReason string  `json:"finish_reason"`
}

type ccChunk struct {
	ID      string          `json:"id"`
	Model   string          `json:"model"`
	Choices []ccChunkChoice `json:"choices"`
	Usage   *ccUsage        `json:"usage"`
	Error   *ccChunkError   `json:"error"`
}

// ccChunkError is an in-stream upstream error object; choice-less chunks
// carrying one are failures, not empty deltas.
type ccChunkError struct {
	Message string `json:"message"`
	Type    string `json:"type"`
	Code    any    `json:"code"`
}

// decodeChunk parses one data payload; malformed JSON yields a
// translation failure carrying only a short redacted snippet (no upstream body
// echo). A non-empty upstream error object
// fails the conversion with a classified error so the fail path closes
// both streams instead of the chunk being silently dropped as a
// zero-choices delta.
//
// Usage is captured here — before any empty-choices early return in the
// format converters: OpenAI's stream_options.include_usage terminal
// chunk is exactly choice-less-with-usage, and dropping it would lose
// the final counts.
func (sc *StreamConverter) decodeChunk(data string) (*ccChunk, *errclass.Error) {
	var chunk ccChunk
	if err := json.Unmarshal([]byte(data), &chunk); err != nil {
		return nil, errclass.Translation("malformed Chat Completions stream chunk: " + shared.RedactedSnippet(data))
	}
	if chunk.Error != nil {
		return nil, chunkError(chunk.Error)
	}
	if chunk.Usage != nil {
		sc.usage = chunk.Usage
	}
	return &chunk, nil
}

// chunkError classifies an in-stream Chat Completions error object onto
// error classes, mirroring the sibling adapters' stream-error paths:
// a numeric code (or rate-limit marker) maps through FromStatus so the
// retryability semantics apply; anything else is a retryable upstream
// server failure.
func chunkError(e *ccChunkError) *errclass.Error {
	status := 0
	switch c := e.Code.(type) {
	case float64:
		status = int(c)
	case string:
		if n, err := strconv.Atoi(c); err == nil {
			status = n
		} else if strings.Contains(c, "rate_limit") {
			status = 429
		}
	}
	if status == 0 && strings.Contains(e.Type, "rate_limit") {
		status = 429
	}
	if status > 0 {
		return errclass.FromStatus(status, e.Message)
	}
	return errclass.UpstreamFallback(e.Message)
}

func mergeToolName(current, next string) string {
	if next == "" || current == next || strings.HasPrefix(current, next) {
		return current
	}
	if current == "" || strings.HasPrefix(next, current) {
		return next
	}
	return current + next
}

func (sc *StreamConverter) validateTools() *errclass.Error {
	for _, tool := range sc.tools {
		if tool.id == "" || tool.name == "" {
			return errclass.Translation("Chat Completions tool ended without an id or name")
		}
		if sc.context.IsShell(tool.name) {
			if err := shared.ValidateShellCallID(tool.id); err != nil {
				return err
			}
			if _, err := shared.ShellAction(tool.args.String()); err != nil {
				return err
			}
		}
	}
	return nil
}

func (sc *StreamConverter) decodeCitations(raw json.RawMessage, to string) []shared.Citation {
	if sc.citationKeys == nil {
		sc.citationKeys = map[string]bool{}
	}
	var out []shared.Citation
	for _, c := range shared.DecodeCitations(raw, "chat-completions", to, "delta.annotations", sc.citationText.String(), 0, sc.context) {
		encoded, _ := json.Marshal(c.Wire(to))
		key := string(encoded)
		if !sc.citationKeys[key] {
			sc.citationKeys[key] = true
			out = append(out, c)
		}
	}
	return out
}
