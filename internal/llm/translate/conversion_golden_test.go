package translate_test

import (
	"bytes"
	"encoding/json"
	"flag"
	"github.com/dillonzq/apix/internal/llm/translate"
	"os"
	"path/filepath"
	"testing"
)

var updateGolden = flag.Bool("update", false, "update reviewed protocol conversion snapshots")

func assertGolden(t *testing.T, name string, value any) {
	t.Helper()
	got, err := json.MarshalIndent(value, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	got = append(got, '\n')
	path := filepath.Join("testdata", "conversion", name+".json")
	if *updateGolden {
		if err := os.MkdirAll(filepath.Dir(path), 0755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, got, 0644); err != nil {
			t.Fatal(err)
		}
		return
	}
	want, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(want, got) {
		t.Fatalf("protocol snapshot changed: %s; review before updating", path)
	}
}
func TestProtocolGolden(t *testing.T) {
	fixtures := map[string]struct{ request, response, stream string }{
		translate.FormatChatCompletions: {
			`{"model":"m","messages":[{"role":"user","content":"hello"}],"tools":[{"type":"function","function":{"name":"f","strict":true,"parameters":{"type":"object"}}}],"response_format":{"type":"json_schema","json_schema":{"name":"answer","strict":true,"schema":{"type":"object"}}}}`,
			`{"id":"r","model":"m","choices":[{"message":{"role":"assistant","content":"hello","reasoning_content":"plan","tool_calls":[{"id":"c1","type":"function","function":{"name":"f","arguments":"{}"}}]},"finish_reason":"tool_calls"}],"usage":{"prompt_tokens":4,"completion_tokens":2}}`,
			"data: {\"id\":\"r\",\"model\":\"m\",\"choices\":[{\"delta\":{\"content\":\"hello\"}}]}\n\ndata: {\"choices\":[{\"delta\":{},\"finish_reason\":\"stop\"}]}\n\ndata: [DONE]\n\n",
		},
		translate.FormatResponses: {
			`{"model":"m","input":"hello","tools":[{"type":"function","name":"f","strict":true,"parameters":{"type":"object"}}],"text":{"format":{"type":"json_schema","name":"answer","strict":true,"schema":{"type":"object"}}}}`,
			`{"id":"r","object":"response","model":"m","status":"completed","output":[{"type":"reasoning","summary":[{"type":"summary_text","text":"plan"}]},{"type":"message","role":"assistant","content":[{"type":"output_text","text":"hello"}]},{"type":"function_call","call_id":"c1","name":"f","arguments":"{}"}],"usage":{"input_tokens":4,"output_tokens":2}}`,
			"event: response.created\ndata: {\"response\":{\"id\":\"r\",\"model\":\"m\"}}\n\nevent: response.output_text.delta\ndata: {\"item_id\":\"m1\",\"content_index\":0,\"delta\":\"hello\"}\n\nevent: response.completed\ndata: {\"type\":\"response.completed\",\"response\":{\"id\":\"r\",\"model\":\"m\",\"object\":\"response\",\"status\":\"completed\",\"output\":[]}}\n\n",
		},
		translate.FormatMessages: {
			`{"model":"m","messages":[{"role":"user","content":"hello"}],"max_tokens":100,"tools":[{"name":"f","strict":true,"input_schema":{"type":"object"}}],"output_config":{"format":{"type":"json_schema","schema":{"type":"object"}}}}`,
			`{"id":"r","model":"m","content":[{"type":"thinking","thinking":"plan"},{"type":"text","text":"hello"},{"type":"tool_use","id":"c1","name":"f","input":{}}],"stop_reason":"tool_use","usage":{"input_tokens":4,"output_tokens":2}}`,
			"event: message_start\ndata: {\"type\":\"message_start\",\"message\":{\"id\":\"r\",\"model\":\"m\"}}\n\nevent: content_block_start\ndata: {\"index\":0,\"content_block\":{\"type\":\"text\",\"text\":\"\"}}\n\nevent: content_block_delta\ndata: {\"index\":0,\"delta\":{\"type\":\"text_delta\",\"text\":\"hello\"}}\n\nevent: content_block_stop\ndata: {\"index\":0}\n\nevent: message_delta\ndata: {\"delta\":{\"stop_reason\":\"end_turn\"}}\n\nevent: message_stop\ndata: {\"type\":\"message_stop\"}\n\n",
		},
	}
	for _, from := range formats {
		for _, to := range formats {
			t.Run(from+"/"+to, func(t *testing.T) {
				fixture := fixtures[from]
				state := translate.NewRequestState()
				request, err := translate.ConvertRequest(to, "m", from, []byte(fixture.request), state)
				if err != nil {
					t.Fatal(err)
				}
				assertGolden(t, "request/"+from+"_to_"+to, object(t, request))
				response, err := translate.ConvertResponse(from, to, 200, []byte(fixture.response), state)
				if err != nil {
					t.Fatal(err)
				}
				doc := object(t, response)
				normalizeEventTimes([]wireEvent{{Data: doc}})
				assertGolden(t, "response/"+from+"_to_"+to, doc)
				stream := streamFrames(t, from, to, fixture.stream, nil)
				normalizeEventTimes(stream)
				assertGolden(t, "stream/"+from+"_to_"+to, stream)
			})
		}
	}
}
