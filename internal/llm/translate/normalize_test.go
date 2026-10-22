package translate

import (
	"encoding/json"
	"strings"
	"testing"
)

func TestNormalizeResponsesRoundTrip(t *testing.T) {
	state := NewRequestState()
	request := []byte(`{"model":"m","metadata":{"n":9007199254740993},"input":[{"type":"additional_tools","tools":[{"type":"function","name":"extra"}]},{"type":"custom_tool_call","name":"run","namespace":"shell","call_id":"c","input":"echo hi","extension":true},{"type":"custom_tool_call_output","call_id":"c","output":[{"type":"input_image","image_url":"https://example.com/a.png"}]}],"tools":[{"type":"namespace","name":"shell","tools":[{"type":"custom","name":"run","description":"Run"}]},{"type":"web_search","filters":{"allowed_domains":["example.com"]}}],"tool_choice":{"type":"custom","name":"run","namespace":"shell"}}`)
	body, err := NormalizeResponsesRequest(request, state)
	if err != nil {
		t.Fatal(err)
	}
	for _, value := range []string{`9007199254740993`, `"name":"shell__run"`, `"type":"function_call_output"`, `"input_image"`, `"extension":true`, `"allowed_domains"`, `"name":"extra"`} {
		if !strings.Contains(string(body), value) {
			t.Fatalf("missing %s in %s", value, body)
		}
	}
	var doc map[string]json.RawMessage
	json.Unmarshal(body, &doc)
	var input []map[string]json.RawMessage
	json.Unmarshal(doc["input"], &input)
	var args string
	json.Unmarshal(input[0]["arguments"], &args)
	if args != `{"input":"echo hi"}` {
		t.Fatalf("args = %s", args)
	}
	response := []byte(`{"id":"r","object":"response","status":"completed","output":[{"type":"function_call","name":"shell__run","call_id":"c","arguments":"{\"input\":\"pwd\"}"}]}`)
	restored, err := ConvertResponse(FormatResponses, FormatResponses, 200, response, state)
	if err != nil {
		t.Fatal(err)
	}
	for _, value := range []string{`"type":"custom_tool_call"`, `"name":"run"`, `"namespace":"shell"`, `"input":"pwd"`} {
		if !strings.Contains(string(restored), value) {
			t.Fatalf("missing %s in %s", value, restored)
		}
	}
	stream, err := NewStreamConverter(FormatResponses, FormatResponses, state)
	if err != nil {
		t.Fatal(err)
	}
	frames, done, err := stream.Feed(append(append([]byte("event: response.completed\ndata: {\"type\":\"response.completed\",\"response\":"), response...), []byte("}\n\n")...))
	if err != nil || !done {
		t.Fatalf("done=%v err=%v", done, err)
	}
	if !strings.Contains(string(joinFrames(frames)), `"custom_tool_call"`) {
		t.Fatalf("frames = %s", joinFrames(frames))
	}
}

func joinFrames(frames [][]byte) []byte {
	var all []byte
	for _, frame := range frames {
		all = append(all, frame...)
	}
	return all
}

func TestNormalizeResponsesFailuresAndPolicy(t *testing.T) {
	if _, err := NormalizeResponsesRequest([]byte(`{}`), nil); err == nil {
		t.Fatal("nil state accepted")
	}
	for _, body := range []string{`null`, `[]`, `{"input":42}`, `{"tools":[null]}`, `{"input":[{"type":"additional_tools"}]}`, `{"tools":[{"type":"namespace","name":"a","tools":[{"type":"function","name":"b"}]},{"type":"function","name":"a__b"}]}`} {
		if _, err := NormalizeResponsesRequest([]byte(body), NewRequestState()); err == nil {
			t.Fatalf("accepted %s", body)
		}
	}
	for _, policy := range []LossPolicy{LossPolicyAllow, LossPolicySafe, LossPolicyStrict} {
		state := NewRequestState()
		state.Options.LossPolicy = policy
		_, err := NormalizeResponsesRequest([]byte(`{"tools":[{"type":"custom","name":"run","format":{"type":"grammar"}}]}`), state)
		if (err != nil) != (policy != LossPolicyAllow) || len(state.Diagnostics()) != 1 {
			t.Fatalf("policy=%s err=%v diagnostics=%v", policy, err, state.Diagnostics())
		}
	}
	state := NewRequestState()
	_, err := NormalizeResponsesRequest([]byte(`{"tools":[{"type":"custom","name":"run"}],"input":[{"type":"custom_tool_call","name":"run","input":3}]}`), state)
	if err == nil {
		t.Fatal("invalid custom input accepted")
	}
	response := []byte(`{"output":[{"type":"function_call","name":"run","arguments":"{}"}]}`)
	out, err := ConvertResponse(FormatResponses, FormatResponses, 200, response, state)
	if err != nil || string(out) != string(response) {
		t.Fatalf("failed request leaked state: %s %v", out, err)
	}
}

func TestNormalizeAllowedToolsAndNativePassthrough(t *testing.T) {
	body := []byte(`{"model":"m","input":"hi","tools":[{"type":"namespace","name":"ns","tools":[{"type":"function","name":"f","parameters":{"const":9007199254740993}}]}],"tool_choice":{"type":"allowed_tools","mode":"auto","tools":[{"type":"function","namespace":"ns","name":"f"}]}}`)
	state := NewRequestState()
	out, err := NormalizeResponsesRequest(body, state)
	if err != nil || !strings.Contains(string(out), `"mode":"auto"`) || strings.Contains(string(out), `"namespace"`) {
		t.Fatalf("%s %v", out, err)
	}
	out, err = ConvertRequest(FormatResponses, "m", FormatResponses, body, NewRequestState())
	if err != nil || string(out) != string(body) {
		t.Fatalf("native behavior changed: %s %v", out, err)
	}
}

func TestNormalizeHistoricalIdentityCollisionBothOrders(t *testing.T) {
	for _, typ := range []string{"function_call", "custom_tool_call"} {
		for _, reverse := range []bool{false, true} {
			namespaced := map[string]any{"type": typ, "namespace": "a", "name": "b", "call_id": "c1", "arguments": "{}", "input": "one"}
			plain := map[string]any{"type": typ, "name": "a__b", "call_id": "c2", "arguments": "{}", "input": "two"}
			items := []any{namespaced, plain}
			if reverse {
				items[0], items[1] = items[1], items[0]
			}
			body, marshalErr := json.Marshal(map[string]any{"input": items})
			if marshalErr != nil {
				t.Fatal(marshalErr)
			}
			state := NewRequestState()
			if _, err := NormalizeResponsesRequest(body, state); err == nil {
				t.Fatalf("collision accepted: type=%s reverse=%v", typ, reverse)
			}
			response := []byte(`{"output":[{"type":"function_call","name":"a__b","arguments":"{}"}]}`)
			out, err := ConvertResponse(FormatResponses, FormatResponses, 200, response, state)
			if err != nil || string(out) != string(response) {
				t.Fatalf("collision leaked state: %s %v", out, err)
			}
		}
	}
}

func TestNormalizeDuplicateDeclarationsFirstWins(t *testing.T) {
	for _, typ := range []string{"function", "custom"} {
		first := map[string]any{"type": "namespace", "name": "ns", "tools": []any{map[string]any{"type": typ, "name": "f", "description": "first"}}}
		second := map[string]any{"type": "namespace", "name": "ns", "tools": []any{map[string]any{"type": typ, "name": "f", "description": "second"}}}
		body, marshalErr := json.Marshal(map[string]any{
			"tools": []any{first},
			"input": []any{map[string]any{"type": "additional_tools", "tools": []any{first, second}}},
		})
		if marshalErr != nil {
			t.Fatal(marshalErr)
		}
		state := NewRequestState()
		out, err := NormalizeResponsesRequest(body, state)
		if err != nil {
			t.Fatal(err)
		}
		var doc struct {
			Tools []struct{ Name, Description string }
			Input []json.RawMessage
		}
		if decodeErr := json.Unmarshal(out, &doc); decodeErr != nil {
			t.Fatal(decodeErr)
		}
		if len(doc.Tools) != 1 || doc.Tools[0].Name != "ns__f" || doc.Tools[0].Description != "first" || len(doc.Input) != 0 {
			t.Fatalf("type=%s normalized=%s", typ, out)
		}
	}
}
