package messages

import (
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"github.com/dillonzq/apix/internal/errclass"
	"github.com/dillonzq/apix/internal/llm/adapter/shared"
)

// StreamConverter converts an upstream Anthropic Messages SSE stream
// (typed `event:`/`data:` pairs) into the client protocol incrementally
// (streaming). It is stateful and sequential-use only:
// one instance per upstream response, Feed called with consecutive
// network chunks in order.
type StreamConverter struct {
	chat             chatOutput
	responses        responsesOutput
	framer           *shared.SSEFramer
	outputFormat     string
	created          int64
	blocks           map[int]*blockState
	msgID            string
	model            string
	promptTokens     int64
	completionTokens int64
	cacheRead        *int64
	cacheCreation    *int64
	stopReason       string // last observed message_delta stop_reason (responses terminal status)
	toolsSeen        bool   // any tool_use block observed (openai finish-reason precedence)
	emitted          bool   // any client event emitted (Flush eligibility)
	done             bool
	ending           bool
	terminalSent     bool
	flushed          bool // one-shot guard for Flush
	context          *shared.ConversionContext
	textRunes        int
	citationKeys     map[string]bool
	citations        []shared.Citation
	search           map[string]*blockState
}

// blockState tracks one open upstream content block by index.
type blockState struct {
	kind          string
	id            string
	name          string
	ordinal       int             // compact tool_calls index for openai chunks
	outIdx        int             // responses: compacted output_index assigned at block start
	emitted       bool            // openai: id/name attached to the first arguments fragment
	text          strings.Builder // accumulated text_delta content (responses target)
	args          strings.Builder // accumulated input_json_delta content (responses target)
	customEmitted int             // custom_tool_call_input bytes already emitted as deltas
	textOffset    int
	closed        bool
	search        *shared.HostedSearch
	searchDone    bool
}

// NewStreamConverter prepares stream conversion for outputFormat
// ("chat-completions" Chat Completions chunks, "responses" Responses events,
// "messages" verbatim passthrough).
func NewStreamConverter(outputFormat string, tools ...*shared.ConversionContext) *StreamConverter {
	sc := &StreamConverter{
		framer:       shared.NewSSEFramer(outputFormat == "messages"),
		outputFormat: outputFormat,
		created:      time.Now().Unix(),
		blocks:       map[int]*blockState{},
	}
	sc.responses.msgIdx = -1
	sc.context = shared.ContextOrNew(tools)
	sc.search = map[string]*blockState{}
	sc.citationKeys = map[string]bool{}
	return sc
}

// Feed consumes one network chunk and returns fully framed client events,
// whether the upstream stream is finished (message_stop seen), and a
// classified error. Partial lines are buffered until complete.
func (sc *StreamConverter) Feed(chunk []byte) (events [][]byte, done bool, eErr *errclass.Error) {
	if sc.done {
		return nil, true, nil
	}
	sc.framer.Push(chunk)
	for {
		etype, data, rawBlock, ok := sc.framer.Next()
		if !ok {
			return events, false, nil
		}
		d, eErr := sc.dispatch(etype, data, rawBlock, &events)
		if len(events) > 0 {
			sc.emitted = true
		}
		if eErr != nil {
			return events, false, eErr
		}
		if d {
			sc.done = true
			return events, true, nil
		}
	}
}

// dispatch routes one complete SSE frame. Conversion targets parse the
// data payload eagerly into typed per-event structs (with the
// type-from-payload fallback when the event: line was absent); claude
// passthrough forwards the verbatim raw block and parses only what error
// handling and done-detection require.
func (sc *StreamConverter) dispatch(etype, data string, rawBlock []byte, events *[][]byte) (bool, *errclass.Error) {
	if (etype == "message_stop" || sc.ending && data != "") && !shared.IsJSONObject(data) {
		return false, errclass.Translation("Messages stream has an incomplete event payload")
	}
	// Validate the discriminator before either native passthrough or synthesis
	// can mark completion. Extra fields remain forward-compatible.
	if etype == "message_stop" {
		var terminal struct {
			Type string `json:"type"`
		}
		if json.Unmarshal([]byte(data), &terminal) != nil || terminal.Type != "message_stop" {
			return false, errclass.Translation("Messages message_stop payload must have type message_stop")
		}
	}
	if sc.outputFormat == "messages" {
		return sc.dispatchClaude(etype, data, rawBlock, events)
	}
	var ev sseEvent
	if data != "" {
		if err := json.Unmarshal([]byte(data), &ev); err != nil {
			return false, errclass.Translation(fmt.Sprintf("malformed SSE data JSON: %s", shared.RedactedSnippet(data)))
		}
		if etype == "" {
			etype = ev.Type // tolerate missing event: line
		}
	}
	if etype == "message_delta" && ev.Delta.StopReason != "" {
		sc.stopReason = ev.Delta.StopReason
	}
	switch sc.outputFormat {
	case "chat-completions":
		return sc.dispatchOpenAI(etype, &ev, events)
	case "responses":
		return sc.dispatchResponses(etype, &ev, events)
	default:
		return false, shared.UnsupportedFormat(sc.outputFormat, EndpointPath)
	}
}

// sseEvent is the typed union of Anthropic Messages SSE payload shapes on
// conversion targets. One struct serves every event type because the
// type may arrive only inside the payload (missing event: line), so the
// decode must precede routing; absent members stay zero-valued at no
// cost, and unknown keys are ignored. Error stays a raw map because the
// classifier reads an open-ended upstream object.
type sseEvent struct {
	Type         string         `json:"type"`
	Index        float64        `json:"index"`
	Message      startMessage   `json:"message"`
	ContentBlock contentBlock   `json:"content_block"`
	Delta        deltaBody      `json:"delta"`
	Usage        usageCounts    `json:"usage"`
	Error        map[string]any `json:"error"`
}

type startMessage struct {
	ID    string      `json:"id"`
	Model string      `json:"model"`
	Usage usageCounts `json:"usage"`
}

type contentBlock struct {
	Thinking  string          `json:"thinking"`
	Type      string          `json:"type"`
	ID        string          `json:"id"`
	Name      string          `json:"name"`
	Text      string          `json:"text"`
	Citations json.RawMessage `json:"citations"`
	Input     json.RawMessage `json:"input"`
	Content   json.RawMessage `json:"content"`
	ToolUseID string          `json:"tool_use_id"`
}

type deltaBody struct {
	Thinking    string          `json:"thinking"`
	Type        string          `json:"type"`
	Text        string          `json:"text"`
	PartialJSON string          `json:"partial_json"`
	StopReason  string          `json:"stop_reason"`
	Citation    json.RawMessage `json:"citation"`
}

type usageCounts struct {
	InputTokens   float64  `json:"input_tokens"`
	OutputTokens  float64  `json:"output_tokens"`
	CacheRead     *float64 `json:"cache_read_input_tokens"`
	CacheCreation *float64 `json:"cache_creation_input_tokens"`
}

// dispatchClaude passes event blocks through verbatim; an in-stream error
// event becomes a classified error instead of reaching the client raw.
// The payload is parsed lazily: only when the typed fallback must resolve
// a missing event: line or when the frame is an error event.
func (sc *StreamConverter) dispatchClaude(etype, data string, raw []byte, events *[][]byte) (bool, *errclass.Error) {
	var payload map[string]any
	if etype == "" && data != "" {
		shared.DecodeProbe([]byte(data), &payload) // best-effort type probe
		etype, _ = payload["type"].(string)
	}
	if etype == "message_delta" {
		if payload == nil {
			shared.DecodeProbe([]byte(data), &payload)
		}
		if delta, ok := payload["delta"].(map[string]any); ok {
			if stop, ok := delta["stop_reason"].(string); ok && stop != "" {
				sc.stopReason = stop
			}
		}
	}
	if etype == "error" {
		if payload == nil && data != "" {
			shared.DecodeProbe([]byte(data), &payload)
		}
		return false, sseError(mget(payload, "error"))
	}
	if len(raw) > 0 {
		*events = append(*events, raw)
	}
	return etype == "message_stop", nil
}

// Flush emits a deferred Responses terminal only after stop_reason was
// observed. EOF without a terminal state must be checked with Finish. Native
// Messages and Chat Completions have no deferred events. One-shot, and a
// previously observed message_stop prevents any duplicate terminal.
func (sc *StreamConverter) Flush() [][]byte {
	if sc.flushed || sc.done {
		return nil
	}
	if !sc.emitted || sc.stopReason == "" {
		return nil
	}
	sc.flushed = true
	switch sc.outputFormat {
	case "responses":
		return sc.responsesCompleted()
	case "chat-completions":
		return nil
	default: // claude passthrough forwards verbatim; nothing deferred
		return nil
	}
}

// Finish accepts EOF only after a terminal state was observed. A missing
// message_stop after stop_reason is compatible; partial output without either
// is a stream error, never a synthesized response.completed.
func (sc *StreamConverter) Finish() ([][]byte, *errclass.Error) {
	if sc.done || sc.flushed {
		return nil, nil
	}
	sc.ending = true
	sc.framer.End()
	events, done, eErr := sc.Feed(nil)
	if eErr != nil {
		return events, eErr
	}
	if done {
		return events, nil
	}
	if sc.stopReason == "" {
		return events, errclass.Translation("Messages stream ended before a terminal state")
	}
	if sc.outputFormat == "responses" {
		if err := sc.validateShellTools(); err != nil {
			return events, err
		}
	}
	return append(events, sc.Flush()...), nil
}

func (sc *StreamConverter) validateShellTools() *errclass.Error {
	for _, block := range sc.blocks {
		if sc.context.IsShell(block.name) {
			if err := shared.ValidateShellCallID(block.id); err != nil {
				return err
			}
			if _, err := shared.ShellAction(block.args.String()); err != nil {
				return err
			}
		}
	}
	return nil
}

func (sc *StreamConverter) captureCache(u usageCounts) {
	if u.CacheRead != nil {
		v := int64(*u.CacheRead)
		sc.cacheRead = &v
	}
	if u.CacheCreation != nil {
		v := int64(*u.CacheCreation)
		sc.cacheCreation = &v
	}
}

// sseError maps an Anthropic in-stream error event onto the shared
// classification via its documented error-type taxonomy.
func sseError(e map[string]any) *errclass.Error {
	errorType := sget(e, "type")
	message := sget(e, "message")
	if strings.Contains(strings.ToLower(errorType), "gousagelimiterror") || strings.Contains(strings.ToLower(errorType), "freeusagelimiterror") {
		message = errorType + ": " + message
	}
	return errclass.FromStatus(anthropicErrorStatus(errorType), message)
}

func anthropicErrorStatus(t string) int {
	switch t {
	case "invalid_request_error":
		return 400
	case "authentication_error":
		return 401
	case "permission_error":
		return 403
	case "not_found_error":
		return 404
	case "request_too_large":
		return 413
	case "rate_limit_error":
		return 429
	case "overloaded_error":
		return 503
	default:
		return 500
	}
}

func mget(m map[string]any, k string) map[string]any {
	v, _ := m[k].(map[string]any)
	return v
}

func sget(m map[string]any, k string) string {
	v, _ := m[k].(string)
	return v
}

func (sc *StreamConverter) emitCitations(block *blockState, raw json.RawMessage, events *[][]byte) {
	if block == nil || block.kind != "text" || len(raw) == 0 {
		return
	}
	citations := shared.DecodeCitations(raw, "messages", sc.outputFormat, "content.citations", block.text.String(), block.textOffset, sc.context)
	for _, c := range citations {
		encoded, _ := json.Marshal(c.Wire(sc.outputFormat))
		key := fmt.Sprintf("%d/%s", block.textOffset, encoded)
		if sc.citationKeys[key] {
			continue
		}
		sc.citationKeys[key] = true
		if sc.outputFormat == "chat-completions" {
			sc.emitRole(events)
			sc.emitChunk(events, map[string]any{"annotations": shared.CitationValues([]shared.Citation{c}, "chat-completions")}, "", false)
		} else {
			index := len(sc.citations)
			sc.citations = append(sc.citations, c)
			*events = append(*events, sc.responsesEm().AnnotationAdded(sc.msgID, sc.responses.msgIdx, 0, index, c))
		}
	}
}
