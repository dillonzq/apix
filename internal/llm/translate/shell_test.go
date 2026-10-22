package translate

import (
	"encoding/json"
	"reflect"
	"strconv"
	"strings"
	"testing"

	"github.com/dillonzq/apix/sse"
)

const shellRequest = `{"model":"m","tools":[{"type":"shell","environment":{"type":"local"}}],"tool_choice":{"type":"shell"},"input":[{"type":"shell_call","id":"sc1","call_id":"c1","action":{"commands":["pwd","echo 中文"],"timeout_ms":120000,"max_output_length":4096}},{"type":"shell_call_output","call_id":"c1","output":[{"stdout":"/tmp\n","stderr":"","outcome":{"type":"exit","exit_code":0}},{"stdout":"","stderr":"slow","outcome":{"type":"timeout"}}]}]}`
const shellArgs = `{"commands":["pwd","echo 中文"],"timeout_ms":120000,"max_output_length":4096}`

func shellJSON(v any) []byte { b, _ := json.Marshal(v); return b }
func shellObject(t *testing.T, b []byte) map[string]any {
	t.Helper()
	var v map[string]any
	if err := json.Unmarshal(b, &v); err != nil {
		t.Fatal(err)
	}
	return v
}
func assertShellItem(t *testing.T, v map[string]any) {
	t.Helper()
	assertShellResponseFields(t, v, "completed")
	if id, _ := v["id"].(string); id == "" {
		t.Fatalf("shell item has no id: %v", v)
	}
	if v["type"] != "shell_call" || v["call_id"] != "c1" || v["name"] != nil || v["arguments"] != nil {
		t.Fatalf("unexpected shell item: %v", v)
	}
	want := shellObject(t, []byte(shellArgs))
	if !reflect.DeepEqual(v["action"], want) {
		t.Fatalf("action=%v want %v", v["action"], want)
	}
}

func TestShellRequests(t *testing.T) {
	for _, target := range []string{FormatResponses, FormatChatCompletions, FormatMessages} {
		for _, policy := range []LossPolicy{LossPolicyAllow, LossPolicySafe, LossPolicyStrict} {
			t.Run(target+"/"+string(policy), func(t *testing.T) {
				state := NewRequestState()
				state.Options.LossPolicy = policy
				var body []byte
				var err *Error
				if target == FormatResponses {
					body, err = NormalizeResponsesRequest([]byte(shellRequest), state)
				} else {
					body, err = ConvertRequest(target, "up", FormatResponses, []byte(shellRequest), state)
				}
				if err != nil {
					t.Fatal(err)
				}
				doc := shellObject(t, body)
				tool := doc["tools"].([]any)[0].(map[string]any)
				if target == FormatChatCompletions {
					tool = tool["function"].(map[string]any)
				}
				if tool["name"] != "shell" {
					t.Fatalf("tool: %v", tool)
				}
				if !strings.Contains(string(body), "commands") || !strings.Contains(string(body), "timeout") || !strings.Contains(string(body), "stdout") || !strings.Contains(string(body), "stderr") {
					t.Fatalf("lost shell content: %s", body)
				}
				if strings.Contains(string(body), `"type":"shell"`) || strings.Contains(string(body), `"type":"shell_call"`) {
					t.Fatalf("unconverted shell: %s", body)
				}
				if target == FormatResponses {
					history := doc["input"].([]any)
					call := history[0].(map[string]any)
					if call["type"] != "function_call" || call["name"] != "shell" {
						t.Fatal(call)
					}
					if !reflect.DeepEqual(shellObject(t, []byte(call["arguments"].(string))), shellObject(t, []byte(shellArgs))) {
						t.Fatal(call)
					}
					var result []map[string]any
					if json.Unmarshal([]byte(history[1].(map[string]any)["output"].(string)), &result) != nil || len(result) != 2 || result[1]["outcome"].(map[string]any)["type"] != "timeout" {
						t.Fatal(history)
					}
				}
			})
		}
	}
}

func shellUpstream(format, args string) []byte {
	switch format {
	case FormatChatCompletions:
		return shellJSON(map[string]any{"id": "r", "model": "m", "choices": []any{map[string]any{"message": map[string]any{"role": "assistant", "tool_calls": []any{map[string]any{"id": "c1", "type": "function", "function": map[string]any{"name": "shell", "arguments": args}}}}, "finish_reason": "tool_calls"}}})
	case FormatMessages:
		var input any
		json.Unmarshal([]byte(args), &input)
		return shellJSON(map[string]any{"id": "r", "model": "m", "type": "message", "role": "assistant", "content": []any{map[string]any{"type": "tool_use", "id": "c1", "name": "shell", "input": input}}, "stop_reason": "tool_use"})
	default:
		return shellJSON(map[string]any{"id": "r", "object": "response", "model": "m", "status": "completed", "output": []any{map[string]any{"type": "function_call", "id": "sc1", "call_id": "c1", "name": "shell", "arguments": args}}})
	}
}

func TestShellResponseRestoration(t *testing.T) {
	for _, upstream := range []string{FormatResponses, FormatChatCompletions, FormatMessages} {
		t.Run(upstream, func(t *testing.T) {
			state := NewRequestState()
			if _, err := NormalizeResponsesRequest([]byte(shellRequest), state); err != nil {
				t.Fatal(err)
			}
			body, err := ConvertResponse(upstream, FormatResponses, 200, shellUpstream(upstream, shellArgs), state)
			if err != nil {
				t.Fatal(err)
			}
			assertShellItem(t, shellObject(t, body)["output"].([]any)[0].(map[string]any))
			for _, args := range []string{`{}`, `{"commands":[]}`, `{"commands":["pwd"],"timeout_ms":-1}`} {
				if _, err := ConvertResponse(upstream, FormatResponses, 200, shellUpstream(upstream, args), state); err == nil {
					t.Fatalf("accepted %s", args)
				}
			}
		})
	}
}

func shellEvent(name string, v any) string {
	return "event: " + name + "\ndata: " + string(shellJSON(v)) + "\n\n"
}
func shellStream(format string) string { return shellStreamWithArgs(format, shellArgs) }

func shellStreamWithArgs(format, args string) string {
	half := len(args) / 2
	switch format {
	case FormatChatCompletions:
		chunk := func(delta any, finish any) string {
			return "data: " + string(shellJSON(map[string]any{"id": "r", "model": "m", "choices": []any{map[string]any{"index": 0, "delta": delta, "finish_reason": finish}}})) + "\n\n"
		}
		return chunk(map[string]any{"tool_calls": []any{map[string]any{"index": 0, "id": "c1", "type": "function", "function": map[string]any{"name": "shell", "arguments": args[:half]}}}}, nil) + chunk(map[string]any{"tool_calls": []any{map[string]any{"index": 0, "function": map[string]any{"arguments": args[half:]}}}}, "tool_calls") + "data: [DONE]\n\n"
	case FormatMessages:
		out := shellEvent("message_start", map[string]any{"type": "message_start", "message": map[string]any{"id": "r", "model": "m", "role": "assistant", "type": "message", "content": []any{}}})
		out += shellEvent("content_block_start", map[string]any{"type": "content_block_start", "index": 0, "content_block": map[string]any{"type": "tool_use", "id": "c1", "name": "shell", "input": map[string]any{}}})
		for _, part := range []string{args[:half], args[half:]} {
			out += shellEvent("content_block_delta", map[string]any{"type": "content_block_delta", "index": 0, "delta": map[string]any{"type": "input_json_delta", "partial_json": part}})
		}
		return out + shellEvent("content_block_stop", map[string]any{"type": "content_block_stop", "index": 0}) + shellEvent("message_delta", map[string]any{"type": "message_delta", "delta": map[string]any{"stop_reason": "tool_use"}}) + shellEvent("message_stop", map[string]any{"type": "message_stop"})
	default:
		out := shellEvent("response.output_item.added", map[string]any{"type": "response.output_item.added", "output_index": 0, "sequence_number": 7, "item": map[string]any{"type": "function_call", "id": "sc1", "call_id": "c1", "name": "shell", "arguments": "", "status": "in_progress"}})
		for _, part := range []string{args[:half], args[half:]} {
			out += shellEvent("response.function_call_arguments.delta", map[string]any{"type": "response.function_call_arguments.delta", "item_id": "sc1", "output_index": 0, "delta": part})
		}
		out += shellEvent("response.function_call_arguments.done", map[string]any{"type": "response.function_call_arguments.done", "item_id": "sc1", "output_index": 0, "arguments": args})
		out += shellEvent("response.output_item.done", map[string]any{"type": "response.output_item.done", "output_index": 0, "item": map[string]any{"type": "function_call", "id": "sc1", "call_id": "c1", "name": "shell", "arguments": args, "status": "completed"}})
		return out + shellEvent("response.completed", map[string]any{"type": "response.completed", "response": json.RawMessage(shellUpstream(FormatResponses, args))})
	}
}

func TestShellStreams(t *testing.T) {
	for _, upstream := range []string{FormatResponses, FormatChatCompletions, FormatMessages} {
		for _, stride := range []int{1, 17, 100000} {
			t.Run(upstream+"/"+strconv.Itoa(stride), func(t *testing.T) {
				state := NewRequestState()
				if _, err := NormalizeResponsesRequest([]byte(shellRequest), state); err != nil {
					t.Fatal(err)
				}
				converter, err := NewStreamConverter(upstream, FormatResponses, state)
				if err != nil {
					t.Fatal(err)
				}
				input := []byte(shellStream(upstream))
				var frames [][]byte
				done := false
				for len(input) > 0 {
					n := min(stride, len(input))
					events, d, err := converter.Feed(input[:n])
					if err != nil {
						t.Fatal(err)
					}
					frames = append(frames, events...)
					done = done || d
					input = input[n:]
				}
				tail, err := converter.Finish()
				if err != nil || !done {
					t.Fatalf("done=%v err=%v", done, err)
				}
				frames = append(frames, tail...)
				all := string(joinFrames(frames))
				if strings.Contains(all, "function_call_arguments") || strings.Contains(all, `"type":"function_call"`) {
					t.Fatalf("leaked function shell: %s", all)
				}
				decoder := &sse.Decoder{}
				events := decoder.Feed(joinFrames(frames))
				added, completed := 0, 0
				itemID := ""
				for _, ev := range events {
					doc := shellObject(t, ev.Data)
					switch ev.Event {
					case "response.output_item.added":
						if doc["item"].(map[string]any)["type"] == "shell_call" {
							assertShellResponseFields(t, doc["item"].(map[string]any), "in_progress")
							itemID, _ = doc["item"].(map[string]any)["id"].(string)
							if itemID == "" {
								t.Fatal("added shell has no id")
							}
							added++
						}
					case "response.output_item.done":
						assertShellItem(t, doc["item"].(map[string]any))
						if doc["item"].(map[string]any)["id"] != itemID {
							t.Fatal("shell id changed at item.done")
						}
					case "response.completed":
						assertShellItem(t, doc["response"].(map[string]any)["output"].([]any)[0].(map[string]any))
						if doc["response"].(map[string]any)["output"].([]any)[0].(map[string]any)["id"] != itemID {
							t.Fatal("shell id changed at response.completed")
						}
						completed++
					}
				}
				if added != 1 || completed != 1 {
					t.Fatalf("added=%d completed=%d", added, completed)
				}
			})
		}
	}
}

func TestShellBoundaries(t *testing.T) {
	for _, body := range []string{
		`{"tools":[{"type":"shell"},{"type":"function","name":"shell"}]}`,
		`{"tools":[{"type":"namespace","name":"n","tools":[{"type":"shell"}]}]}`,
		`{"tools":[{"type":"shell"}],"input":[{"type":"shell_call","call_id":"c","action":{"commands":[]}}]}`,
		`{"tools":[{"type":"shell"}],"input":[{"type":"shell_call_output","call_id":"c","output":[{"stdout":null,"stderr":"","outcome":{"type":"timeout"}}]}]}`,
	} {
		state := NewRequestState()
		if _, err := NormalizeResponsesRequest([]byte(body), state); err == nil {
			t.Fatalf("accepted %s", body)
		}
		plain := shellUpstream(FormatResponses, shellArgs)
		restored, err := ConvertResponse(FormatResponses, FormatResponses, 200, plain, state)
		if err != nil || string(restored) != string(plain) {
			t.Fatalf("failed normalization changed state: %s %v", restored, err)
		}
	}
	hosted := `{"tools":[{"type":"shell","environment":{"type":"container_reference","container_id":"cn1"}}],"input":[{"type":"shell_call","call_id":"c1","action":{"commands":["pwd"]}}]}`
	normalized, err := NormalizeResponsesRequest([]byte(hosted), NewRequestState())
	if err != nil || !strings.Contains(string(normalized), `"type":"shell_call"`) {
		t.Fatalf("hosted shell changed: %s %v", normalized, err)
	}
	for _, target := range []string{FormatChatCompletions, FormatMessages} {
		if _, err := ConvertRequest(target, "m", FormatResponses, []byte(hosted), NewRequestState()); err == nil {
			t.Fatal("hosted shell accepted")
		}
	}
	native, err := ConvertRequest(FormatResponses, "m", FormatResponses, []byte(shellRequest), NewRequestState())
	if err != nil || string(native) != shellRequest {
		t.Fatalf("native request changed: %s %v", native, err)
	}
}

func TestShellAdditionalToolsChoiceAndExactValues(t *testing.T) {
	request := []byte(`{"metadata":{"n":9007199254740993},"input":[{"type":"additional_tools","tools":[{"type":"shell"},{"type":"custom","name":"patch"}]},{"type":"shell_call","call_id":"c1","extension":9007199254740993,"action":{"commands":["pwd"],"timeout_ms":null,"max_output_length":9007199254740993}},{"type":"shell_call_output","call_id":"c1","max_output_length":9007199254740993,"output":[{"stdout":"ok","stderr":"","outcome":{"type":"exit","exit_code":0}}]}],"tool_choice":{"type":"allowed_tools","mode":"required","tools":[{"type":"shell"},{"type":"custom","name":"patch"}]}}`)
	state := NewRequestState()
	normalized, err := NormalizeResponsesRequest(request, state)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Count(string(normalized), "9007199254740993") != 4 || strings.Contains(string(normalized), `"additional_tools"`) {
		t.Fatalf("lost precise values: %s", normalized)
	}
	doc := shellObject(t, normalized)
	choice := doc["tool_choice"].(map[string]any)
	if choice["tools"].([]any)[0].(map[string]any)["name"] != "shell" {
		t.Fatal(choice)
	}
	output := doc["input"].([]any)[1].(map[string]any)
	if output["max_output_length"] != nil || !strings.Contains(output["output"].(string), "9007199254740993") {
		t.Fatal(output)
	}
	body := []byte(`{"id":"r","object":"response","status":"completed","output":[{"type":"function_call","id":"sc1","call_id":"c1","name":"shell","arguments":"{\"commands\":[\"pwd\"],\"max_output_length\":9007199254740993}","extension":9007199254740993},{"type":"function_call","call_id":"p1","name":"patch","arguments":"{\"input\":\"diff\"}"},{"type":"function_call","call_id":"o1","name":"other","arguments":"{}"}]}`)
	restored, err := ConvertResponse(FormatResponses, FormatResponses, 200, body, state)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Count(string(restored), "9007199254740993") != 2 {
		t.Fatalf("response precision lost: %s", restored)
	}
	items := shellObject(t, restored)["output"].([]any)
	if items[0].(map[string]any)["type"] != "shell_call" || items[1].(map[string]any)["type"] != "custom_tool_call" || items[2].(map[string]any)["type"] != "function_call" {
		t.Fatal(items)
	}
}

func TestShellStreamRejectsInvalidActions(t *testing.T) {
	for _, upstream := range []string{FormatResponses, FormatChatCompletions, FormatMessages} {
		t.Run(upstream, func(t *testing.T) {
			state := NewRequestState()
			if _, err := NormalizeResponsesRequest([]byte(shellRequest), state); err != nil {
				t.Fatal(err)
			}
			stream, err := NewStreamConverter(upstream, FormatResponses, state)
			if err != nil {
				t.Fatal(err)
			}
			malformed := strings.ReplaceAll(shellStream(upstream), "commands", "unknown")
			_, done, err := stream.Feed([]byte(malformed))
			if err == nil || done {
				t.Fatalf("invalid action accepted: done=%v err=%v", done, err)
			}
			if _, err := stream.Finish(); err == nil {
				t.Fatal("invalid action accepted at EOF")
			}
		})
	}
}

func TestShellDoesNotRewriteOrdinaryTools(t *testing.T) {
	plain := shellUpstream(FormatResponses, shellArgs)
	state := NewRequestState()
	if _, err := NormalizeResponsesRequest([]byte(`{"tools":[{"type":"function","name":"shell"}]}`), state); err != nil {
		t.Fatal(err)
	}
	output, err := ConvertResponse(FormatResponses, FormatResponses, 200, plain, state)
	if err != nil || string(output) != string(plain) {
		t.Fatalf("ordinary shell-named function changed: %s %v", output, err)
	}
	legacy := []byte(`{"tools":[{"type":"local_shell"}]}`)
	output, err = NormalizeResponsesRequest(legacy, NewRequestState())
	if err != nil || string(output) != string(legacy) {
		t.Fatalf("legacy tool changed: %s %v", output, err)
	}
}

func TestShellMessagesEOFAndInitialInput(t *testing.T) {
	for _, valid := range []bool{true, false} {
		state := NewRequestState()
		if _, err := NormalizeResponsesRequest([]byte(shellRequest), state); err != nil {
			t.Fatal(err)
		}
		converter, err := NewStreamConverter(FormatMessages, FormatResponses, state)
		if err != nil {
			t.Fatal(err)
		}
		input := shellStream(FormatMessages)
		cut := strings.Index(input, "event: message_stop")
		if cut < 0 {
			t.Fatal("fixture is missing the message_stop event")
		}
		input = input[:cut]
		if !valid {
			input = strings.ReplaceAll(input, "commands", "unknown")
		}
		if _, _, err := converter.Feed([]byte(input)); err != nil {
			t.Fatal(err)
		}
		_, err = converter.Finish()
		if (err == nil) != valid {
			t.Fatalf("valid=%v err=%v", valid, err)
		}
	}
	state := NewRequestState()
	if _, err := NormalizeResponsesRequest([]byte(shellRequest), state); err != nil {
		t.Fatal(err)
	}
	converter, _ := NewStreamConverter(FormatMessages, FormatResponses, state)
	input := shellEvent("message_start", map[string]any{"type": "message_start", "message": map[string]any{"id": "r", "model": "m"}}) + shellEvent("content_block_start", map[string]any{"type": "content_block_start", "index": 0, "content_block": map[string]any{"type": "tool_use", "id": "c1", "name": "shell", "input": json.RawMessage(shellArgs)}}) + shellEvent("content_block_stop", map[string]any{"type": "content_block_stop", "index": 0}) + shellEvent("message_stop", map[string]any{"type": "message_stop"})
	events, done, err := converter.Feed([]byte(input))
	if err != nil || !done {
		t.Fatalf("done=%v err=%v", done, err)
	}
	decoder := &sse.Decoder{}
	for _, event := range decoder.Feed(joinFrames(events)) {
		if event.Event == "response.completed" {
			doc := shellObject(t, event.Data)
			assertShellItem(t, doc["response"].(map[string]any)["output"].([]any)[0].(map[string]any))
		}
	}
}

func TestShellRequestFailurePreservesIdentities(t *testing.T) {
	failures := []string{
		`{"tools":[{"type":"shell"}],"input":42}`,
		`{"tools":[{"type":"shell"}],"instructions":42}`,
		`{"tools":[{"type":"shell"},{"type":"namespace","name":"n","tools":[{"type":"custom","name":"run"}]}],"input":[{"type":"message","role":"invalid","content":"hi"}]}`,
	}
	for _, target := range []string{FormatChatCompletions, FormatMessages} {
		for _, request := range failures {
			state := NewRequestState()
			if _, err := NormalizeResponsesRequest([]byte(`{"tools":[{"type":"custom","name":"existing"}]}`), state); err != nil {
				t.Fatal(err)
			}
			if _, err := ConvertRequest(target, "m", FormatResponses, []byte(request), state); err == nil {
				t.Fatalf("accepted invalid request: %s", request)
			}
			if conversionContext(state).IsShell("shell") || conversionContext(state).IsCustom("n__run") || !conversionContext(state).IsCustom("existing") {
				t.Fatal("failed request changed existing tool identities")
			}
			// A corrected retry using an ordinary function must keep that identity.
			if _, err := ConvertRequest(target, "m", FormatResponses, []byte(`{"tools":[{"type":"function","name":"shell"}],"input":"hi"}`), state); err != nil {
				t.Fatal(err)
			}
			plain := shellUpstream(FormatResponses, shellArgs)
			output, err := ConvertResponse(FormatResponses, FormatResponses, 200, plain, state)
			if err != nil || string(output) != string(plain) {
				t.Fatalf("retry restored ordinary function as shell: %s %v", output, err)
			}
		}
		state := NewRequestState()
		if _, err := ConvertRequest(target, "m", FormatResponses, []byte(shellRequest), state); err != nil {
			t.Fatal(err)
		}
		if !conversionContext(state).IsShell("shell") {
			t.Fatal("successful conversion did not commit shell identity")
		}
	}
}

func TestShellMessagesEmptyInitialObject(t *testing.T) {
	for _, empty := range []string{`{}`, `{ }`, "{\t  }"} {
		for _, eof := range []bool{false, true} {
			state := NewRequestState()
			if _, err := NormalizeResponsesRequest([]byte(shellRequest), state); err != nil {
				t.Fatal(err)
			}
			converter, _ := NewStreamConverter(FormatMessages, FormatResponses, state)
			input := strings.Replace(shellStream(FormatMessages), `"input":{}`, `"input":`+empty, 1)
			if eof {
				cut := strings.Index(input, "event: message_stop")
				if cut < 0 {
					t.Fatal("fixture is missing the message_stop event")
				}
				input = input[:cut]
			}
			frames, done, err := converter.Feed([]byte(input))
			if err != nil || done == eof {
				t.Fatalf("input=%q eof=%v done=%v err=%v", empty, eof, done, err)
			}
			tail, err := converter.Finish()
			if err != nil {
				t.Fatalf("input=%q eof=%v err=%v", empty, eof, err)
			}
			frames = append(frames, tail...)
			completed := 0
			decoder := &sse.Decoder{}
			for _, event := range decoder.Feed(joinFrames(frames)) {
				if event.Event == "response.completed" {
					doc := shellObject(t, event.Data)
					assertShellItem(t, doc["response"].(map[string]any)["output"].([]any)[0].(map[string]any))
					completed++
				}
			}
			if completed != 1 {
				t.Fatalf("completed=%d", completed)
			}
		}
	}
}

func TestShellLocalSkills(t *testing.T) {
	for _, target := range []string{FormatResponses, FormatChatCompletions, FormatMessages} {
		for _, policy := range []LossPolicy{LossPolicyAllow, LossPolicySafe, LossPolicyStrict} {
			convert := func(request string, state *RequestState) ([]byte, *Error) {
				if target == FormatResponses {
					return NormalizeResponsesRequest([]byte(request), state)
				}
				return ConvertRequest(target, "m", FormatResponses, []byte(request), state)
			}
			state := NewRequestState()
			state.Options.LossPolicy = policy
			request := strings.Replace(shellRequest, `"type":"local"`, `"type":"local","skills":[]`, 1)
			body, err := convert(request, state)
			if err != nil || !conversionContext(state).IsShell("shell") || strings.Contains(string(body), `"skills"`) {
				t.Fatalf("%s/%s empty skills: %s %v", target, policy, body, err)
			}
			for _, skills := range []string{`[{}]`, `"invalid"`, `null`, `{}`, `false`, `42`} {
				state := NewRequestState()
				state.Options.LossPolicy = policy
				bad := strings.Replace(request, `"skills":[]`, `"skills":`+skills, 1)
				if _, err := convert(bad, state); err == nil {
					t.Fatalf("%s/%s accepted skills %s", target, policy, skills)
				}
				if conversionContext(state).IsShell("shell") {
					t.Fatal("unsupported skills committed shell identity")
				}
			}
		}
	}
	// The same environment handling applies to merged declarations and history.
	state := NewRequestState()
	request := `{"input":[{"type":"additional_tools","tools":[{"type":"shell","environment":{"type":"local","skills":[]}}]},{"type":"shell_call","call_id":"c1","environment":{"type":"local","skills":[]},"action":{"commands":["pwd"]}}]}`
	body, err := NormalizeResponsesRequest([]byte(request), state)
	if err != nil || !conversionContext(state).IsShell("shell") || strings.Contains(string(body), `"environment"`) {
		t.Fatalf("additional tools/history: %s %v", body, err)
	}
}

func TestShellNativeUnrelatedArgumentsDonePassthrough(t *testing.T) {
	for _, declaration := range []string{"", `{"tools":[{"type":"function","name":"ordinary"}]}`, shellRequest} {
		var state *RequestState
		if declaration != "" {
			state = NewRequestState()
			if _, err := NormalizeResponsesRequest([]byte(declaration), state); err != nil {
				t.Fatal(err)
			}
		}
		converter, _ := NewStreamConverter(FormatResponses, FormatResponses, state)
		frame := []byte("event: response.function_call_arguments.done\ndata: {broken\n\n")
		frames, done, err := converter.Feed(frame)
		if err != nil || done || string(joinFrames(frames)) != string(frame) {
			t.Fatalf("native frame changed: %s done=%v err=%v", joinFrames(frames), done, err)
		}
	}
	// Mapped shell events still receive strict validation.
	state := NewRequestState()
	if _, err := NormalizeResponsesRequest([]byte(shellRequest), state); err != nil {
		t.Fatal(err)
	}
	converter, _ := NewStreamConverter(FormatResponses, FormatResponses, state)
	added := shellEvent("response.output_item.added", map[string]any{"type": "response.output_item.added", "item": map[string]any{"id": "sc1", "call_id": "c1", "type": "function_call", "name": "shell", "arguments": ""}})
	if _, _, err := converter.Feed([]byte(added)); err != nil {
		t.Fatal(err)
	}
	invalid := shellEvent("response.function_call_arguments.done", map[string]any{"type": "response.function_call_arguments.done", "item_id": "sc1", "arguments": 42})
	if _, done, err := converter.Feed([]byte(invalid)); err == nil || done {
		t.Fatalf("mapped shell event not validated: done=%v err=%v", done, err)
	}
}

func TestShellItemIDsPreserveNativeAndDistinguishCalls(t *testing.T) {
	state := NewRequestState()
	if _, err := NormalizeResponsesRequest([]byte(shellRequest), state); err != nil {
		t.Fatal(err)
	}
	var ids []string
	for _, callID := range []string{"c1", "c2"} {
		response := strings.ReplaceAll(string(shellUpstream(FormatChatCompletions, shellArgs)), `"id":"c1"`, `"id":"`+callID+`"`)
		output, err := ConvertResponse(FormatChatCompletions, FormatResponses, 200, []byte(response), state)
		if err != nil {
			t.Fatal(err)
		}
		item := shellObject(t, output)["output"].([]any)[0].(map[string]any)
		id, _ := item["id"].(string)
		if id == "" || item["call_id"] != callID {
			t.Fatal(item)
		}
		ids = append(ids, id)
	}
	if ids[0] == ids[1] {
		t.Fatal("distinct calls have the same item id")
	}
	native := string(shellUpstream(FormatResponses, shellArgs))
	for _, withID := range []bool{true, false} {
		body := native
		if !withID {
			body = strings.Replace(body, `"id":"sc1",`, "", 1)
		}
		restored, err := ConvertResponse(FormatResponses, FormatResponses, 200, []byte(body), state)
		if err != nil {
			t.Fatal(err)
		}
		item := shellObject(t, restored)["output"].([]any)[0].(map[string]any)
		want := "sc1"
		if !withID {
			want = ids[0]
		}
		if item["id"] != want {
			t.Fatalf("id=%v want %s", item["id"], want)
		}
	}
}

func assertShellResponseFields(t *testing.T, item map[string]any, status string) {
	t.Helper()
	for _, key := range []string{"id", "type", "call_id", "status", "environment", "action"} {
		if _, ok := item[key]; !ok {
			t.Fatalf("shell response missing %s: %v", key, item)
		}
	}
	if item["status"] != status {
		t.Fatalf("shell status=%v want %s", item["status"], status)
	}
	environment, ok := item["environment"].(map[string]any)
	if !ok || environment["type"] != "local" {
		t.Fatalf("shell environment=%v", item["environment"])
	}
	action, ok := item["action"].(map[string]any)
	if !ok {
		t.Fatalf("shell action=%v", item["action"])
	}
	for _, key := range []string{"commands", "timeout_ms", "max_output_length"} {
		if _, ok := action[key]; !ok {
			t.Fatalf("shell response action missing %s: %v", key, action)
		}
	}
}

func TestShellResponseNullableFields(t *testing.T) {
	arguments := []string{
		`{"commands":["pwd"]}`,
		`{"commands":["pwd"],"timeout_ms":3000}`,
		`{"commands":["pwd"],"max_output_length":4096}`,
		`{"commands":["pwd"],"timeout_ms":null,"max_output_length":null}`,
	}
	for _, upstream := range []string{FormatResponses, FormatChatCompletions, FormatMessages} {
		for _, args := range arguments {
			state := NewRequestState()
			if _, err := NormalizeResponsesRequest([]byte(shellRequest), state); err != nil {
				t.Fatal(err)
			}
			expected := shellObject(t, []byte(args))
			for _, key := range []string{"timeout_ms", "max_output_length"} {
				if _, ok := expected[key]; !ok {
					expected[key] = nil
				}
			}
			verify := func(item map[string]any) {
				t.Helper()
				assertShellResponseFields(t, item, "completed")
				if !reflect.DeepEqual(item["action"], expected) {
					t.Fatalf("%s action=%v want %v", upstream, item["action"], expected)
				}
			}
			body, err := ConvertResponse(upstream, FormatResponses, 200, shellUpstream(upstream, args), state)
			if err != nil {
				t.Fatal(err)
			}
			verify(shellObject(t, body)["output"].([]any)[0].(map[string]any))
			converter, _ := NewStreamConverter(upstream, FormatResponses, state)
			// Native added events may also omit status; infer in_progress at announce.
			input := strings.ReplaceAll(shellStreamWithArgs(upstream, args), `,"status":"in_progress"`, "")
			frames, done, err := converter.Feed([]byte(input))
			if err != nil || !done {
				t.Fatalf("%s done=%v err=%v", upstream, done, err)
			}
			decoder := &sse.Decoder{}
			added, itemDone, completed := 0, 0, 0
			for _, event := range decoder.Feed(joinFrames(frames)) {
				doc := shellObject(t, event.Data)
				switch event.Event {
				case "response.output_item.added":
					item := doc["item"].(map[string]any)
					assertShellResponseFields(t, item, "in_progress")
					action := item["action"].(map[string]any)
					if action["timeout_ms"] != nil || action["max_output_length"] != nil {
						t.Fatalf("added action=%v", action)
					}
					added++
				case "response.output_item.done":
					verify(doc["item"].(map[string]any))
					itemDone++
				case "response.completed":
					verify(doc["response"].(map[string]any)["output"].([]any)[0].(map[string]any))
					completed++
				}
			}
			if added != 1 || itemDone != 1 || completed != 1 {
				t.Fatalf("%s lifecycle counts=%d/%d/%d", upstream, added, itemDone, completed)
			}
		}
	}
}

func TestShellValidationPreservesUpstreamFailures(t *testing.T) {
	state := NewRequestState()
	if _, err := NormalizeResponsesRequest([]byte(shellRequest), state); err != nil {
		t.Fatal(err)
	}
	for _, upstream := range []string{FormatResponses, FormatChatCompletions, FormatMessages} {
		for _, code := range []string{"server_error", "rate_limit_exceeded", "authentication_error", "503"} {
			body := shellObject(t, shellUpstream(upstream, ""))
			body["error"] = map[string]any{"code": code, "message": "failed"}
			if upstream == FormatResponses {
				body["status"] = "failed"
			}
			payload := shellJSON(body)
			_, baseline := ConvertResponse(upstream, FormatResponses, 200, payload, nil)
			_, restored := ConvertResponse(upstream, FormatResponses, 200, payload, state)
			if baseline == nil || restored == nil || !reflect.DeepEqual(baseline, restored) || restored.Class == ClassTranslation {
				t.Fatalf("%s/%s baseline=%v restored=%v", upstream, code, baseline, restored)
			}
			if !restored.Retryable {
				t.Fatalf("%s/%s lost retryability: %v", upstream, code, restored)
			}
		}
		// Successful responses with incomplete shell parameters still fail validation.
		if _, err := ConvertResponse(upstream, FormatResponses, 200, shellUpstream(upstream, ""), state); err == nil || err.Class != ClassTranslation {
			t.Fatalf("%s invalid successful shell response accepted: %v", upstream, err)
		}
	}
	for _, status := range []string{"failed", "cancelled"} {
		body := shellObject(t, shellUpstream(FormatResponses, ""))
		body["status"] = status
		_, err := ConvertResponse(FormatResponses, FormatResponses, 200, shellJSON(body), state)
		if err == nil || err.Class != ClassUpstream || !err.Retryable {
			t.Fatalf("status=%s err=%v", status, err)
		}
	}
}

func TestShellNativeStreamRetainsFirstItemIDs(t *testing.T) {
	for _, terminal := range []string{"completed", "incomplete"} {
		for _, stride := range []int{1, 100000} {
			state := NewRequestState()
			if _, err := NormalizeResponsesRequest([]byte(shellRequest), state); err != nil {
				t.Fatal(err)
			}
			item := func(callID string) map[string]any {
				return map[string]any{"type": "function_call", "call_id": callID, "name": "shell", "arguments": shellArgs}
			}
			stream := ""
			expected := map[string]string{"c1": "native_a", "c2": "native_b"}
			for _, callID := range []string{"c1", "c2"} {
				added := item(callID)
				added["id"] = expected[callID]
				added["arguments"] = ""
				stream += shellEvent("response.output_item.added", map[string]any{"type": "response.output_item.added", "item": added})
			}
			for _, callID := range []string{"c2", "c1"} {
				// Done snapshots and the terminal response both omit the native item id.
				stream += shellEvent("response.output_item.done", map[string]any{"type": "response.output_item.done", "item": item(callID)})
			}
			stream += shellEvent("response."+terminal, map[string]any{"type": "response." + terminal, "response": map[string]any{"id": "r", "object": "response", "status": terminal, "output": []any{item("c1"), item("c2")}}})
			converter, _ := NewStreamConverter(FormatResponses, FormatResponses, state)
			input := []byte(stream)
			var frames [][]byte
			done := false
			for len(input) > 0 {
				n := min(stride, len(input))
				out, d, err := converter.Feed(input[:n])
				if err != nil {
					t.Fatal(err)
				}
				frames = append(frames, out...)
				done = done || d
				input = input[n:]
			}
			if !done {
				t.Fatal("stream did not terminate")
			}
			decoder := &sse.Decoder{}
			observations := 0
			check := func(v map[string]any) {
				t.Helper()
				callID := v["call_id"].(string)
				if v["id"] != expected[callID] {
					t.Fatalf("%s id=%v want %s", callID, v["id"], expected[callID])
				}
				observations++
			}
			for _, event := range decoder.Feed(joinFrames(frames)) {
				doc := shellObject(t, event.Data)
				if event.Event == "response.output_item.added" || event.Event == "response.output_item.done" {
					check(doc["item"].(map[string]any))
				} else {
					for _, v := range doc["response"].(map[string]any)["output"].([]any) {
						check(v.(map[string]any))
					}
				}
			}
			if observations != 6 {
				t.Fatalf("observations=%d", observations)
			}
		}
	}
}

func TestShellRejectsBlankResponseCallIDs(t *testing.T) {
	state := NewRequestState()
	if _, err := NormalizeResponsesRequest([]byte(shellRequest), state); err != nil {
		t.Fatal(err)
	}
	for _, format := range []string{FormatResponses, FormatChatCompletions, FormatMessages} {
		for _, id := range []any{nil, "", " \t ", 42} {
			body := shellObject(t, shellUpstream(format, shellArgs))
			var call map[string]any
			key := "id"
			switch format {
			case FormatResponses:
				call = body["output"].([]any)[0].(map[string]any)
				key = "call_id"
			case FormatMessages:
				call = body["content"].([]any)[0].(map[string]any)
			default:
				call = body["choices"].([]any)[0].(map[string]any)["message"].(map[string]any)["tool_calls"].([]any)[0].(map[string]any)
			}
			if id == nil {
				delete(call, key)
			} else {
				call[key] = id
			}
			if _, err := ConvertResponse(format, FormatResponses, 200, shellJSON(body), state); err == nil || err.Class != ClassTranslation {
				t.Fatalf("%s id=%v err=%v", format, id, err)
			}
		}
	}
	for _, id := range []any{nil, "", " \t ", 42} {
		for _, event := range []string{"response.output_item.added", "response.output_item.done", "response.completed", "response.incomplete"} {
			item := map[string]any{"type": "function_call", "name": "shell", "id": "sc1", "arguments": shellArgs}
			if id != nil {
				item["call_id"] = id
			}
			payload := map[string]any{"type": event, "item": item}
			if strings.HasSuffix(event, "completed") || strings.HasSuffix(event, "incomplete") {
				delete(payload, "item")
				payload["response"] = map[string]any{"id": "r", "object": "response", "status": strings.TrimPrefix(event, "response."), "output": []any{item}}
			}
			converter, _ := NewStreamConverter(FormatResponses, FormatResponses, state)
			out, done, err := converter.Feed([]byte(shellEvent(event, payload)))
			if err == nil || done || strings.Contains(string(joinFrames(out)), `"type":"shell_call"`) {
				t.Fatalf("%s id=%v done=%v err=%v frames=%s", event, id, done, err, joinFrames(out))
			}
		}
	}
}

func TestShellActionRejectsUnknownProperties(t *testing.T) {
	args := `{"commands":["pwd"],"cwd":"/tmp"}`
	for _, target := range []string{FormatResponses, FormatChatCompletions, FormatMessages} {
		state := NewRequestState()
		request := `{"tools":[{"type":"shell"}],"input":[{"type":"shell_call","call_id":"c1","action":` + args + `}]}`
		var err *Error
		if target == FormatResponses {
			_, err = NormalizeResponsesRequest([]byte(request), state)
		} else {
			_, err = ConvertRequest(target, "m", FormatResponses, []byte(request), state)
		}
		if err == nil || err.Class != ClassTranslation || conversionContext(state).IsShell("shell") {
			t.Fatalf("%s accepted invalid history: %v", target, err)
		}
	}
	state := NewRequestState()
	if _, err := NormalizeResponsesRequest([]byte(shellRequest), state); err != nil {
		t.Fatal(err)
	}
	for _, format := range []string{FormatResponses, FormatChatCompletions, FormatMessages} {
		if _, err := ConvertResponse(format, FormatResponses, 200, shellUpstream(format, args), state); err == nil || err.Class != ClassTranslation {
			t.Fatalf("%s accepted invalid action: %v", format, err)
		}
		converter, _ := NewStreamConverter(format, FormatResponses, state)
		_, done, err := converter.Feed([]byte(shellStreamWithArgs(format, args)))
		if err == nil || done {
			t.Fatalf("%s stream accepted invalid action: done=%v err=%v", format, done, err)
		}
	}
}

func TestShellHistoryRejectsNullSkills(t *testing.T) {
	for _, target := range []string{FormatResponses, FormatChatCompletions, FormatMessages} {
		for _, source := range []string{
			`{"input":[{"type":"additional_tools","tools":[{"type":"shell","environment":{"type":"local","skills":null}}]}]}`,
			`{"input":[{"type":"shell_call","call_id":"c1","environment":{"type":"local","skills":null},"action":{"commands":["pwd"]}}]}`,
		} {
			state := NewRequestState()
			var err *Error
			if target == FormatResponses {
				_, err = NormalizeResponsesRequest([]byte(source), state)
			} else {
				_, err = ConvertRequest(target, "m", FormatResponses, []byte(source), state)
			}
			if err == nil || err.Class != ClassTranslation || conversionContext(state).IsShell("shell") {
				t.Fatalf("%s accepted null skills: %v", target, err)
			}
		}
	}
}

func TestShellRejectsConflictingCustomHistory(t *testing.T) {
	sources := []string{
		`{"tools":[{"type":"shell"}],"input":[{"type":"custom_tool_call","name":"shell","call_id":"old","input":"echo old"}]}`,
		`{"input":[{"type":"custom_tool_call","name":"shell","call_id":"old","input":"echo old"},{"type":"additional_tools","tools":[{"type":"shell"}]}]}`,
		`{"input":[{"type":"shell_call","call_id":"c1","action":{"commands":["pwd"]}},{"type":"custom_tool_call","name":"shell","call_id":"old","input":"echo old"}]}`,
	}
	for _, target := range []string{FormatResponses, FormatChatCompletions, FormatMessages} {
		for _, policy := range []LossPolicy{LossPolicyAllow, LossPolicySafe, LossPolicyStrict} {
			for _, source := range sources {
				state := NewRequestState()
				state.Options.LossPolicy = policy
				if _, err := NormalizeResponsesRequest([]byte(`{"tools":[{"type":"custom","name":"existing"}]}`), state); err != nil {
					t.Fatal(err)
				}
				var out []byte
				var err *Error
				if target == FormatResponses {
					out, err = NormalizeResponsesRequest([]byte(source), state)
				} else {
					out, err = ConvertRequest(target, "m", FormatResponses, []byte(source), state)
				}
				if err == nil || err.Class != ClassTranslation || out != nil {
					t.Fatalf("%s/%s accepted conflicting custom shell history: %s %v", target, policy, out, err)
				}
				if conversionContext(state).IsShell("shell") || !conversionContext(state).IsCustom("existing") {
					t.Fatal("rejected history changed tool identities")
				}
			}
		}
	}
}

func TestShellPreservesNamespacedCustomHistory(t *testing.T) {
	source := []byte(`{"tools":[{"type":"shell"},{"type":"namespace","name":"old","tools":[{"type":"custom","name":"shell"}]}],"input":[{"type":"custom_tool_call","name":"shell","namespace":"old","call_id":"old1","input":"echo old"}]}`)
	for _, target := range []string{FormatResponses, FormatChatCompletions, FormatMessages} {
		state := NewRequestState()
		var out []byte
		var err *Error
		if target == FormatResponses {
			out, err = NormalizeResponsesRequest(source, state)
		} else {
			out, err = ConvertRequest(target, "m", FormatResponses, source, state)
		}
		if err != nil || !strings.Contains(string(out), `"name":"old__shell"`) || !conversionContext(state).IsShell("shell") || !conversionContext(state).IsCustom("old__shell") {
			t.Fatalf("%s lost distinct namespaced history: %s %v", target, out, err)
		}
		body := []byte(`{"id":"r","object":"response","status":"completed","output":[{"type":"function_call","call_id":"old1","name":"old__shell","arguments":"{\"input\":\"echo old\"}"}]}`)
		restored, err := ConvertResponse(FormatResponses, FormatResponses, 200, body, state)
		if err != nil {
			t.Fatal(err)
		}
		call := shellObject(t, restored)["output"].([]any)[0].(map[string]any)
		if call["type"] != "custom_tool_call" || call["namespace"] != "old" || call["name"] != "shell" || call["input"] != "echo old" {
			t.Fatalf("%s custom history restored as shell: %v", target, call)
		}
	}
}
