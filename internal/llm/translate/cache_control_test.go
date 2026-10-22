package translate

import (
	"bytes"
	"encoding/json"
	"reflect"
	"strings"
	"testing"
)

func TestCacheControlToMessages(t *testing.T) {
	for _, source := range []string{FormatChatCompletions, FormatResponses} {
		t.Run(source, func(t *testing.T) {
			var body string
			if source == FormatChatCompletions {
				body = `{"messages":[
     {"role":"system","content":[{"type":"text","text":"s1","cache_control":{"type":"ephemeral","ttl":"1h"}},{"type":"text","text":"s2"}],"cache_control":{"type":"ephemeral"}},
     {"role":"user","content":[{"type":"text","text":"u1","cache_control":{"type":"ephemeral","ttl":"1h"}},{"type":"image_url","image_url":{"url":"https://example.com/i"}}],"cache_control":{"type":"ephemeral"}},
     {"role":"assistant","content":[{"type":"text","text":"a1","cache_control":{"type":"ephemeral","ttl":"1h"}},{"type":"text","text":"a2"}],"tool_calls":[{"id":"c","type":"function","function":{"name":"f","arguments":"{}"}}],"cache_control":{"type":"ephemeral"}},
     {"role":"tool","tool_call_id":"c","content":[{"type":"text","text":"result","cache_control":{"type":"ephemeral","ttl":"1h"}}],"cache_control":{"type":"ephemeral"}},
     {"role":"user","content":"next","cache_control":{"type":"ephemeral","ttl":"1h"}}
    ],"tools":[{"type":"function","cache_control":{"type":"ephemeral"},"function":{"name":"f","cache_control":{"type":"ephemeral","ttl":"1h"}}},{"type":"function","function":{"name":"g","cache_control":{"type":"ephemeral","ttl":"1h"}}}]}`
			} else {
				body = `{"input":[
     {"role":"system","content":[{"type":"input_text","text":"s1","cache_control":{"type":"ephemeral","ttl":"1h"}},{"type":"input_text","text":"s2"}],"cache_control":{"type":"ephemeral"}},
     {"type":"message","role":"user","content":[{"type":"input_text","text":"u1","cache_control":{"type":"ephemeral","ttl":"1h"}},{"type":"input_image","image_url":"https://example.com/i"}],"cache_control":{"type":"ephemeral"}},
     {"type":"message","role":"assistant","content":[{"type":"output_text","text":"a1","cache_control":{"type":"ephemeral","ttl":"1h"}},{"type":"output_text","text":"a2"}]},
     {"type":"function_call","call_id":"c","name":"f","arguments":"{}","cache_control":{"type":"ephemeral"}},
     {"type":"function_call_output","call_id":"c","output":[{"type":"input_text","text":"result","cache_control":{"type":"ephemeral","ttl":"1h"}}],"cache_control":{"type":"ephemeral"}},
     {"role":"user","content":"next","cache_control":{"type":"ephemeral","ttl":"1h"}}
    ],"tools":[{"type":"function","name":"f","cache_control":{"type":"ephemeral"}},{"type":"namespace","name":"ns","tools":[{"type":"custom","name":"g","cache_control":{"type":"ephemeral","ttl":"1h"}}]}]}`
			}
			state := &RequestState{Options: ConversionOptions{LossPolicy: LossPolicyStrict}}
			out, err := ConvertRequest(FormatMessages, "m", source, []byte(body), state)
			if err != nil {
				t.Fatal(err)
			}
			if len(state.Diagnostics()) != 0 {
				t.Fatalf("unexpected losses: %v", state.Diagnostics())
			}
			var doc map[string]any
			if err := json.Unmarshal(out, &doc); err != nil {
				t.Fatal(err)
			}
			hour := map[string]any{"type": "ephemeral", "ttl": "1h"}
			short := map[string]any{"type": "ephemeral"}
			check := func(v any, want any) {
				t.Helper()
				if !reflect.DeepEqual(v, want) {
					t.Fatalf("got %v, want %v; body=%s", v, want, out)
				}
			}
			system := doc["system"].([]any)
			check(system[0].(map[string]any)["cache_control"], hour)
			check(system[1].(map[string]any)["cache_control"], short)
			messages := doc["messages"].([]any)
			if len(messages) != 3 {
				t.Fatalf("messages=%s", out)
			}
			user := messages[0].(map[string]any)["content"].([]any)
			check(user[0].(map[string]any)["cache_control"], hour)
			check(user[1].(map[string]any)["cache_control"], short)
			assistant := messages[1].(map[string]any)["content"].([]any)
			check(assistant[0].(map[string]any)["cache_control"], hour)
			check(assistant[1].(map[string]any)["cache_control"], nil)
			check(assistant[2].(map[string]any)["cache_control"], short)
			result := messages[2].(map[string]any)["content"].([]any)
			check(result[0].(map[string]any)["cache_control"], hour)
			check(result[0].(map[string]any)["content"], "result")
			check(result[1].(map[string]any)["cache_control"], hour)
			tools := doc["tools"].([]any)
			check(tools[0].(map[string]any)["cache_control"], short)
			check(tools[1].(map[string]any)["cache_control"], hour)
		})
	}
}

func TestCacheControlPrecedenceAndSingleBlock(t *testing.T) {
	body := []byte(`{"messages":[{"role":"user","content":[{"type":"text","text":"x","cache_control":{"type":"ephemeral","ttl":"1h"}}],"cache_control":{"type":"ephemeral"}},{"role":"assistant","content":"reply","cache_control":{"type":"ephemeral"}}]}`)
	out, err := ConvertRequest(FormatMessages, "m", FormatChatCompletions, body, nil)
	if err != nil {
		t.Fatal(err)
	}
	var doc struct {
		Messages []struct {
			Content []struct {
				Cache map[string]any `json:"cache_control"`
			} `json:"content"`
		} `json:"messages"`
	}
	if err := json.Unmarshal(out, &doc); err != nil {
		t.Fatalf("cache block flattened: %s: %v", out, err)
	}
	if doc.Messages[0].Content[0].Cache["ttl"] != "1h" || doc.Messages[1].Content[0].Cache["type"] != "ephemeral" {
		t.Fatalf("cache lost: %s", out)
	}
}

func TestCacheControlLossOutsideMessages(t *testing.T) {
	for _, tc := range []struct{ from, to, body string }{
		{FormatChatCompletions, FormatResponses, `{"messages":[{"role":"user","content":"x","cache_control":{"type":"ephemeral"}}]}`},
		{FormatResponses, FormatChatCompletions, `{"input":[{"role":"user","content":"x","cache_control":{"type":"ephemeral"}}]}`},
		{FormatMessages, FormatChatCompletions, `{"messages":[{"role":"user","content":[{"type":"text","text":"x","cache_control":{"type":"ephemeral"}}]}]}`},
	} {
		state := &RequestState{Options: ConversionOptions{LossPolicy: LossPolicyStrict}}
		if _, err := ConvertRequest(tc.to, "m", tc.from, []byte(tc.body), state); err == nil {
			t.Fatalf("expected cache loss: %s", tc.body)
		}
	}
}

func TestCacheControlDocumentAndAdditionalTools(t *testing.T) {
	state := &RequestState{Options: ConversionOptions{LossPolicy: LossPolicyStrict}}
	body := []byte(`{"instructions":"base","input":[{"role":"user","content":[{"type":"input_file","file_url":"https://example.com/f.pdf","cache_control":{"type":"ephemeral","ttl":"1h","extension":9007199254740993}}]},{"type":"additional_tools","tools":[{"type":"function","name":"f","cache_control":{"type":"ephemeral"}}]}]}`)
	out, err := ConvertRequest(FormatMessages, "m", FormatResponses, body, state)
	if err != nil {
		t.Fatal(err)
	}
	var doc struct {
		System   string `json:"system"`
		Messages []struct {
			Content []struct {
				Type  string          `json:"type"`
				Cache json.RawMessage `json:"cache_control"`
			} `json:"content"`
		} `json:"messages"`
		Tools []struct {
			Cache json.RawMessage `json:"cache_control"`
		} `json:"tools"`
	}
	if err := json.Unmarshal(out, &doc); err != nil {
		t.Fatal(err)
	}
	var cache map[string]json.RawMessage
	json.Unmarshal(doc.Messages[0].Content[0].Cache, &cache)
	if doc.System != "base" || doc.Messages[0].Content[0].Type != "document" || string(cache["extension"]) != "9007199254740993" || len(doc.Tools[0].Cache) == 0 {
		t.Fatalf("lost hints: %s", out)
	}
}

func TestCacheControlEmptyMessageDoesNotAffectPreviousTurn(t *testing.T) {
	body := []byte(`{"messages":[{"role":"user","content":"x"},{"role":"assistant","content":"","cache_control":{"type":"ephemeral"}}]}`)
	state := NewRequestState()
	out, err := ConvertRequest(FormatMessages, "m", FormatChatCompletions, body, state)
	if err != nil {
		t.Fatal(err)
	}
	var doc struct {
		Messages []struct {
			Content string `json:"content"`
		} `json:"messages"`
	}
	if err := json.Unmarshal(out, &doc); err != nil {
		t.Fatalf("empty hint applied to previous message: %s", out)
	}
	if len(state.Diagnostics()) != 1 || state.Diagnostics()[0].Path != "messages[1].cache_control" {
		t.Fatalf("missing empty cache diagnostic: %v", state.Diagnostics())
	}
}

func TestToolResultCacheControlBoundaries(t *testing.T) {
	for _, source := range []string{FormatChatCompletions, FormatResponses} {
		for _, tc := range []struct {
			name, parts, wantTTL string
			lost                 bool
		}{
			{"last text", `[{"type":"text","text":"a"},{"type":"text","text":"b","cache_control":{"type":"ephemeral","ttl":"1h"}}]`, "1h", false},
			{"last image", `[{"type":"text","text":"a"},{"type":"image_url","image_url":{"url":"https://example.com/i"},"cache_control":{"type":"ephemeral","ttl":"1h"}}]`, "1h", false},
			{"earlier only", `[{"type":"text","text":"a","cache_control":{"type":"ephemeral","ttl":"1h"}},{"type":"text","text":"b"}]`, "5m", true},
			{"earlier and last", `[{"type":"text","text":"a","cache_control":{"type":"ephemeral","ttl":"5m"}},{"type":"text","text":"b","cache_control":{"type":"ephemeral","ttl":"1h"}}]`, "1h", true},
			{"empty tail", `[{"type":"text","text":"a","cache_control":{"type":"ephemeral","ttl":"1h"}},{"type":"text","text":""}]`, "5m", true},
			{"empty last hint", `[{"type":"text","text":"a"},{"type":"text","text":"","cache_control":{"type":"ephemeral","ttl":"1h"}}]`, "5m", true},
		} {
			t.Run(source+"/"+tc.name, func(t *testing.T) {
				envelope := map[string]any{}
				prefix := "messages[0].content["
				if source == FormatChatCompletions {
					envelope["messages"] = []any{map[string]any{"role": "tool", "tool_call_id": "c", "content": json.RawMessage(tc.parts), "cache_control": map[string]any{"type": "ephemeral", "ttl": "5m"}}}
				} else {
					prefix = "input[0].output["
					envelope["input"] = []any{map[string]any{"type": "custom_tool_call_output", "call_id": "c", "output": json.RawMessage(tc.parts), "cache_control": map[string]any{"type": "ephemeral", "ttl": "5m"}}}
				}
				body, err := json.Marshal(envelope)
				if err != nil {
					t.Fatal(err)
				}
				for _, policy := range []LossPolicy{LossPolicyAllow, LossPolicySafe, LossPolicyStrict} {
					state := &RequestState{Options: ConversionOptions{LossPolicy: policy}}
					out, err := ConvertRequest(FormatMessages, "m", source, body, state)
					if tc.lost && policy == LossPolicyStrict {
						if err == nil || out != nil {
							t.Fatalf("strict accepted lost hint: %s", out)
						}
					} else {
						if err != nil {
							t.Fatal(err)
						}
						var doc struct {
							Messages []struct {
								Content []map[string]any `json:"content"`
							} `json:"messages"`
						}
						if err := json.Unmarshal(out, &doc); err != nil {
							t.Fatal(err)
						}
						block := doc.Messages[0].Content[0]
						if block["cache_control"].(map[string]any)["ttl"] != tc.wantTTL {
							t.Fatalf("incorrect parent hint: %s", out)
						}
						if parts, ok := block["content"].([]any); ok {
							for _, part := range parts {
								if _, present := part.(map[string]any)["cache_control"]; present {
									t.Fatalf("nested hint: %s", out)
								}
							}
						}
					}
					losses := state.Diagnostics()
					if !tc.lost && len(losses) != 0 {
						t.Fatalf("unexpected losses: %v", losses)
					}
					if tc.lost && (len(losses) != 1 || !strings.HasPrefix(losses[0].Path, prefix) || losses[0].Semantic) {
						t.Fatalf("incorrect losses: %v", losses)
					}
				}
			})
		}
	}
}

// TestUncacheableResponsesItems checks cache-hint losses independently of
// summary replay losses under each request policy.
func TestUncacheableResponsesItems(t *testing.T) {
	for _, summary := range []string{`[]`, `[{"type":"summary_text","text":"analysis"}]`} {
		body := []byte(`{"input":[{"role":"user","content":"hello"},{"type":"additional_tools","tools":[{"type":"function","name":"f"}],"cache_control":{"type":"ephemeral"}},{"type":"reasoning","summary":` + summary + `,"cache_control":{"type":"ephemeral","ttl":"1h"}},{"role":"assistant","content":"answer"}]}`)
		for _, policy := range []LossPolicy{LossPolicyAllow, LossPolicySafe, LossPolicyStrict} {
			state := &RequestState{Options: ConversionOptions{LossPolicy: policy}}
			out, err := ConvertRequest(FormatMessages, "m", FormatResponses, body, state)
			if policy == LossPolicyStrict || (policy == LossPolicySafe && summary != `[]`) {
				if err == nil || out != nil {
					t.Fatalf("strict accepted uncacheable items: %s", out)
				}
			} else {
				if err != nil {
					t.Fatal(err)
				}
				if bytes.Contains(out, []byte(`"cache_control"`)) {
					t.Fatalf("hint applied to thinking or neighboring block: %s", out)
				}
				if bytes.Contains(out, []byte(`"thinking":"analysis"`)) {
					t.Fatalf("summary promoted to original reasoning: %s", out)
				}
			}
			losses := state.Diagnostics()
			wantLosses := 2
			if summary != `[]` {
				wantLosses++
			}
			if len(losses) != wantLosses || losses[0].Path != "input[1].cache_control" || losses[1].Path != "input[2].cache_control" {
				t.Fatalf("missing diagnostics: %v", losses)
			}
		}
	}
}

func TestEscapedToolRoleCacheLoss(t *testing.T) {
	for _, role := range []string{`"tool"`, `"\u0074ool"`, `"t\u006fol"`, `"\u0074\u006f\u006f\u006c"`} {
		body := []byte(`{"messages":[{"role":"assistant","tool_calls":[{"id":"c","type":"function","function":{"name":"f","arguments":"{}"}}]},{"role":` + role + `,"tool_call_id":"c","content":[{"type":"text","text":"a","cache_control":{"type":"ephemeral"}},{"type":"text","text":"b"}]}]}`)
		for _, policy := range []LossPolicy{LossPolicyAllow, LossPolicySafe, LossPolicyStrict} {
			state := &RequestState{Options: ConversionOptions{LossPolicy: policy}}
			out, err := ConvertRequest(FormatMessages, "m", FormatChatCompletions, body, state)
			if policy == LossPolicyStrict {
				if err == nil || out != nil {
					t.Fatalf("strict accepted role %s", role)
				}
			} else {
				if err != nil {
					t.Fatal(err)
				}
				if bytes.Contains(out, []byte(`"cache_control"`)) {
					t.Fatalf("unexpected hint: %s", out)
				}
			}
			losses := state.Diagnostics()
			if len(losses) != 1 || losses[0].Path != "messages[1].content[0].cache_control" || losses[0].Semantic {
				t.Fatalf("role %s: incorrect losses: %v", role, losses)
			}
		}
	}
}

func TestDuplicateNamespaceToolCacheLoss(t *testing.T) {
	for _, tc := range []struct{ name, body, path string }{
		{"top level", `{"input":"hello","tools":[{"type":"namespace","name":"ns","tools":[{"type":"function","name":"f"}]},{"type":"namespace","name":"ns","tools":[{"type":"function","name":"f","cache_control":{"type":"ephemeral","ttl":"1h"}}]}]}`, "tools[1].tools[0].cache_control"},
		{"same namespace", `{"input":"hello","tools":[{"type":"namespace","name":"ns","tools":[{"type":"custom","name":"f"},{"type":"custom","name":"f","cache_control":{"type":"ephemeral","ttl":"1h"}}]}]}`, "tools[0].tools[1].cache_control"},
		{"additional tools", `{"input":[{"role":"user","content":"hello"},{"type":"additional_tools","tools":[{"type":"namespace","name":"ns","tools":[{"type":"function","name":"f","cache_control":{"type":"ephemeral","ttl":"1h"}}]}]}],"tools":[{"type":"namespace","name":"ns","tools":[{"type":"function","name":"f"}]}]}`, "input[1].tools[0].tools[0].cache_control"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			for _, policy := range []LossPolicy{LossPolicyAllow, LossPolicySafe, LossPolicyStrict} {
				state := &RequestState{Options: ConversionOptions{LossPolicy: policy}}
				out, err := ConvertRequest(FormatMessages, "m", FormatResponses, []byte(tc.body), state)
				if policy == LossPolicyStrict {
					if err == nil || out != nil {
						t.Fatalf("strict accepted duplicate hint: %s", out)
					}
					// Failed normalization must not retain identities in the execution state.
					// This plain name would collide if the preceding namespace survived.
					_, err = ConvertRequest(FormatMessages, "m", FormatResponses, []byte(`{"input":"hello","tools":[{"type":"function","name":"ns__f"}]}`), state)
					if err != nil {
						t.Fatalf("failed conversion changed tool identities: %v", err)
					}
				} else {
					if err != nil {
						t.Fatal(err)
					}
					var doc struct {
						Tools []struct {
							Name  string          `json:"name"`
							Cache json.RawMessage `json:"cache_control"`
						} `json:"tools"`
					}
					if err := json.Unmarshal(out, &doc); err != nil {
						t.Fatal(err)
					}
					if len(doc.Tools) != 1 || doc.Tools[0].Name != "ns__f" || len(doc.Tools[0].Cache) != 0 {
						t.Fatalf("first declaration not preserved: %s", out)
					}
				}
				losses := state.Diagnostics()
				if len(losses) != 1 || losses[0].Path != tc.path || losses[0].Semantic {
					t.Fatalf("incorrect losses: %v", losses)
				}
			}
		})
	}
}
