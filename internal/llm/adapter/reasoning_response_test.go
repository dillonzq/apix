package adapters

import (
	"encoding/json"
	"fmt"
	"strings"
	"testing"

	"github.com/dillonzq/apix/internal/errclass"
	"github.com/dillonzq/apix/internal/llm/adapter/chatcompletions"
	"github.com/dillonzq/apix/internal/llm/adapter/messages"
	"github.com/dillonzq/apix/internal/llm/adapter/responses"
	"github.com/dillonzq/apix/internal/llm/adapter/shared"
)

type reasoningConverter func(string, int, []byte, ...*shared.ConversionContext) ([]byte, *errclass.Error)

func reasoningSSE(kind, body string) string { return "event: " + kind + "\ndata: " + body + "\n\n" }

// All six routes retain Unicode, whitespace, reasoning-only turns, tools,
// and repeated thinking blocks. Byte-at-a-time input exercises SSE buffering.
func TestReasoningResponseRoutes(t *testing.T) {
	const reasoning = " 思考\n🙂继续 "
	sources := []struct {
		format  string
		convert reasoningConverter
		stream  func(string) feeder
		body    string
		sse     string
	}{
		{"chat-completions", chatcompletions.ConvertNonStreamResponse, func(f string) feeder { return chatcompletions.NewStreamConverter(f) },
			`{"id":"r","model":"m","choices":[{"message":{"reasoning_content":" 思考\n🙂继续 ","content":"answer","tool_calls":[{"id":"t","type":"function","function":{"name":"f","arguments":"{}"}}]},"finish_reason":"tool_calls"}]}`,
			"data: {\"id\":\"r\",\"model\":\"m\",\"choices\":[{\"delta\":{\"reasoning_content\":\" 思考\\n🙂\"}}]}\n\n" +
				"data: {\"choices\":[{\"delta\":{\"reasoning_content\":\"继续 \",\"content\":\"answer\",\"tool_calls\":[{\"index\":0,\"id\":\"t\",\"function\":{\"name\":\"f\",\"arguments\":\"{}\"}}]},\"finish_reason\":\"tool_calls\"}]}\n\ndata: [DONE]\n\n"},
		{"messages", messages.ConvertNonStreamResponse, func(f string) feeder { return messages.NewStreamConverter(f) },
			`{"id":"r","model":"m","content":[{"type":"thinking","thinking":" 思考\n🙂","signature":"secret"},{"type":"thinking","thinking":"继续 "},{"type":"redacted_thinking","data":"opaque"},{"type":"text","text":"answer"},{"type":"tool_use","id":"t","name":"f","input":{}}],"stop_reason":"tool_use"}`,
			reasoningSSE("message_start", `{"message":{"id":"r","model":"m"}}`) +
				reasoningSSE("content_block_start", `{"index":0,"content_block":{"type":"thinking","thinking":" 思考\n"}}`) +
				reasoningSSE("content_block_delta", `{"index":0,"delta":{"type":"thinking_delta","thinking":"🙂"}}`) +
				reasoningSSE("content_block_delta", `{"index":0,"delta":{"type":"signature_delta","signature":"secret"}}`) +
				reasoningSSE("content_block_stop", `{"index":0}`) +
				reasoningSSE("content_block_start", `{"index":1,"content_block":{"type":"thinking"}}`) +
				reasoningSSE("content_block_delta", `{"index":1,"delta":{"type":"thinking_delta","thinking":"继续 "}}`) +
				reasoningSSE("content_block_stop", `{"index":1}`) +
				reasoningSSE("content_block_start", `{"index":2,"content_block":{"type":"redacted_thinking","data":"opaque"}}`) +
				reasoningSSE("content_block_start", `{"index":3,"content_block":{"type":"text"}}`) +
				reasoningSSE("content_block_delta", `{"index":3,"delta":{"type":"text_delta","text":"answer"}}`) +
				reasoningSSE("content_block_start", `{"index":4,"content_block":{"type":"tool_use","id":"t","name":"f"}}`) +
				reasoningSSE("content_block_delta", `{"index":4,"delta":{"type":"input_json_delta","partial_json":"{}"}}`) +
				reasoningSSE("message_delta", `{"delta":{"stop_reason":"tool_use"}}`) +
				reasoningSSE("message_stop", `{"type":"message_stop"}`)},
		{"responses", responses.ConvertNonStreamResponse, func(f string) feeder { return responses.NewStreamConverter(f) },
			`{"id":"r","model":"m","status":"completed","output":[{"id":"rs","type":"reasoning","summary":[{"type":"summary_text","text":" 思考\n🙂"}],"content":[{"type":"reasoning_text","text":"继续 "}],"encrypted_content":"opaque"},{"type":"message","content":[{"type":"output_text","text":"answer"}]},{"type":"function_call","id":"fc","call_id":"t","name":"f","arguments":"{}"}]}`,
			reasoningSSE("response.created", `{"response":{"id":"r","model":"m"}}`) +
				reasoningSSE("response.reasoning_summary_text.delta", `{"item_id":"rs","delta":" 思考\n🙂"}`) +
				reasoningSSE("response.reasoning_summary_text.done", `{"item_id":"rs","text":" 思考\n🙂"}`) +
				reasoningSSE("response.reasoning_text.delta", `{"item_id":"rs","delta":"继续 "}`) +
				reasoningSSE("response.output_item.done", `{"item":{"id":"rs","type":"reasoning","summary":[{"type":"summary_text","text":" 思考\n🙂"}],"content":[{"type":"reasoning_text","text":"继续 "}],"encrypted_content":"opaque"}}`) +
				reasoningSSE("response.output_text.delta", `{"delta":"answer"}`) +
				reasoningSSE("response.output_item.added", `{"item":{"type":"function_call","id":"fc","call_id":"t","name":"f","arguments":"{}"}}`) +
				reasoningSSE("response.completed", `{"type":"response.completed","response":{"id":"r","object":"response","model":"m","status":"completed","output":[]}}`)},
	}
	for _, src := range sources {
		for _, target := range []string{"chat-completions", "messages", "responses"} {
			if src.format == target {
				continue
			}
			t.Run(src.format+"->"+target, func(t *testing.T) {
				body, e := src.convert(target, 200, []byte(src.body))
				if e != nil {
					t.Fatal(e)
				}
				if got := responseReasoning(t, target, body); got != reasoning {
					t.Fatalf("non-stream reasoning = %q; body=%s", got, body)
				}
				sc := src.stream(target)
				var events [][]byte
				for _, b := range []byte(src.sse) {
					ev, _, e := sc.Feed([]byte{b})
					if e != nil {
						t.Fatal(e)
					}
					events = append(events, ev...)
				}
				var got strings.Builder
				for _, event := range events {
					m := decodeReasoningFrame(t, event)
					switch target {
					case "chat-completions":
						if choices, ok := m["choices"].([]any); ok && len(choices) > 0 {
							delta, _ := choices[0].(map[string]any)["delta"].(map[string]any)
							if text, ok := delta["reasoning_content"].(string); ok {
								got.WriteString(text)
							}
						}
					case "messages":
						delta, _ := m["delta"].(map[string]any)
						if text, ok := delta["thinking"].(string); ok {
							got.WriteString(text)
						}
					case "responses":
						if m["type"] == "response.reasoning_summary_text.delta" {
							got.WriteString(m["delta"].(string))
						}
						if m["type"] == "response.completed" {
							raw, _ := json.Marshal(m["response"])
							if final := responseReasoning(t, target, raw); final != reasoning {
								t.Fatalf("terminal reasoning = %q", final)
							}
						}
					}
					if strings.Contains(string(event), "secret") || strings.Contains(string(event), "opaque") {
						t.Fatalf("opaque metadata leaked: %s", event)
					}
				}
				if got.String() != reasoning {
					t.Fatalf("stream reasoning = %q", got.String())
				}
			})
		}
	}
}

func decodeReasoningFrame(t *testing.T, event []byte) map[string]any {
	t.Helper()
	raw := string(event)
	if idx := strings.Index(raw, "data: "); idx >= 0 {
		raw = strings.TrimSpace(raw[idx+6:])
	}
	var m map[string]any
	if err := json.Unmarshal([]byte(raw), &m); err != nil {
		t.Fatalf("frame=%s: %v", event, err)
	}
	return m
}

func responseReasoning(t *testing.T, target string, raw []byte) string {
	t.Helper()
	m := decodeReasoningFrame(t, raw)
	if target == "chat-completions" {
		msg := m["choices"].([]any)[0].(map[string]any)["message"].(map[string]any)
		text, _ := msg["reasoning_content"].(string)
		return text
	}
	var b strings.Builder
	key := "output"
	if target == "messages" {
		key = "content"
	}
	for _, raw := range m[key].([]any) {
		item := raw.(map[string]any)
		if target == "messages" && item["type"] == "thinking" {
			b.WriteString(item["thinking"].(string))
		}
		if target == "responses" && item["type"] == "reasoning" {
			for _, p := range item["summary"].([]any) {
				b.WriteString(p.(map[string]any)["text"].(string))
			}
		}
	}
	return b.String()
}

func TestChatReasoningAliases(t *testing.T) {
	for _, field := range []string{`"reasoning_content":"primary","reasoning":"fallback"`, `"reasoning_content":null,"reasoning":[{"text":"primary"}]`, `"reasoning_details":[{"type":"reasoning.text","text":"primary"},{"type":"reasoning.encrypted","data":"secret"}]`} {
		body := []byte(fmt.Sprintf(`{"id":"r","choices":[{"message":{%s},"finish_reason":"stop"}]}`, field))
		for _, target := range []string{"messages", "responses"} {
			out, e := chatcompletions.ConvertNonStreamResponse(target, 200, body)
			if e != nil {
				t.Fatal(e)
			}
			if got := responseReasoning(t, target, out); got != "primary" {
				t.Fatalf("reasoning=%q, body=%s", got, out)
			}
			if target == "responses" {
				m := decodeReasoningFrame(t, out)
				if len(m["output"].([]any)) != 1 {
					t.Fatalf("reasoning-only turn carries phantom output: %s", out)
				}
			}
		}
	}
}

func TestResponsesReasoningSnapshots(t *testing.T) {
	item := `{"id":"rs","type":"reasoning","summary":[{"type":"summary_text","text":"plan one"},{"type":"summary_text","text":"two"}],"content":[{"type":"reasoning_text","text":"three"}]}`
	terminal := reasoningSSE("response.completed", `{"type":"response.completed","response":{"id":"r","object":"response","status":"completed","output":[`+item+`]}}`)
	for _, target := range []string{"chat-completions", "messages"} {
		for _, mode := range []string{"terminal-only", "done-only", "partial", "all-deltas"} {
			t.Run(target+"/"+mode, func(t *testing.T) {
				stream := reasoningSSE("response.created", `{"response":{"id":"r","model":"m"}}`)
				switch mode {
				case "done-only":
					stream += reasoningSSE("response.reasoning_summary_text.done", `{"item_id":"rs","text":"plan one"}`)
				case "partial":
					stream += reasoningSSE("response.reasoning_summary_text.delta", `{"item_id":"rs","delta":"plan "}`)
				case "all-deltas":
					stream += reasoningSSE("response.reasoning_summary_text.delta", `{"item_id":"rs","delta":"plan one"}`)
					stream += reasoningSSE("response.reasoning_summary_text.delta", `{"item_id":"rs","summary_index":1,"delta":"two"}`)
					stream += reasoningSSE("response.reasoning_text.delta", `{"item_id":"rs","content_index":0,"delta":"three"}`)
				}
				if mode != "terminal-only" {
					stream += reasoningSSE("response.output_item.done", `{"item":`+item+`}`)
				}
				stream += terminal
				sc := responses.NewStreamConverter(target)
				events, done, e := sc.Feed([]byte(stream))
				if e != nil || !done {
					t.Fatalf("done=%v, err=%v", done, e)
				}
				var got strings.Builder
				for _, event := range events {
					m := decodeReasoningFrame(t, event)
					if target == "chat-completions" {
						d := m["choices"].([]any)[0].(map[string]any)["delta"].(map[string]any)
						if text, ok := d["reasoning_content"].(string); ok {
							got.WriteString(text)
						}
					} else {
						d, _ := m["delta"].(map[string]any)
						if text, ok := d["thinking"].(string); ok {
							got.WriteString(text)
						}
					}
				}
				if got.String() != "plan onetwothree" {
					t.Fatalf("reasoning=%q", got.String())
				}
			})
		}
	}
}

func TestChatReasoningStreamLifecycle(t *testing.T) {
	for _, order := range []string{"r", "rt", "tra", "tar", "rat", "arar", "trart"} {
		for _, target := range []string{"messages", "responses"} {
			t.Run(order+"/"+target, func(t *testing.T) {
				var stream, expected strings.Builder
				for idx, kind := range order {
					delta := map[string]any{}
					switch kind {
					case 'r':
						delta["reasoning_details"] = []any{map[string]any{"text": "plan"}}
						expected.WriteString("plan")
					case 'a':
						delta["content"] = "answer"
					case 't':
						delta["tool_calls"] = []any{map[string]any{"index": idx, "id": fmt.Sprintf("t%d", idx), "function": map[string]any{"name": "f", "arguments": "{}"}}}
					}
					raw, _ := json.Marshal(map[string]any{"id": "r", "model": "m", "choices": []any{map[string]any{"delta": delta}}})
					stream.WriteString("data: " + string(raw) + "\n\n")
				}
				// Exercise terminal synthesis on EOF without [DONE].
				stream.WriteString("data: {\"choices\":[{\"delta\":{},\"finish_reason\":\"stop\"}]}\n\n")
				sc := chatcompletions.NewStreamConverter(target)
				events, _, e := sc.Feed([]byte(stream.String()))
				if e != nil {
					t.Fatal(e)
				}
				tail, e := sc.Finish()
				if e != nil {
					t.Fatal(e)
				}
				events = append(events, tail...)
				if target == "messages" {
					open := -1
					var got strings.Builder
					for _, event := range events {
						m := decodeReasoningFrame(t, event)
						switch m["type"] {
						case "content_block_start":
							if open >= 0 {
								t.Fatalf("overlapping blocks: %s", event)
							}
							open = int(m["index"].(float64))
						case "content_block_delta":
							if open != int(m["index"].(float64)) {
								t.Fatalf("delta references closed block: %s", event)
							}
							d := m["delta"].(map[string]any)
							if text, ok := d["thinking"].(string); ok {
								got.WriteString(text)
							}
						case "content_block_stop":
							if open != int(m["index"].(float64)) {
								t.Fatalf("stop references closed block: %s", event)
							}
							open = -1
						}
					}
					if open >= 0 || got.String() != expected.String() {
						t.Fatalf("open=%d, reasoning=%q", open, got.String())
					}
				} else {
					added := map[int]string{}
					done := map[int]bool{}
					var output []any
					for _, event := range events {
						m := decodeReasoningFrame(t, event)
						switch m["type"] {
						case "response.output_item.added":
							item := m["item"].(map[string]any)
							added[int(m["output_index"].(float64))] = item["type"].(string)
						case "response.output_item.done":
							done[int(m["output_index"].(float64))] = true
						case "response.completed":
							raw, _ := json.Marshal(m["response"])
							if got := responseReasoning(t, target, raw); got != expected.String() {
								t.Fatalf("reasoning=%q", got)
							}
							output = m["response"].(map[string]any)["output"].([]any)
						}
					}
					if len(output) != len(added) {
						t.Fatalf("output count=%d, announced=%v", len(output), added)
					}
					for i, item := range output {
						if item.(map[string]any)["type"] != added[i] || !done[i] {
							t.Fatalf("output index %d disagrees with lifecycle: %v", i, item)
						}
					}
				}
			})
		}
	}
}
