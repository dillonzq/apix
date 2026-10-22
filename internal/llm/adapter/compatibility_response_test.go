package adapters

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/dillonzq/apix/internal/llm/adapter/chatcompletions"
	"github.com/dillonzq/apix/internal/llm/adapter/messages"
	"github.com/dillonzq/apix/internal/llm/adapter/responses"
)

func TestRefusalNonStreamCompatibility(t *testing.T) {
	for _, src := range []struct {
		convert reasoningConverter
		body    string
		targets []string
	}{
		{chatcompletions.ConvertNonStreamResponse, `{"id":"r","choices":[{"message":{"content":"prefix","refusal":"拒绝"},"finish_reason":"stop"}]}`, []string{"messages", "responses"}},
		{responses.ConvertNonStreamResponse, `{"id":"r","status":"completed","output":[{"type":"message","content":[{"type":"output_text","text":"prefix"},{"type":"refusal","refusal":"拒绝"}]}]}`, []string{"messages", "chat-completions"}},
	} {
		for _, target := range src.targets {
			out, e := src.convert(target, 200, []byte(src.body))
			if e != nil {
				t.Fatal(e)
			}
			m := decodeReasoningFrame(t, out)
			if !strings.Contains(string(out), "prefix") || !strings.Contains(string(out), "拒绝") {
				t.Fatalf("text lost: %s", out)
			}
			switch target {
			case "chat-completions":
				choice := m["choices"].([]any)[0].(map[string]any)
				msg := choice["message"].(map[string]any)
				if msg["content"] != "prefix" || msg["refusal"] != "拒绝" || choice["finish_reason"] != "content_filter" {
					t.Fatalf("refusal=%s", out)
				}
			case "messages":
				if m["stop_reason"] != "refusal" {
					t.Fatalf("refusal=%s", out)
				}
			case "responses":
				content := m["output"].([]any)[0].(map[string]any)["content"].([]any)
				if len(content) != 2 || content[1].(map[string]any)["refusal"] != "拒绝" {
					t.Fatalf("refusal=%s", out)
				}
			}
		}
	}
}

func TestChatRefusalStreamPartIndexes(t *testing.T) {
	for _, order := range []string{"refusal-only", "text-first", "refusal-first"} {
		t.Run(order, func(t *testing.T) {
			text := `data: {"id":"r","choices":[{"delta":{"content":"prefix"}}]}` + "\n\n"
			refusal := `data: {"id":"r","choices":[{"delta":{"refusal":"拒绝"}}]}` + "\n\n"
			raw := refusal
			if order == "text-first" {
				raw = text + refusal
			} else if order == "refusal-first" {
				raw = refusal + text
			}
			raw += `data: {"choices":[{"delta":{},"finish_reason":"stop"}]}` + "\n\ndata: [DONE]\n\n"
			sc := chatcompletions.NewStreamConverter("responses")
			events, done, e := sc.Feed([]byte(raw))
			if e != nil || !done {
				t.Fatalf("done=%v,err=%v", done, e)
			}
			var parts []any
			deltas := map[int]string{}
			finished := map[int]int{}
			partFinished := map[int]int{}
			for _, event := range events {
				m := decodeReasoningFrame(t, event)
				switch m["type"] {
				case "response.refusal.delta", "response.output_text.delta":
					deltas[int(m["content_index"].(float64))] = m["delta"].(string)
				case "response.refusal.done", "response.output_text.done":
					finished[int(m["content_index"].(float64))]++
				case "response.content_part.done":
					partFinished[int(m["content_index"].(float64))]++
				case "response.completed":
					parts = m["response"].(map[string]any)["output"].([]any)[0].(map[string]any)["content"].([]any)
				}
			}
			if len(parts) != len(deltas) || len(finished) != len(parts) || len(partFinished) != len(parts) {
				t.Fatalf("parts=%v,deltas=%v", parts, deltas)
			}
			for i, part := range parts {
				p := part.(map[string]any)
				text, _ := p["text"].(string)
				if p["type"] == "refusal" {
					text = p["refusal"].(string)
				}
				if deltas[i] != text || finished[i] != 1 || partFinished[i] != 1 {
					t.Fatalf("part %d lifecycle mismatch: %v, deltas=%v, done=%v, partDone=%v", i, p, deltas, finished, partFinished)
				}
			}
			claude := chatcompletions.NewStreamConverter("messages")
			events, done, e = claude.Feed([]byte(raw))
			if e != nil || !done {
				t.Fatalf("done=%v,err=%v", done, e)
			}
			joined := ""
			stop := ""
			for _, event := range events {
				m := decodeReasoningFrame(t, event)
				d, _ := m["delta"].(map[string]any)
				if text, ok := d["text"].(string); ok {
					joined += text
				}
				if reason, ok := d["stop_reason"].(string); ok {
					stop = reason
				}
			}
			if !strings.Contains(joined, "拒绝") || stop != "refusal" {
				t.Fatalf("text=%q,stop=%q", joined, stop)
			}
		})
	}
}

func TestResponsesVisibleSnapshotsAndToolArguments(t *testing.T) {
	for _, target := range []string{"chat-completions", "messages"} {
		for _, mode := range []string{"terminal-only", "done-only", "partial", "full-deltas"} {
			t.Run(target+"/"+mode, func(t *testing.T) {
				sc := responses.NewStreamConverter(target)
				raw := reasoningSSE("response.created", `{"response":{"id":"r","model":"m"}}`)
				tool := `{"id":"fc","type":"function_call","call_id":"call","name":"f","arguments":"{\"q\":1}"}`
				message := `{"id":"msg","type":"message","content":[{"type":"output_text","text":"answer"},{"type":"refusal","refusal":"拒绝"}]}`
				if mode != "terminal-only" {
					raw += reasoningSSE("response.output_item.added", `{"item":{"id":"fc","type":"function_call","call_id":"call","name":"f","arguments":""}}`)
				}
				if mode == "partial" || mode == "full-deltas" {
					args := `{"item_id":"fc","delta":"{\"q\":"}`
					if mode == "full-deltas" {
						args = `{"item_id":"fc","delta":"{\"q\":1}"}`
					}
					raw += reasoningSSE("response.function_call_arguments.delta", args)
					raw += reasoningSSE("response.output_text.delta", `{"item_id":"msg","content_index":0,"delta":"ans"}`)
				}
				if mode != "terminal-only" {
					raw += reasoningSSE("response.function_call_arguments.done", `{"item_id":"fc","arguments":"{\"q\":1}"}`)
					raw += reasoningSSE("response.output_text.done", `{"item_id":"msg","content_index":0,"text":"answer"}`)
					if mode == "partial" || mode == "full-deltas" {
						raw += reasoningSSE("response.refusal.delta", `{"item_id":"msg","content_index":1,"delta":"拒"}`)
					}
					raw += reasoningSSE("response.refusal.done", `{"item_id":"msg","content_index":1,"refusal":"拒绝"}`)
					raw += reasoningSSE("response.output_item.done", `{"item":`+message+`}`)
					raw += reasoningSSE("response.output_item.done", `{"item":`+tool+`}`)
				}
				raw += reasoningSSE("response.completed", `{"type":"response.completed","response":{"id":"r","object":"response","status":"completed","output":[`+tool+`,`+message+`]}}`)
				var events [][]byte
				for _, b := range []byte(raw) {
					more, _, e := sc.Feed([]byte{b})
					if e != nil {
						t.Fatal(e)
					}
					events = append(events, more...)
				}
				if target == "messages" {
					assertMessagesToolLifecycle(t, events)
				}
				var args, text, refusal strings.Builder
				openings := 0
				for _, event := range events {
					m := decodeReasoningFrame(t, event)
					if target == "chat-completions" {
						d := m["choices"].([]any)[0].(map[string]any)["delta"].(map[string]any)
						if v, ok := d["content"].(string); ok {
							text.WriteString(v)
						}
						if v, ok := d["refusal"].(string); ok {
							refusal.WriteString(v)
						}
						if calls, ok := d["tool_calls"].([]any); ok {
							for _, v := range calls {
								c := v.(map[string]any)
								if _, ok := c["id"]; ok {
									openings++
								}
								a, _ := c["function"].(map[string]any)["arguments"].(string)
								args.WriteString(a)
							}
						}
					} else {
						if block, ok := m["content_block"].(map[string]any); ok && block["type"] == "tool_use" {
							openings++
						}
						d, _ := m["delta"].(map[string]any)
						if v, ok := d["text"].(string); ok {
							text.WriteString(v)
						}
						if v, ok := d["partial_json"].(string); ok {
							args.WriteString(v)
						}
					}
				}
				if args.String() != `{"q":1}` || openings != 1 {
					t.Fatalf("args=%q,openings=%d", args.String(), openings)
				}
				if target == "chat-completions" {
					if text.String() != "answer" || refusal.String() != "拒绝" {
						t.Fatalf("text=%q,refusal=%q", text.String(), refusal.String())
					}
				} else if text.String() != "answer拒绝" {
					t.Fatalf("text=%q", text.String())
				}
			})
		}
	}
}

func TestCacheWriteUsageAcrossRoutes(t *testing.T) {
	for _, write := range []string{"0", "2"} {
		for _, src := range []struct {
			format    string
			convert   reasoningConverter
			stream    func(string) feeder
			body, raw string
		}{
			{"chat-completions", chatcompletions.ConvertNonStreamResponse, func(f string) feeder { return chatcompletions.NewStreamConverter(f) },
				`{"id":"r","choices":[{"message":{"content":"x"},"finish_reason":"stop"}],"usage":{"prompt_tokens":10,"completion_tokens":4,"prompt_tokens_details":{"cached_tokens":3,"cache_write_tokens":` + write + `}}}`,
				`data: {"id":"r","choices":[{"delta":{"content":"x"},"finish_reason":"stop"}]}` + "\n\n" + `data: {"choices":[],"usage":{"prompt_tokens":10,"completion_tokens":4,"prompt_tokens_details":{"cached_tokens":3,"cache_write_tokens":` + write + `}}}` + "\n\ndata: [DONE]\n\n"},
			{"messages", messages.ConvertNonStreamResponse, func(f string) feeder { return messages.NewStreamConverter(f) },
				`{"id":"r","content":[{"type":"text","text":"x"}],"stop_reason":"end_turn","usage":{"input_tokens":5,"output_tokens":4,"cache_read_input_tokens":3,"cache_creation_input_tokens":` + write + `}}`,
				reasoningSSE("message_start", `{"message":{"id":"r","usage":{"input_tokens":5,"cache_read_input_tokens":3,"cache_creation_input_tokens":`+write+`}}}`) + reasoningSSE("message_delta", `{"delta":{"stop_reason":"end_turn"},"usage":{"output_tokens":4}}`) + reasoningSSE("message_stop", `{"type":"message_stop"}`)},
			{"responses", responses.ConvertNonStreamResponse, func(f string) feeder { return responses.NewStreamConverter(f) },
				`{"id":"r","status":"completed","output":[],"usage":{"input_tokens":10,"output_tokens":4,"input_tokens_details":{"cached_tokens":3,"cache_write_tokens":` + write + `}}}`,
				reasoningSSE("response.created", `{"response":{"id":"r"}}`) + reasoningSSE("response.completed", `{"type":"response.completed","response":{"id":"r","object":"response","status":"completed","output":[],"usage":{"input_tokens":10,"output_tokens":4,"input_tokens_details":{"cached_tokens":3,"cache_write_tokens":`+write+`}}}}`)},
		} {
			for _, target := range []string{"chat-completions", "messages", "responses"} {
				if target == src.format {
					continue
				}
				t.Run(src.format+"->"+target+"/write="+write, func(t *testing.T) {
					out, e := src.convert(target, 200, []byte(src.body))
					if e != nil {
						t.Fatal(e)
					}
					inputTotal := 10
					if src.format == "messages" && write == "0" {
						inputTotal = 8
					}
					checkCacheWrite(t, target, decodeReasoningFrame(t, out), write, inputTotal)
					sc := src.stream(target)
					events, done, e := sc.Feed([]byte(src.raw))
					if e != nil || !done {
						t.Fatalf("done=%v,err=%v", done, e)
					}
					found := false
					for _, event := range events {
						m := decodeReasoningFrame(t, event)
						if target == "responses" {
							if m["type"] != "response.completed" {
								continue
							}
							m = m["response"].(map[string]any)
						} else if target == "messages" && m["type"] != "message_delta" {
							continue
						}
						if _, ok := m["usage"]; ok {
							checkCacheWrite(t, target, m, write, inputTotal)
							found = true
						}
					}
					if !found {
						t.Fatal("no terminal usage")
					}
				})
			}
		}
	}
}

func checkCacheWrite(t *testing.T, target string, m map[string]any, write string, inputTotal int) {
	t.Helper()
	u := m["usage"].(map[string]any)
	var v any
	switch target {
	case "chat-completions":
		v = u["prompt_tokens_details"].(map[string]any)["cache_write_tokens"]
	case "responses":
		v = u["input_tokens_details"].(map[string]any)["cache_write_tokens"]
	case "messages":
		v = u["cache_creation_input_tokens"]
	}
	inputKey, outputKey := "input_tokens", "output_tokens"
	if target == "chat-completions" {
		inputKey, outputKey = "prompt_tokens", "completion_tokens"
	}
	expected := inputTotal
	if target == "messages" {
		expected -= 3
		if write == "2" {
			expected -= 2
		}
	}
	if u[inputKey] != float64(expected) || u[outputKey] != float64(4) {
		t.Fatalf("usage accounting changed: %v,want input=%d", u, expected)
	}
	raw, _ := json.Marshal(v)
	if string(raw) != write {
		t.Fatalf("cache write=%s,want=%s,usage=%v", raw, write, u)
	}
}

// Validate the actual block lifecycle, not just concatenated arguments.
// A duplicate tool block or a delta after stop is a protocol error even
// when the joined argument text happens to be correct.
type observedTool struct {
	name string
	args string
}

func assertMessagesToolLifecycle(t *testing.T, events [][]byte) map[string]*observedTool {
	t.Helper()
	tools := map[string]*observedTool{}
	open, last := -1, -1
	kind, toolID := "", ""
	for _, event := range events {
		m := decodeReasoningFrame(t, event)
		switch m["type"] {
		case "content_block_start":
			index := int(m["index"].(float64))
			if open != -1 || index <= last {
				t.Fatalf("invalid block start: open=%d,last=%d,event=%s", open, last, event)
			}
			open, last = index, index
			block := m["content_block"].(map[string]any)
			kind = block["type"].(string)
			if kind == "tool_use" {
				toolID, _ = block["id"].(string)
				name, _ := block["name"].(string)
				if toolID == "" || name == "" || tools[toolID] != nil {
					t.Fatalf("missing or repeated tool identity: %s", event)
				}
				tools[toolID] = &observedTool{name: name}
			}
		case "content_block_delta":
			index := int(m["index"].(float64))
			if open != index {
				t.Fatalf("delta targets a closed block: open=%d,event=%s", open, event)
			}
			delta := m["delta"].(map[string]any)
			switch delta["type"] {
			case "input_json_delta":
				if kind != "tool_use" {
					t.Fatalf("tool arguments target %q: %s", kind, event)
				}
				tools[toolID].args += delta["partial_json"].(string)
			case "text_delta":
				if kind != "text" {
					t.Fatalf("text targets %q: %s", kind, event)
				}
			case "thinking_delta":
				if kind != "thinking" {
					t.Fatalf("thinking targets %q: %s", kind, event)
				}
			}
		case "content_block_stop":
			if open != int(m["index"].(float64)) {
				t.Fatalf("stop targets a closed block: %s", event)
			}
			open = -1
		case "message_delta", "message_stop":
			if open != -1 {
				t.Fatalf("terminal event with open block %d: %s", open, event)
			}
		}
	}
	if open != -1 {
		t.Fatalf("block %d still open", open)
	}
	return tools
}

func TestMessagesToolArgumentsRemainOpenUntilCompletion(t *testing.T) {
	for _, completion := range []string{"arguments-done", "item-done", "terminal", "incomplete-terminal"} {
		t.Run(completion, func(t *testing.T) {
			sc := responses.NewStreamConverter("messages")
			first := reasoningSSE("response.created", `{"response":{"id":"r","model":"m"}}`) +
				reasoningSSE("response.output_item.added", `{"item":{"id":"fc","type":"function_call","call_id":"call","name":"f","arguments":""}}`) +
				reasoningSSE("response.function_call_arguments.delta", `{"item_id":"fc","delta":"{\"q\":"}`) +
				reasoningSSE("response.output_text.delta", `{"item_id":"msg","delta":"answer"}`) +
				reasoningSSE("response.reasoning_summary_text.delta", `{"item_id":"rs","delta":"plan"}`) +
				reasoningSSE("response.reasoning_summary_text.done", `{"item_id":"rs","text":"plan"}`)
			events, done, e := sc.Feed([]byte(first))
			if e != nil || done {
				t.Fatalf("done=%v,err=%v", done, e)
			}
			for _, event := range events {
				if strings.Contains(string(event), "text_delta") || strings.Contains(string(event), "thinking_delta") {
					t.Fatalf("content emitted before tool completion: %s", event)
				}
			}
			tool := `{"id":"fc","type":"function_call","call_id":"call","name":"f","arguments":"{\"q\":1}"}`
			tail := ""
			switch completion {
			case "arguments-done":
				tail = reasoningSSE("response.function_call_arguments.done", `{"item_id":"fc","arguments":"{\"q\":1}"}`)
			case "item-done":
				tail = reasoningSSE("response.output_item.done", `{"item":`+tool+`}`)
			}
			status, eventType := "completed", "response.completed"
			if completion == "incomplete-terminal" {
				status, eventType = "incomplete", "response.incomplete"
			}
			tail += reasoningSSE(eventType, `{"type":"`+eventType+`","response":{"id":"r","object":"response","status":"`+status+`","output":[`+tool+`]}}`)
			more, done, e := sc.Feed([]byte(tail))
			if e != nil || !done {
				t.Fatalf("done=%v,err=%v", done, e)
			}
			events = append(events, more...)
			tools := assertMessagesToolLifecycle(t, events)
			if len(tools) != 1 || tools["call"].args != `{"q":1}` {
				t.Fatalf("tools=%v", tools)
			}
			var text, reasoning strings.Builder
			for _, event := range events {
				m := decodeReasoningFrame(t, event)
				d, _ := m["delta"].(map[string]any)
				if v, ok := d["text"].(string); ok {
					text.WriteString(v)
				}
				if v, ok := d["thinking"].(string); ok {
					reasoning.WriteString(v)
				}
			}
			if text.String() != "answer" || reasoning.String() != "plan" {
				t.Fatalf("text=%q,reasoning=%q", text.String(), reasoning.String())
			}
		})
	}
}

func TestArgumentsDoneWaitsForIdentity(t *testing.T) {
	for _, target := range []string{"chat-completions", "messages"} {
		for _, identity := range []string{"added-empty", "item-done", "terminal"} {
			t.Run(target+"/"+identity, func(t *testing.T) {
				sc := responses.NewStreamConverter(target)
				events, _, e := sc.Feed([]byte(reasoningSSE("response.created", `{"response":{"id":"r","model":"m"}}`)))
				if e != nil {
					t.Fatal(e)
				}
				early, done, e := sc.Feed([]byte(reasoningSSE("response.function_call_arguments.done", `{"item_id":"fc","arguments":"{\"q\":1}"}`)))
				if e != nil || done || len(early) != 0 {
					t.Fatalf("tool announced without identity: events=%s,done=%v,err=%v", early, done, e)
				}
				tool := `{"id":"fc","type":"function_call","call_id":"call","name":"f","arguments":"{\"q\":1}"}`
				raw := ""
				if identity == "added-empty" {
					raw = reasoningSSE("response.output_item.added", `{"item":{"id":"fc","type":"function_call","call_id":"call","name":"f","arguments":""}}`)
				}
				if identity != "terminal" {
					raw += reasoningSSE("response.output_item.done", `{"item":`+tool+`}`)
				}
				raw += reasoningSSE("response.completed", `{"type":"response.completed","response":{"id":"r","object":"response","status":"completed","output":[`+tool+`]}}`)
				for _, b := range []byte(raw) {
					more, _, e := sc.Feed([]byte{b})
					if e != nil {
						t.Fatal(e)
					}
					events = append(events, more...)
				}
				if target == "messages" {
					tools := assertMessagesToolLifecycle(t, events)
					if len(tools) != 1 || tools["call"] == nil || tools["call"].name != "f" || tools["call"].args != `{"q":1}` {
						t.Fatalf("tool identity/args lost: %v", tools)
					}
				} else {
					openings := 0
					args := ""
					for _, event := range events {
						m := decodeReasoningFrame(t, event)
						d := m["choices"].([]any)[0].(map[string]any)["delta"].(map[string]any)
						if calls, ok := d["tool_calls"].([]any); ok {
							for _, v := range calls {
								c := v.(map[string]any)
								fn := c["function"].(map[string]any)
								args += fn["arguments"].(string)
								if id, ok := c["id"]; ok {
									openings++
									if id != "call" || fn["name"] != "f" {
										t.Fatalf("tool identity lost: %v", c)
									}
								}
							}
						}
					}
					if openings != 1 || args != `{"q":1}` {
						t.Fatalf("openings=%d,args=%q", openings, args)
					}
				}
			})
		}
	}
}

func TestArgumentsDoneWithoutIdentityCannotSucceed(t *testing.T) {
	for _, target := range []string{"chat-completions", "messages"} {
		sc := responses.NewStreamConverter(target)
		events, done, e := sc.Feed([]byte(reasoningSSE("response.function_call_arguments.done", `{"item_id":"fc","arguments":"{}"}`)))
		if e != nil || done || len(events) != 0 {
			t.Fatalf("early done=%v,err=%v,events=%s", done, e, events)
		}
		_, done, e = sc.Feed([]byte(reasoningSSE("response.completed", `{"type":"response.completed","response":{"id":"r","object":"response","status":"completed","output":[]}}`)))
		if e == nil || done {
			t.Fatalf("missing tool identity succeeded: done=%v,err=%v", done, e)
		}
	}
}

func TestMessagesInterleavedToolsSerializeWithoutDuplicates(t *testing.T) {
	sc := responses.NewStreamConverter("messages")
	raw := reasoningSSE("response.created", `{"response":{"id":"r","model":"m"}}`) +
		reasoningSSE("response.output_item.added", `{"item":{"id":"fa","type":"function_call","call_id":"ca","name":"a","arguments":""}}`) +
		reasoningSSE("response.function_call_arguments.delta", `{"item_id":"fa","delta":"{\"a\":"}`) +
		reasoningSSE("response.output_text.delta", `{"delta":"between"}`) +
		reasoningSSE("response.output_item.added", `{"item":{"id":"fb","type":"function_call","call_id":"cb","name":"b","arguments":""}}`) +
		reasoningSSE("response.function_call_arguments.delta", `{"item_id":"fb","delta":"{\"b\":2}"}`) +
		reasoningSSE("response.function_call_arguments.done", `{"item_id":"fb","arguments":"{\"b\":2}"}`) +
		reasoningSSE("response.output_text.delta", `{"delta":"after"}`) +
		reasoningSSE("response.function_call_arguments.done", `{"item_id":"fa","arguments":"{\"a\":1}"}`) +
		reasoningSSE("response.completed", `{"type":"response.completed","response":{"id":"r","object":"response","status":"completed","output":[]}}`)
	events, done, e := sc.Feed([]byte(raw))
	if e != nil || !done {
		t.Fatalf("done=%v,err=%v", done, e)
	}
	tools := assertMessagesToolLifecycle(t, events)
	if len(tools) != 2 || tools["ca"].args != `{"a":1}` || tools["cb"].args != `{"b":2}` {
		t.Fatalf("tools=%v", tools)
	}
	var order []string
	for _, event := range events {
		m := decodeReasoningFrame(t, event)
		if cb, ok := m["content_block"].(map[string]any); ok && cb["type"] == "tool_use" {
			order = append(order, cb["id"].(string))
		}
		if d, ok := m["delta"].(map[string]any); ok {
			if text, ok := d["text"].(string); ok {
				order = append(order, text)
			}
		}
	}
	if strings.Join(order, ",") != "ca,between,cb,after" {
		t.Fatalf("emission order=%v", order)
	}
}

func TestMessagesQueuedContentCannotCompleteOnEOF(t *testing.T) {
	sc := responses.NewStreamConverter("messages")
	raw := reasoningSSE("response.created", `{"response":{"id":"r"}}`) +
		reasoningSSE("response.output_item.added", `{"item":{"id":"fc","type":"function_call","call_id":"call","name":"f","arguments":""}}`) +
		reasoningSSE("response.function_call_arguments.delta", `{"item_id":"fc","delta":"{"}`) +
		reasoningSSE("response.output_text.delta", `{"delta":"waiting"}`)
	events, done, e := sc.Feed([]byte(raw))
	if e != nil || done {
		t.Fatalf("done=%v,err=%v", done, e)
	}
	for _, event := range events {
		if strings.Contains(string(event), "waiting") {
			t.Fatalf("queued content escaped early: %s", event)
		}
	}
	tail, e := sc.Finish()
	if e == nil || len(tail) != 0 {
		t.Fatalf("EOF synthesized success: tail=%s,err=%v", tail, e)
	}
}

func TestMessagesToolDeltaAfterCompletionReportsError(t *testing.T) {
	sc := responses.NewStreamConverter("messages")
	raw := reasoningSSE("response.created", `{"response":{"id":"r"}}`) +
		reasoningSSE("response.output_item.done", `{"item":{"id":"fc","type":"function_call","call_id":"call","name":"f","arguments":"{}"}}`)
	events, done, e := sc.Feed([]byte(raw))
	if e != nil || done {
		t.Fatalf("done=%v,err=%v", done, e)
	}
	assertMessagesToolLifecycle(t, events)
	late, done, e := sc.Feed([]byte(reasoningSSE("response.function_call_arguments.delta", `{"item_id":"fc","delta":"extra"}`)))
	if e == nil || done || len(late) != 0 {
		t.Fatalf("delta sent after tool stop: events=%s,done=%v,err=%v", late, done, e)
	}
}

func TestToolArgumentsWaitForCompleteIdentity(t *testing.T) {
	for _, target := range []string{"chat-completions", "messages"} {
		for _, order := range []string{"delta", "delta-done", "partial-item", "split-identity"} {
			t.Run(target+"/"+order, func(t *testing.T) {
				sc := responses.NewStreamConverter(target)
				events, _, e := sc.Feed([]byte(reasoningSSE("response.created", `{"response":{"id":"r","model":"m"}}`)))
				if e != nil {
					t.Fatal(e)
				}
				early := reasoningSSE("response.function_call_arguments.delta", `{"item_id":"fc","delta":"{\"q\":"}`)
				if order == "partial-item" || order == "split-identity" {
					early += reasoningSSE("response.output_item.added", `{"item":{"id":"fc","type":"function_call","call_id":"call","arguments":""}}`)
				}
				if order != "delta" {
					early += reasoningSSE("response.function_call_arguments.done", `{"item_id":"fc","arguments":"{\"q\":1}"}`)
				}
				buffered, done, e := sc.Feed([]byte(early))
				if e != nil || done || len(buffered) != 0 {
					t.Fatalf("announced before identity was complete: events=%s,done=%v,err=%v", buffered, done, e)
				}
				item := `{"id":"fc","type":"function_call","call_id":"call","name":"f","arguments":""}`
				if order == "split-identity" {
					item = `{"id":"fc","type":"function_call","name":"f","arguments":""}`
				}
				tail := reasoningSSE("response.output_item.added", `{"item":`+item+`}`) +
					reasoningSSE("response.function_call_arguments.done", `{"item_id":"fc","arguments":"{\"q\":1}"}`) +
					reasoningSSE("response.output_item.done", `{"item":{"id":"fc","type":"function_call","call_id":"call","name":"f","arguments":"{\"q\":1}"}}`) +
					reasoningSSE("response.completed", `{"type":"response.completed","response":{"id":"r","object":"response","status":"completed","output":[{"id":"fc","type":"function_call","call_id":"call","name":"f","arguments":"{\"q\":1}"}]}}`)
				for _, b := range []byte(tail) {
					more, _, e := sc.Feed([]byte{b})
					if e != nil {
						t.Fatal(e)
					}
					events = append(events, more...)
				}
				if target == "messages" {
					tools := assertMessagesToolLifecycle(t, events)
					if len(tools) != 1 || tools["call"] == nil || tools["call"].name != "f" || tools["call"].args != `{"q":1}` {
						t.Fatalf("tool identity/arguments incorrect: %v", tools)
					}
				} else {
					openings := 0
					args := ""
					for _, event := range events {
						m := decodeReasoningFrame(t, event)
						d := m["choices"].([]any)[0].(map[string]any)["delta"].(map[string]any)
						if calls, ok := d["tool_calls"].([]any); ok {
							for _, v := range calls {
								call := v.(map[string]any)
								fn := call["function"].(map[string]any)
								args += fn["arguments"].(string)
								if id, ok := call["id"]; ok {
									openings++
									if id != "call" || fn["name"] != "f" {
										t.Fatalf("incomplete identity: %v", call)
									}
								}
							}
						}
					}
					if openings != 1 || args != `{"q":1}` {
						t.Fatalf("openings=%d,args=%q", openings, args)
					}
				}
			})
		}
	}
}

func TestArgumentDeltaAndDoneWithoutIdentityCannotSucceed(t *testing.T) {
	for _, target := range []string{"chat-completions", "messages"} {
		sc := responses.NewStreamConverter(target)
		early := reasoningSSE("response.function_call_arguments.delta", `{"item_id":"fc","delta":"{"}`) +
			reasoningSSE("response.function_call_arguments.done", `{"item_id":"fc","arguments":"{}"}`)
		events, done, e := sc.Feed([]byte(early))
		if e != nil || done || len(events) != 0 {
			t.Fatalf("unknown tool identity emitted: events=%s,done=%v,err=%v", events, done, e)
		}
		_, done, e = sc.Feed([]byte(reasoningSSE("response.completed", `{"type":"response.completed","response":{"id":"r","object":"response","status":"completed","output":[]}}`)))
		if e == nil || done {
			t.Fatalf("unresolved tool identity succeeded: done=%v,err=%v", done, e)
		}
	}
}
