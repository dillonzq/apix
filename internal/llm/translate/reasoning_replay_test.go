package translate_test

import (
	"encoding/json"
	"fmt"
	"reflect"
	"strings"
	"testing"

	"github.com/dillonzq/apix/internal/llm/translate"
)

// Distinct rounds, parallel calls, and visible text catch both loss and
// accidental attachment of one turn's reasoning to another turn's calls.
func TestReasoningHistoryAllRequestRoutes(t *testing.T) {
	sources := map[string]string{
		translate.FormatChatCompletions: `{"messages":[{"role":"assistant","reasoning_content":" 第一轮\n🙂 ","content":"checking","tool_calls":[{"id":"a","type":"function","function":{"name":"f","arguments":"{}"}},{"id":"b","type":"function","function":{"name":"f","arguments":"{}"}}]},{"role":"tool","tool_call_id":"a","content":"A"},{"role":"tool","tool_call_id":"b","content":"B"},{"role":"assistant","reasoning_content":"second","content":null,"tool_calls":[{"id":"c","type":"function","function":{"name":"f","arguments":"{}"}}]},{"role":"tool","tool_call_id":"c","content":"C"}]}`,
		translate.FormatResponses:       `{"input":[{"type":"reasoning","content":[{"type":"reasoning_text","text":" 第一轮\n🙂 "}],"summary":[]},{"type":"message","role":"assistant","content":[{"type":"output_text","text":"checking"}]},{"type":"function_call","call_id":"a","name":"f","arguments":"{}"},{"type":"function_call","call_id":"b","name":"f","arguments":"{}"},{"type":"function_call_output","call_id":"a","output":"A"},{"type":"function_call_output","call_id":"b","output":"B"},{"type":"reasoning","content":[{"type":"reasoning_text","text":"second"}]},{"type":"function_call","call_id":"c","name":"f","arguments":"{}"},{"type":"function_call_output","call_id":"c","output":"C"}]}`,
		translate.FormatMessages:        `{"messages":[{"role":"assistant","content":[{"type":"thinking","thinking":" 第一轮\n🙂 "},{"type":"text","text":"checking"},{"type":"tool_use","id":"a","name":"f","input":{}},{"type":"tool_use","id":"b","name":"f","input":{}}]},{"role":"user","content":[{"type":"tool_result","tool_use_id":"a","content":"A"},{"type":"tool_result","tool_use_id":"b","content":"B"}]},{"role":"assistant","content":[{"type":"thinking","thinking":"second"},{"type":"tool_use","id":"c","name":"f","input":{}}]},{"role":"user","content":[{"type":"tool_result","tool_use_id":"c","content":"C"}]}]}`,
	}
	for from, body := range sources {
		for _, to := range formats {
			t.Run(from+"/"+to, func(t *testing.T) {
				state := translate.NewRequestState()
				state.Options.AllowUnsignedThinking = true // Exercise the explicit compatible-upstream mapping.
				out, err := translate.ConvertRequest(to, "deepseek-test", from, []byte(body), state)
				if err != nil {
					t.Fatal(err)
				}
				doc := object(t, out)
				got := map[string]string{}
				results := map[string]string{}
				switch to {
				case translate.FormatChatCompletions:
					for _, v := range doc["messages"].([]any) {
						m := v.(map[string]any)
						if m["role"] == "tool" {
							results[m["tool_call_id"].(string)] = m["content"].(string)
						}
						calls, _ := m["tool_calls"].([]any)
						for _, c := range calls {
							got[c.(map[string]any)["id"].(string)], _ = m["reasoning_content"].(string)
						}
					}
				case translate.FormatMessages:
					for _, v := range doc["messages"].([]any) {
						m := v.(map[string]any)
						text := ""
						for _, v := range m["content"].([]any) {
							b := v.(map[string]any)
							switch b["type"] {
							case "thinking":
								text += b["thinking"].(string)
							case "tool_use":
								got[b["id"].(string)] = text
							case "tool_result":
								results[b["tool_use_id"].(string)] = b["content"].(string)
							}
						}
					}
				case translate.FormatResponses:
					text := ""
					for _, v := range doc["input"].([]any) {
						item := v.(map[string]any)
						switch item["type"] {
						case "reasoning":
							for _, v := range item["content"].([]any) {
								p := v.(map[string]any)
								if p["type"] != "reasoning_text" {
									t.Fatal(p)
								}
								text += p["text"].(string)
							}
						case "function_call":
							got[item["call_id"].(string)] = text
						case "function_call_output":
							results[item["call_id"].(string)] = item["output"].(string)
							text = ""
						}
					}
				}
				want := map[string]string{"a": " 第一轮\n🙂 ", "b": " 第一轮\n🙂 ", "c": "second"}
				if !reflect.DeepEqual(got, want) {
					t.Fatalf("reasoning association: %v; body %s", got, out)
				}
				if !reflect.DeepEqual(results, map[string]string{"a": "A", "b": "B", "c": "C"}) {
					t.Fatalf("tool results: %v", results)
				}
			})
		}
	}
}

// TestReasoningReplayLossPolicy distinguishes representable original text
// from summaries, opaque metadata, alias conflicts, and signature losses.
func TestReasoningReplayLossPolicy(t *testing.T) {
	cases := []struct {
		name, from, to, body, path string
		loss                       bool
	}{
		{"raw", "responses", "chat-completions", `{"input":[{"type":"reasoning","content":[{"type":"reasoning_text","text":"raw"}]}]}`, "", false},
		{"chat", "chat-completions", "responses", `{"messages":[{"role":"assistant","reasoning_content":"raw"}]}`, "", false},
		{"alias", "chat-completions", "responses", `{"messages":[{"role":"assistant","reasoning_details":[{"type":"reasoning.text","text":"raw"}]}]}`, "", false},
		{"thinking", "messages", "chat-completions", `{"messages":[{"role":"assistant","content":[{"type":"thinking","thinking":"raw"}]}]}`, "", false},
		{"summary", "responses", "chat-completions", `{"input":[{"type":"reasoning","summary":[{"type":"summary_text","text":"summary-only"}]}]}`, "input[0].summary", true},
		{"summary-messages", "responses", "messages", `{"input":[{"type":"reasoning","summary":[{"type":"summary_text","text":"summary-only"}]}]}`, "input[0].summary", true},
		{"different-summary", "responses", "chat-completions", `{"input":[{"type":"reasoning","content":[{"type":"reasoning_text","text":"raw"}],"summary":[{"type":"summary_text","text":"summary-only"}]}]}`, "input[0].summary", true},
		{"encrypted", "responses", "chat-completions", `{"input":[{"type":"reasoning","encrypted_content":"opaque"}]}`, "input[0].encrypted_content", true},
		{"unknown-content", "responses", "chat-completions", `{"input":[{"type":"reasoning","content":[{"type":"future","text":"opaque"}]}]}`, "input[0].content", true},
		{"opaque-alias", "chat-completions", "responses", `{"messages":[{"role":"assistant","reasoning_details":[{"type":"reasoning.encrypted","text":"opaque"}]}]}`, "messages[0].reasoning_details", true},
		{"incomplete-empty-alias", "chat-completions", "responses", `{"messages":[{"role":"assistant","reasoning_content":"raw","reasoning_details":[{"type":"reasoning.encrypted"}]}]}`, "messages[0].reasoning_details", true},
		{"non-assistant-empty-alias", "chat-completions", "responses", `{"messages":[{"role":"user","content":"hello","reasoning_details":[]}]}`, "messages[0].reasoning_details", true},
		{"conflicting-alias", "chat-completions", "responses", `{"messages":[{"role":"assistant","reasoning_content":"raw","reasoning":"other"}]}`, "messages[0].reasoning", true},
		{"signature", "messages", "chat-completions", `{"messages":[{"role":"assistant","content":[{"type":"thinking","thinking":"raw","signature":"opaque"}]}]}`, "messages[0].content[0].signature", true},
		{"redacted", "messages", "responses", `{"messages":[{"role":"assistant","content":[{"type":"redacted_thinking","data":"opaque"}]}]}`, "messages[0].content[0]", true},
		{"unsigned-chat", "chat-completions", "messages", `{"messages":[{"role":"assistant","reasoning_content":"raw"}]}`, "messages[0].reasoning_content", true},
		{"unsigned-responses", "responses", "messages", `{"input":[{"type":"reasoning","content":[{"type":"reasoning_text","text":"raw"}]}]}`, "input[0].content", true},
	}
	for _, tc := range cases {
		for _, policy := range []translate.LossPolicy{translate.LossPolicyAllow, translate.LossPolicySafe, translate.LossPolicyStrict} {
			t.Run(tc.name+"/"+string(policy), func(t *testing.T) {
				state := translate.NewRequestState()
				state.Options.LossPolicy = policy
				out, err := translate.ConvertRequest(tc.to, "m", tc.from, []byte(tc.body), state)
				if tc.loss && policy != translate.LossPolicyAllow {
					if err == nil || err.Class != translate.ClassUnsupported || out != nil {
						t.Fatalf("loss accepted: %s %v", out, err)
					}
				} else if err != nil {
					t.Fatal(err)
				}
				if tc.loss {
					found := false
					for _, d := range state.Diagnostics() {
						found = found || (d.Path == tc.path && d.Semantic)
					}
					if !found {
						t.Fatalf("missing %s: %+v", tc.path, state.Diagnostics())
					}
				} else if len(state.Diagnostics()) != 0 {
					t.Fatalf("false loss: %+v", state.Diagnostics())
				}
				if strings.Contains(string(out), "summary-only") || strings.Contains(string(out), "opaque") {
					t.Fatalf("summary or opaque data became original reasoning: %s", out)
				}
			})
		}
	}
}

// TestDeepSeekResponseHistoryRequestRoundTrip replays ordinary and streamed
// completion items across two tool rounds, checking text ownership and deduplication.
func TestDeepSeekResponseHistoryRequestRoundTrip(t *testing.T) {
	for _, streaming := range []bool{false, true} {
		t.Run(fmt.Sprint(streaming), func(t *testing.T) {
			var history []any
			for round := 1; round <= 2; round++ {
				reasoning := fmt.Sprintf(" round %d\n思考🙂 ", round)
				call := fmt.Sprintf("call%d", round)
				var output []any
				if !streaming {
					body, _ := json.Marshal(map[string]any{"id": fmt.Sprintf("r%d", round), "choices": []any{map[string]any{"message": map[string]any{"role": "assistant", "reasoning_content": reasoning, "content": "checking", "tool_calls": []any{map[string]any{"id": call, "type": "function", "function": map[string]any{"name": "f", "arguments": "{}"}}}}, "finish_reason": "tool_calls"}}})
					out, err := translate.ConvertResponse(translate.FormatChatCompletions, translate.FormatResponses, 200, body, nil)
					if err != nil {
						t.Fatal(err)
					}
					output = object(t, out)["output"].([]any)
				} else {
					delta, _ := json.Marshal(map[string]any{"id": fmt.Sprintf("r%d", round), "choices": []any{map[string]any{"delta": map[string]any{"reasoning_content": reasoning, "content": "checking", "tool_calls": []any{map[string]any{"index": 0, "id": call, "function": map[string]any{"name": "f", "arguments": "{}"}}}}, "finish_reason": "tool_calls"}}})
					frames := streamFrames(t, translate.FormatChatCompletions, translate.FormatResponses, "data: "+string(delta)+"\n\ndata: [DONE]\n\n", nil)
					var completed []any
					for _, frame := range frames {
						if frame.Name == "response.output_item.done" {
							output = append(output, frame.Data["item"])
						}
						if frame.Name == "response.completed" {
							completed = frame.Data["response"].(map[string]any)["output"].([]any)
						}
					}
					if !reflect.DeepEqual(output, completed) {
						t.Fatalf("completed items differ: %v / %v", output, completed)
					}
					// Converting our summary display plus content snapshots must not echo text.
					var wire strings.Builder
					for _, frame := range frames {
						raw, _ := json.Marshal(frame.Data)
						wire.WriteString("event: " + frame.Name + "\ndata: " + string(raw) + "\n\n")
					}
					var got strings.Builder
					for _, frame := range streamFrames(t, translate.FormatResponses, translate.FormatChatCompletions, wire.String(), nil) {
						choices, _ := frame.Data["choices"].([]any)
						if len(choices) > 0 {
							d, _ := choices[0].(map[string]any)["delta"].(map[string]any)
							text, _ := d["reasoning_content"].(string)
							got.WriteString(text)
						}
					}
					if got.String() != reasoning {
						t.Fatalf("reasoning echoed: %q", got.String())
					}
				}
				history = append(history, output...)
				history = append(history, map[string]any{"type": "function_call_output", "call_id": call, "output": "ok"})
				payload, _ := json.Marshal(map[string]any{"input": history})
				state := translate.NewRequestState()
				state.Options.LossPolicy = translate.LossPolicyStrict
				replay, err := translate.ConvertRequest(translate.FormatChatCompletions, "deepseek-test", translate.FormatResponses, payload, state)
				if err != nil {
					t.Fatal(err)
				}
				count := 0
				for _, v := range object(t, replay)["messages"].([]any) {
					msg := v.(map[string]any)
					calls, _ := msg["tool_calls"].([]any)
					for _, v := range calls {
						count++
						tc := v.(map[string]any)
						if tc["id"] != fmt.Sprintf("call%d", count) || msg["reasoning_content"] != fmt.Sprintf(" round %d\n思考🙂 ", count) || msg["content"] != "checking" {
							t.Fatalf("replay association: %s", replay)
						}
					}
				}
				if count != round {
					t.Fatalf("calls=%d want %d: %s", count, round, replay)
				}
			}
		})
	}
}

// TestReasoningHistoryBoundaries prevents prior reasoning from attaching to
// a new call across user, system, tool-result, or reasoning boundaries.
func TestReasoningHistoryBoundaries(t *testing.T) {
	for _, boundary := range []string{
		`{"type":"message","role":"user","content":"next"}`,
		`{"type":"message","role":"system","content":"next"}`,
		`{"type":"function_call_output","call_id":"old","output":"ok"}`,
		`{"type":"reasoning","summary":[{"type":"summary_text","text":"display only"}]}`,
	} {
		t.Run(boundary, func(t *testing.T) {
			body := `{"input":[{"type":"reasoning","content":[{"type":"reasoning_text","text":"previous"}]},{"type":"function_call","call_id":"old","name":"f","arguments":"{}"},` + boundary + `,{"type":"function_call","call_id":"new","name":"f","arguments":"{}"}]}`
			out, err := translate.ConvertRequest(translate.FormatChatCompletions, "m", translate.FormatResponses, []byte(body), translate.NewRequestState())
			if err != nil {
				t.Fatal(err)
			}
			found := false
			for _, v := range object(t, out)["messages"].([]any) {
				msg := v.(map[string]any)
				calls, _ := msg["tool_calls"].([]any)
				for _, c := range calls {
					if c.(map[string]any)["id"] == "new" {
						found = true
						if _, ok := msg["reasoning_content"]; ok {
							t.Fatalf("reasoning crossed boundary: %s", out)
						}
					}
				}
			}
			if !found {
				t.Fatal("new call missing")
			}
		})
	}
}

// TestNativeReasoningHistoryPreservesOpaqueData protects native passthrough
// of reasoning, signatures, and encrypted fields even under strict policy.
func TestNativeReasoningHistoryPreservesOpaqueData(t *testing.T) {
	for from, body := range map[string]string{
		"responses":        `{"input":[{"type":"reasoning","content":[{"type":"reasoning_text","text":"raw"}],"summary":[{"type":"summary_text","text":"different"}],"encrypted_content":"opaque","future":9007199254740993}]}`,
		"messages":         `{"messages":[{"role":"assistant","content":[{"type":"thinking","thinking":"raw","signature":"authentic"},{"type":"redacted_thinking","data":"opaque"}]}]}`,
		"chat-completions": `{"messages":[{"role":"assistant","reasoning_content":"raw","reasoning_details":[{"type":"reasoning.encrypted","data":"opaque"}]}]}`,
	} {
		t.Run(from, func(t *testing.T) {
			state := translate.NewRequestState()
			state.Options.LossPolicy = translate.LossPolicyStrict
			out, err := translate.ConvertRequest(from, "m", from, []byte(body), state)
			if err != nil {
				t.Fatal(err)
			}
			got := object(t, out)
			delete(got, "model")
			if !reflect.DeepEqual(got, object(t, []byte(body))) || len(state.Diagnostics()) != 0 {
				t.Fatalf("native history changed: %s %+v", out, state.Diagnostics())
			}
		})
	}
}

// TestOpaqueChatResponseIsNotReplayableReasoning prevents typed summary or
// encrypted records from becoming synthesized original reasoning content.
func TestOpaqueChatResponseIsNotReplayableReasoning(t *testing.T) {
	for _, typ := range []string{"reasoning.encrypted", "reasoning.summary"} {
		t.Run(typ, func(t *testing.T) {
			body := []byte(`{"id":"r","choices":[{"message":{"role":"assistant","reasoning_details":[{"type":"` + typ + `","text":"not original"}],"content":"answer"},"finish_reason":"stop"}]}`)
			out, err := translate.ConvertResponse(translate.FormatChatCompletions, translate.FormatResponses, 200, body, nil)
			if err != nil {
				t.Fatal(err)
			}
			if strings.Contains(string(out), "not original") {
				t.Fatalf("opaque/summary text promoted to replay content: %s", out)
			}
		})
	}
}
