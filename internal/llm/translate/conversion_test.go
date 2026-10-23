package translate_test

import (
	"encoding/json"
	"fmt"
	"strings"
	"testing"

	"github.com/dillonzq/apix/internal/llm/translate"
)

type wireEvent struct {
	Name string
	Data map[string]any
}

func wireEvents(t *testing.T, frames [][]byte) []wireEvent {
	t.Helper()
	var out []wireEvent
	for _, frame := range frames {
		e := wireEvent{}
		var data []string
		normalized := strings.ReplaceAll(strings.ReplaceAll(string(frame), "\r\n", "\n"), "\r", "\n")
		if !strings.HasSuffix(normalized, "\n\n") {
			t.Fatalf("unterminated frame: %q", frame)
		}
		for _, line := range strings.Split(normalized, "\n") {
			if strings.HasPrefix(line, "event:") {
				e.Name = strings.TrimSpace(strings.TrimPrefix(line, "event:"))
			}
			if strings.HasPrefix(line, "data:") {
				data = append(data, strings.TrimPrefix(strings.TrimPrefix(line, "data:"), " "))
			}
		}
		payload := strings.Join(data, "\n")
		if payload == "[DONE]" {
			e.Name = "DONE"
		} else if json.Unmarshal([]byte(payload), &e.Data) != nil {
			t.Fatalf("invalid frame: %q", frame)
		}
		out = append(out, e)
	}
	return out
}
func streamFrames(t *testing.T, from, to, body string, state *translate.RequestState) []wireEvent {
	t.Helper()
	sc, err := translate.NewStreamConverter(from, to, state)
	if err != nil {
		t.Fatal(err)
	}
	var frames [][]byte
	for _, b := range []byte(body) {
		events, _, err := sc.Feed([]byte{b})
		if err != nil {
			t.Fatal(err)
		}
		frames = append(frames, events...)
	}
	tail, err := sc.Finish()
	if err != nil {
		t.Fatal(err)
	}
	frames = append(frames, tail...)
	again, err := sc.Finish()
	if err != nil || len(again) > 0 {
		t.Fatalf("repeated EOF: %q %v", again, err)
	}
	return wireEvents(t, frames)
}
func TestConversionNamespaceCollisionBothOrders(t *testing.T) {
	plain := `{"type":"function","name":"ns__f","parameters":{"type":"object"}}`
	group := `{"type":"namespace","name":"ns","tools":[{"type":"function","name":"f","parameters":{"type":"object"}}]}`
	for _, tools := range []string{plain + "," + group, group + "," + plain} {
		for _, target := range []string{translate.FormatChatCompletions, translate.FormatMessages} {
			_, err := translate.ConvertRequest(target, "m", translate.FormatResponses, []byte(`{"input":"hi","tools":[`+tools+`]}`), translate.NewRequestState())
			if err == nil {
				t.Fatalf("ambiguous names accepted by %s", target)
			}
		}
	}
}
func TestConversionFailedAndUnknownResponses(t *testing.T) {
	for _, target := range []string{translate.FormatChatCompletions, translate.FormatMessages} {
		for _, body := range []string{
			`{"id":"r","status":"failed","error":{"code":"server_error","message":"Bearer secret"},"output":[]}`,
			`{"id":"r","status":"cancelled","output":[]}`,
			`{"id":"r","status":"in_progress","output":[]}`,
			`{"id":"r","status":"completed","output":[{"type":"image_generation_call","result":"actual image"}]}`,
			`{"id":"r","status":"completed","output":[{"type":"future","action":{"content":"real"}}]}`,
		} {
			_, err := translate.ConvertResponse(translate.FormatResponses, target, 200, []byte(body), nil)
			if err == nil || strings.Contains(err.Message, "secret") {
				t.Fatalf("failure accepted/leaked: %s -> %v", body, err)
			}
		}
	}
}
func TestConversionChatToolsLateIdentityAndInterleaving(t *testing.T) {
	body := ""
	chunks := []string{
		`{"id":"r","model":"m","choices":[{"delta":{"tool_calls":[{"index":0,"function":{"name":"look","arguments":"{\"x\":"}}]}}]}`,
		`{"choices":[{"delta":{"tool_calls":[{"index":1,"id":"b","function":{"name":"g","arguments":"{}"}}]}}]}`,
		`{"choices":[{"delta":{"content":"after tools"}}]}`,
		`{"choices":[{"delta":{"tool_calls":[{"index":0,"id":"a","function":{"name":"up","arguments":"1}"}}]}}]}`,
		`{"choices":[{"delta":{},"finish_reason":"tool_calls"}],"usage":{"prompt_tokens":4,"completion_tokens":2}}`,
	}
	for _, chunk := range chunks {
		body += "data: " + chunk + "\n\n"
	}
	body += "data: [DONE]\n\n"
	events := streamFrames(t, translate.FormatChatCompletions, translate.FormatMessages, body, nil)
	active := -1
	args := map[int]string{}
	tools := map[int]map[string]any{}
	for _, e := range events {
		idx, _ := e.Data["index"].(float64)
		i := int(idx)
		switch e.Name {
		case "content_block_start":
			if active != -1 {
				t.Fatal("overlapping blocks")
			}
			active = i
			block := e.Data["content_block"].(map[string]any)
			if block["type"] == "tool_use" {
				if block["id"] == "" || block["name"] == "" {
					t.Fatal("empty tool identity")
				}
				tools[i] = block
			}
		case "content_block_delta":
			if active != i {
				t.Fatal("delta sent to closed block")
			}
			delta := e.Data["delta"].(map[string]any)
			if s, ok := delta["partial_json"].(string); ok {
				args[i] += s
			}
		case "content_block_stop":
			if active != i {
				t.Fatal("duplicate block stop")
			}
			active = -1
		}
	}
	if active != -1 || len(tools) != 2 {
		t.Fatalf("invalid final lifecycle: %+v", tools)
	}
	for i, tool := range tools {
		if !json.Valid([]byte(args[i])) {
			t.Fatalf("invalid arguments %s", args[i])
		}
		if tool["id"] == "a" && tool["name"] != "lookup" {
			t.Fatalf("fragmented name lost: %+v", tool)
		}
	}
}
func TestConversionSSEMultiDataAndSequence(t *testing.T) {
	body := "data: {\"id\":\"r\",\n" + "data: \"choices\":[{\"delta\":{\"content\":\"你好\"}}]}\n\ndata: {\"choices\":[{\"delta\":{},\"finish_reason\":\"stop\"}]}\n\ndata: [DONE]\n\n"
	for _, target := range formats {
		events := streamFrames(t, translate.FormatChatCompletions, target, body, nil)
		var all strings.Builder
		sequence := 0
		for _, e := range events {
			raw, _ := json.Marshal(e.Data)
			all.Write(raw)
			if target == translate.FormatResponses {
				if e.Data["sequence_number"] != float64(sequence) {
					t.Fatalf("sequence gap: %+v", e)
				}
				sequence++
			}
		}
		if !strings.Contains(all.String(), "你好") {
			t.Fatal("multi-data text lost")
		}
	}
}
func TestConversionStickyStreamErrors(t *testing.T) {
	sc, _ := translate.NewStreamConverter(translate.FormatChatCompletions, translate.FormatChatCompletions, nil)
	events, done, err := sc.Feed([]byte("data: {\"error\":{\"message\":\"Bearer secret\",\"type\":\"server_error\"}}\n\ndata: [DONE]\n\n"))
	if err == nil || done || len(events) > 0 || strings.Contains(err.Message, "secret") {
		t.Fatalf("native error not classified: %q %t %v", events, done, err)
	}
	tail, finishErr := sc.Finish()
	if finishErr == nil || len(tail) > 0 {
		t.Fatal("error converted into successful EOF")
	}
}
func TestConversionStructuredOutputAndToolStrict(t *testing.T) {
	schema := `{"type":"object","properties":{"value":{"type":"string"}},"required":["value"],"additionalProperties":false}`
	sources := map[string]string{
		translate.FormatChatCompletions: `{"messages":[{"role":"user","content":"hi"}],"response_format":{"type":"json_schema","json_schema":{"name":"answer","strict":true,"schema":` + schema + `}},"tools":[{"type":"function","function":{"name":"f","strict":false,"parameters":` + schema + `}}]}`,
		translate.FormatResponses:       `{"input":"hi","text":{"format":{"type":"json_schema","name":"answer","strict":true,"schema":` + schema + `}},"tools":[{"type":"function","name":"f","strict":false,"parameters":` + schema + `}]}`,
		translate.FormatMessages:        `{"messages":[{"role":"user","content":"hi"}],"max_tokens":100,"output_config":{"format":{"type":"json_schema","schema":` + schema + `}},"tools":[{"name":"f","strict":false,"input_schema":` + schema + `}]}`,
	}
	for from, body := range sources {
		for _, to := range formats {
			if from == to {
				continue
			}
			t.Run(from+"/"+to, func(t *testing.T) {
				out, err := translate.ConvertRequest(to, "m", from, []byte(body), translate.NewRequestState())
				if err != nil {
					t.Fatal(err)
				}
				doc := object(t, out)
				var format map[string]any
				switch to {
				case translate.FormatChatCompletions:
					format = doc["response_format"].(map[string]any)["json_schema"].(map[string]any)
				case translate.FormatResponses:
					format = doc["text"].(map[string]any)["format"].(map[string]any)
				case translate.FormatMessages:
					format = doc["output_config"].(map[string]any)["format"].(map[string]any)
				}
				raw, _ := json.Marshal(format["schema"])
				want := object(t, []byte(schema))
				got := object(t, raw)
				if fmt.Sprint(got) != fmt.Sprint(want) {
					t.Fatalf("schema changed: %s", out)
				}
				tool := doc["tools"].([]any)[0].(map[string]any)
				if to == translate.FormatChatCompletions {
					tool = tool["function"].(map[string]any)
				}
				if strict, ok := tool["strict"]; !ok || strict != false {
					t.Fatalf("explicit false strict lost: %s", out)
				}
			})
		}
	}
}
func TestConversionLossPolicies(t *testing.T) {
	for _, tc := range []struct {
		policy translate.LossPolicy
		body   string
		reject bool
	}{{translate.LossPolicyAllow, `{"messages":[],"seed":42}`, false}, {translate.LossPolicySafe, `{"messages":[],"seed":42}`, true}, {translate.LossPolicySafe, `{"messages":[],"metadata":{"label":"private"}}`, false}, {translate.LossPolicyStrict, `{"messages":[],"metadata":{"label":"private"}}`, true}} {
		state := translate.NewRequestState()
		state.Options.LossPolicy = tc.policy
		_, err := translate.ConvertRequest(translate.FormatResponses, "m", translate.FormatChatCompletions, []byte(tc.body), state)
		if (err != nil) != tc.reject || len(state.Diagnostics()) == 0 {
			t.Fatalf("policy %s: %v %+v", tc.policy, err, state.Diagnostics())
		}
		for _, d := range state.Diagnostics() {
			if strings.Contains(d.Message, "private") {
				t.Fatal("diagnostic exposed request value")
			}
		}
	}
}

func TestConversionCitationsAllDirections(t *testing.T) {
	sources := map[string]string{
		translate.FormatChatCompletions: `{"id":"r","choices":[{"message":{"role":"assistant","content":"你好来源","annotations":[{"type":"url_citation","url_citation":{"url":"https://example.com","title":"来源","start_index":2,"end_index":4}}]},"finish_reason":"stop"}]}`,
		translate.FormatResponses:       `{"id":"r","status":"completed","output":[{"type":"message","role":"assistant","content":[{"type":"output_text","text":"你好来源","annotations":[{"type":"url_citation","url":"https://example.com","title":"来源","start_index":2,"end_index":4}]}]}]}`,
		translate.FormatMessages:        `{"id":"r","content":[{"type":"text","text":"你好来源","citations":[{"type":"web_search_result_location","url":"https://example.com","title":"来源","cited_text":"来源"}]}],"stop_reason":"end_turn"}`,
	}
	for from, body := range sources {
		for _, to := range formats {
			if from == to {
				continue
			}
			t.Run(from+"/"+to, func(t *testing.T) {
				out, err := translate.ConvertResponse(from, to, 200, []byte(body), translate.NewRequestState())
				if err != nil {
					t.Fatal(err)
				}
				if !strings.Contains(string(out), "https://example.com") || !strings.Contains(string(out), "来源") {
					t.Fatalf("citation lost: %s", out)
				}
				if to != translate.FormatMessages && !strings.Contains(string(out), `"start_index":2`) {
					t.Fatalf("Unicode offset lost: %s", out)
				}
			})
		}
	}
}
func TestConversionStreamCitationsAllDirections(t *testing.T) {
	chat := "data: {\"id\":\"r\",\"choices\":[{\"delta\":{\"content\":\"来源\",\"annotations\":[{\"type\":\"url_citation\",\"url_citation\":{\"url\":\"https://example.com\",\"title\":\"来源\",\"start_index\":0,\"end_index\":2}}]}}]}\n\ndata: {\"choices\":[{\"delta\":{},\"finish_reason\":\"stop\"}]}\n\ndata: [DONE]\n\n"
	responses := "event: response.created\ndata: {\"response\":{\"id\":\"r\"}}\n\nevent: response.output_text.delta\ndata: {\"item_id\":\"m1\",\"content_index\":0,\"delta\":\"来源\"}\n\nevent: response.output_text.annotation.added\ndata: {\"item_id\":\"m1\",\"content_index\":0,\"annotation_index\":0,\"annotation\":{\"type\":\"url_citation\",\"url\":\"https://example.com\",\"title\":\"来源\",\"start_index\":0,\"end_index\":2}}\n\nevent: response.completed\ndata: {\"type\":\"response.completed\",\"response\":{\"id\":\"r\",\"object\":\"response\",\"status\":\"completed\",\"output\":[]}}\n\n"
	messages := "event: message_start\ndata: {\"type\":\"message_start\",\"message\":{\"id\":\"r\"}}\n\nevent: content_block_start\ndata: {\"index\":0,\"content_block\":{\"type\":\"text\",\"text\":\"\"}}\n\nevent: content_block_delta\ndata: {\"index\":0,\"delta\":{\"type\":\"text_delta\",\"text\":\"来源\"}}\n\nevent: content_block_delta\ndata: {\"index\":0,\"delta\":{\"type\":\"citations_delta\",\"citation\":{\"type\":\"web_search_result_location\",\"url\":\"https://example.com\",\"title\":\"来源\",\"cited_text\":\"来源\"}}}\n\nevent: content_block_stop\ndata: {\"index\":0}\n\nevent: message_delta\ndata: {\"delta\":{\"stop_reason\":\"end_turn\"}}\n\nevent: message_stop\ndata: {\"type\":\"message_stop\"}\n\n"
	for from, body := range map[string]string{translate.FormatChatCompletions: chat, translate.FormatResponses: responses, translate.FormatMessages: messages} {
		for _, to := range formats {
			if from == to {
				continue
			}
			t.Run(from+"/"+to, func(t *testing.T) {
				events := streamFrames(t, from, to, body, translate.NewRequestState())
				raw, _ := json.Marshal(events)
				if !strings.Contains(string(raw), "https://example.com") {
					t.Fatalf("stream citation lost: %s", raw)
				}
				if to == translate.FormatMessages {
					open := -1
					for _, e := range events {
						idx, _ := e.Data["index"].(float64)
						switch e.Name {
						case "content_block_start":
							open = int(idx)
						case "content_block_delta":
							if int(idx) != open {
								t.Fatal("citation sent to closed block")
							}
						case "content_block_stop":
							open = -1
						}
					}
				}
			})
		}
	}
}
func TestConversionSearchToolMapping(t *testing.T) {
	for _, tc := range []struct{ from, to, body, want string }{
		{translate.FormatResponses, translate.FormatMessages, `{"input":"hi","tools":[{"type":"web_search","filters":{"allowed_domains":["example.com"]},"user_location":{"type":"approximate","country":"CN"}}]}`, "web_search_20250305"},
		{translate.FormatMessages, translate.FormatResponses, `{"messages":[{"role":"user","content":"hi"}],"max_tokens":100,"tools":[{"type":"web_search_20250305","name":"web_search","allowed_domains":["example.com"],"user_location":{"country":"CN"}}]}`, "web_search"},
	} {
		state := translate.NewRequestState()
		out, err := translate.ConvertRequest(tc.to, "m", tc.from, []byte(tc.body), state)
		if err != nil {
			t.Fatal(err)
		}
		tools := object(t, out)["tools"].([]any)
		if len(tools) != 1 || tools[0].(map[string]any)["type"] != tc.want || !strings.Contains(string(out), "example.com") {
			t.Fatalf("search declaration lost: %s", out)
		}
	}
	state := translate.NewRequestState()
	state.Options.LossPolicy = translate.LossPolicySafe
	_, err := translate.ConvertRequest(translate.FormatChatCompletions, "m", translate.FormatResponses, []byte(`{"input":"hi","tools":[{"type":"web_search"}]}`), state)
	if err == nil || len(state.Diagnostics()) == 0 {
		t.Fatal("unrepresentable hosted execution not diagnosed")
	}
}
func TestConversionDirectCustomToolResponses(t *testing.T) {
	body := `{"id":"r","object":"response","status":"completed","output":[{"type":"custom_tool_call","id":"item1","call_id":"c1","name":"exec","input":"echo hello"}]}`
	for _, to := range []string{translate.FormatChatCompletions, translate.FormatMessages} {
		out, err := translate.ConvertResponse(translate.FormatResponses, to, 200, []byte(body), nil)
		if err != nil {
			t.Fatal(err)
		}
		if !strings.Contains(string(out), "echo hello") || !strings.Contains(string(out), "exec") {
			t.Fatalf("custom input lost: %s", out)
		}
		stream := "event: response.created\ndata: {\"response\":{\"id\":\"r\"}}\n\nevent: response.output_item.added\ndata: {\"item\":{\"type\":\"custom_tool_call\",\"id\":\"item1\",\"call_id\":\"c1\",\"name\":\"exec\"}}\n\nevent: response.custom_tool_call_input.delta\ndata: {\"item_id\":\"item1\",\"delta\":\"echo \"}\n\nevent: response.custom_tool_call_input.done\ndata: {\"item_id\":\"item1\",\"input\":\"echo hello\"}\n\nevent: response.completed\ndata: {\"type\":\"response.completed\",\"response\": " + body + "}\n\n"
		events := streamFrames(t, translate.FormatResponses, to, stream, nil)
		raw, _ := json.Marshal(events)
		if !strings.Contains(string(raw), "echo hello") {
			t.Fatalf("custom stream lost: %s", raw)
		}
	}
}
func TestConversionNativeCustomToolRestoration(t *testing.T) {
	state := translate.NewRequestState()
	_, err := translate.ConvertRequest(translate.FormatChatCompletions, "m", translate.FormatResponses, []byte(`{"input":"hi","tools":[{"type":"namespace","name":"shell","tools":[{"type":"custom","name":"exec"}]}]}`), state)
	if err != nil {
		t.Fatal(err)
	}
	item := `{"type":"function_call","id":"i1","call_id":"c1","name":"shell__exec","arguments":"{\"input\":\"pwd\"}"}`
	response := `{"id":"r","object":"response","status":"completed","future_number":9007199254740993,"output":[` + item + `]}`
	out, err := translate.ConvertResponse(translate.FormatResponses, translate.FormatResponses, 200, []byte(response), state)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(out), `"namespace":"shell"`) || !strings.Contains(string(out), "9007199254740993") {
		t.Fatalf("identity/numeric extension lost: %s", out)
	}
	stream := "event: response.output_item.added\ndata: {\"item\":" + item + "}\n\nevent: response.function_call_arguments.delta\ndata: {\"item_id\":\"i1\",\"delta\":\"{\\\"input\\\":\\\"pwd\\\"}\"}\n\nevent: response.function_call_arguments.done\ndata: {\"item_id\":\"i1\",\"arguments\":\"{\\\"input\\\":\\\"pwd\\\"}\",\"sequence_number\":7}\n\nevent: response.completed\ndata: {\"type\":\"response.completed\",\"response\":" + response + "}\n\n"
	events := streamFrames(t, translate.FormatResponses, translate.FormatResponses, stream, state)
	var sawDelta, sawDone, sawTerminal bool
	for _, e := range events {
		switch e.Name {
		case "response.custom_tool_call_input.delta":
			sawDelta = e.Data["delta"] == "pwd"
		case "response.custom_tool_call_input.done":
			sawDone = e.Data["input"] == "pwd" && e.Data["sequence_number"] == float64(7)
		case "response.completed":
			items := e.Data["response"].(map[string]any)["output"].([]any)
			sawTerminal = items[0].(map[string]any)["type"] == "custom_tool_call"
		case "response.function_call_arguments.delta":
			t.Fatal("raw wrapper leaked into custom stream")
		}
	}
	if !sawDelta || !sawDone || !sawTerminal {
		t.Fatalf("incomplete custom lifecycle: %+v", events)
	}
}

func TestConversionSearchResponseAndStreamArtifacts(t *testing.T) {
	message := `{"id":"r","content":[{"type":"server_tool_use","id":"search1","name":"web_search","input":{"query":"Go protocol"}},{"type":"web_search_tool_result","tool_use_id":"search1","content":[{"type":"web_search_result","url":"https://example.com","title":"Source"}]},{"type":"text","text":"answer"}],"stop_reason":"end_turn"}`
	response := `{"id":"r","object":"response","status":"completed","output":[{"type":"web_search_call","id":"search1","status":"completed","action":{"type":"search","query":"Go protocol","sources":[{"type":"url","url":"https://example.com"}]}},{"type":"message","role":"assistant","content":[{"type":"output_text","text":"answer"}]}]}`
	for _, tc := range []struct{ from, to, body, kind string }{{translate.FormatMessages, translate.FormatResponses, message, "web_search_call"}, {translate.FormatResponses, translate.FormatMessages, response, "server_tool_use"}} {
		out, err := translate.ConvertResponse(tc.from, tc.to, 200, []byte(tc.body), translate.NewRequestState())
		if err != nil {
			t.Fatal(err)
		}
		if !strings.Contains(string(out), tc.kind) || !strings.Contains(string(out), "Go protocol") || !strings.Contains(string(out), "https://example.com") {
			t.Fatalf("search artifact lost: %s", out)
		}
	}
	messagesStream := "event: message_start\ndata: {\"type\":\"message_start\",\"message\":{\"id\":\"r\"}}\n\nevent: content_block_start\ndata: {\"index\":0,\"content_block\":{\"type\":\"server_tool_use\",\"id\":\"search1\",\"name\":\"web_search\",\"input\":{}}}\n\nevent: content_block_delta\ndata: {\"index\":0,\"delta\":{\"type\":\"input_json_delta\",\"partial_json\":\"{\\\"query\\\":\\\"Go protocol\\\"}\"}}\n\nevent: content_block_stop\ndata: {\"index\":0}\n\nevent: content_block_start\ndata: {\"index\":1,\"content_block\":{\"type\":\"web_search_tool_result\",\"tool_use_id\":\"search1\",\"content\":[{\"type\":\"web_search_result\",\"url\":\"https://example.com\",\"title\":\"Source\"}]}}\n\nevent: content_block_stop\ndata: {\"index\":1}\n\nevent: message_delta\ndata: {\"delta\":{\"stop_reason\":\"end_turn\"}}\n\nevent: message_stop\ndata: {\"type\":\"message_stop\"}\n\n"
	responsesStream := "event: response.created\ndata: {\"response\":{\"id\":\"r\"}}\n\nevent: response.completed\ndata: {\"type\":\"response.completed\",\"response\":" + response + "}\n\n"
	for _, tc := range []struct{ from, to, body, kind string }{{translate.FormatMessages, translate.FormatResponses, messagesStream, "web_search_call"}, {translate.FormatResponses, translate.FormatMessages, responsesStream, "server_tool_use"}} {
		events := streamFrames(t, tc.from, tc.to, tc.body, translate.NewRequestState())
		raw, _ := json.Marshal(events)
		if !strings.Contains(string(raw), tc.kind) || !strings.Contains(string(raw), "Go protocol") || !strings.Contains(string(raw), "https://example.com") {
			t.Fatalf("stream search artifact lost: %s", raw)
		}
	}
}

func TestConversionUnsupportedMessagePartsFail(t *testing.T) {
	body := `{"id":"r","object":"response","status":"completed","output":[{"type":"message","content":[{"type":"output_audio","audio":"actual payload"}]}]}`
	for _, to := range []string{translate.FormatChatCompletions, translate.FormatMessages} {
		if _, err := translate.ConvertResponse(translate.FormatResponses, to, 200, []byte(body), nil); err == nil {
			t.Fatal("unknown content silently discarded")
		}
		sc, _ := translate.NewStreamConverter(translate.FormatResponses, to, nil)
		frame := "event: response.completed\ndata: {\"type\":\"response.completed\",\"response\":" + body + "}\n\n"
		events, done, err := sc.Feed([]byte(frame))
		if err == nil || done || len(events) > 0 {
			t.Fatal("unknown streaming content silently discarded")
		}
	}
}
func TestConversionSearchPolicyAndForcedChoice(t *testing.T) {
	source := `{"messages":[{"role":"user","content":"hi"}],"tools":[{"type":"web_search_20250305","name":"web_search"}],"tool_choice":{"type":"tool","name":"web_search"}}`
	out, err := translate.ConvertRequest(translate.FormatResponses, "m", translate.FormatMessages, []byte(source), translate.NewRequestState())
	if err != nil {
		t.Fatal(err)
	}
	if object(t, out)["tool_choice"].(map[string]any)["type"] != "web_search" {
		t.Fatalf("invalid forced hosted choice: %s", out)
	}
	if _, err := translate.ConvertRequest(translate.FormatChatCompletions, "m", translate.FormatMessages, []byte(source), translate.NewRequestState()); err == nil {
		t.Fatal("unrepresentable forced search accepted")
	}
	state := translate.NewRequestState()
	state.Options.LossPolicy = translate.LossPolicySafe
	_, err = translate.ConvertRequest(translate.FormatResponses, "m", translate.FormatMessages, []byte(`{"messages":[],"tools":[{"type":"web_search_20250305","name":"web_search","blocked_domains":["example.com"]}]}`), state)
	if err == nil {
		t.Fatal("search restriction silently weakened under safe policy")
	}
}
func TestConversionCitationSnapshotOffsetsAndDeduplication(t *testing.T) {
	body := "event: response.created\ndata: {\"response\":{\"id\":\"r\"}}\n\nevent: response.output_text.delta\ndata: {\"item_id\":\"m1\",\"delta\":\"前文\"}\n\nevent: response.output_text.delta\ndata: {\"item_id\":\"m2\",\"delta\":\"来源\"}\n\nevent: response.output_text.annotation.added\ndata: {\"item_id\":\"m2\",\"annotation\":{\"type\":\"url_citation\",\"url\":\"https://example.com\",\"start_index\":0,\"end_index\":2}}\n\nevent: response.completed\ndata: {\"type\":\"response.completed\",\"response\":{\"id\":\"r\",\"object\":\"response\",\"status\":\"completed\",\"output\":[{\"type\":\"message\",\"id\":\"m1\",\"content\":[{\"type\":\"output_text\",\"text\":\"前文\"}]},{\"type\":\"message\",\"id\":\"m2\",\"content\":[{\"type\":\"output_text\",\"text\":\"来源\",\"annotations\":[{\"type\":\"url_citation\",\"url\":\"https://example.com\",\"start_index\":0,\"end_index\":2}]}]}]}}\n\n"
	events := streamFrames(t, translate.FormatResponses, translate.FormatChatCompletions, body, nil)
	count := 0
	for _, e := range events {
		choices, ok := e.Data["choices"].([]any)
		if !ok || len(choices) == 0 {
			continue
		}
		delta, _ := choices[0].(map[string]any)["delta"].(map[string]any)
		annotations, _ := delta["annotations"].([]any)
		for _, annotation := range annotations {
			count++
			cite := annotation.(map[string]any)["url_citation"].(map[string]any)
			if cite["start_index"] != float64(2) || cite["end_index"] != float64(4) {
				t.Fatalf("wrong aggregated offsets: %+v", cite)
			}
		}
	}
	if count != 1 {
		t.Fatalf("snapshot duplicated citation: %d", count)
	}
}

func TestLossPolicyDoesNotInspectToolBusinessData(t *testing.T) {
	source := `{"max_tokens":128,"messages":[{"role":"assistant","content":[{"type":"tool_use","id":"c1","name":"f","input":{"encrypted_content":"business","signature":"value","nested":{"type":"thinking","signature":"business"}}}]},{"role":"user","content":[{"type":"tool_result","tool_use_id":"c1","content":"ok"}]}],"tools":[{"name":"f","input_schema":{"type":"object","properties":{"encrypted_content":{"type":"string"},"signature":{"type":"string"}}}}]}`
	for _, policy := range []translate.LossPolicy{translate.LossPolicySafe, translate.LossPolicyStrict} {
		for _, to := range []string{translate.FormatChatCompletions, translate.FormatResponses} {
			state := translate.NewRequestState()
			state.Options.LossPolicy = policy
			out, err := translate.ConvertRequest(to, "m", translate.FormatMessages, []byte(source), state)
			if err != nil || len(state.Diagnostics()) != 0 || !strings.Contains(string(out), "business") {
				t.Fatalf("business data misclassified as protocol loss: %v %+v %s", err, state.Diagnostics(), out)
			}
		}
	}
}

func TestSearchFailureDoesNotEmitCompletedLifecycle(t *testing.T) {
	body := "event: message_start\ndata: {\"type\":\"message_start\",\"message\":{\"id\":\"r\"}}\n\nevent: content_block_start\ndata: {\"index\":0,\"content_block\":{\"type\":\"server_tool_use\",\"id\":\"search1\",\"name\":\"web_search\",\"input\":{\"query\":\"q\"}}}\n\nevent: content_block_stop\ndata: {\"index\":0}\n\nevent: content_block_start\ndata: {\"index\":1,\"content_block\":{\"type\":\"web_search_tool_result\",\"tool_use_id\":\"search1\",\"content\":{\"type\":\"web_search_tool_result_error\",\"error_code\":\"unavailable\"}}}\n\nevent: content_block_stop\ndata: {\"index\":1}\n\nevent: message_delta\ndata: {\"delta\":{\"stop_reason\":\"end_turn\"}}\n\nevent: message_stop\ndata: {\"type\":\"message_stop\"}\n\n"
	events := streamFrames(t, translate.FormatMessages, translate.FormatResponses, body, translate.NewRequestState())
	sawFailed := false
	for _, event := range events {
		if event.Name == "response.web_search_call.completed" {
			t.Fatal("failed search emitted successful lifecycle")
		}
		if event.Name == "response.output_item.done" {
			item := event.Data["item"].(map[string]any)
			if item["type"] == "web_search_call" {
				sawFailed = item["status"] == "failed"
			}
		}
		if event.Name == "response.completed" {
			items := event.Data["response"].(map[string]any)["output"].([]any)
			if len(items) != 1 || items[0].(map[string]any)["status"] != "failed" {
				t.Fatalf("failed item changed at finalization: %+v", items)
			}
		}
	}
	if !sawFailed {
		t.Fatal("failed search item missing")
	}
}
func TestLateCitationDoesNotMoveToAnotherBlock(t *testing.T) {
	frame := func(name, payload string) string { return "event: " + name + "\ndata: " + payload + "\n\n" }
	body := frame("response.created", `{"response":{"id":"r"}}`) +
		frame("response.output_text.delta", `{"item_id":"a","delta":"first"}`) +
		frame("response.output_item.added", `{"item":{"type":"function_call","id":"tool1","call_id":"c1","name":"f","arguments":"{}"}}`) +
		frame("response.output_item.done", `{"item":{"type":"function_call","id":"tool1","call_id":"c1","name":"f","arguments":"{}"}}`) +
		frame("response.output_text.delta", `{"item_id":"b","delta":"second"}`) +
		frame("response.output_item.done", `{"item":{"type":"message","id":"a","content":[{"type":"output_text","text":"first","annotations":[{"type":"url_citation","url":"https://first.example","start_index":0,"end_index":5}]}]}}`) +
		frame("response.completed", `{"type":"response.completed","response":{"id":"r","object":"response","status":"completed","output":[]}}`)
	state := translate.NewRequestState()
	events := streamFrames(t, translate.FormatResponses, translate.FormatMessages, body, state)
	for _, event := range events {
		if event.Name == "content_block_delta" && event.Data["delta"].(map[string]any)["type"] == "citations_delta" {
			t.Fatal("closed first block's citation attached to a later block")
		}
	}
	found := false
	for _, d := range state.Diagnostics() {
		if strings.Contains(d.Path, "a/0/false") && strings.Contains(d.Path, "closed_block") {
			found = true
		}
	}
	if !found {
		t.Fatalf("unrepresentable late citation not reported: %+v", state.Diagnostics())
	}
}
func TestCitationBeforeQueuedTextKeepsItsOwner(t *testing.T) {
	frame := func(name, payload string) string { return "event: " + name + "\ndata: " + payload + "\n\n" }
	body := frame("response.created", `{"response":{"id":"r"}}`) +
		frame("response.output_item.added", `{"item":{"type":"function_call","id":"tool1","call_id":"c1","name":"f","arguments":""}}`) +
		frame("response.output_text.annotation.added", `{"item_id":"a","annotation":{"type":"url_citation","url":"https://first.example","start_index":0,"end_index":5}}`) +
		frame("response.output_text.delta", `{"item_id":"a","delta":"first"}`) +
		frame("response.output_item.done", `{"item":{"type":"function_call","id":"tool1","call_id":"c1","name":"f","arguments":"{}"}}`) +
		frame("response.completed", `{"type":"response.completed","response":{"id":"r","object":"response","status":"completed","output":[]}}`)
	state := translate.NewRequestState()
	events := streamFrames(t, translate.FormatResponses, translate.FormatMessages, body, state)
	found := false
	for _, event := range events {
		if event.Name == "content_block_delta" {
			delta := event.Data["delta"].(map[string]any)
			if delta["type"] == "citations_delta" {
				found = delta["citation"].(map[string]any)["cited_text"] == "first"
			}
		}
	}
	if !found || len(state.Diagnostics()) != 0 {
		t.Fatalf("pending citation lost or wrongly bound: %v %+v", found, state.Diagnostics())
	}
}
func TestChatToolDeclarationHasNoNestedEnvelope(t *testing.T) {
	for _, from := range []string{translate.FormatMessages, translate.FormatResponses} {
		source := `{"messages":[],"tools":[{"name":"f","strict":true,"input_schema":{"type":"object"}}]}`
		if from == translate.FormatResponses {
			source = `{"input":[],"tools":[{"type":"function","name":"f","strict":true,"parameters":{"type":"object"}}]}`
		}
		out, err := translate.ConvertRequest(translate.FormatChatCompletions, "m", from, []byte(source), translate.NewRequestState())
		if err != nil {
			t.Fatal(err)
		}
		tool := object(t, out)["tools"].([]any)[0].(map[string]any)
		fn := tool["function"].(map[string]any)
		if len(tool) != 2 || fn["function"] != nil || fn["type"] != nil || fn["strict"] != true {
			t.Fatalf("malformed function envelope: %s", out)
		}
	}
}

func TestPendingCitationPrecedesLaterQueuedBlocks(t *testing.T) {
	frame := func(name, payload string) string { return "event: " + name + "\ndata: " + payload + "\n\n" }
	body := frame("response.created", `{"response":{"id":"r"}}`) +
		frame("response.output_item.added", `{"item":{"type":"function_call","id":"t1","call_id":"c1","name":"f","arguments":""}}`) +
		frame("response.output_text.annotation.added", `{"item_id":"a","annotation":{"type":"url_citation","url":"https://first.example","start_index":0,"end_index":5}}`) +
		frame("response.output_text.delta", `{"item_id":"a","delta":"first"}`) +
		frame("response.output_item.added", `{"item":{"type":"function_call","id":"t2","call_id":"c2","name":"g","arguments":""}}`) +
		frame("response.output_text.delta", `{"item_id":"b","delta":"second"}`) +
		frame("response.output_item.done", `{"item":{"type":"function_call","id":"t1","call_id":"c1","name":"f","arguments":"{}"}}`) +
		frame("response.output_item.done", `{"item":{"type":"function_call","id":"t2","call_id":"c2","name":"g","arguments":"{}"}}`) +
		frame("response.completed", `{"type":"response.completed","response":{"id":"r","object":"response","status":"completed","output":[]}}`)
	state := translate.NewRequestState()
	events := streamFrames(t, translate.FormatResponses, translate.FormatMessages, body, state)
	active := -1
	textBlock := -1
	citationCount := 0
	secondToolStarted := false
	for _, event := range events {
		idx, _ := event.Data["index"].(float64)
		switch event.Name {
		case "content_block_start":
			active = int(idx)
			block := event.Data["content_block"].(map[string]any)
			if block["id"] == "c2" {
				secondToolStarted = true
				if citationCount != 1 {
					t.Fatal("second tool opened before first text's citation")
				}
			}
		case "content_block_delta":
			delta := event.Data["delta"].(map[string]any)
			if delta["text"] == "first" {
				textBlock = int(idx)
			}
			if delta["type"] == "citations_delta" {
				citationCount++
				if secondToolStarted || textBlock < 0 || int(idx) != textBlock || active != textBlock || delta["citation"].(map[string]any)["cited_text"] != "first" {
					t.Fatalf("citation delayed or bound to wrong block: %+v", event)
				}
			}
		case "content_block_stop":
			active = -1
		}
	}
	if citationCount != 1 || !secondToolStarted || len(state.Diagnostics()) != 0 {
		t.Fatalf("pending citation lost or diagnosed: %d %+v", citationCount, state.Diagnostics())
	}
}

func TestRefusalDoesNotShiftChatCitationOffsets(t *testing.T) {
	frame := func(name, payload string) string { return "event: " + name + "\ndata: " + payload + "\n\n" }
	body := frame("response.created", `{"response":{"id":"r"}}`) +
		frame("response.output_text.delta", `{"item_id":"a","delta":"前"}`) +
		frame("response.refusal.delta", `{"item_id":"refusal","delta":"NO"}`) +
		frame("response.output_text.delta", `{"item_id":"a","delta":"文"}`) +
		frame("response.output_text.delta", `{"item_id":"b","delta":"abc"}`) +
		frame("response.output_text.annotation.added", `{"item_id":"b","annotation":{"type":"url_citation","url":"https://source.example","start_index":0,"end_index":3}}`) +
		frame("response.completed", `{"type":"response.completed","response":{"id":"r","object":"response","status":"completed","output":[]}}`)
	for _, target := range []string{translate.FormatChatCompletions, translate.FormatMessages} {
		state := translate.NewRequestState()
		events := streamFrames(t, translate.FormatResponses, target, body, state)
		var content, refusal strings.Builder
		citations := 0
		for _, event := range events {
			if target == translate.FormatChatCompletions {
				choices, _ := event.Data["choices"].([]any)
				if len(choices) == 0 {
					continue
				}
				delta := choices[0].(map[string]any)["delta"].(map[string]any)
				if text, ok := delta["content"].(string); ok {
					content.WriteString(text)
				}
				if text, ok := delta["refusal"].(string); ok {
					refusal.WriteString(text)
				}
				annotations, _ := delta["annotations"].([]any)
				for _, raw := range annotations {
					citations++
					c := raw.(map[string]any)["url_citation"].(map[string]any)
					if c["start_index"] != float64(2) || c["end_index"] != float64(5) {
						t.Fatalf("refusal shifted content offset: %+v", c)
					}
				}
			} else if event.Name == "content_block_delta" {
				delta := event.Data["delta"].(map[string]any)
				if text, ok := delta["text"].(string); ok {
					content.WriteString(text)
				}
				if delta["type"] == "citations_delta" {
					citations++
					if delta["citation"].(map[string]any)["cited_text"] != "abc" {
						t.Fatal("Messages citation text changed")
					}
				}
			}
		}
		want := "前文abc"
		if target == translate.FormatMessages {
			want = "前NO文abc"
		} else if refusal.String() != "NO" {
			t.Fatal("refusal not preserved separately")
		}
		if content.String() != want || citations != 1 || len(state.Diagnostics()) != 0 {
			t.Fatalf("unexpected content/citation: %s %d %+v", content.String(), citations, state.Diagnostics())
		}
	}
}

func TestMissingSearchResultAgreesAcrossResponseModes(t *testing.T) {
	frame := func(name, payload string) string { return "event: " + name + "\ndata: " + payload + "\n\n" }
	for _, stop := range []string{"max_tokens", "end_turn"} {
		body := `{"id":"r","model":"m","content":[{"type":"server_tool_use","id":"search1","name":"web_search","input":{"query":"q"}}],"stop_reason":"` + stop + `"}`
		ordinaryState := translate.NewRequestState()
		out, err := translate.ConvertResponse(translate.FormatMessages, translate.FormatResponses, 200, []byte(body), ordinaryState)
		if err != nil {
			t.Fatal(err)
		}
		ordinary := object(t, out)
		items := ordinary["output"].([]any)
		if len(items) != 1 || items[0].(map[string]any)["status"] != "failed" {
			t.Fatalf("missing result invented successful search: %s", out)
		}
		stream := frame("message_start", `{"type":"message_start","message":{"id":"r","model":"m"}}`) +
			frame("content_block_start", `{"index":0,"content_block":{"type":"server_tool_use","id":"search1","name":"web_search","input":{"query":"q"}}}`) +
			frame("content_block_stop", `{"index":0}`) +
			frame("message_delta", `{"delta":{"stop_reason":"`+stop+`"}}`) +
			frame("message_stop", `{"type":"message_stop"}`)
		streamState := translate.NewRequestState()
		events := streamFrames(t, translate.FormatMessages, translate.FormatResponses, stream, streamState)
		var terminal map[string]any
		for _, event := range events {
			if event.Name == "response.web_search_call.completed" {
				t.Fatal("missing result generated success event")
			}
			if event.Name == "response.completed" {
				terminal = event.Data["response"].(map[string]any)
			}
		}
		if terminal == nil || terminal["status"] != ordinary["status"] {
			t.Fatalf("response completion differs: %+v %s", terminal, out)
		}
		streamed, _ := json.Marshal(terminal["output"])
		expected, _ := json.Marshal(items)
		if string(streamed) != string(expected) {
			t.Fatalf("search status differs across modes: %s %s", expected, streamed)
		}
		for _, state := range []*translate.RequestState{ordinaryState, streamState} {
			ds := state.Diagnostics()
			if len(ds) != 1 || ds[0].Path != "content.web_search_tool_result" || !ds[0].Semantic {
				t.Fatalf("missing-result diagnostic differs: %+v", ds)
			}
		}
	}
}

func TestPendingCitationsWaitForTheirTextFragments(t *testing.T) {
	frame := func(name, payload string) string { return "event: " + name + "\ndata: " + payload + "\n\n" }
	for _, blocked := range []bool{false, true} {
		t.Run(fmt.Sprintf("blocked=%t", blocked), func(t *testing.T) {
			body := frame("response.created", `{"response":{"id":"r"}}`)
			if blocked {
				body += frame("response.output_item.added", `{"item":{"type":"function_call","id":"t1","call_id":"c1","name":"f","arguments":""}}`)
			}
			body += frame("response.output_text.annotation.added", `{"item_id":"a","annotation":{"type":"url_citation","url":"https://full.example","start_index":0,"end_index":5}}`) +
				frame("response.output_text.annotation.added", `{"item_id":"a","annotation":{"type":"url_citation","url":"https://prefix.example","start_index":0,"end_index":2}}`) +
				frame("response.output_text.delta", `{"item_id":"a","delta":"fi"}`) +
				frame("response.output_text.delta", `{"item_id":"a","delta":"rst"}`)
			if blocked {
				body += frame("response.output_item.added", `{"item":{"type":"function_call","id":"t2","call_id":"c2","name":"g","arguments":"{}"}}`) + frame("response.output_text.delta", `{"item_id":"b","delta":"second"}`) + frame("response.output_item.done", `{"item":{"type":"function_call","id":"t1","call_id":"c1","name":"f","arguments":"{}"}}`) + frame("response.output_item.done", `{"item":{"type":"function_call","id":"t2","call_id":"c2","name":"g","arguments":"{}"}}`)
			}
			// A terminal snapshot repeats both citations; successful delivery is once.
			body += frame("response.completed", `{"type":"response.completed","response":{"id":"r","object":"response","status":"completed","output":[{"type":"message","id":"a","content":[{"type":"output_text","text":"first","annotations":[{"type":"url_citation","url":"https://full.example","start_index":0,"end_index":5},{"type":"url_citation","url":"https://prefix.example","start_index":0,"end_index":2}]}]}]}}`)
			state := translate.NewRequestState()
			events := streamFrames(t, translate.FormatResponses, translate.FormatMessages, body, state)
			active := -1
			owner := -1
			text := ""
			seen := map[string]int{}
			later := false
			for _, event := range events {
				idx, _ := event.Data["index"].(float64)
				switch event.Name {
				case "content_block_start":
					active = int(idx)
					block := event.Data["content_block"].(map[string]any)
					if block["id"] == "c2" {
						later = true
					}
				case "content_block_stop":
					active = -1
				case "content_block_delta":
					delta := event.Data["delta"].(map[string]any)
					if fragment, ok := delta["text"].(string); ok && !later {
						if owner < 0 {
							owner = int(idx)
						}
						text += fragment
					}
					if delta["type"] == "citations_delta" {
						citation := delta["citation"].(map[string]any)
						url := citation["url"].(string)
						seen[url]++
						if active != owner || int(idx) != owner || later {
							t.Fatal("citation moved outside its block")
						}
						want := "fi"
						if url == "https://full.example" {
							want = "first"
						}
						if citation["cited_text"] != want || !strings.Contains(text, want) {
							t.Fatalf("citation delivered before quoted text: %+v text=%q", citation, text)
						}
					}
				}
			}
			if seen["https://full.example"] != 1 || seen["https://prefix.example"] != 1 || len(state.Diagnostics()) != 0 {
				t.Fatalf("fragment citations lost/duplicated: %+v diagnostics=%+v", seen, state.Diagnostics())
			}
		})
	}
}
