package translate_test

import (
	"strings"
	"testing"

	"github.com/dillonzq/apix/internal/llm/translate"
)

// Custom tool identities are restored from wire names, so a responses-side
// function_call for a registered custom tool is rewritten into the
// custom_tool_call shape. The rewrite reads arguments as a JSON string: when
// upstream sends another type, the original bytes must survive instead of
// being replaced with an empty input.
func TestCustomToolNonStringArgumentsPreserved(t *testing.T) {
	state := translate.NewRequestState()
	if _, err := translate.ConvertRequest(translate.FormatChatCompletions, "m", translate.FormatResponses,
		[]byte(`{"input":"hi","tools":[{"type":"custom","name":"exec"}]}`), state); err != nil {
		t.Fatal(err)
	}
	item := `{"type":"function_call","id":"i1","call_id":"c1","name":"exec","arguments":{"nested":true},"sequence_number":7}`
	response := `{"id":"r","object":"response","status":"completed","output":[` + item + `]}`
	out, err := translate.ConvertResponse(translate.FormatResponses, translate.FormatResponses, 200, []byte(response), state)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(out), `"arguments":{"nested":true}`) {
		t.Fatalf("non-string arguments were dropped: %s", out)
	}
	if strings.Contains(string(out), `"input":""`) {
		t.Fatalf("empty custom input was invented: %s", out)
	}
}

// Shell calls validate their arguments as an action object, so a non-string
// value must fail instead of becoming an empty action.
func TestShellNonStringArgumentsRejectedNotInvented(t *testing.T) {
	state := translate.NewRequestState()
	if _, err := translate.ConvertRequest(translate.FormatChatCompletions, "m", translate.FormatResponses,
		[]byte(`{"input":"hi","tools":[{"type":"shell"}]}`), state); err != nil {
		t.Fatal(err)
	}
	item := `{"type":"function_call","id":"i1","call_id":"c1","name":"shell","arguments":["pwd"]}`
	response := `{"id":"r","object":"response","status":"completed","output":[` + item + `]}`
	out, err := translate.ConvertResponse(translate.FormatResponses, translate.FormatResponses, 200, []byte(response), state)
	if err == nil {
		t.Fatalf("malformed shell arguments converted without an error: %s", out)
	}
	if strings.Contains(string(out), `"action":{"commands":[]}`) {
		t.Fatalf("empty shell action was invented: %s", out)
	}
}

// A custom tool's streamed arguments-done event with a non-string arguments
// value has nothing to unwrap; with no accumulated delta either, the event
// passes through verbatim rather than announcing an empty input.
func TestCustomToolNonStringArgumentsDonePassesThrough(t *testing.T) {
	state := translate.NewRequestState()
	if _, err := translate.ConvertRequest(translate.FormatChatCompletions, "m", translate.FormatResponses,
		[]byte(`{"input":"hi","tools":[{"type":"custom","name":"exec"}]}`), state); err != nil {
		t.Fatal(err)
	}
	stream := "event: response.output_item.added\ndata: {\"item\":{\"type\":\"function_call\",\"id\":\"i1\",\"call_id\":\"c1\",\"name\":\"exec\"}}\n\n" +
		"event: response.function_call_arguments.done\ndata: {\"item_id\":\"i1\",\"call_id\":\"c1\",\"name\":\"exec\",\"arguments\":{\"nested\":true},\"sequence_number\":7}\n\n" +
		"event: response.completed\ndata: {\"type\":\"response.completed\",\"response\":{\"id\":\"r\",\"object\":\"response\",\"status\":\"completed\",\"output\":[]}}\n\n"
	events := streamFrames(t, translate.FormatResponses, translate.FormatResponses, stream, state)
	var sawPassthrough, sawCompleted bool
	for _, e := range events {
		switch e.Name {
		case "response.custom_tool_call_input.done":
			if e.Data["input"] == "" {
				t.Fatalf("empty custom input was invented: %+v", e.Data)
			}
		case "response.function_call_arguments.done":
			sawPassthrough = true
			if _, ok := e.Data["arguments"].(map[string]any); !ok {
				t.Fatalf("arguments payload changed shape: %+v", e.Data)
			}
		case "response.completed":
			sawCompleted = true
		}
	}
	if !sawPassthrough {
		t.Fatalf("malformed arguments-done event was rewritten: %+v", events)
	}
	if !sawCompleted {
		t.Fatalf("stream did not terminate: %+v", events)
	}
}

// Tool inputs are decoded into `any` before they are re-encoded as tool call
// arguments. A float64 roundtrip rounds integers beyond 2^53 and turns
// out-of-range literals into 0, so the decoder must keep exact numbers.
func TestMessagesToolInputNumericFidelity(t *testing.T) {
	body := `{"id":"r","model":"m","stop_reason":"tool_use","content":[{"type":"tool_use","id":"c1","name":"f","input":{"big":9007199254740993,"exp":1e400,"small":1.5}}]}`
	out, err := translate.ConvertResponse(translate.FormatMessages, translate.FormatChatCompletions, 200, []byte(body), nil)
	if err != nil {
		t.Fatal(err)
	}
	got := string(out)
	for _, want := range []string{"9007199254740993", "1e400", "1.5"} {
		if !strings.Contains(got, want) {
			t.Fatalf("tool input lost %s: %s", want, got)
		}
	}
	if strings.Contains(got, "9007199254740992") || strings.Contains(got, `"exp":0`) {
		t.Fatalf("tool input went through a float64 roundtrip: %s", got)
	}
}

// A server_tool_use block whose input is not the expected shape cannot be
// converted into a hosted search call; failing is the only option that does
// not invent an empty query.
func TestMessagesServerToolUseInvalidInputFails(t *testing.T) {
	body := `{"id":"r","model":"m","stop_reason":"end_turn","content":[{"type":"server_tool_use","id":"s1","name":"web_search","input":{"query":123}}]}`
	if _, err := translate.ConvertResponse(translate.FormatMessages, translate.FormatResponses, 200, []byte(body), nil); err == nil {
		t.Fatal("expected a translation error for a malformed web search input")
	}
	if _, err := translate.ConvertResponse(translate.FormatMessages, translate.FormatResponses, 200,
		[]byte(`{"id":"r","model":"m","stop_reason":"end_turn","content":[{"type":"server_tool_use","id":"s1","name":"web_search","input":{"query":"Go"}}]}`), nil); err != nil {
		t.Fatalf("valid search input must still convert: %v", err)
	}
}

// A citations field that is not an array used to be dropped without a trace.
// The reference loss is recorded so callers can see it under every policy that
// reports diagnostics.
func TestCitationsContainerShapeIsReported(t *testing.T) {
	state := translate.NewRequestState()
	body := `{"id":"r","model":"m","stop_reason":"end_turn","content":[{"type":"text","text":"answer","citations":{"url":"https://example.com"}}]}`
	if _, err := translate.ConvertResponse(translate.FormatMessages, translate.FormatChatCompletions, 200, []byte(body), state); err != nil {
		t.Fatal(err)
	}
	for _, d := range state.Diagnostics() {
		if d.Path == "content.citations" {
			return
		}
	}
	t.Fatalf("citations loss not reported: %+v", state.Diagnostics())
}

// An inline error envelope that does not match the modeled shape is still an
// upstream failure. Reporting it as success would hand the client an empty
// completion for a failed request.
func TestUnreadableInlineErrorEnvelopeFails(t *testing.T) {
	_, err := translate.ConvertResponse(translate.FormatChatCompletions, translate.FormatMessages, 200,
		[]byte(`{"error":{"message":123}}`), nil)
	if err == nil {
		t.Fatal("unreadable error envelope was treated as success")
	}
	if err.Class != translate.ClassUpstream {
		t.Fatalf("class = %v, want %v", err.Class, translate.ClassUpstream)
	}
	// A string envelope is the other shape providers send inline.
	if _, err := translate.ConvertResponse(translate.FormatChatCompletions, translate.FormatMessages, 200,
		[]byte(`{"error":"upstream exploded"}`), nil); err == nil {
		t.Fatal("string error envelope was treated as success")
	}
}

func TestResponsesSearchMalformedActionFails(t *testing.T) {
	for _, action := range []string{
		`{"type":"search","query":123}`,
		`{"type":"search","query":{}}`,
		`{"type":"search","query":"Go","sources":[{"url":123}]}`,
	} {
		t.Run(action, func(t *testing.T) {
			body := `{"id":"r","object":"response","status":"completed","output":[{"type":"web_search_call","id":"s1","status":"completed","action":` + action + `}]}`
			if out, err := translate.ConvertResponse(translate.FormatResponses, translate.FormatMessages, 200, []byte(body), nil); err == nil || err.Class != translate.ClassTranslation {
				t.Fatalf("malformed action accepted: out=%s err=%v", out, err)
			}
			stream := "event: response.created\ndata: {\"response\":{\"id\":\"r\"}}\n\n" +
				"event: response.completed\ndata: {\"type\":\"response.completed\",\"response\":" + body + "}\n\n"
			assertSearchStreamFails(t, translate.FormatResponses, translate.FormatMessages, stream)
		})
	}
}

func TestMessagesStreamInitialSearchInputFails(t *testing.T) {
	for _, input := range []string{`{"query":123}`, `{"query":[]}`, `"Go"`} {
		t.Run(input, func(t *testing.T) {
			stream := "event: message_start\ndata: {\"type\":\"message_start\",\"message\":{\"id\":\"r\",\"model\":\"m\"}}\n\n" +
				"event: content_block_start\ndata: {\"type\":\"content_block_start\",\"index\":0,\"content_block\":{\"type\":\"server_tool_use\",\"id\":\"s1\",\"name\":\"web_search\",\"input\":" + input + "}}\n\n" +
				"event: content_block_stop\ndata: {\"type\":\"content_block_stop\",\"index\":0}\n\n" +
				"event: message_stop\ndata: {\"type\":\"message_stop\"}\n\n"
			assertSearchStreamFails(t, translate.FormatMessages, translate.FormatResponses, stream)
		})
	}
}

func assertSearchStreamFails(t *testing.T, from, to, stream string) {
	t.Helper()
	for _, body := range []string{stream, strings.TrimRight(stream, "\n")} {
		for _, step := range []int{1, len(body)} {
			sc, err := translate.NewStreamConverter(from, to, nil)
			if err != nil {
				t.Fatal(err)
			}
			var frames [][]byte
			for i := 0; i < len(body); i += step {
				var events [][]byte
				var done bool
				events, done, err = sc.Feed([]byte(body[i:min(i+step, len(body))]))
				frames = append(frames, events...)
				if done {
					t.Fatal("malformed search completed successfully")
				}
				if err != nil {
					break
				}
			}
			if err == nil {
				var tail [][]byte
				tail, err = sc.Finish()
				frames = append(frames, tail...)
			}
			if err == nil || err.Class != translate.ClassTranslation {
				t.Fatalf("malformed search was not rejected: %v", err)
			}
			if len(frames) == 0 {
				t.Fatal("events preceding the malformed search were discarded")
			}
			for _, frame := range frames {
				if strings.Contains(string(frame), `"query":""`) {
					t.Fatalf("empty query invented before failure: %s", frame)
				}
			}
			if _, err := sc.Finish(); err == nil {
				t.Fatal("failed stream recovered at EOF")
			}
		}
	}
}

func TestResponsesStreamMalformedAnnotationsReported(t *testing.T) {
	item := `{"type":"message","id":"m1","role":"assistant","content":[{"type":"output_text","text":"answer","annotations":{"url":"https://example.com"}}]}`
	for _, target := range []string{translate.FormatChatCompletions, translate.FormatMessages} {
		for _, snapshot := range []struct{ name, frame string }{
			{"item-and-terminal", "event: response.output_item.done\ndata: {\"item\":" + item + "}\n\n"},
			{"terminal-only", ""},
		} {
			t.Run(target+"/"+snapshot.name, func(t *testing.T) {
				state := translate.NewRequestState()
				stream := "event: response.created\ndata: {\"response\":{\"id\":\"r\"}}\n\n" + snapshot.frame +
					"event: response.completed\ndata: {\"type\":\"response.completed\",\"response\":{\"id\":\"r\",\"object\":\"response\",\"status\":\"completed\",\"output\":[" + item + "]}}\n\n"
				events := streamFrames(t, translate.FormatResponses, target, stream, state)
				var textCount int
				for _, event := range events {
					if event.Name == "content_block_delta" {
						if delta, ok := event.Data["delta"].(map[string]any); ok && delta["text"] == "answer" {
							textCount++
						}
					} else if choices, ok := event.Data["choices"].([]any); ok && len(choices) > 0 {
						if delta, ok := choices[0].(map[string]any)["delta"].(map[string]any); ok && delta["content"] == "answer" {
							textCount++
						}
					}
				}
				if textCount != 1 {
					t.Fatalf("visible text should survive once: %+v", events)
				}
				for _, d := range state.Diagnostics() {
					if d.Path == "output.m1/0/false.annotations" && !d.Semantic && d.From == translate.FormatResponses && d.To == target {
						return
					}
				}
				t.Fatalf("missing annotations loss diagnostic: %+v", state.Diagnostics())
			})
		}
	}
}
