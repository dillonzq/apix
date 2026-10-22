package chatcompletions

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/dillonzq/apix/internal/errclass"
	"github.com/dillonzq/apix/internal/llm/adapter/shared"
)

type sseEvt struct {
	Name string
	Data map[string]any
}

// parseEvents decodes synthesized "event: X\ndata: {...}\n\n" frames.
func parseEvents(t *testing.T, events [][]byte) []sseEvt {
	t.Helper()
	var out []sseEvt
	for _, raw := range events {
		head, data, ok := strings.Cut(string(raw), "\ndata: ")
		if !ok || !strings.HasPrefix(head, "event: ") {
			t.Fatalf("malformed synthesized event: %q", raw)
		}
		var m map[string]any
		if err := json.Unmarshal([]byte(strings.TrimSuffix(data, "\n\n")), &m); err != nil {
			t.Fatalf("event payload not JSON: %v (%q)", err, data)
		}
		out = append(out, sseEvt{Name: strings.TrimPrefix(head, "event: "), Data: m})
	}
	return out
}

func feedAll(t *testing.T, sc *StreamConverter, lines ...string) []sseEvt {
	t.Helper()
	var all [][]byte
	for _, line := range lines {
		evs, _, eErr := sc.Feed([]byte(line + "\n"))
		if eErr != nil {
			t.Fatalf("feed %q: %v", line, eErr)
		}
		all = append(all, evs...)
	}
	return parseEvents(t, all)
}

func TestStreamConverterOpenAIPassthrough(t *testing.T) {
	sc := NewStreamConverter("chat-completions")
	frame := "data: {\"id\":\"1\",\"choices\":[{\"delta\":{\"content\":\"hi\"}}]}\n\n"
	evs, done, eErr := sc.Feed([]byte(frame[:20]))
	if eErr != nil || done || len(evs) != 0 {
		t.Fatalf("partial line must buffer: evs=%v done=%v err=%v", evs, done, eErr)
	}
	evs, done, eErr = sc.Feed([]byte(frame[20:]))
	if eErr != nil || done {
		t.Fatalf("unexpected: done=%v err=%v", done, eErr)
	}
	if len(evs) != 1 || string(evs[0]) != `{"id":"1","choices":[{"delta":{"content":"hi"}}]}` {
		t.Fatalf("bare JSON payload not extracted: %q", evs)
	}
	evs, _, eErr = sc.Feed([]byte(": keep-alive\n"))
	if eErr != nil || len(evs) != 0 {
		t.Fatalf("keep-alive line should be skipped: %v %v", evs, eErr)
	}
	if _, done, _ = sc.Feed([]byte("data: [DONE]\n")); !done {
		t.Fatal("[DONE] must set done")
	}
}

func TestStreamConverterUnsupportedFormat(t *testing.T) {
	sc := NewStreamConverter("grpc")
	_, _, eErr := sc.Feed([]byte("data: {}\n"))
	if eErr == nil || eErr.Class != errclass.ClassUnsupported {
		t.Fatalf("want ClassUnsupported, got %+v", eErr)
	}
}

func TestStreamConverterClaude(t *testing.T) {
	sc := NewStreamConverter("messages")
	var evs []sseEvt

	evs = feedAll(t, sc,
		`data: {"id":"r1","model":"m","choices":[{"index":0,"delta":{"role":"assistant"}}],"usage":{"prompt_tokens":7}}`,
		`data: {"choices":[{"delta":{"content":"He"}}]}`)
	if len(evs) != 3 {
		t.Fatalf("want message_start + text block start + text_delta, got %v", evs)
	}
	msg := evs[0].Data["message"].(map[string]any)
	if msg["id"] != "r1" || msg["role"] != "assistant" || msg["model"] != "m" {
		t.Fatalf("message_start wrong: %v", msg)
	}
	if u := msg["usage"].(map[string]any); u["input_tokens"] != float64(7) || u["output_tokens"] != float64(0) {
		t.Fatalf("message_start usage wrong: %v", u)
	}
	if evs[1].Name != "content_block_start" || evs[1].Data["index"] != float64(0) ||
		evs[1].Data["content_block"].(map[string]any)["type"] != "text" {
		t.Fatalf("text block start wrong: %v", evs[1])
	}
	if d := evs[2].Data["delta"].(map[string]any); d["type"] != "text_delta" || d["text"] != "He" {
		t.Fatalf("text_delta wrong: %v", evs[2])
	}

	// Tool call opens: closes the text block, starts tool_use at index 1.
	evs = feedAll(t, sc, `data: {"choices":[{"delta":{"tool_calls":[{"index":0,"id":"c1","function":{"name":"f","arguments":""}}]}}]}`)
	if len(evs) != 2 || evs[0].Name != "content_block_stop" || evs[0].Data["index"] != float64(0) {
		t.Fatalf("text block not closed before tool block: %v", evs)
	}
	blk := evs[1].Data["content_block"].(map[string]any)
	if evs[1].Data["index"] != float64(1) || blk["type"] != "tool_use" || blk["id"] != "c1" || blk["name"] != "f" {
		t.Fatalf("tool_use start wrong: %v", evs[1])
	}

	// Argument fragments accumulate per index across chunks.
	frag := feedAll(t, sc,
		`data: {"choices":[{"delta":{"tool_calls":[{"index":0,"function":{"arguments":"{\"k\":"}}]}}]}`,
		`data: {"choices":[{"delta":{"tool_calls":[{"index":0,"function":{"arguments":"1}"}}]}}]}`)
	if len(frag) != 2 {
		t.Fatalf("want two input_json_delta events, got %v", frag)
	}
	if d := frag[0].Data["delta"].(map[string]any); d["type"] != "input_json_delta" || d["partial_json"] != `{"k":` {
		t.Fatalf("first fragment wrong: %v", frag[0])
	}
	if d := frag[1].Data["delta"].(map[string]any); d["partial_json"] != "1}" {
		t.Fatalf("second fragment wrong: %v", frag[1])
	}

	// The second tool waits until the first tool completes, so later deltas
	// can never reference a block that has already been closed.
	evs = feedAll(t, sc, `data: {"choices":[{"delta":{"tool_calls":[{"index":1,"id":"c2","function":{"name":"g","arguments":"{\"j\":2}"}}]}}]}`)
	if len(evs) != 0 {
		t.Fatalf("second tool must stay queued: %v", evs)
	}

	// Empty-choices chunk is ignored.
	if evs = feedAll(t, sc, `data: {"id":"x","choices":[]}`); len(evs) != 0 {
		t.Fatalf("empty choices must emit nothing: %v", evs)
	}

	// finish_reason closes blocks; message_delta is held until the next
	// data line (the standard include_usage trailer follows finish).
	// Completion closes the first tool and emits the queued second tool.
	evs = feedAll(t, sc, `data: {"choices":[{"delta":{},"finish_reason":"tool_calls"}],"usage":{"prompt_tokens":7,"completion_tokens":5}}`)
	if len(evs) != 4 || evs[0].Name != "content_block_stop" || evs[1].Name != "content_block_start" || evs[2].Name != "content_block_delta" || evs[3].Name != "content_block_stop" {
		t.Fatalf("queued tool lifecycle wrong: %v", evs)
	}
	if blk := evs[1].Data["content_block"].(map[string]any); blk["id"] != "c2" || blk["name"] != "g" {
		t.Fatalf("queued identity wrong: %v", blk)
	}

	// The post-finish line flushes the held message_delta (the line's own
	// finish_reason is ignored as a repeat).
	evs = feedAll(t, sc, `data: {"choices":[{"delta":{},"finish_reason":"stop"}]}`)
	if len(evs) != 1 {
		t.Fatalf("want only the flushed message_delta, got %v", evs)
	}
	md := evs[0]
	if md.Name != "message_delta" || md.Data["delta"].(map[string]any)["stop_reason"] != "tool_use" {
		t.Fatalf("message_delta wrong: %v", md)
	}
	if u := md.Data["usage"].(map[string]any); u["input_tokens"] != float64(7) || u["output_tokens"] != float64(5) {
		t.Fatalf("message_delta usage wrong: %v", u)
	}

	// [DONE] emits message_stop and terminates.
	all, done, eErr := sc.Feed([]byte("data: [DONE]\n\n"))
	if eErr != nil || !done {
		t.Fatalf("[DONE] must finish the stream: %v", eErr)
	}
	evs = parseEvents(t, all)
	if len(evs) != 1 || evs[0].Name != "message_stop" {
		t.Fatalf("want only message_stop on [DONE], got %v", evs)
	}
}

// Pure tool-call streams (function_call deltas + done, zero content) must
// mirror non-stream chatToClaude's shape: only tool_use blocks, starting
// at index 0 — no phantom empty text block opened ahead of them (
// sibling parity).
func TestStreamConverterClaudeToolOnlyStream(t *testing.T) {
	sc := NewStreamConverter("messages")
	evs := feedAll(t, sc,
		`data: {"id":"r","model":"m","choices":[{"index":0,"delta":{"role":"assistant","tool_calls":[{"index":0,"id":"c1","function":{"name":"f","arguments":""}}]}}]}`,
		`data: {"choices":[{"delta":{"tool_calls":[{"index":0,"function":{"arguments":"{}"}}]}}]}`,
		`data: {"choices":[{"delta":{},"finish_reason":"tool_calls"}],"usage":{"prompt_tokens":3,"completion_tokens":2}}`,
		`data: [DONE]`)
	var toolStartIdx = float64(-1)
	stops := map[float64]bool{}
	for i, e := range evs {
		switch e.Name {
		case "content_block_start":
			blk := e.Data["content_block"].(map[string]any)
			if blk["type"] == "text" {
				t.Fatalf("event %d: phantom text block in tool-only stream: %v", i, e)
			}
			if blk["type"] == "tool_use" && toolStartIdx < 0 {
				toolStartIdx = e.Data["index"].(float64)
				if blk["id"] != "c1" || blk["name"] != "f" {
					t.Fatalf("tool_use start wrong: %v", e)
				}
			}
		case "content_block_stop":
			stops[e.Data["index"].(float64)] = true
		case "content_block_delta":
			if d := e.Data["delta"].(map[string]any); d["type"] != "input_json_delta" || d["partial_json"] != "{}" {
				t.Fatalf("unexpected delta in tool-only stream: %v", e)
			}
		}
	}
	if toolStartIdx != 0 {
		t.Fatalf("tool_use must start at index 0 like non-stream chatToClaude, got %v", toolStartIdx)
	}
	if !stops[0] {
		t.Fatalf("tool_use block at index 0 never stopped: %v", evs)
	}
	var md sseEvt
	for _, e := range evs {
		if e.Name == "message_delta" {
			md = e
		}
	}
	if md.Data == nil || md.Data["delta"].(map[string]any)["stop_reason"] != "tool_use" {
		t.Fatalf("stop_reason wrong: %v", md)
	}
	u := md.Data["usage"].(map[string]any)
	if u["input_tokens"] != float64(3) || u["output_tokens"] != float64(2) {
		t.Fatalf("usage wrong: %v", md)
	}
}

func TestStreamConverterClaudeInterleavedTextTool(t *testing.T) {
	sc := NewStreamConverter("messages")
	evs := feedAll(t, sc,
		`data: {"id":"r","model":"m","choices":[{"delta":{"role":"assistant"}}]}`,
		`data: {"choices":[{"delta":{"content":"a"}}]}`,
		`data: {"choices":[{"delta":{"tool_calls":[{"index":0,"id":"c1","function":{"name":"f","arguments":"{}"}}]}}]}`,
		`data: {"choices":[{"delta":{"content":"b"}}]}`,
		`data: {"choices":[{"delta":{},"finish_reason":"stop"}]}`)

	// Strict per-index lifecycle: start -> delta(s) -> stop, no reuse.
	state := map[float64]int{} // 0 unseen, 1 started, 2 delta seen, 3 stopped
	types := map[float64]string{}
	deltas := map[float64][]string{}
	for _, e := range evs {
		idx, ok := e.Data["index"].(float64)
		if !ok {
			continue
		}
		switch e.Name {
		case "content_block_start":
			if state[idx] != 0 {
				t.Fatalf("content_block_start at index %v out of order: %v", idx, evs)
			}
			state[idx] = 1
			types[idx] = e.Data["content_block"].(map[string]any)["type"].(string)
		case "content_block_delta":
			if state[idx] != 1 {
				t.Fatalf("content_block_delta at index %v out of order: %v", idx, e)
			}
			state[idx] = 2
			deltas[idx] = append(deltas[idx], e.Data["delta"].(map[string]any)["type"].(string))
		case "content_block_stop":
			if state[idx] != 1 && state[idx] != 2 {
				t.Fatalf("content_block_stop at index %v out of order: %v", idx, e)
			}
			state[idx] = 3
		}
	}
	wantTypes := map[float64]string{0: "text", 1: "tool_use", 2: "text"}
	for idx, want := range wantTypes {
		if types[idx] != want || state[idx] != 3 {
			t.Fatalf("block %v: type=%q state=%d, want %q fully closed", idx, types[idx], state[idx], want)
		}
	}
	if len(state) != 3 {
		t.Fatalf("unexpected extra blocks: %v", types)
	}
	if len(deltas[0]) != 1 || deltas[0][0] != "text_delta" ||
		len(deltas[1]) != 1 || deltas[1][0] != "input_json_delta" ||
		len(deltas[2]) != 1 || deltas[2][0] != "text_delta" {
		t.Fatalf("delta types wrong: %v", deltas)
	}
}

// assertCloseBeforeStart enforces Anthropic's close-before-next-start
// discipline: at most one content block open at any time, every start
// finding zero open blocks, and the stream ending with none open.
func assertCloseBeforeStart(t *testing.T, evs []sseEvt) {
	t.Helper()
	open := map[float64]bool{}
	for i, e := range evs {
		idx, ok := e.Data["index"].(float64)
		if !ok {
			continue
		}
		switch e.Name {
		case "content_block_start":
			if len(open) != 0 {
				t.Fatalf("event %d: content_block_start for index %v while %d block(s) still open: %v", i, idx, len(open), evs[:i+1])
			}
			open[idx] = true
		case "content_block_stop":
			if !open[idx] {
				t.Fatalf("event %d: content_block_stop for unopened index %v", i, idx)
			}
			delete(open, idx)
		}
	}
	if len(open) != 0 {
		t.Fatalf("blocks left open: %v", open)
	}
}

// PoC (a): interleaved text→tool→text must never hold two blocks open —
// the reopened text block may only start after the tool block stopped.
func TestStreamConverterClaudeInterleaveClosesBlocks(t *testing.T) {
	sc := NewStreamConverter("messages")
	evs := feedAll(t, sc,
		`data: {"id":"r","model":"m","choices":[{"delta":{"content":"a"}}]}`,
		`data: {"choices":[{"delta":{"tool_calls":[{"index":0,"id":"c1","function":{"name":"f","arguments":"{}"}}]}}]}`,
		`data: {"choices":[{"delta":{"content":"b"}}]}`,
		`data: {"choices":[{"delta":{},"finish_reason":"stop"}]}`,
		`data: [DONE]`)
	assertCloseBeforeStart(t, evs)
}

// PoC (b): consecutive tool_use blocks (new upstream tool_calls index while
// a sibling tool block is still streaming) must not overlap either.
func TestStreamConverterClaudeToolToolClosesBlocks(t *testing.T) {
	sc := NewStreamConverter("messages")
	evs := feedAll(t, sc,
		`data: {"id":"r","model":"m","choices":[{"delta":{"role":"assistant"}}]}`,
		`data: {"choices":[{"delta":{"tool_calls":[{"index":0,"id":"c1","function":{"name":"f","arguments":""}}]}}]}`,
		`data: {"choices":[{"delta":{"tool_calls":[{"index":0,"function":{"arguments":"{}"}}]}}]}`,
		`data: {"choices":[{"delta":{"tool_calls":[{"index":1,"id":"c2","function":{"name":"g","arguments":"{}"}}]}}]}`,
		`data: {"choices":[{"delta":{},"finish_reason":"tool_calls"}]}`,
		`data: [DONE]`)
	assertCloseBeforeStart(t, evs)
}

func TestStreamConverterClaudeStopReasons(t *testing.T) {
	cases := map[string]string{"stop": "end_turn", "length": "max_tokens"}
	for finish, want := range cases {
		sc := NewStreamConverter("messages")
		evs := feedAll(t, sc,
			`data: {"id":"r","model":"m","choices":[{"delta":{"role":"assistant"}}]}`,
			`data: {"choices":[{"delta":{"content":"x"}}]}`,
			`data: {"choices":[{"delta":{},"finish_reason":"`+finish+`"}]}`,
			`data: [DONE]`)
		var md sseEvt
		stops := 0
		for _, e := range evs {
			if e.Name == "message_delta" {
				md = e
			}
			if e.Name == "content_block_stop" {
				stops++
			}
		}
		if md.Data == nil || md.Data["delta"].(map[string]any)["stop_reason"] != want {
			t.Fatalf("finish %q: message_delta wrong: %v", finish, evs)
		}
		// Usage is always attached; zero-valued when upstream sent none.
		u := md.Data["usage"].(map[string]any)
		if u["input_tokens"] != float64(0) || u["output_tokens"] != float64(0) {
			t.Fatalf("finish %q: usage must default to zeros: %v", finish, md)
		}
		if stops != 1 {
			t.Fatalf("finish %q: want one text block stop, got %d", finish, stops)
		}
	}
}

func TestStreamConverterClaudeVariants(t *testing.T) {
	t.Run("choice-less usage chunk feeds the terminal message_delta", func(t *testing.T) {
		sc := NewStreamConverter("messages")
		evs := feedAll(t, sc,
			`data: {"id":"r","model":"m","choices":[{"delta":{"role":"assistant"}}]}`,
			`data: {"choices":[{"delta":{"content":"x"}}]}`,
			`data: {"choices":[],"usage":{"prompt_tokens":9,"completion_tokens":4}}`,
			`data: {"choices":[{"delta":{},"finish_reason":"stop"}]}`,
			`data: [DONE]`)
		var md sseEvt
		for _, e := range evs {
			if e.Name == "message_delta" {
				md = e
			}
		}
		u, ok := md.Data["usage"].(map[string]any)
		if !ok || u["output_tokens"] != float64(4) {
			t.Fatalf("terminal event lost choice-less usage: %v", md)
		}
	})
	t.Run("include_usage trailer after finish feeds the terminal message_delta", func(t *testing.T) {
		sc := NewStreamConverter("messages")
		evs := feedAll(t, sc,
			`data: {"id":"r","model":"m","choices":[{"delta":{"role":"assistant"}}]}`,
			`data: {"choices":[{"delta":{"content":"x"}}]}`,
			`data: {"choices":[{"delta":{},"finish_reason":"stop"}]}`,
			`data: {"choices":[],"usage":{"prompt_tokens":9,"completion_tokens":4}}`,
			`data: [DONE]`)
		mdIdx, stopIdx := -1, -1
		for i, e := range evs {
			switch e.Name {
			case "message_delta":
				mdIdx = i
			case "message_stop":
				stopIdx = i
			}
		}
		if mdIdx < 0 || stopIdx < 0 || mdIdx > stopIdx {
			t.Fatalf("message_delta must precede message_stop: %v", evs)
		}
		md := evs[mdIdx]
		if md.Data["delta"].(map[string]any)["stop_reason"] != "end_turn" {
			t.Fatalf("stop reason wrong: %v", md)
		}
		u, ok := md.Data["usage"].(map[string]any)
		if !ok || u["output_tokens"] != float64(4) {
			t.Fatalf("terminal event lost trailing include_usage usage: %v", md)
		}
	})
	t.Run("first chunk without usage starts at zero", func(t *testing.T) {
		sc := NewStreamConverter("messages")
		evs := feedAll(t, sc, `data: {"id":"r","model":"m","choices":[{"delta":{"role":"assistant"}}]}`)
		u := evs[0].Data["message"].(map[string]any)["usage"].(map[string]any)
		if u["input_tokens"] != float64(0) {
			t.Fatalf("input_tokens not defaulted: %v", u)
		}
	})
	t.Run("abrupt DONE with no content emits no phantom text block", func(t *testing.T) {
		sc := NewStreamConverter("messages")
		all, done, eErr := sc.Feed([]byte(
			"data: {\"id\":\"r\",\"choices\":[{\"delta\":{\"role\":\"assistant\"}}]}\n\ndata: [DONE]\n\n"))
		if eErr != nil || !done {
			t.Fatalf("unexpected: %v", eErr)
		}
		evs := parseEvents(t, all)
		if len(evs) != 3 || evs[0].Name != "message_start" || evs[1].Name != "message_delta" || evs[2].Name != "message_stop" {
			t.Fatalf("abrupt DONE sequence wrong: %v", evs)
		}
	})
	t.Run("CRLF line endings tolerated", func(t *testing.T) {
		sc := NewStreamConverter("messages")
		evs, done, eErr := sc.Feed([]byte("data: {\"choices\":[{\"delta\":{\"content\":\"a\"}}]}\r\n"))
		if eErr != nil || done || len(evs) != 3 {
			t.Fatalf("CRLF chunk wrong: %d events %v", len(evs), eErr)
		}
	})
	t.Run("non-data lines ignored", func(t *testing.T) {
		sc := NewStreamConverter("messages")
		if evs, _, eErr := sc.Feed([]byte(": ping\nevent: ping\n\n")); eErr != nil || len(evs) != 0 {
			t.Fatalf("comment lines must be ignored: %v %v", evs, eErr)
		}
	})
	t.Run("malformed chunk errors with short snippet", func(t *testing.T) {
		sc := NewStreamConverter("messages")
		payload := `{"choices":[{"delta":{"content":"` + strings.Repeat("x", 300) + `TAIL_SHOULD_BE_TRUNCATED"`
		_, _, eErr := sc.Feed([]byte("data: " + payload + "\n"))
		if eErr == nil || eErr.Class != errclass.ClassTranslation {
			t.Fatalf("want ClassTranslation, got %+v", eErr)
		}
		if len(eErr.Message) > 350 || strings.Contains(eErr.Message, "TAIL_SHOULD_BE_TRUNCATED") {
			t.Fatalf("error echoes too much upstream body: %q", eErr.Message)
		}
	})
	t.Run("partial line carried across feeds", func(t *testing.T) {
		sc := NewStreamConverter("messages")
		line := `data: {"choices":[{"delta":{"content":"hi"}}]}` + "\n"
		if evs, _, eErr := sc.Feed([]byte(line[:25])); eErr != nil || len(evs) != 0 {
			t.Fatalf("partial line must buffer: %v %v", evs, eErr)
		}
		evs, _, eErr := sc.Feed([]byte(line[25:]))
		if eErr != nil || len(evs) != 3 {
			t.Fatalf("completed line must emit: %d events %v", len(evs), eErr)
		}
	})
}

func TestStreamConverterResponses(t *testing.T) {
	sc := NewStreamConverter("responses")
	var evs []sseEvt

	// The role chunk primes id/model and announces response.created.
	evs = feedAll(t, sc, `data: {"id":"r1","model":"m","choices":[{"index":0,"delta":{"role":"assistant"}}]}`)
	if len(evs) != 1 || evs[0].Name != "response.created" {
		t.Fatalf("first chunk must announce response.created: %v", evs)
	}
	if resp := evs[0].Data["response"].(map[string]any); resp["id"] != "r1" || resp["status"] != "in_progress" {
		t.Fatalf("created payload wrong: %v", resp)
	}
	evs = feedAll(t, sc, `data: {"choices":[{"delta":{"content":"He"}}]}`)
	if len(evs) != 2 || evs[0].Name != "response.output_item.added" || evs[1].Name != "response.output_text.delta" {
		t.Fatalf("text chunk must announce the message item first: %v", evs)
	}
	item := evs[0].Data["item"].(map[string]any)
	if item["type"] != "message" || item["role"] != "assistant" || evs[0].Data["output_index"] != float64(0) {
		t.Fatalf("message item wrong: %v", evs[0])
	}
	td := evs[1].Data
	if td["item_id"] != "r1" || td["output_index"] != float64(0) || td["delta"] != "He" {
		t.Fatalf("output_text delta wrong: %v", evs[1])
	}

	// Tool call: registration chunk announces output_item.added after
	// the message item's output_index, then argument fragments follow.
	if evs = feedAll(t, sc, `data: {"choices":[{"delta":{"tool_calls":[{"index":0,"id":"c1","function":{"name":"f","arguments":""}}]}}]}`); len(evs) != 1 || evs[0].Name != "response.output_item.added" {
		t.Fatalf("tool registration must announce the item first: %v", evs)
	}
	if evs[0].Data["output_index"] != float64(1) {
		t.Fatalf("tool item must follow the message item's index: %v", evs[0])
	}
	evs = feedAll(t, sc,
		`data: {"choices":[{"delta":{"tool_calls":[{"index":0,"function":{"arguments":"{\"j\":"}}]}}]}`,
		`data: {"choices":[{"delta":{"tool_calls":[{"index":0,"function":{"arguments":"2}"}}]}}]}`)
	if len(evs) != 2 {
		t.Fatalf("want two argument deltas, got %v", evs)
	}
	for i, want := range []string{`{"j":`, `2}`} {
		e := evs[i]
		if e.Name != "response.function_call_arguments.delta" ||
			e.Data["item_id"] != "c1" || e.Data["output_index"] != float64(1) || e.Data["delta"] != want {
			t.Fatalf("argument delta %d wrong: %v", i, e)
		}
	}

	if evs = feedAll(t, sc, `data: {"id":"x","choices":[]}`); len(evs) != 0 {
		t.Fatalf("empty choices must emit nothing: %v", evs)
	}

	// finish_reason holds response.completed; the next data line flushes
	// it (the line's own finish_reason is ignored as a repeat).
	if evs = feedAll(t, sc, `data: {"choices":[{"delta":{},"finish_reason":"stop"}],"usage":{"prompt_tokens":7,"completion_tokens":5}}`); len(evs) != 0 {
		t.Fatalf("response.completed must be deferred past finish_reason: %v", evs)
	}
	evs = feedAll(t, sc, `data: {"choices":[{"delta":{},"finish_reason":"length"}]}`)
	completedIdx := -1
	for i, e := range evs {
		if e.Name == "response.completed" {
			completedIdx = i
		}
	}
	if completedIdx < 0 {
		t.Fatalf("missing response.completed: %v", evs)
	}
	want := []string{
		"response.output_text.done",
		"response.content_part.done",
		"response.output_item.done",
		"response.function_call_arguments.done",
		"response.output_item.done",
		"response.completed",
	}
	if completedIdx+1 != len(want) || len(evs) != len(want) {
		t.Fatalf("want %d terminal events ending in response.completed, got %v", len(want), evs)
	}
	for i, w := range want {
		if evs[i].Name != w {
			t.Fatalf("terminal event %d: want %q, got %q (full: %v)", i, w, evs[i].Name, evs)
		}
	}
	if td := evs[0].Data; td["item_id"] != "r1" || td["output_index"] != float64(0) || td["text"] != "He" {
		t.Fatalf("response.output_text.done wrong: %v", td)
	}
	if args := evs[3].Data; args["item_id"] != "c1" || args["output_index"] != float64(1) || args["arguments"] != `{"j":2}` {
		t.Fatalf("response.function_call_arguments.done wrong: %v", args)
	}
	resp := evs[completedIdx].Data["response"].(map[string]any)
	if resp["id"] != "r1" || resp["object"] != "response" || resp["status"] != "completed" {
		t.Fatalf("completed response wrong: %v", resp)
	}
	u := resp["usage"].(map[string]any)
	if u["input_tokens"] != float64(7) || u["output_tokens"] != float64(5) || u["total_tokens"] != float64(12) {
		t.Fatalf("completed usage wrong: %v", u)
	}

	if _, done, eErr := sc.Feed([]byte("data: [DONE]\n")); eErr != nil || !done {
		t.Fatalf("[DONE] must finish the stream: %v", eErr)
	}
}

func TestStreamConverterResponsesVariants(t *testing.T) {
	t.Run("choice-less usage chunk feeds response.completed", func(t *testing.T) {
		sc := NewStreamConverter("responses")
		evs := feedAll(t, sc,
			`data: {"id":"r","choices":[{"delta":{"role":"assistant"}}]}`,
			`data: {"choices":[{"delta":{"content":"x"}}]}`,
			`data: {"choices":[],"usage":{"prompt_tokens":9,"completion_tokens":4}}`,
			`data: {"choices":[{"delta":{},"finish_reason":"stop"}]}`,
			`data: [DONE]`)
		var completed sseEvt
		for _, e := range evs {
			if e.Name == "response.completed" {
				completed = e
			}
		}
		resp := completed.Data["response"].(map[string]any)
		u, ok := resp["usage"].(map[string]any)
		if !ok || u["input_tokens"] != float64(9) || u["output_tokens"] != float64(4) || u["total_tokens"] != float64(13) {
			t.Fatalf("terminal event lost choice-less usage: %v", resp)
		}
	})
	t.Run("include_usage trailer after finish feeds response.completed", func(t *testing.T) {
		sc := NewStreamConverter("responses")
		evs := feedAll(t, sc,
			`data: {"id":"r","choices":[{"delta":{"role":"assistant"}}]}`,
			`data: {"choices":[{"delta":{"content":"x"}}]}`,
			`data: {"choices":[{"delta":{},"finish_reason":"stop"}]}`,
			`data: {"choices":[],"usage":{"prompt_tokens":9,"completion_tokens":4}}`,
			`data: [DONE]`)
		var completed sseEvt
		for _, e := range evs {
			if e.Name == "response.completed" {
				completed = e
			}
		}
		resp := completed.Data["response"].(map[string]any)
		if resp["status"] != "completed" {
			t.Fatalf("status wrong: %v", resp)
		}
		output, ok := resp["output"].([]any)
		if !ok || len(output) != 1 || output[0].(map[string]any)["type"] != "message" {
			t.Fatalf("output items wrong: %v", resp["output"])
		}
		u, ok := resp["usage"].(map[string]any)
		if !ok || u["input_tokens"] != float64(9) || u["output_tokens"] != float64(4) || u["total_tokens"] != float64(13) {
			t.Fatalf("terminal event lost trailing include_usage usage: %v", resp)
		}
	})
	t.Run("length without usage maps to incomplete", func(t *testing.T) {
		sc := NewStreamConverter("responses")
		evs := feedAll(t, sc,
			`data: {"id":"r","choices":[{"delta":{"role":"assistant"}}]}`,
			`data: {"choices":[{"delta":{},"finish_reason":"length"}]}`,
			`data: [DONE]`)
		var completed sseEvt
		for _, e := range evs {
			if e.Name == "response.completed" {
				completed = e
			}
		}
		resp := completed.Data["response"].(map[string]any)
		if resp["status"] != "incomplete" {
			t.Fatalf("status wrong: %v", resp)
		}
		// Usage is always attached; zero-valued when upstream sent none.
		u := resp["usage"].(map[string]any)
		if u["input_tokens"] != float64(0) || u["output_tokens"] != float64(0) || u["total_tokens"] != float64(0) {
			t.Fatalf("usage must default to zeros: %v", resp)
		}
	})
	t.Run("malformed chunk errors", func(t *testing.T) {
		sc := NewStreamConverter("responses")
		_, _, eErr := sc.Feed([]byte("data: {bad\n"))
		if eErr == nil || eErr.Class != errclass.ClassTranslation {
			t.Fatalf("want ClassTranslation, got %+v", eErr)
		}
	})
	t.Run("in-stream error chunk fails both conversion targets", func(t *testing.T) {
		for _, source := range []string{"messages", "responses"} {
			sc := NewStreamConverter(source)
			events, done, eErr := sc.Feed([]byte("data: {\"error\":{\"message\":\"boom\",\"type\":\"server_error\"}}\n"))
			if done {
				t.Fatalf("%s: error chunk must not complete the stream", source)
			}
			if eErr == nil || eErr.Class != errclass.ClassUpstream || !eErr.Retryable {
				t.Fatalf("%s: want retryable upstream failure, got %+v", source, eErr)
			}
			if len(events) != 0 {
				t.Fatalf("%s: no partial success events may be emitted: %v", source, events)
			}
		}
	})
	t.Run("in-stream rate-limit error chunk classifies 429", func(t *testing.T) {
		sc := NewStreamConverter("messages")
		_, _, eErr := sc.Feed([]byte("data: {\"error\":{\"message\":\"slow down\",\"code\":\"rate_limit_exceeded\"}}\n"))
		if eErr == nil || eErr.Class != errclass.ClassRateLimit || !eErr.Retryable || eErr.StatusCode != 429 {
			t.Fatalf("want retryable rate limit, got %+v", eErr)
		}
	})
	t.Run("non-data lines ignored", func(t *testing.T) {
		sc := NewStreamConverter("responses")
		if evs, _, eErr := sc.Feed([]byte(": ping\n\n")); eErr != nil || len(evs) != 0 {
			t.Fatalf("comment lines must be ignored: %v %v", evs, eErr)
		}
	})
}

// Streamed tool calls + finish_reason:"length": the claude target's
// stop_reason keeps tool_use outranking max_tokens (TerminalReason),
// mirroring non-stream chatToClaude. The Responses target derives
// status from finish alone — tools live in output items. No-tools
// streams keep length/max_tokens.
func TestStreamConverterTerminalReasonToolsOutrankLength(t *testing.T) {
	t.Run("claude stop_reason tool_use", func(t *testing.T) {
		sc := NewStreamConverter("messages")
		evs := feedAll(t, sc,
			`data: {"id":"r","model":"m","choices":[{"delta":{"role":"assistant"}}]}`,
			`data: {"choices":[{"delta":{"tool_calls":[{"index":0,"id":"c1","function":{"name":"f","arguments":"{}"}}]}}]}`,
			`data: {"choices":[{"delta":{},"finish_reason":"length"}]}`,
			`data: [DONE]`)
		var md sseEvt
		for _, e := range evs {
			if e.Name == "message_delta" {
				md = e
			}
		}
		if md.Data == nil || md.Data["delta"].(map[string]any)["stop_reason"] != "tool_use" {
			t.Fatalf("tool calls must outrank length: %v", md)
		}
	})
	t.Run("responses keeps finish-derived status, tools live in output", func(t *testing.T) {
		sc := NewStreamConverter("responses")
		evs := feedAll(t, sc,
			`data: {"id":"r","choices":[{"delta":{"role":"assistant"}}]}`,
			`data: {"choices":[{"delta":{"tool_calls":[{"index":0,"id":"c1","function":{"name":"f","arguments":"{}"}}]}}]}`,
			`data: {"choices":[{"delta":{},"finish_reason":"length"}]}`,
			`data: [DONE]`)
		var completed sseEvt
		for _, e := range evs {
			if e.Name == "response.completed" {
				completed = e
			}
		}
		resp := completed.Data["response"].(map[string]any)
		// Status derives from finish alone (length→incomplete); tool
		// calls are represented by output items, not status vocabulary.
		if resp["status"] != "incomplete" || len(resp["output"].([]any)) != 1 {
			t.Fatalf("length must map to incomplete with tool item in output: %v", resp)
		}
		item := resp["output"].([]any)[0].(map[string]any)
		if item["call_id"] != "c1" {
			t.Fatalf("terminal function_call missing call_id: %v", item)
		}
	})
	t.Run("no tools keeps status mapping unchanged", func(t *testing.T) {
		sc := NewStreamConverter("messages")
		evs := feedAll(t, sc,
			`data: {"id":"r","model":"m","choices":[{"delta":{"content":"x"}}]}`,
			`data: {"choices":[{"delta":{},"finish_reason":"length"}]}`,
			`data: [DONE]`)
		for _, e := range evs {
			if e.Name == "message_delta" && e.Data["delta"].(map[string]any)["stop_reason"] != "max_tokens" {
				t.Fatalf("no-tools length must stay max_tokens: %v", e)
			}
		}
	})
}

// Malformed-chunk errors redact bearer tokens from the echoed snippet and
// stay bounded (never an upstream body echo).
func TestStreamConverterMalformedChunkRedactsBearer(t *testing.T) {
	sc := NewStreamConverter("messages")
	payload := `{"garbage":"prefix Bearer sk-secret123-token suffix ` + strings.Repeat("y", 300) + `"`
	_, _, eErr := sc.Feed([]byte("data: " + payload + "\n"))
	if eErr == nil || eErr.Class != errclass.ClassTranslation {
		t.Fatalf("want ClassTranslation, got %+v", eErr)
	}
	if strings.Contains(eErr.Message, "sk-secret123") {
		t.Fatalf("error leaks bearer token: %q", eErr.Message)
	}
	if len(eErr.Message) > 350 {
		t.Fatalf("error not bounded: %d chars", len(eErr.Message))
	}
}

// chunkError code classification: numeric JSON codes decode as
// float64, string codes may be numeric or rate-limit markers, and a
// rate-limit type upgrades an unclassified error to 429.
func TestChunkErrorCodeClassification(t *testing.T) {
	cases := []struct {
		name       string
		err        *ccChunkError
		wantClass  errclass.Class
		wantStatus int
	}{
		{"float64 numeric code", &ccChunkError{Message: "m", Code: float64(429)}, errclass.ClassRateLimit, 429},
		{"float64 server code", &ccChunkError{Message: "m", Code: float64(503)}, errclass.ClassUpstream, 503},
		{"string numeric code", &ccChunkError{Message: "m", Code: "500"}, errclass.ClassUpstream, 500},
		{"type rate limit no code", &ccChunkError{Message: "m", Type: "rate_limit_error"}, errclass.ClassRateLimit, 429},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := chunkError(tc.err)
			if got.Class != tc.wantClass || got.StatusCode != tc.wantStatus || !got.Retryable {
				t.Fatalf("want %s/%d retryable, got %+v", tc.wantClass, tc.wantStatus, got)
			}
		})
	}
}

// F-output pin: response.completed carries the output array — an
// aggregated output_text message for text-only streams.
func TestStreamResponsesCompletedOutputText(t *testing.T) {
	sc := NewStreamConverter("responses")
	evs := feedAll(t, sc,
		`data: {"id":"r9","choices":[{"index":0,"delta":{"role":"assistant"}}]}`,
		`data: {"choices":[{"delta":{"content":"Hel"}}]}`,
		`data: {"choices":[{"delta":{"content":"lo"}}]}`,
		`data: {"choices":[{"delta":{},"finish_reason":"stop"}],"usage":{"prompt_tokens":2,"completion_tokens":3}}`,
		`data: [DONE]`)
	var completed map[string]any
	for _, e := range evs {
		if e.Name == "response.completed" {
			completed = e.Data["response"].(map[string]any)
		}
	}
	if completed == nil {
		t.Fatal("missing response.completed")
	}
	output, ok := completed["output"].([]any)
	if !ok || len(output) != 1 {
		t.Fatalf("output = %v", completed["output"])
	}
	item := output[0].(map[string]any)
	if item["type"] != "message" || item["role"] != "assistant" || item["id"] != "r9" {
		t.Fatalf("message item wrong: %v", item)
	}
	content := item["content"].([]any)
	part := content[0].(map[string]any)
	if part["type"] != "output_text" || part["text"] != "Hello" {
		t.Fatalf("output_text wrong: %v", content)
	}
}

// F-output pin: function_call items land in response.completed.output with
// their complete accumulated arguments.
func TestStreamResponsesCompletedOutputFunctionCall(t *testing.T) {
	sc := NewStreamConverter("responses")
	evs := feedAll(t, sc,
		`data: {"id":"r8","choices":[{"index":0,"delta":{"role":"assistant","tool_calls":[{"index":0,"id":"cc","function":{"name":"f","arguments":""}}]}}]}`,
		`data: {"choices":[{"delta":{"tool_calls":[{"index":0,"function":{"arguments":"{\"j\":1,"}}]}}]}`,
		`data: {"choices":[{"delta":{"tool_calls":[{"index":0,"function":{"arguments":"\"k\":2}"}}]}}]}`,
		`data: {"choices":[{"delta":{},"finish_reason":"tool_calls"}],"usage":{"prompt_tokens":1,"completion_tokens":1}}`,
		`data: [DONE]`)
	var completed map[string]any
	for _, e := range evs {
		t.Logf("EVENT %s", e)
		if e.Name == "response.completed" {
			completed = e.Data["response"].(map[string]any)
		}
	}
	if completed == nil {
		t.Fatal("missing response.completed")
	}
	output, ok := completed["output"].([]any)
	if !ok || len(output) != 1 {
		t.Fatalf("output = %v", completed["output"])
	}
	item := output[0].(map[string]any)
	// Function_call items carry call_id only — no "id" key.
	if item["type"] != "function_call" || item["call_id"] != "cc" || item["name"] != "f" {
		t.Fatalf("function_call item wrong: %v", item)
	}
	if _, has := item["id"]; has {
		t.Fatalf("function_call must not carry an id key: %v", item)
	}
	if item["arguments"] != `{"j":1,"k":2}` {
		t.Fatalf("accumulated arguments wrong: %v", item["arguments"])
	}
}

// Messages-route invariant parity: when the upstream stream
// announces delta.tool_calls BEFORE delta.content — legal Chat Completions
// interleaving — response.completed.output must reproduce the streamed
// output_item.added indexes exactly: function_call at 0, message at 1,
// never displaced by unreserved text assembly.
func TestStreamResponsesToolsBeforeTextTerminalOrderMatchesAnnouncements(t *testing.T) {
	sc := NewStreamConverter("responses")
	evs := feedAll(t, sc,
		`data: {"id":"rt","model":"m","choices":[{"index":0,"delta":{"role":"assistant"}}]}`,
		`data: {"choices":[{"delta":{"tool_calls":[{"index":0,"id":"c1","function":{"name":"f","arguments":""}}]}}]}`,
		`data: {"choices":[{"delta":{"tool_calls":[{"index":0,"function":{"arguments":"{}"}}]}}]}`,
		`data: {"choices":[{"delta":{"content":"Hi"}}]}`,
		`data: {"choices":[{"delta":{},"finish_reason":"stop"}],"usage":{"prompt_tokens":1,"completion_tokens":1}}`,
		`data: [DONE]`)
	var added []sseEvt
	var completed map[string]any
	for _, e := range evs {
		switch e.Name {
		case "response.output_item.added":
			added = append(added, e)
		case "response.completed":
			completed = e.Data["response"].(map[string]any)
		}
	}
	if len(added) != 2 {
		t.Fatalf("want function_call + message announcements, got %v", added)
	}
	first := added[0].Data["item"].(map[string]any)
	if added[0].Data["output_index"] != float64(0) || first["type"] != "function_call" || first["call_id"] != "c1" {
		t.Fatalf("function_call must be announced at output_index 0: %v", added[0])
	}
	second := added[1].Data["item"].(map[string]any)
	if added[1].Data["output_index"] != float64(1) || second["type"] != "message" {
		t.Fatalf("message must be announced at output_index 1: %v", added[1])
	}
	output, ok := completed["output"].([]any)
	if !ok || len(output) != 2 {
		t.Fatalf("terminal output = %v", completed["output"])
	}
	termFC, ok := output[0].(map[string]any)
	if !ok || termFC["type"] != "function_call" || termFC["call_id"] != "c1" || termFC["arguments"] != "{}" {
		t.Fatalf("terminal output[0] must be the announced function_call: %v", output[0])
	}
	termMsg, ok := output[1].(map[string]any)
	if !ok || termMsg["type"] != "message" || termMsg["id"] != "rt" {
		t.Fatalf("terminal output[1] must be the announced message: %v", output[1])
	}
	content := termMsg["content"].([]any)
	if content[0].(map[string]any)["text"] != "Hi" {
		t.Fatalf("aggregated text lost: %v", content)
	}
}

// An announced-but-zero-fragment tool call emits "{}" arguments in the
// terminal response.completed output like every sibling (DefaultArgs).
func TestStreamResponsesCompletedZeroFragmentArgs(t *testing.T) {
	sc := NewStreamConverter("responses")
	evs := feedAll(t, sc,
		`data: {"id":"r7","choices":[{"index":0,"delta":{"role":"assistant","tool_calls":[{"index":0,"id":"cz","function":{"name":"f","arguments":""}}]}}]}`,
		`data: {"choices":[{"delta":{},"finish_reason":"tool_calls"}]}`,
		`data: [DONE]`)
	var completed map[string]any
	for _, e := range evs {
		if e.Name == "response.completed" {
			completed = e.Data["response"].(map[string]any)
		}
	}
	if completed == nil {
		t.Fatal("missing response.completed")
	}
	output, ok := completed["output"].([]any)
	if !ok || len(output) != 1 {
		t.Fatalf("output = %v", completed["output"])
	}
	if got := output[0].(map[string]any)["arguments"]; got != "{}" {
		t.Fatalf("zero-fragment arguments = %v, want {}", got)
	}
}

// The upstream may legally close after the finish_reason chunk without
// [DONE]; Flush must deliver the deferred terminal exactly once — with the
// stranded usage (input_tokens alongside output_tokens).
func TestStreamConverterFlushAfterFinishWithoutDONE(t *testing.T) {
	t.Run("claude target receives message_delta and message_stop once", func(t *testing.T) {
		sc := NewStreamConverter("messages")
		if evs := feedAll(t, sc,
			`data: {"id":"r","model":"m","choices":[{"delta":{"role":"assistant","content":"x"}}]}`,
			`data: {"choices":[{"delta":{},"finish_reason":"stop"}],"usage":{"prompt_tokens":7,"completion_tokens":5}}`,
		); len(evs) == 0 {
			t.Fatal("finish chunk lost the block stops")
		}
		flushed := parseEvents(t, sc.Flush())
		if len(flushed) != 2 || flushed[0].Name != "message_delta" || flushed[1].Name != "message_stop" {
			t.Fatalf("flush = %v", flushed)
		}
		u, ok := flushed[0].Data["usage"].(map[string]any)
		if !ok || u["input_tokens"] != float64(7) || u["output_tokens"] != float64(5) {
			t.Fatalf("flushed message_delta usage wrong: %v", flushed[0])
		}
		if again := sc.Flush(); len(again) != 0 {
			t.Fatalf("second flush must return nothing: %v", again)
		}
	})
	t.Run("responses target receives response.completed", func(t *testing.T) {
		sc := NewStreamConverter("responses")
		feedAll(t, sc,
			`data: {"id":"r2","choices":[{"delta":{"role":"assistant","content":"x"}}]}`,
			`data: {"choices":[{"delta":{},"finish_reason":"stop"}],"usage":{"prompt_tokens":3,"completion_tokens":4}}`)
		flushed := parseEvents(t, sc.Flush())
		completedIdx := -1
		for i, e := range flushed {
			if e.Name == "response.completed" {
				completedIdx = i
			}
		}
		if completedIdx < 0 {
			t.Fatalf("missing response.completed: %v", flushed)
		}
		want := []string{
			"response.output_text.done",
			"response.content_part.done",
			"response.output_item.done",
			"response.completed",
		}
		if completedIdx+1 != len(want) || len(flushed) != len(want) {
			t.Fatalf("want %d terminal events ending in response.completed, got %v", len(want), flushed)
		}
		for i, w := range want {
			if flushed[i].Name != w {
				t.Fatalf("terminal event %d: want %q, got %q (full: %v)", i, w, flushed[i].Name, flushed)
			}
		}
		if td := flushed[0].Data; td["item_id"] != "r2" || td["output_index"] != float64(0) || td["text"] != "x" {
			t.Fatalf("response.output_text.done wrong: %v", td)
		}
		resp := flushed[completedIdx].Data["response"].(map[string]any)
		u := resp["usage"].(map[string]any)
		if u["input_tokens"] != float64(3) || u["output_tokens"] != float64(4) {
			t.Fatalf("flushed completed usage wrong: %v", resp)
		}
		if again := sc.Flush(); len(again) != 0 {
			t.Fatalf("second flush must return nothing: %v", again)
		}
	})
	t.Run("no-op without a pending terminal or after DONE", func(t *testing.T) {
		passthrough := NewStreamConverter("chat-completions")
		if evs := passthrough.Flush(); len(evs) != 0 {
			t.Fatalf("passthrough flush must be a no-op: %v", evs)
		}
		sc := NewStreamConverter("messages")
		feedAll(t, sc,
			`data: {"id":"r","model":"m","choices":[{"delta":{"role":"assistant"}}]}`,
			`data: [DONE]`)
		if evs := sc.Flush(); len(evs) != 0 {
			t.Fatalf("flush after [DONE] must return nothing: %v", evs)
		}
		noFinish := NewStreamConverter("messages")
		feedAll(t, noFinish, `data: {"id":"r","model":"m","choices":[{"delta":{"content":"x"}}]}`)
		if evs := noFinish.Flush(); len(evs) != 0 {
			t.Fatalf("flush without finish_reason must return nothing: %v", evs)
		}
	})
	t.Run("claude post-finish usage trailer then close-without-DONE still emits message_stop once", func(t *testing.T) {
		sc := NewStreamConverter("messages")
		var deltaCount, stopCount int
		for _, ev := range feedAll(t, sc,
			`data: {"id":"r","model":"m","choices":[{"delta":{"content":"x"}}]}`,
			`data: {"choices":[{"delta":{},"finish_reason":"stop"}]}`,
			`data: {"id":"r","model":"m","choices":[],"usage":{"prompt_tokens":7,"completion_tokens":5}}`,
		) {
			switch ev.Name {
			case "message_delta":
				deltaCount++
				u := ev.Data["usage"].(map[string]any)
				if u["input_tokens"] != float64(7) || u["output_tokens"] != float64(5) {
					t.Fatalf("trailer usage not carried into terminal: %v", ev)
				}
			case "message_stop":
				stopCount++
			}
		}
		if deltaCount != 1 || stopCount != 0 {
			t.Fatalf("trailer must deliver exactly one message_delta and no message_stop: delta=%d stop=%d", deltaCount, stopCount)
		}
		flushed := parseEvents(t, sc.Flush())
		if len(flushed) != 1 || flushed[0].Name != "message_stop" {
			t.Fatalf("flush after delivered terminal = %v, want lone message_stop", flushed)
		}
		if again := sc.Flush(); len(again) != 0 {
			t.Fatalf("second flush must return nothing: %v", again)
		}
	})
}

// The Responses terminal must close each output
// item before response.completed — text done, part done, message done, args
// done, function-call done — so strict clients (Codex) keep assistant output
// and tool calls. Currently responsesTerminal emits only response.completed,
// so this fails until Milestones 2 & 3 land.
func TestStreamResponses_ItemDoneEvents(t *testing.T) {
	sc := NewStreamConverter("responses")
	evs := feedAll(t, sc,
		`data: {"id":"r1","model":"m","choices":[{"index":0,"delta":{"role":"assistant"}}]}`,
		`data: {"choices":[{"delta":{"content":"Hello"}}]}`,
		`data: {"choices":[{"delta":{"tool_calls":[{"index":0,"id":"c1","function":{"name":"f","arguments":"{\"a\":1}"}}]}}]}`,
		`data: {"choices":[{"delta":{},"finish_reason":"stop"}],"usage":{"prompt_tokens":7,"completion_tokens":5}}`,
		`data: [DONE]`)

	completedIdx := -1
	for i, e := range evs {
		if e.Name == "response.completed" {
			completedIdx = i
		}
	}
	if completedIdx < 0 {
		t.Fatalf("missing response.completed: %v", evs)
	}
	want := []string{
		"response.output_text.done",
		"response.content_part.done",
		"response.output_item.done",
		"response.function_call_arguments.done",
		"response.output_item.done",
		"response.completed",
	}
	if completedIdx+1 < len(want) {
		t.Fatalf("want %d terminal events ending in response.completed, got %d events: %v", len(want), len(evs), evs)
	}
	base := completedIdx + 1 - len(want)
	for i, w := range want {
		if evs[base+i].Name != w {
			t.Fatalf("terminal event %d: want %q, got %q (full: %v)", i, w, evs[base+i].Name, evs)
		}
	}

	textDone := evs[base].Data
	if textDone["item_id"] != "r1" || textDone["output_index"] != float64(0) || textDone["text"] != "Hello" {
		t.Fatalf("response.output_text.done wrong: %v", textDone)
	}

	partDone := evs[base+1].Data
	if partDone["item_id"] != "r1" || partDone["output_index"] != float64(0) {
		t.Fatalf("response.content_part.done identity wrong: %v", partDone)
	}
	part, ok := partDone["part"].(map[string]any)
	if !ok || part["type"] != "output_text" || part["text"] != "Hello" {
		t.Fatalf("response.content_part.done part wrong: %v", partDone)
	}

	msgDone := evs[base+2].Data
	if msgDone["output_index"] != float64(0) {
		t.Fatalf("message output_item.done index wrong: %v", msgDone)
	}
	msgItem, ok := msgDone["item"].(map[string]any)
	if !ok || msgItem["type"] != "message" || msgItem["id"] != "r1" || msgItem["role"] != "assistant" {
		t.Fatalf("message output_item.done item wrong: %v", msgDone)
	}
	msgContent, ok := msgItem["content"].([]any)
	if !ok || len(msgContent) != 1 || msgContent[0].(map[string]any)["text"] != "Hello" {
		t.Fatalf("message output_item.done content wrong: %v", msgItem)
	}

	argsDone := evs[base+3].Data
	itemRef, _ := argsDone["item_id"].(string)
	if itemRef == "" {
		itemRef, _ = argsDone["call_id"].(string)
	}
	if itemRef != "c1" || argsDone["output_index"] != float64(1) || argsDone["arguments"] != `{"a":1}` {
		t.Fatalf("response.function_call_arguments.done wrong: %v", argsDone)
	}

	fnDone := evs[base+4].Data
	if fnDone["output_index"] != float64(1) {
		t.Fatalf("function_call output_item.done index wrong: %v", fnDone)
	}
	fnItem, ok := fnDone["item"].(map[string]any)
	if !ok || fnItem["type"] != "function_call" || fnItem["call_id"] != "c1" || fnItem["name"] != "f" || fnItem["arguments"] != `{"a":1}` {
		t.Fatalf("function_call output_item.done item wrong: %v", fnDone)
	}
}

func TestStreamResponsesCustomToolCallRestored(t *testing.T) {
	rt := shared.NewConversionContext()
	reqBody := `{"input":"hi","tools":[{"type":"custom","name":"exec"}]}`
	if _, eErr := BuildRequest("m", "responses", []byte(reqBody), rt); eErr != nil {
		t.Fatalf("BuildRequest: %v", eErr)
	}
	sc := NewStreamConverter("responses", rt)
	evs := feedAll(t, sc,
		`data: {"id":"r1","model":"m","choices":[{"index":0,"delta":{"role":"assistant"}}]}`,
		`data: {"choices":[{"delta":{"tool_calls":[{"index":0,"id":"c1","function":{"name":"exec","arguments":""}}]}}]}`)
	var added map[string]any
	for _, e := range evs {
		if e.Name == "response.output_item.added" {
			added = e.Data
		}
	}
	if added == nil {
		t.Fatalf("missing response.output_item.added: %v", evs)
	}
	item, ok := added["item"].(map[string]any)
	if !ok {
		t.Fatalf("added item not an object: %v", added)
	}
	if item["type"] != "custom_tool_call" {
		t.Fatalf("added item.type = %v want custom_tool_call (full: %v)", item["type"], item)
	}
	if item["call_id"] != "c1" {
		t.Fatalf("added item.call_id = %v want c1", item["call_id"])
	}
	if item["name"] != "exec" {
		t.Fatalf("added item.name = %v want exec", item["name"])
	}
	if item["input"] != "" {
		t.Fatalf("added item.input = %v want empty string", item["input"])
	}

	evs = feedAll(t, sc,
		`data: {"choices":[{"delta":{"tool_calls":[{"index":0,"function":{"arguments":"{\"input\":\"ls -la\"}"}}]}}]}`)
	if len(evs) != 1 {
		t.Fatalf("want one argument delta, got %v", evs)
	}
	if evs[0].Name != "response.custom_tool_call_input.delta" {
		t.Fatalf("delta event = %q want response.custom_tool_call_input.delta (full: %v)", evs[0].Name, evs[0])
	}
	if evs[0].Data["delta"] != "ls -la" {
		t.Fatalf("delta = %v want ls -la", evs[0].Data["delta"])
	}
}

// Fragmented custom-tool arguments must not leak raw {"input":" wrapper
// tokens into client custom tool input deltas: the three upstream JSON
// fragments below concatenate to {"input":"hello"}, so the client must
// see only "hello" across its input deltas.
func TestStreamResponsesCustomToolFragmentedDeltasDoNotLeakWrapper(t *testing.T) {
	rt := shared.NewConversionContext()
	reqBody := `{"input":"hi","tools":[{"type":"custom","name":"exec"}]}`
	if _, eErr := BuildRequest("m", "responses", []byte(reqBody), rt); eErr != nil {
		t.Fatalf("BuildRequest: %v", eErr)
	}
	sc := NewStreamConverter("responses", rt)
	evs := feedAll(t, sc,
		`data: {"id":"r1","model":"m","choices":[{"index":0,"delta":{"role":"assistant"}}]}`,
		`data: {"choices":[{"delta":{"tool_calls":[{"index":0,"id":"c1","function":{"name":"exec","arguments":""}}]}}]}`,
		`data: {"choices":[{"delta":{"tool_calls":[{"index":0,"function":{"arguments":"{\"input\":\""}}]}}]}`,
		`data: {"choices":[{"delta":{"tool_calls":[{"index":0,"function":{"arguments":"hello"}}]}}]}`,
		`data: {"choices":[{"delta":{"tool_calls":[{"index":0,"function":{"arguments":"\"}"}}]}}]}`)
	var got []string
	for _, e := range evs {
		switch e.Name {
		case "response.function_call_arguments.delta":
			t.Fatalf("custom tool must not emit function args deltas: %v", e)
		case "response.custom_tool_call_input.delta":
			delta, _ := e.Data["delta"].(string)
			if strings.Contains(delta, `{"input":"`) {
				t.Fatalf("raw wrapper leaked into input delta: %q (full: %v)", delta, evs)
			}
			got = append(got, delta)
		}
	}
	if strings.Join(got, "") != "hello" {
		t.Fatalf("joined input deltas = %q want %q (full: %v)", strings.Join(got, ""), "hello", evs)
	}
}

func TestStreamResponsesNamespaceRestored(t *testing.T) {
	rt := shared.NewConversionContext()
	reqBody := `{"input":"hi","tools":[{"type":"namespace","name":"subagents","tools":[{"type":"function","name":"spawn_agent","parameters":{"type":"object"}}]}]}`
	if _, eErr := BuildRequest("m", "responses", []byte(reqBody), rt); eErr != nil {
		t.Fatalf("BuildRequest: %v", eErr)
	}
	sc := NewStreamConverter("responses", rt)
	evs := feedAll(t, sc,
		`data: {"id":"r1","model":"m","choices":[{"index":0,"delta":{"role":"assistant"}}]}`,
		`data: {"choices":[{"delta":{"tool_calls":[{"index":0,"id":"c1","function":{"name":"subagents__spawn_agent","arguments":"{}"}}]}}]}`,
		`data: {"choices":[{"delta":{},"finish_reason":"tool_calls"}]}`,
		`data: [DONE]`)
	var added, argsDone, itemDone, completed map[string]any
	for _, e := range evs {
		switch e.Name {
		case "response.output_item.added":
			added = e.Data
		case "response.function_call_arguments.done":
			argsDone = e.Data
		case "response.output_item.done":
			itemDone = e.Data
		case "response.completed":
			completed = e.Data
		}
	}
	if added == nil || argsDone == nil || itemDone == nil || completed == nil {
		t.Fatalf("missing lifecycle events: added=%v argsDone=%v itemDone=%v completed=%v", added != nil, argsDone != nil, itemDone != nil, completed != nil)
	}
	if item := added["item"].(map[string]any); item["name"] != "spawn_agent" || item["namespace"] != "subagents" {
		t.Fatalf("ItemAdded identity not restored: %v", item)
	}
	if argsDone["item_id"] != "c1" || argsDone["arguments"] != "{}" {
		t.Fatalf("ArgsDone wrong: %v", argsDone)
	}
	if item := itemDone["item"].(map[string]any); item["name"] != "spawn_agent" || item["namespace"] != "subagents" {
		t.Fatalf("ItemDone identity not restored: %v", item)
	}
	output := completed["response"].(map[string]any)["output"].([]any)
	if len(output) != 1 {
		t.Fatalf("terminal output = %v", completed["response"])
	}
	if fc := output[0].(map[string]any); fc["name"] != "spawn_agent" || fc["namespace"] != "subagents" {
		t.Fatalf("terminal identity not restored: %v", fc)
	}
}
