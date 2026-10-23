package translate_test

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/dillonzq/apix"
	"github.com/dillonzq/apix/internal/llm/translate"
	"github.com/dillonzq/apix/reasoning"
)

var formats = []string{translate.FormatChatCompletions, translate.FormatMessages, translate.FormatResponses}

func object(t *testing.T, body []byte) map[string]any {
	t.Helper()
	var out map[string]any
	if err := json.Unmarshal(body, &out); err != nil {
		t.Fatal(err)
	}
	return out
}

func TestPublicConversionMatrix(t *testing.T) {
	requests := map[string]string{
		"chat-completions": `{"model":"client","messages":[{"role":"user","content":"hello"}]}`,
		"messages":         `{"model":"client","max_tokens":64,"messages":[{"role":"user","content":"hello"}]}`,
		"responses":        `{"model":"client","input":"hello"}`,
	}
	responses := map[string]string{
		"chat-completions": `{"id":"r","model":"upstream","choices":[{"message":{"role":"assistant","content":"answer"},"finish_reason":"stop"}],"usage":{"prompt_tokens":7,"completion_tokens":3}}`,
		"messages":         `{"id":"r","model":"upstream","type":"message","role":"assistant","content":[{"type":"text","text":"answer"}],"stop_reason":"end_turn","usage":{"input_tokens":7,"output_tokens":3}}`,
		"responses":        `{"id":"r","model":"upstream","status":"completed","output":[{"type":"message","role":"assistant","content":[{"type":"output_text","text":"answer"}]}],"usage":{"input_tokens":7,"output_tokens":3}}`,
	}
	for _, from := range formats {
		for _, to := range formats {
			t.Run(from+"/"+to, func(t *testing.T) {
				state := translate.NewRequestState()
				body, err := translate.ConvertRequest(to, "upstream", from, []byte(requests[from]), state)
				if err != nil {
					t.Fatal(err)
				}
				if object(t, body)["model"] != "upstream" || !strings.Contains(string(body), "hello") {
					t.Fatalf("request: %s", body)
				}
				body, err = translate.ConvertResponse(from, to, 200, []byte(responses[from]), state)
				if err != nil {
					t.Fatal(err)
				}
				if !strings.Contains(string(body), "answer") {
					t.Fatalf("response: %s", body)
				}
				usage := object(t, body)["usage"].(map[string]any)
				in, out := "input_tokens", "output_tokens"
				if to == "chat-completions" {
					in, out = "prompt_tokens", "completion_tokens"
				}
				if usage[in] != float64(7) || usage[out] != float64(3) {
					t.Fatalf("usage: %v", usage)
				}
			})
		}
	}
}

func TestNativeRequestPreservesProviderFields(t *testing.T) {
	for _, format := range formats {
		for _, model := range []string{"gpt-5", "non-gpt"} {
			body := []byte(`{"model":"` + model + `","messages":[{"role":"developer","content":"hello"},{"role":"system","content":"history"}],"thinking":{"levels":["high"]},"input":[{"type":"reasoning","encrypted_content":"secret"},{"type":"compaction","data":"opaque"}],"tools":[{"type":"custom","name":"apply_patch"}],"unknown":9007199254740993}`)
			got, err := translate.ConvertRequest(format, model, format, body, nil)
			if err != nil || string(got) != string(body) {
				t.Fatalf("native %s: %s %v", format, got, err)
			}
			got, err = translate.ConvertRequest(format, "replacement", format, body, nil)
			if err != nil {
				t.Fatal(err)
			}
			var original, rewritten map[string]json.RawMessage
			_ = json.Unmarshal(body, &original)
			_ = json.Unmarshal(got, &rewritten)
			delete(original, "model")
			delete(rewritten, "model")
			a, _ := json.Marshal(original)
			b, _ := json.Marshal(rewritten)
			if string(a) != string(b) {
				t.Fatalf("native fields changed: %s", got)
			}
		}
	}
}

func TestToolStateRestoresCustomNamespace(t *testing.T) {
	for _, target := range []string{"chat-completions", "messages"} {
		var state translate.RequestState // the zero value must work
		request := []byte(`{"input":"hello","tools":[{"type":"namespace","name":"shell","tools":[{"type":"custom","name":"exec"}]}]}`)
		body, err := translate.ConvertRequest(target, "model", "responses", request, &state)
		if err != nil {
			t.Fatal(err)
		}
		tool := object(t, body)["tools"].([]any)[0].(map[string]any)
		name := tool["name"]
		if target == "chat-completions" {
			name = tool["function"].(map[string]any)["name"]
		}
		var upstream []byte
		if target == "chat-completions" {
			upstream, _ = json.Marshal(map[string]any{"id": "r", "choices": []any{map[string]any{"message": map[string]any{"tool_calls": []any{map[string]any{"id": "call1", "type": "function", "function": map[string]any{"name": name, "arguments": `{"input":"pwd"}`}}}}, "finish_reason": "tool_calls"}}})
		} else {
			upstream, _ = json.Marshal(map[string]any{"id": "r", "content": []any{map[string]any{"type": "tool_use", "id": "call1", "name": name, "input": map[string]any{"input": "pwd"}}}, "stop_reason": "tool_use"})
		}
		body, err = translate.ConvertResponse(target, "responses", 200, upstream, &state)
		if err != nil {
			t.Fatal(err)
		}
		restored := object(t, body)["output"].([]any)[0].(map[string]any)
		if restored["type"] != "custom_tool_call" || restored["name"] != "exec" || restored["namespace"] != "shell" || restored["input"] != "pwd" {
			t.Fatalf("restored: %s", body)
		}
	}
}

func TestPublicStreamsChunkingAndEOF(t *testing.T) {
	streams := map[string][2]string{
		"chat-completions": {"data: {\"id\":\"r\",\"choices\":[{\"delta\":{\"content\":\"hello\"}}]}\n\n", "data: {\"id\":\"r\",\"choices\":[{\"delta\":{},\"finish_reason\":\"stop\"}]}\n\ndata: [DONE]\n\n"},
		"messages":         {"event: message_start\ndata: {\"message\":{\"id\":\"r\"}}\n\nevent: content_block_start\ndata: {\"index\":0,\"content_block\":{\"type\":\"text\",\"text\":\"\"}}\n\nevent: content_block_delta\ndata: {\"index\":0,\"delta\":{\"type\":\"text_delta\",\"text\":\"hello\"}}\n\n", "event: content_block_stop\ndata: {\"index\":0}\n\nevent: message_delta\ndata: {\"delta\":{\"stop_reason\":\"end_turn\"}}\n\nevent: message_stop\ndata: {\"type\":\"message_stop\"}\n\n"},
		"responses":        {"event: response.created\ndata: {\"response\":{\"id\":\"r\"}}\n\nevent: response.output_text.delta\ndata: {\"delta\":\"hello\"}\n\n", "event: response.completed\ndata: {\"type\":\"response.completed\",\"response\":{\"id\":\"r\",\"object\":\"response\",\"status\":\"completed\",\"output\":[]}}\n\n"},
	}
	for _, from := range formats {
		for _, to := range formats {
			t.Run(from+"/"+to, func(t *testing.T) {
				parts := streams[from]
				sc, err := translate.NewStreamConverter(from, to, nil)
				if err != nil {
					t.Fatal(err)
				}
				var output strings.Builder
				for _, b := range []byte(parts[0] + parts[1]) {
					events, _, err := sc.Feed([]byte{b})
					if err != nil {
						t.Fatal(err)
					}
					for _, event := range events {
						if to == translate.FormatChatCompletions {
							assertChatSSEFrame(t, event)
						}
						output.Write(event)
					}
				}
				tail, err := sc.Finish()
				if err != nil {
					t.Fatal(err)
				}
				for _, event := range tail {
					output.Write(event)
				}
				if !strings.Contains(output.String(), "hello") {
					t.Fatalf("missing text: %s", output.String())
				}
				if to == translate.FormatChatCompletions && strings.Count(output.String(), "data: [DONE]\n\n") != 1 {
					t.Fatalf("expected exactly one terminal frame: %s", output.String())
				}
				sc, _ = translate.NewStreamConverter(from, to, nil)
				if _, _, err := sc.Feed([]byte(parts[0])); err != nil {
					t.Fatal(err)
				}
				if _, err := sc.Finish(); err == nil {
					t.Fatal("accepted incomplete EOF")
				}
			})
		}
	}
}

func TestErrorsAndSuffixPriority(t *testing.T) {
	if _, err := translate.ConvertRequest("codex", "m", "chat-completions", []byte(`{}`), nil); err == nil || err.Class != translate.ClassUnsupported {
		t.Fatal(err)
	}
	if _, err := translate.ConvertRequest("chat-completions", "m", "responses", []byte(`{}`), nil); err == nil {
		t.Fatal("missing tool state accepted")
	}
	for _, format := range formats {
		if _, err := translate.NewStreamConverter(format, "codex", nil); err == nil {
			t.Fatal("codex accepted")
		}
		if _, err := translate.ConvertRequest(format, "m", format, []byte(`null`), nil); err == nil {
			t.Fatal("null accepted")
		}
	}
	_, err := translate.ConvertResponse("chat-completions", "messages", 429, []byte(`{"error":{"message":"quota Bearer abc123"}}`), nil)
	if err == nil || err.Class != translate.ClassQuota || !err.Retryable {
		t.Fatal(err)
	}
	if err.StatusCode != 429 || strings.Contains(err.Message, "abc123") {
		t.Fatalf("classified error: %+v", err)
	}
	suffix := reasoning.ParseSuffix("m(8192)")
	source := translate.StripReasoning("messages", "chat-completions", []byte(`{"messages":[],"reasoning_effort":"unrepresentable"}`))
	state := translate.NewRequestState()
	state.Options.MessagesThinkingMode = "manual"
	body, err := translate.ConvertRequest("messages", suffix.Model, "chat-completions", source, state)
	if err != nil {
		t.Fatal(err)
	}
	body, err = translate.ApplyReasoning("messages", "chat-completions", body, *suffix.Reasoning(), state)
	if err != nil {
		t.Fatal(err)
	}
	if object(t, body)["thinking"].(map[string]any)["budget_tokens"] != float64(8192) {
		t.Fatalf("suffix budget: %s", body)
	}
	// Gemini has no level above high: the override is approximated and
	// reported, and strict policies reject the approximation.
	for _, policy := range []translate.LossPolicy{translate.LossPolicyAllow, translate.LossPolicyStrict} {
		state := translate.NewRequestState()
		state.Options.LossPolicy = policy
		body, err := translate.ApplyReasoning("gemini", "chat-completions", []byte(`{"contents":[]}`), apix.Reasoning{Effort: "max"}, state)
		if policy == translate.LossPolicyStrict {
			if err == nil {
				t.Fatalf("strict accepted an approximated level: %s", body)
			}
			continue
		}
		if err != nil || !strings.Contains(string(body), `"thinkingLevel":"high"`) || len(state.Diagnostics()) != 1 {
			t.Fatalf("approximated level = %s %v %+v", body, err, state.Diagnostics())
		}
	}
}

func assertChatSSEFrame(t *testing.T, frame []byte) {
	t.Helper()
	s := string(frame)
	if !strings.HasPrefix(s, "data: ") || !strings.HasSuffix(s, "\n\n") {
		t.Fatalf("incomplete SSE frame: %q", s)
	}
	payload := strings.TrimSuffix(strings.TrimPrefix(s, "data: "), "\n\n")
	if payload != "[DONE]" && !json.Valid([]byte(payload)) {
		t.Fatalf("invalid SSE data: %q", payload)
	}
}

func TestPublicChatSSECompletionAtEOF(t *testing.T) {
	for _, terminal := range []string{
		`data: {"choices":[{"delta":{},"finish_reason":"stop"}]}`,
		"data: [DONE]",
	} {
		sc, err := translate.NewStreamConverter(translate.FormatChatCompletions, translate.FormatChatCompletions, nil)
		if err != nil {
			t.Fatal(err)
		}
		if events, done, err := sc.Feed([]byte(terminal)); err != nil || done || len(events) != 0 {
			t.Fatalf("unterminated line: %q %t %v", events, done, err)
		}
		tail, err := sc.Finish()
		if err != nil {
			t.Fatal(err)
		}
		var output strings.Builder
		for _, frame := range tail {
			assertChatSSEFrame(t, frame)
			output.Write(frame)
		}
		if strings.Count(output.String(), "data: [DONE]\n\n") != 1 {
			t.Fatalf("missing EOF terminal: %q", output.String())
		}
		if events, err := sc.Finish(); err != nil || len(events) != 0 {
			t.Fatalf("repeated EOF: %q %v", events, err)
		}
		if events, done, err := sc.Feed([]byte("data: [DONE]\n\n")); err != nil || !done || len(events) != 0 {
			t.Fatalf("feed after EOF: %q %t %v", events, done, err)
		}
	}
}

func TestInvalidFormatIdentifiersAreRejected(t *testing.T) {
	for _, invalid := range []string{"unknown", "openai", "openai-response", "claude"} {
		t.Run(invalid, func(t *testing.T) {
			for _, pair := range [][2]string{{invalid, translate.FormatChatCompletions}, {translate.FormatChatCompletions, invalid}} {
				_, requestErr := translate.ConvertRequest(pair[0], "m", pair[1], []byte(`{}`), nil)
				_, responseErr := translate.ConvertResponse(pair[0], pair[1], 200, []byte(`{}`), nil)
				_, streamErr := translate.NewStreamConverter(pair[0], pair[1], nil)
				for name, err := range map[string]*translate.Error{"request": requestErr, "response": responseErr, "stream": streamErr} {
					if err == nil || err.Class != translate.ClassUnsupported {
						t.Fatalf("%s accepted invalid format or returned wrong error: %v", name, err)
					}
				}
			}
		})
	}
}
