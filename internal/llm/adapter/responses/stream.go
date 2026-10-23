package responses

import (
	"encoding/json"
	"fmt"
	"strings"

	"github.com/dillonzq/apix/internal/errclass"
	"github.com/dillonzq/apix/internal/llm/adapter/shared"
)

// StreamConverter incrementally converts an upstream Responses SSE stream
// into the selected output protocol.
//
// Contract: instances are single-use and strictly sequential — Feed is
// called from one goroutine with consecutive network chunks; there is no
// internal locking. Partial SSE lines are buffered until a blank line
// completes the `event:`/`data:` pair.
type StreamConverter struct {
	messages     messagesOutput
	native       nativeOutput
	done         bool
	ending       bool
	framer       *shared.SSEFramer
	outputFormat string

	context *shared.ConversionContext
	// Response identity captured from response.created.
	id      string
	model   string
	created int64

	// openai (Chat Completions) target state.
	toolCallsSeen bool

	// Reasoning parts track both target formats; block state is Messages-only.
	reasoningParts map[string]*strings.Builder
	refused        bool
	nextIndex      int

	// Source tool identities and argument replay are shared across targets.
	// Messages block indexes belong to messagesOutput, independently of tool ordinals.
	tracker      *toolCallTracker
	textParts    map[string]*visibleTextPart
	textRunes    int
	lastTextPart string
}

// NewStreamConverter builds a converter for outputFormat ("chat-completions",
// "messages", "responses"); unknown formats fail on first Feed with
// ClassUnsupported (same classes as BuildRequest).
func NewStreamConverter(outputFormat string, tools ...*shared.ConversionContext) *StreamConverter {
	sc := &StreamConverter{
		// wantRaw only for the Responses passthrough, which
		// forwards verbatim blocks; a rebuilt single data line would
		// embed raw newlines from legally multi-data-line frames and
		// corrupt native framing. Conversion targets parse the joined
		// payload and never read raw.
		framer:       shared.NewSSEFramer(outputFormat == "responses"),
		outputFormat: outputFormat,
		id:           "apix",
		context:      shared.ContextOrNew(tools),
	}
	sc.native = newNativeOutput()
	alloc := sc.allocIndex
	if outputFormat == "messages" {
		// Tool tracker ordinals are separate from wire block indexes. Blocks
		// receive their index when emitted from the serialized Messages queue.
		ordinal := 0
		alloc = func() int { i := ordinal; ordinal++; return i }
	}
	sc.tracker = newToolCallTracker(alloc)
	return sc
}

// allocIndex allocates compact tool-call indexes for Chat Completions output.
func (sc *StreamConverter) allocIndex() int {
	i := sc.nextIndex
	sc.nextIndex++
	return i
}

// toolCallTracker is the single announce-or-replay decision table for
// upstream function_call items, keyed by call_id and owned by both
// conversion targets so their delivery decisions cannot diverge:
// announce once per call, and deliver only a complete snapshot's missing
// suffix after any previously streamed argument fragments.
//
// OpenAI-conformant Responses streams identify one item in TWO distinct
// namespaces: output_item events carry call_id ("call_…") while
// argument-delta events reference the item's own id ("fc_…"). Every
// registration therefore binds BOTH identifiers to the same state and
// lookups resolve through either before creating new state — otherwise
// each delta fragment would fork a second tracker state and clients
// would see a ghost tool entry beside the announced call.
type toolCallTracker struct {
	calls  map[string]*toolCallState
	states []*toolCallState
	alloc  func() int
}

type toolCallState struct {
	index     int
	args      strings.Builder
	callID    string
	name      string
	complete  bool
	announced bool
	sent      int
}

func (st *toolCallState) hasIdentity() bool { return st.callID != "" && st.name != "" }

func newToolCallTracker(alloc func() int) *toolCallTracker {
	return &toolCallTracker{calls: map[string]*toolCallState{}, alloc: alloc}
}

// Observe records identity and a complete argument snapshot separately.
// An argument-only state is not a downstream announcement.
func (t *toolCallTracker) Observe(callID, itemID, name, args string, complete bool) *toolCallState {
	st := t.state(callID, itemID)
	if callID != "" {
		st.callID = callID
	}
	if name != "" {
		st.name = name
	}
	previous := st.args.String()
	if strings.HasPrefix(args, previous) {
		st.args.WriteString(args[len(previous):])
	}
	st.complete = st.complete || complete
	return st
}

func (t *toolCallTracker) StreamArgs(itemID, delta string) *toolCallState {
	st := t.state(itemID, "")
	st.args.WriteString(delta)
	return st
}

// state resolves key, then alias, binding whichever identifiers are
// present to the shared state so both namespaces stay one call.
func (t *toolCallTracker) state(key, alias string) *toolCallState {
	if st, ok := t.calls[key]; ok {
		t.bind(alias, st)
		return st
	}
	if st, ok := t.calls[alias]; ok {
		t.bind(key, st)
		return st
	}
	st := &toolCallState{index: t.alloc()}
	t.states = append(t.states, st)
	t.bind(key, st)
	t.bind(alias, st)
	return st
}

func (t *toolCallTracker) bind(key string, st *toolCallState) {
	if key != "" {
		t.calls[key] = st
	}
}

// ---- upstream Responses SSE payload shapes ----
//
// One typed struct per handled event type, keyed off the framer's event
// name; unknown keys are ignored and absent members stay zero-valued at
// no cost. Unknown EVENT types are JSON-validated without decoding and
// ignored.

type usageCounts struct {
	InputTokens  float64 `json:"input_tokens"`
	OutputTokens float64 `json:"output_tokens"`
	InputDetails *struct {
		CachedTokens     *float64 `json:"cached_tokens"`
		CacheWriteTokens *float64 `json:"cache_write_tokens"`
	} `json:"input_tokens_details"`
	OutputDetails *struct {
		ReasoningTokens *float64 `json:"reasoning_tokens"`
	} `json:"output_tokens_details"`
}

type errorMessage struct {
	Message string `json:"message"`
}

type responseMeta struct {
	ID         string             `json:"id"`
	Model      string             `json:"model"`
	CreatedAt  float64            `json:"created_at"`
	StatusCode float64            `json:"status_code"`
	Output     []functionCallItem `json:"output"`
	Usage      usageCounts        `json:"usage"`
	Error      errorMessage       `json:"error"`
}

type functionCallItem struct {
	Summary   []respTextPart             `json:"summary"`
	Content   []respTextPart             `json:"content"`
	Type      string                     `json:"type"`
	CallID    string                     `json:"call_id"`
	ID        string                     `json:"id"`
	Name      string                     `json:"name"`
	Input     string                     `json:"input"`
	Arguments string                     `json:"arguments"`
	Raw       map[string]json.RawMessage `json:"-"`
}

func (item *functionCallItem) UnmarshalJSON(data []byte) error {
	type plain functionCallItem
	if err := json.Unmarshal(data, (*plain)(item)); err != nil {
		return err
	}
	return json.Unmarshal(data, &item.Raw)
}

type createdEvent struct {
	Response responseMeta `json:"response"`
}

type textDeltaEvent struct {
	ItemID       string `json:"item_id"`
	ContentIndex int    `json:"content_index"`
	Delta        string `json:"delta"`
	Text         string `json:"text"`
	Refusal      string `json:"refusal"`
}

type itemEvent struct {
	Item functionCallItem `json:"item"`
}

type argsDeltaEvent struct {
	ItemID    string `json:"item_id"`
	CallID    string `json:"call_id"`
	Name      string `json:"name"`
	Delta     string `json:"delta"`
	Arguments string `json:"arguments"`
}

type terminalEvent struct {
	Response responseMeta `json:"response"`
}

type failureEvent struct {
	StatusCode float64      `json:"status_code"`
	Message    string       `json:"message"`
	Response   responseMeta `json:"response"`
}

// decodeEvent parses one SSE data payload into its typed shape; malformed
// JSON yields a translation failure carrying only a short redacted
// snippet (no upstream body echo). An empty payload
// leaves the event fields unset.
func decodeEvent[T any](eventType, payload string, v *T) *errclass.Error {
	if payload == "" {
		return nil
	}
	if err := json.Unmarshal([]byte(payload), v); err != nil {
		return errclass.Translation(fmt.Sprintf(
			"malformed %s event payload: %s", eventType, shared.RedactedSnippet(payload)))
	}
	return nil
}

// checkTarget rejects unknown conversion sources after payload decode, so
// malformed-payload translation failures keep their pre-existing
// precedence over ClassUnsupported.
func (sc *StreamConverter) checkTarget() *errclass.Error {
	if sc.outputFormat == "chat-completions" || sc.outputFormat == "messages" {
		return nil
	}
	return shared.UnsupportedFormat(sc.outputFormat, EndpointPath)
}

// Feed consumes one network chunk and returns synthesized client events,
// whether the upstream stream reached a terminal state, and a classified
// error for failed/malformed streams. Native non-terminal frames
// are forwarded verbatim; terminal snapshots are validated before forwarding.
// Conversion targets parse eagerly into typed per-event structs.
func (sc *StreamConverter) Feed(chunk []byte) (events [][]byte, done bool, eErr *errclass.Error) {
	if sc.done {
		return nil, true, nil
	}
	sc.framer.Push(chunk)
	for {
		eventType, payload, raw, ok := sc.framer.Next()
		if !ok {
			return events, done, nil
		}
		if (eventType == "response.completed" || eventType == "response.incomplete" || sc.ending && payload != "") && !shared.IsJSONObject(payload) {
			return events, false, errclass.Translation("Responses stream has an incomplete event payload")
		}
		if eventType == "response.completed" || eventType == "response.incomplete" {
			if eErr := validateTerminalPayload(eventType, payload); eErr != nil {
				return events, false, eErr
			}
		}
		if sc.outputFormat == "responses" {
			evs, d, dErr := sc.passthroughEvent(eventType, payload, raw)
			events = append(events, evs...)
			if dErr != nil {
				return events, false, dErr
			}
			if d {
				sc.done = true
				return events, true, nil
			}
			continue
		}
		evs, d, dErr := sc.convertEvent(eventType, payload)
		events = append(events, evs...)
		if dErr != nil {
			return events, false, dErr
		}
		if d {
			sc.done = true
			return events, true, nil
		}
	}
}

// A terminal event must carry a coherent response snapshot, not merely an
// arbitrary JSON object. Validate the fields that establish response identity,
// terminal status and final output before native passthrough or conversion.
// Usage and unrelated metadata may be absent; extra fields stay compatible.
func validateTerminalPayload(eventType, payload string) *errclass.Error {
	var event struct {
		Type     string `json:"type"`
		Response *struct {
			ID     string          `json:"id"`
			Object string          `json:"object"`
			Status string          `json:"status"`
			Output json.RawMessage `json:"output"`
		} `json:"response"`
	}
	fail := func() *errclass.Error {
		return errclass.Translation("Responses terminal payload requires matching type and response id, object, status, and output array")
	}
	if json.Unmarshal([]byte(payload), &event) != nil || event.Type != eventType || event.Response == nil {
		return fail()
	}
	response := event.Response
	if strings.TrimSpace(response.ID) == "" || response.Object != "response" || response.Status != strings.TrimPrefix(eventType, "response.") {
		return fail()
	}
	output := strings.TrimSpace(string(response.Output))
	if !strings.HasPrefix(output, "[") {
		return fail()
	}
	var items []json.RawMessage
	if json.Unmarshal(response.Output, &items) != nil {
		return fail()
	}
	for _, item := range items {
		if !shared.IsJSONObject(string(item)) {
			return fail()
		}
	}
	return nil
}

// Finish drains the last SSE frame and requires an explicit Responses
// terminal event. EOF alone cannot complete text or function arguments.
func (sc *StreamConverter) Finish() ([][]byte, *errclass.Error) {
	if sc.done {
		return nil, nil
	}
	sc.ending = true
	sc.framer.End()
	events, done, eErr := sc.Feed(nil)
	if eErr != nil {
		return events, eErr
	}
	if !done {
		return events, errclass.Translation("Responses stream ended before a terminal state")
	}
	return events, nil
}

// convertEvent routes one complete SSE frame to a conversion target:
// handled event types decode into their typed structs and share the
// field extraction below; unknown types only validate their payload.
func (sc *StreamConverter) convertEvent(eventType, payload string) ([][]byte, bool, *errclass.Error) {
	switch eventType {
	case "response.created":
		var ev createdEvent
		if eErr := decodeEvent(eventType, payload, &ev); eErr != nil {
			return nil, false, eErr
		}
		if eErr := sc.checkTarget(); eErr != nil {
			return nil, false, eErr
		}
		sc.captureResponse(&ev.Response)
		if sc.outputFormat == "chat-completions" {
			return [][]byte{sc.chatChunks().RoleChunk()}, false, nil
		}
		// Input token count only becomes known at completion; Claude
		// clients read authoritative usage from message_delta, so
		// starting at zero is lossless here.
		return [][]byte{sc.claudeChunks().MessageStart(0)}, false, nil
	case "response.reasoning_summary_text.delta", "response.reasoning_text.delta", "response.reasoning_summary_text.done", "response.reasoning_text.done":
		var ev reasoningEvent
		if eErr := decodeEvent(eventType, payload, &ev); eErr != nil {
			return nil, false, eErr
		}
		if eErr := sc.checkTarget(); eErr != nil {
			return nil, false, eErr
		}
		kind, index := "content", ev.ContentIndex
		if strings.Contains(eventType, "summary") {
			kind, index = "summary", ev.SummaryIndex
		}
		key := fmt.Sprintf("%s/%s/%d", ev.ItemID, kind, index)
		if strings.HasSuffix(eventType, ".done") {
			events := sc.reasoningPart(key, ev.Text, true)
			if sc.outputFormat == "messages" {
				events = append(events, sc.queueMessagesContent("reasoning_stop", "")...)
			}
			return events, false, nil
		}
		return sc.reasoningPart(key, ev.Delta, false), false, nil
	case "response.output_text.annotation.added":
		var ev struct {
			ItemID       string          `json:"item_id"`
			ContentIndex int             `json:"content_index"`
			Annotation   json.RawMessage `json:"annotation"`
		}
		if err := decodeEvent(eventType, payload, &ev); err != nil {
			return nil, false, err
		}
		return sc.annotation(ev.ItemID, ev.ContentIndex, ev.Annotation), false, nil
	case "response.output_text.delta", "response.output_text.done", "response.refusal.delta", "response.refusal.done":
		var ev textDeltaEvent
		if eErr := decodeEvent(eventType, payload, &ev); eErr != nil {
			return nil, false, eErr
		}
		if eErr := sc.checkTarget(); eErr != nil {
			return nil, false, eErr
		}
		refused := strings.HasPrefix(eventType, "response.refusal.")
		complete := strings.HasSuffix(eventType, ".done")
		text := ev.Delta
		if complete {
			text = ev.Text
			if refused {
				text = ev.Refusal
			}
		}
		return sc.visiblePart(ev.ItemID, ev.ContentIndex, refused, text, complete)
	case "response.output_item.added", "response.output_item.done":
		var ev itemEvent
		if eErr := decodeEvent(eventType, payload, &ev); eErr != nil {
			return nil, false, eErr
		}
		if eErr := sc.checkTarget(); eErr != nil {
			return nil, false, eErr
		}
		if err := validateOutputItem(ev.Item); err != nil {
			return nil, false, err
		}
		if ev.Item.Type == "web_search_call" {
			return sc.searchItem(ev.Item, eventType == "response.output_item.done")
		}
		if ev.Item.Type == "reasoning" {
			return sc.reasoningSnapshot(ev.Item), false, nil
		}
		if ev.Item.Type == "message" {
			return sc.messageSnapshot(ev.Item)
		}
		return sc.outputItem(&ev.Item, eventType == "response.output_item.done")
	case "response.custom_tool_call_input.delta", "response.custom_tool_call_input.done":
		var ev struct {
			ItemID string `json:"item_id"`
			CallID string `json:"call_id"`
			Name   string `json:"name"`
			Delta  string `json:"delta"`
			Input  string `json:"input"`
		}
		if err := decodeEvent(eventType, payload, &ev); err != nil {
			return nil, false, err
		}
		if ev.ItemID == "" {
			return nil, false, errclass.Translation("custom tool input has no item identity")
		}
		if strings.HasSuffix(eventType, ".delta") {
			sc.customArguments(ev.ItemID, ev.Delta)
			return nil, false, nil
		}
		input := ev.Input
		if input == "" {
			input = sc.customArguments(ev.ItemID, "")
		}
		args, _ := json.Marshal(map[string]string{"input": input})
		return sc.outputItem(&functionCallItem{Type: "function_call", ID: ev.ItemID, CallID: ev.CallID, Name: ev.Name, Arguments: string(args)}, true)
	case "response.function_call_arguments.done":
		var ev argsDeltaEvent
		if eErr := decodeEvent(eventType, payload, &ev); eErr != nil {
			return nil, false, eErr
		}
		if eErr := sc.checkTarget(); eErr != nil {
			return nil, false, eErr
		}
		item := functionCallItem{Type: "function_call", ID: ev.ItemID, CallID: ev.CallID, Name: ev.Name, Arguments: ev.Arguments}
		return sc.outputItem(&item, true)
	case "response.function_call_arguments.delta":
		var ev argsDeltaEvent
		if eErr := decodeEvent(eventType, payload, &ev); eErr != nil {
			return nil, false, eErr
		}
		if eErr := sc.checkTarget(); eErr != nil {
			return nil, false, eErr
		}
		return sc.argsFragment(ev.ItemID, ev.Delta)
	case "response.completed", "response.incomplete":
		var ev terminalEvent
		if eErr := decodeEvent(eventType, payload, &ev); eErr != nil {
			return nil, false, eErr
		}
		if eErr := sc.checkTarget(); eErr != nil {
			return nil, false, eErr
		}
		details := shared.UsageDetails{}
		if ev.Response.Usage.InputDetails != nil {
			if ev.Response.Usage.InputDetails.CachedTokens != nil {
				v := int64(*ev.Response.Usage.InputDetails.CachedTokens)
				details.CachedTokens = &v
			}
			if ev.Response.Usage.InputDetails.CacheWriteTokens != nil {
				v := int64(*ev.Response.Usage.InputDetails.CacheWriteTokens)
				details.CacheWriteTokens = &v
			}
		}
		if ev.Response.Usage.OutputDetails != nil && ev.Response.Usage.OutputDetails.ReasoningTokens != nil {
			v := int64(*ev.Response.Usage.OutputDetails.ReasoningTokens)
			details.ReasoningTokens = &v
		}
		var events [][]byte
		for _, item := range ev.Response.Output {
			if err := validateOutputItem(item); err != nil {
				return events, false, err
			}
			switch item.Type {
			case "reasoning":
				events = append(events, sc.reasoningSnapshot(item)...)
			case "message":
				more, _, eErr := sc.messageSnapshot(item)
				events = append(events, more...)
				if eErr != nil {
					return events, false, eErr
				}
			case "web_search_call":
				more, _, err := sc.searchItem(item, true)
				events = append(events, more...)
				if err != nil {
					return events, false, err
				}
			case "function_call", "custom_tool_call":
				more, _, eErr := sc.outputItem(&item, true)
				events = append(events, more...)
				if eErr != nil {
					return events, false, eErr
				}
			}
		}
		tail, done, eErr := sc.terminal(eventType == "response.incomplete", int(ev.Response.Usage.InputTokens), int(ev.Response.Usage.OutputTokens), details)
		return append(events, tail...), done, eErr
	case "response.failed", "error":
		var ev failureEvent
		if eErr := decodeEvent(eventType, payload, &ev); eErr != nil {
			return nil, false, eErr
		}
		if eErr := sc.checkTarget(); eErr != nil {
			return nil, false, eErr
		}
		return nil, false, failureError(&ev)
	default:
		if !knownResponseMetadataEvent(eventType) {
			sc.context.Report("responses", sc.outputFormat, "event."+eventType, true)
		}
		// Informational events (response.in_progress, annotations)
		// have no client equivalent and are omitted
		// per the compatibility policy; the payload is
		// validated without materializing a discarded generic graph.
		if payload != "" && !json.Valid([]byte(payload)) {
			return nil, false, errclass.Translation(fmt.Sprintf(
				"malformed %s event payload: %s", eventType, shared.RedactedSnippet(payload)))
		}
		return nil, false, nil
	}
}

// textDelta emits one output text delta: a plain Chat Completions content
// chunk, or a Messages content_block_delta that auto-opens (and may
// reopen) the text block.
func (sc *StreamConverter) emitTextDelta(delta string) ([][]byte, bool, *errclass.Error) {
	if sc.outputFormat == "chat-completions" {
		return [][]byte{sc.chatChunks().Delta(map[string]any{"content": delta})}, false, nil
	}
	var out [][]byte
	if !sc.messages.blocks.Active("text") {
		// A tool_use block start closed the previous text block; a
		// later text delta (legal Responses interleaving) reopens a
		// fresh text block at the next free index to keep the
		// mandated start/delta/stop pairing per block index (same
		// stop/reopen pattern as the Chat Completions route). Any
		// still-open block stops first: close-before-next-start.
		out = append(out, sc.closeOpenBlocks()...)
		_, opening := sc.messages.blocks.Start(sc.claudeChunks(), "text", map[string]any{"text": ""})
		out = append(out, opening...)
	}
	out = append(out, sc.messages.blocks.Delta(sc.claudeChunks(),
		map[string]any{"type": "text_delta", "text": delta})...)
	return out, false, nil
}

// outputItem handles output_item.added/.done through the shared tracker:
// Chat Completions announces/replays tool_calls entries, Messages opens
// tool_use blocks with mandated pairing; non-function items are ignored.
func (sc *StreamConverter) outputItem(item *functionCallItem, complete bool) ([][]byte, bool, *errclass.Error) {
	if item.Type == "custom_tool_call" {
		copyItem := *item
		copyItem.Type = "function_call"
		if complete {
			raw, _ := json.Marshal(map[string]string{"input": item.Input})
			copyItem.Arguments = string(raw)
		}
		item = &copyItem
	}
	if item.Type != "function_call" {
		return nil, false, nil
	}
	if item.ID == "" && item.CallID == "" {
		return nil, false, errclass.Translation("Responses tool item has no item identity")
	}
	st := sc.tracker.Observe(item.CallID, item.ID, item.Name, item.Arguments, complete)
	return sc.emitToolState(st)
}

// Argument deltas may precede tool identity. Both target protocols buffer
// those bytes until the real call_id and name are available.
func (sc *StreamConverter) argsFragment(itemID, delta string) ([][]byte, bool, *errclass.Error) {
	if itemID == "" {
		return nil, false, errclass.Translation("Responses tool arguments delta has no item identity")
	}
	return sc.emitToolState(sc.tracker.StreamArgs(itemID, delta))
}

func (sc *StreamConverter) emitToolState(st *toolCallState) ([][]byte, bool, *errclass.Error) {
	sc.toolCallsSeen = true
	args := st.args.String()
	if sc.outputFormat == "chat-completions" {
		if !st.hasIdentity() {
			return nil, false, nil
		}
		if !st.announced {
			st.announced = true
			st.sent = len(args)
			return [][]byte{sc.chatChunks().Delta(map[string]any{"tool_calls": []any{shared.CCToolCallOpeningEntry(st.index, st.callID, st.name, args)}})}, false, nil
		}
		tail := args[st.sent:]
		if tail == "" {
			return nil, false, nil
		}
		st.sent = len(args)
		return [][]byte{sc.chatChunks().Delta(map[string]any{"tool_calls": []any{map[string]any{"index": st.index, "function": map[string]any{"arguments": tail}}}})}, false, nil
	}
	events, eErr := sc.queueMessagesTool(st.index, st.callID, st.name, args[st.sent:], st.complete)
	if eErr == nil {
		st.sent = len(args)
	}
	return events, false, eErr
}

// terminal renders the completed/incomplete tail: Chat Completions gets a
// finish_reason chunk plus [DONE]; Messages gets close-before-stop block
// pairing, message_delta (stop_sequence omitted entirely) and
// message_stop. Shared precedence: tool calls outrank the status-derived
// reason, so response.incomplete cannot downgrade them.
func (sc *StreamConverter) terminal(incomplete bool, in, out int, details shared.UsageDetails) ([][]byte, bool, *errclass.Error) {
	for _, tool := range sc.tracker.states {
		if !tool.hasIdentity() {
			return nil, false, errclass.Translation("Responses stream ended before tool identity was complete")
		}
	}
	st := "completed"
	if incomplete {
		st = "incomplete"
	}
	if sc.outputFormat == "chat-completions" {
		finish := shared.TerminalReason(sc.toolCallsSeen, "tool_calls", shared.FinishWithRefusal(shared.CCFinishFromResponseStatus(st), sc.refused))
		// Always attached (Parity with the Messages route's terminal
		// chunk): typed clients prefer a stable schema, zero-valued fields
		// when upstream reported none. Shared kernel keeps total_tokens
		// consistent with the non-stream Chat Completions mapper.
		chunk := sc.chatChunks().Finish(finish, shared.CCUsageFrom(int64(in), int64(out), details))
		return [][]byte{chunk}, true, nil
	}
	statusStop := shared.FinishToClaudeStop(shared.FinishWithRefusal(shared.CCFinishFromResponseStatus(st), sc.refused))
	stop := shared.TerminalReason(sc.toolCallsSeen, "tool_use", statusStop)
	cacheRead, cacheWrite := details.CachedTokens, details.CacheWriteTokens
	em := sc.claudeChunks()
	for _, tool := range sc.messages.tools {
		tool.complete = true
	}
	events := sc.drainMessages()
	for key, part := range sc.textParts {
		if len(part.pending) > 0 {
			sc.context.Report("responses", sc.outputFormat, "output."+key+".annotations.missing_text", false)
			part.pending = nil
		}
	}
	events = append(events, sc.closeOpenBlocks()...)
	events = append(events,
		em.MessageDelta(&stop, shared.ClaudeUsage(shared.ClampSubtract(int64(in), cacheRead, cacheWrite), int64(out), cacheRead, cacheWrite)),
		em.MessageStop(),
	)
	return events, true, nil
}

// chatChunks binds the shared chunk kernel to the captured upstream
// response identity so this route's frames cannot diverge from the
// sibling Messages-route synthesizer; a zero Created defaults
// to render-time now inside the kernel.
func (sc *StreamConverter) chatChunks() shared.ChatChunkBuilder {
	return shared.ChatChunkBuilder{ID: sc.id, Model: sc.model, Created: sc.created}
}

// claudeChunks binds the shared Claude emitter kernel to the captured
// upstream response identity so this route's Messages frames cannot
// diverge from the sibling Chat Completions-route synthesizer.
func (sc *StreamConverter) claudeChunks() shared.ClaudeEventEmitter {
	return shared.NewClaudeEventEmitter(sc.id, sc.model)
}

// closeOpenBlocks emits content_block_stop for every currently open
// content block and resets open-block tracking, so the next
// content_block_start obeys Anthropic's close-before-next-start rule;
// the text block is marked closed so a later text delta reopens a fresh
// block (stop/reopen parity with the Chat Completions route).
func (sc *StreamConverter) closeOpenBlocks() [][]byte {
	if sc.messages.blocks.Active("text") {
		for key, part := range sc.textParts {
			if part.block == sc.messages.blocks.Index() {
				part.closed = true
				if len(part.pending) > 0 {
					sc.context.Report("responses", "messages", "output."+key+".annotations.closed_block", false)
					part.pending = nil
				}
			}
		}
	}
	return sc.messages.blocks.Close(sc.claudeChunks())
}

// argsDelta renders one input_json_delta content block delta.
func (sc *StreamConverter) argsDelta(index int, partial string) []byte {
	return sc.claudeChunks().ContentBlockDelta(index,
		map[string]any{"type": "input_json_delta", "partial_json": partial})
}

// captureResponse records stream identity from response.created.
func (sc *StreamConverter) captureResponse(resp *responseMeta) {
	if resp.ID != "" {
		sc.id = resp.ID
	}
	if resp.Model != "" {
		sc.model = resp.Model
	}
	sc.created = int64(resp.CreatedAt)
}

// failureError maps response.failed / error events onto error classes,
// honoring an upstream status_code when present; without
// one the failure is treated as a retryable upstream server failure.
func failureError(ev *failureEvent) *errclass.Error {
	status := int(ev.StatusCode)
	if status == 0 {
		status = int(ev.Response.StatusCode)
	}
	msg := ev.Message
	if msg == "" {
		msg = ev.Response.Error.Message
	}
	if status > 0 {
		return errclass.FromStatus(status, msg)
	}
	return errclass.UpstreamFallback(msg)
}

type reasoningEvent struct {
	ItemID       string `json:"item_id"`
	SummaryIndex int    `json:"summary_index"`
	ContentIndex int    `json:"content_index"`
	Delta        string `json:"delta"`
	Text         string `json:"text"`
}

type visibleTextPart struct {
	text         strings.Builder
	base         int
	block        int
	closed       bool
	interleaved  bool
	citations    map[string]bool
	pending      []json.RawMessage
	emittedRunes int
}

func validateOutputItem(item functionCallItem) *errclass.Error {
	switch item.Type {
	case "reasoning", "function_call", "custom_tool_call", "web_search_call":
		return nil
	case "message":
		for _, part := range item.Content {
			if err := part.unsupported(); err != nil {
				return err
			}
		}
		return nil
	}
	for key, raw := range item.Raw {
		if key != "id" && key != "type" && key != "status" && string(raw) != "null" && string(raw) != `""` && string(raw) != "[]" && string(raw) != "{}" {
			return errclass.Translation("target protocol cannot represent Responses output item")
		}
	}
	return nil
}

func knownResponseMetadataEvent(name string) bool {
	switch name {
	case "response.in_progress", "response.content_part.added", "response.content_part.done", "response.reasoning_summary_part.added", "response.reasoning_summary_part.done", "response.web_search_call.in_progress", "response.web_search_call.searching", "response.web_search_call.completed":
		return true
	}
	return false
}
