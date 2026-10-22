package translate_test

import (
	"encoding/json"
	"fmt"
	"reflect"
	"testing"

	"github.com/dillonzq/apix/internal/llm/translate"
)

func TestResponsesToolImagesPublicLossPolicies(t *testing.T) {
	for _, target := range []string{translate.FormatChatCompletions, translate.FormatMessages} {
		for _, policy := range []translate.LossPolicy{translate.LossPolicyAllow, translate.LossPolicySafe, translate.LossPolicyStrict} {
			for _, outputType := range []string{"function_call_output", "custom_tool_call_output"} {
				for _, stream := range []bool{false, true} {
					for _, loss := range []struct {
						field    string
						value    string
						semantic bool
					}{{}, {"metadata", `{"label":"test"}`, false}, {"seed", `42`, true}} {
						t.Run(fmt.Sprintf("%s/%s/%s/stream=%t/loss=%s", target, policy, outputType, stream, loss.field), func(t *testing.T) {
							extra := ""
							if loss.field != "" {
								extra = fmt.Sprintf(",%q:%s", loss.field, loss.value)
							}
							body := fmt.Sprintf(`{"stream":%t%s,"input":[
								{"type":"function_call","call_id":"c1","name":"read","arguments":"{}"},
								{"type":%q,"call_id":"c1","output":[
									{"type":"input_text","text":"before"},
									{"type":"input_image","image_url":"data:image/png;base64,AAAA"},
									{"type":"output_text","text":"after"}]}]}`, stream, extra, outputType)
							state := translate.NewRequestState()
							state.Options.LossPolicy = policy
							out, err := translate.ConvertRequest(target, "upstream", translate.FormatResponses, []byte(body), state)
							reject := loss.field != "" && (policy == translate.LossPolicyStrict || policy == translate.LossPolicySafe && loss.semantic)
							if (err != nil) != reject {
								t.Fatalf("reject=%t: %v", reject, err)
							}
							diagnostics := state.Diagnostics()
							if loss.field == "" {
								if len(diagnostics) != 0 {
									t.Fatalf("representable tool images diagnosed as loss: %+v", diagnostics)
								}
							} else if len(diagnostics) != 1 || diagnostics[0].Path != loss.field || diagnostics[0].Semantic != loss.semantic || diagnostics[0].From != translate.FormatResponses || diagnostics[0].To != target {
								t.Fatalf("unexpected loss diagnostics: %+v", diagnostics)
							}
							if reject {
								if err.Class != translate.ClassUnsupported || len(out) != 0 {
									t.Fatalf("rejected conversion returned %s, %v", out, err)
								}
								return
							}
							var got map[string]any
							if err := json.Unmarshal(out, &got); err != nil {
								t.Fatal(err)
							}
							if got["model"] != "upstream" || (got["stream"] == true) != stream {
								t.Fatalf("model/stream lost: %s", out)
							}
							want := `[
								{"role":"assistant","content":null,"tool_calls":[{"id":"c1","type":"function","function":{"name":"read","arguments":"{}"}}]},
								{"role":"tool","tool_call_id":"c1","content":"beforeafter"},
								{"role":"user","content":[{"type":"text","text":"Tool output for call_id \"c1\" (1 images):"},{"type":"text","text":"before"},{"type":"image_url","image_url":{"url":"data:image/png;base64,AAAA"}},{"type":"text","text":"after"}]}]`
							if target == translate.FormatMessages {
								want = `[
									{"role":"assistant","content":[{"type":"tool_use","id":"c1","name":"read","input":{}}]},
									{"role":"user","content":[{"type":"tool_result","tool_use_id":"c1","content":[{"type":"text","text":"before"},{"type":"image","source":{"type":"base64","media_type":"image/png","data":"AAAA"}},{"type":"text","text":"after"}]}]}]`
							}
							assertToolImageMessages(t, got["messages"], want)
						})
					}
				}
			}
		}
	}
}

func TestResponsesToolImagesPublicReplyBoundaries(t *testing.T) {
	for _, target := range []string{translate.FormatChatCompletions, translate.FormatMessages} {
		for _, stream := range []bool{false, true} {
			for _, nextRound := range []bool{false, true} {
				t.Run(fmt.Sprintf("%s/stream=%t/nextRound=%t", target, stream, nextRound), func(t *testing.T) {
					tail := ""
					if nextRound {
						tail = `,{"type":"function_call","call_id":"c3","name":"read","arguments":"{}"},
							{"type":"function_call_output","call_id":"c3","output":[{"type":"input_image","image_url":"https://x/second.png"}]}`
					}
					body := fmt.Sprintf(`{"stream":%t,"input":[
						{"type":"function_call","call_id":"c1","name":"read","arguments":"{}"},
						{"type":"function_call","call_id":"c2","name":"read","arguments":"{}"},
						{"type":"function_call_output","call_id":"c1","output":[{"type":"input_image","image_url":"https://x/first.png"}]},
						{"type":"function_call_output","call_id":"c2","output":"plain"}%s]}`, stream, tail)
					state := translate.NewRequestState()
					state.Options.LossPolicy = translate.LossPolicyStrict
					out, err := translate.ConvertRequest(target, "upstream", translate.FormatResponses, []byte(body), state)
					if err != nil || len(state.Diagnostics()) != 0 {
						t.Fatalf("conversion failed: %v %+v", err, state.Diagnostics())
					}
					var got map[string]any
					if err := json.Unmarshal(out, &got); err != nil {
						t.Fatal(err)
					}
					want := `[
						{"role":"assistant","content":null,"tool_calls":[{"id":"c1","type":"function","function":{"name":"read","arguments":"{}"}},{"id":"c2","type":"function","function":{"name":"read","arguments":"{}"}}]},
						{"role":"tool","tool_call_id":"c1","content":""},
						{"role":"tool","tool_call_id":"c2","content":"plain"},
						{"role":"user","content":[{"type":"text","text":"Tool output for call_id \"c1\" (1 images):"},{"type":"image_url","image_url":{"url":"https://x/first.png"}}]}`
					if nextRound {
						want += `,
							{"role":"assistant","content":null,"tool_calls":[{"id":"c3","type":"function","function":{"name":"read","arguments":"{}"}}]},
							{"role":"tool","tool_call_id":"c3","content":""},
							{"role":"user","content":[{"type":"text","text":"Tool output for call_id \"c3\" (1 images):"},{"type":"image_url","image_url":{"url":"https://x/second.png"}}]}`
					}
					if target == translate.FormatMessages {
						want = `[
							{"role":"assistant","content":[{"type":"tool_use","id":"c1","name":"read","input":{}},{"type":"tool_use","id":"c2","name":"read","input":{}}]},
							{"role":"user","content":[{"type":"tool_result","tool_use_id":"c1","content":[{"type":"image","source":{"type":"url","url":"https://x/first.png"}}]},{"type":"tool_result","tool_use_id":"c2","content":"plain"}]}`
						if nextRound {
							want += `,
								{"role":"assistant","content":[{"type":"tool_use","id":"c3","name":"read","input":{}}]},
								{"role":"user","content":[{"type":"tool_result","tool_use_id":"c3","content":[{"type":"image","source":{"type":"url","url":"https://x/second.png"}}]}]}`
						}
					}
					assertToolImageMessages(t, got["messages"], want+`]`)
				})
			}
		}
	}
}

func assertToolImageMessages(t *testing.T, got any, wantJSON string) {
	t.Helper()
	var want any
	if err := json.Unmarshal([]byte(wantJSON), &want); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("tool image messages differ:\ngot: %#v\nwant: %#v", got, want)
	}
}
