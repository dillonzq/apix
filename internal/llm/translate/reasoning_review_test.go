package translate_test

import (
	"fmt"
	"strings"
	"testing"

	"github.com/dillonzq/apix/internal/llm/translate"
)

// TestUnsignedThinkingRequestOption keeps default Messages requests free of
// unsigned replay blocks without weakening diagnostics or strict loss policies.
func TestUnsignedThinkingRequestOption(t *testing.T) {
	bodies := map[string]string{
		translate.FormatChatCompletions: `{"stream":%t,"messages":[{"role":"assistant","reasoning_content":" raw ","content":"checking","tool_calls":[{"id":"c","type":"function","function":{"name":"f","arguments":"{}"}}]},{"role":"tool","tool_call_id":"c","content":"ok"}]}`,
		translate.FormatResponses:       `{"stream":%t,"input":[{"type":"reasoning","content":[{"type":"reasoning_text","text":" raw "}]},{"type":"message","role":"assistant","content":"checking"},{"type":"function_call","call_id":"c","name":"f","arguments":"{}"},{"type":"function_call_output","call_id":"c","output":"ok"}]}`,
	}
	for from, body := range bodies {
		for _, stream := range []bool{false, true} {
			for _, allow := range []bool{false, true} {
				for _, policy := range []translate.LossPolicy{"", translate.LossPolicyAllow, translate.LossPolicySafe, translate.LossPolicyStrict} {
					t.Run(fmt.Sprintf("%s/stream=%t/unsigned=%t/policy=%s", from, stream, allow, policy), func(t *testing.T) {
						state := translate.NewRequestState()
						state.Options = translate.ConversionOptions{LossPolicy: policy, AllowUnsignedThinking: allow}
						out, err := translate.ConvertRequest(translate.FormatMessages, "m", from, []byte(fmt.Sprintf(body, stream)), state)
						losses := state.Diagnostics()
						if len(losses) != 1 || losses[0].Code != "unsigned_thinking" || !losses[0].Semantic {
							t.Fatalf("unsigned loss missing: %+v", losses)
						}
						if policy == translate.LossPolicySafe || policy == translate.LossPolicyStrict {
							if err == nil || err.Class != translate.ClassUnsupported || out != nil {
								t.Fatalf("strict loss accepted: %s %v", out, err)
							}
							return
						}
						if err != nil {
							t.Fatal(err)
						}
						doc := object(t, out)
						if (doc["stream"] == true) != stream {
							t.Fatalf("stream changed: %s", out)
						}
						msgs := doc["messages"].([]any)
						if len(msgs) != 2 {
							t.Fatalf("turns changed: %s", out)
						}
						blocks := msgs[0].(map[string]any)["content"].([]any)
						if allow {
							thinking := blocks[0].(map[string]any)
							if thinking["type"] != "thinking" || thinking["thinking"] != " raw " || thinking["signature"] != nil {
								t.Fatalf("opt-in reasoning changed: %s", out)
							}
							blocks = blocks[1:]
						} else if strings.Contains(string(out), `"thinking"`) {
							t.Fatalf("unsigned thinking emitted by default: %s", out)
						}
						if len(blocks) != 2 || blocks[0].(map[string]any)["text"] != "checking" || blocks[1].(map[string]any)["id"] != "c" {
							t.Fatalf("assistant text or call changed: %s", out)
						}
						result := msgs[1].(map[string]any)["content"].([]any)[0].(map[string]any)
						if result["type"] != "tool_result" || result["tool_use_id"] != "c" || result["content"] != "ok" {
							t.Fatalf("tool result changed: %s", out)
						}
					})
				}
			}
		}
	}
}

// TestUnsignedThinkingDefaults also checks nil and zero-value state, so callers
// cannot accidentally enable unsigned replay by omitting the constructor.
func TestUnsignedThinkingDefaults(t *testing.T) {
	for _, state := range []*translate.RequestState{nil, {}, translate.NewRequestState()} {
		out, err := translate.ConvertRequest(translate.FormatMessages, "m", translate.FormatChatCompletions, []byte(`{"messages":[{"role":"assistant","reasoning_content":"raw"}]}`), state)
		if err != nil {
			t.Fatal(err)
		}
		if len(object(t, out)["messages"].([]any)) != 0 {
			t.Fatalf("unsigned-only turn survived: %s", out)
		}
	}
}

// TestEmptyReasoningAliasesAreLossless exercises alias precedence with empty
// fields both before and after the readable value, under every loss policy.
func TestEmptyReasoningAliasesAreLossless(t *testing.T) {
	for _, fields := range []string{
		`"reasoning_content":"raw","reasoning":"","reasoning_details":[]`,
		`"reasoning_content":"","reasoning":"raw","reasoning_details":[]`,
		`"reasoning_content":null,"reasoning":[],"reasoning_details":[{"type":"reasoning.text","text":"raw"}]`,
		`"reasoning_content":"raw","reasoning":{"text":""},"reasoning_details":[{"type":"reasoning.text","text":""}]`,
	} {
		for _, policy := range []translate.LossPolicy{"", translate.LossPolicyAllow, translate.LossPolicySafe, translate.LossPolicyStrict} {
			t.Run(fields+"/"+string(policy), func(t *testing.T) {
				state := translate.NewRequestState()
				state.Options.LossPolicy = policy
				body := []byte(`{"messages":[{"role":"assistant",` + fields + `}]}`)
				out, err := translate.ConvertRequest(translate.FormatResponses, "m", translate.FormatChatCompletions, body, state)
				if err != nil || len(state.Diagnostics()) != 0 {
					t.Fatalf("lossless alias rejected: %v %+v", err, state.Diagnostics())
				}
				items := object(t, out)["input"].([]any)
				if len(items) != 1 || items[0].(map[string]any)["content"].([]any)[0].(map[string]any)["text"] != "raw" {
					t.Fatalf("reasoning changed: %s", out)
				}
			})
		}
	}
}
