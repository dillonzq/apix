package gemini_test

import (
	"bytes"
	"encoding/json"
	"reflect"
	"slices"
	"strings"
	"testing"

	"github.com/dillonzq/apix"
	"github.com/dillonzq/apix/protocol/anthropic"
	"github.com/dillonzq/apix/protocol/gemini"
	"github.com/dillonzq/apix/protocol/openai"
	"github.com/dillonzq/apix/sse"
)

type fixture struct{ request, response, stream string }

var fixtures = map[apix.Protocol]fixture{
	openai.ChatCompletions: {`{"model":"m","messages":[{"role":"user","content":"hello"}]}`, `{"id":"r","model":"m","choices":[{"index":0,"message":{"role":"assistant","content":"answer"},"finish_reason":"stop"}],"usage":{"prompt_tokens":7,"completion_tokens":3}}`, "data: {\"id\":\"r\",\"model\":\"m\",\"choices\":[{\"index\":0,\"delta\":{\"content\":\"answer\"}}]}\n\ndata: {\"choices\":[{\"index\":0,\"delta\":{},\"finish_reason\":\"stop\"}],\"usage\":{\"prompt_tokens\":7,\"completion_tokens\":3}}\n\ndata: [DONE]\n\n"},
	openai.Responses:       {`{"model":"m","input":"hello"}`, `{"id":"r","object":"response","model":"m","status":"completed","output":[{"type":"message","id":"msg","role":"assistant","content":[{"type":"output_text","text":"answer"}]}],"usage":{"input_tokens":7,"output_tokens":3}}`, "event: response.created\ndata: {\"response\":{\"id\":\"r\",\"model\":\"m\"}}\n\nevent: response.output_text.delta\ndata: {\"item_id\":\"msg\",\"content_index\":0,\"delta\":\"answer\"}\n\nevent: response.completed\ndata: {\"type\":\"response.completed\",\"response\":{\"id\":\"r\",\"object\":\"response\",\"model\":\"m\",\"status\":\"completed\",\"output\":[{\"type\":\"message\",\"id\":\"msg\",\"role\":\"assistant\",\"content\":[{\"type\":\"output_text\",\"text\":\"answer\"}]}],\"usage\":{\"input_tokens\":7,\"output_tokens\":3}}}\n\n"},
	anthropic.Messages:     {`{"model":"m","max_tokens":100,"messages":[{"role":"user","content":"hello"}]}`, `{"id":"r","type":"message","model":"m","role":"assistant","content":[{"type":"text","text":"answer"}],"stop_reason":"end_turn","usage":{"input_tokens":7,"output_tokens":3}}`, "event: message_start\ndata: {\"type\":\"message_start\",\"message\":{\"id\":\"r\",\"model\":\"m\",\"usage\":{\"input_tokens\":7}}}\n\nevent: content_block_start\ndata: {\"index\":0,\"content_block\":{\"type\":\"text\",\"text\":\"\"}}\n\nevent: content_block_delta\ndata: {\"index\":0,\"delta\":{\"type\":\"text_delta\",\"text\":\"answer\"}}\n\nevent: content_block_stop\ndata: {\"index\":0}\n\nevent: message_delta\ndata: {\"delta\":{\"stop_reason\":\"end_turn\"},\"usage\":{\"output_tokens\":3}}\n\nevent: message_stop\ndata: {\"type\":\"message_stop\"}\n\n"},
	gemini.GenerateContent: {`{"contents":[{"role":"user","parts":[{"text":"hello"}]}]}`, `{"responseId":"r","modelVersion":"m","candidates":[{"index":0,"content":{"role":"model","parts":[{"text":"answer"}]},"finishReason":"STOP"}],"usageMetadata":{"promptTokenCount":7,"candidatesTokenCount":3,"totalTokenCount":10}}`, "data: {\"responseId\":\"r\",\"modelVersion\":\"m\",\"candidates\":[{\"index\":0,\"content\":{\"parts\":[{\"text\":\"answer\"}]}}]}\n\ndata: {\"candidates\":[{\"index\":0,\"finishReason\":\"STOP\"}],\"usageMetadata\":{\"promptTokenCount\":7,\"candidatesTokenCount\":3,\"totalTokenCount\":10}}\n\n"},
}

func registry(t *testing.T) *apix.Registry {
	t.Helper()
	r, e := apix.NewRegistry(slices.Concat(openai.Codecs(), anthropic.Codecs(), gemini.Codecs())...)
	if e != nil {
		t.Fatal(e)
	}
	return r
}
func payload(p apix.Protocol, stream bool) apix.Payload {
	v := apix.Payload{Body: []byte(fixtures[p].request)}
	if p == gemini.GenerateContent {
		v.Metadata = apix.RequestMetadata{Model: "m", Stream: &stream}
	}
	return v
}
func decode(t *testing.T, b []byte) map[string]any {
	t.Helper()
	var o map[string]any
	if e := json.Unmarshal(b, &o); e != nil {
		t.Fatal(e)
	}
	return o
}
func frame(v any) string { b, _ := json.Marshal(v); return "data: " + string(b) + "\n\n" }
func drain(t *testing.T, c apix.StreamConverter, wire string, step int) []sse.Event {
	t.Helper()
	var frames [][]byte
	for i := 0; i < len(wire); i += step {
		out, _, err := c.Feed([]byte(wire[i:min(i+step, len(wire))]))
		if err != nil {
			t.Fatal(err)
		}
		frames = append(frames, out...)
	}
	tail, err := c.Finish()
	if err != nil {
		t.Fatal(err)
	}
	frames = append(frames, tail...)
	var d sse.Decoder
	out := d.Feed(bytes.Join(frames, nil))
	out = append(out, d.Finish()...)
	for i, event := range out {
		var doc map[string]json.RawMessage
		if json.Unmarshal(event.Data, &doc) == nil {
			if _, ok := doc["created"]; ok {
				doc["created"] = json.RawMessage("0")
				out[i].Data, _ = json.Marshal(doc)
			}
		}
	}
	return out
}
func TestMatrixAndIndependentOutput(t *testing.T) {
	r := registry(t)
	ps := []apix.Protocol{openai.ChatCompletions, openai.Responses, anthropic.Messages, gemini.GenerateContent}
	for _, client := range ps {
		for _, up := range ps {
			for _, output := range ps {
				t.Run(string(client)+"/"+string(up)+"/"+string(output), func(t *testing.T) {
					ex, err := r.NewExchange(client, up, apix.ExchangeOptions{Output: output})
					if err != nil {
						t.Fatal(err)
					}
					req, err := ex.ConvertRequest(payload(client, false), "up")
					if err != nil || !bytes.Contains(req.Body, []byte("hello")) {
						t.Fatalf("request %s %v", req.Body, err)
					}
					if up == gemini.GenerateContent {
						if bytes.Contains(req.Body, []byte(`"model"`)) || req.Metadata.Model != "up" {
							t.Fatalf("Gemini transport metadata %s %+v", req.Body, req.Metadata)
						}
					} else if decode(t, req.Body)["model"] != "up" {
						t.Fatal("model not rewritten")
					}
					resp, err := ex.ConvertResponse(200, apix.Payload{Body: []byte(fixtures[up].response)})
					if err != nil || !bytes.Contains(resp.Body, []byte("answer")) {
						t.Fatalf("response %s %v", resp.Body, err)
					}
					var results [][]sse.Event
					for _, step := range []int{len(fixtures[up].stream), 1} {
						sc, err := ex.NewStreamConverter()
						if err != nil {
							t.Fatal(err)
						}
						results = append(results, drain(t, sc, fixtures[up].stream, step))
					}
					if !reflect.DeepEqual(results[0], results[1]) {
						t.Fatal("output depends on network chunk size")
					}
					if len(results[0]) == 0 {
						t.Fatal("empty stream")
					}
				})
			}
		}
	}
}
func TestNativeGeminiAndThinkingOptions(t *testing.T) {
	r := registry(t)
	body := []byte(` {"contents":[{"parts":[{"text":"hello"}]}],"generationConfig":{"thinkingConfig":{"thinkingLevel":"high","includeThoughts":false}},"unknown":9007199254740993} `)
	ex, _ := r.NewExchange(gemini.GenerateContent, gemini.GenerateContent, apix.ExchangeOptions{})
	out, err := ex.ConvertRequest(apix.Payload{Body: body, Metadata: apix.RequestMetadata{Model: "m"}}, "new")
	if err != nil || !bytes.Equal(body, out.Body) {
		t.Fatalf("native changed: %s %v", out.Body, err)
	}
	visible := true
	ex, _ = r.NewExchange(gemini.GenerateContent, gemini.GenerateContent, apix.ExchangeOptions{Ext: gemini.GenerateContentOptions{ThinkingMode: gemini.ThinkingBudget, IncludeThoughts: &visible}})
	visible = false // Options are copied on exchange construction.
	out, err = ex.ConvertRequest(apix.Payload{Body: body, Metadata: apix.RequestMetadata{Model: "m"}}, "m")
	if err != nil || !bytes.Contains(out.Body, []byte(`"thinkingBudget":24576`)) || !bytes.Contains(out.Body, []byte(`"includeThoughts":true`)) {
		t.Fatalf("options %s %v", out.Body, err)
	}
	if _, err = r.NewExchange(openai.ChatCompletions, gemini.GenerateContent, apix.ExchangeOptions{Ext: gemini.GenerateContentOptions{ThinkingMode: "unknown"}}); err == nil {
		t.Fatal("invalid option accepted")
	}
}
func TestSignatureAndExactArgumentsRoundTrip(t *testing.T) {
	r := registry(t)
	response := `{"responseId":"r","modelVersion":"m","candidates":[{"content":{"role":"model","parts":[{"text":"thinking","thought":true,"thoughtSignature":"sig-thought"},{"text":"visible","thoughtSignature":"sig-text"},{"functionCall":{"id":"call1","name":"lookup","args":{"integer":9007199254740993}},"thoughtSignature":"sig-call"}]},"finishReason":"STOP"}]}`
	for _, p := range []apix.Protocol{openai.ChatCompletions, openai.Responses, anthropic.Messages} {
		t.Run(string(p), func(t *testing.T) {
			ex, _ := r.NewExchange(p, gemini.GenerateContent, apix.ExchangeOptions{})
			out, err := ex.ConvertResponse(200, apix.Payload{Body: []byte(response)})
			if err != nil {
				t.Fatal(err)
			}
			if !bytes.Contains(out.Body, []byte("9007199254740993")) {
				t.Fatal("rounded arguments")
			}
			var doc map[string]json.RawMessage
			json.Unmarshal(out.Body, &doc)
			var body []byte
			if p == openai.Responses {
				body = append([]byte(`{"model":"m","input":`), doc["output"]...)
				body = append(body, '}')
			} else {
				var message json.RawMessage
				if p == openai.ChatCompletions {
					var choices []map[string]json.RawMessage
					json.Unmarshal(doc["choices"], &choices)
					message = choices[0]["message"]
				} else {
					message = out.Body
				}
				body = append([]byte(`{"model":"m","messages":[`), message...)
				body = append(body, ']', '}')
			}
			back, err := ex.ConvertRequest(apix.Payload{Body: body}, "m")
			if err != nil {
				t.Fatal(err)
			}
			for _, s := range []string{"sig-thought", "sig-text", "sig-call", "9007199254740993"} {
				if !bytes.Contains(back.Body, []byte(s)) {
					t.Fatalf("lost %s: %s", s, back.Body)
				}
			}
		})
	}
}
func TestLateSignatureUsageAndStreamIDs(t *testing.T) {
	r := registry(t)
	wire := frame(map[string]any{"responseId": "r", "candidates": []any{map[string]any{"content": map[string]any{"parts": []any{map[string]any{"text": "hi"}}}}}}) + frame(map[string]any{"candidates": []any{map[string]any{"content": map[string]any{"parts": []any{map[string]any{"functionCall": map[string]any{"name": "f", "args": map[string]any{"x": "y"}}}}}, "finishReason": "STOP"}}}) + frame(map[string]any{"candidates": []any{map[string]any{"content": map[string]any{"parts": []any{map[string]any{"thoughtSignature": "late"}}}}}, "usageMetadata": map[string]any{"promptTokenCount": 7, "candidatesTokenCount": 3, "thoughtsTokenCount": 2, "cachedContentTokenCount": 4, "totalTokenCount": 12}})
	for _, p := range []apix.Protocol{openai.ChatCompletions, openai.Responses, anthropic.Messages, gemini.GenerateContent} {
		t.Run(string(p), func(t *testing.T) {
			ex, _ := r.NewExchange(p, gemini.GenerateContent, apix.ExchangeOptions{})
			sc, _ := ex.NewStreamConverter()
			first := strings.Index(wire, "\n\n") + 2
			out, done, err := sc.Feed([]byte(wire[:first]))
			if err != nil || done || len(out) == 0 {
				t.Fatalf("initial streaming events %d %v %v", len(out), done, err)
			}
			rest, done, err := sc.Feed([]byte(wire[first:]))
			if err != nil || done {
				t.Fatalf("terminal committed before tail: %v %v", done, err)
			}
			out = append(out, rest...)
			tail, err := sc.Finish()
			if err != nil {
				t.Fatal(err)
			}
			out = append(out, tail...)
			if !bytes.Contains(bytes.Join(out, nil), []byte("late")) {
				t.Fatal("late signature lost")
			}
			if p == openai.Responses {
				var d sse.Decoder
				var expected float64
				ids := map[string]string{}
				for _, ev := range d.Feed(bytes.Join(out, nil)) {
					o := decode(t, ev.Data)
					if o["sequence_number"] != expected {
						t.Fatal("sequence")
					}
					expected++
					if ev.Event == "response.output_item.added" || ev.Event == "response.output_item.done" {
						item := o["item"].(map[string]any)
						if id, ok := item["call_id"].(string); ok {
							key := item["id"].(string)
							if prior := ids[key]; prior != "" && prior != id {
								t.Fatal("unstable call id")
							}
							ids[key] = id
						}
					}
				}
			}
		})
	}
}
func TestInteractionsNativeBoundary(t *testing.T) {
	r := registry(t)
	for _, p := range []apix.Protocol{gemini.GenerateContent, openai.ChatCompletions, openai.Responses, anthropic.Messages} {
		if r.CanConvert(p, gemini.Interactions) || r.CanConvert(gemini.Interactions, p) {
			t.Fatal("advertised an unimplemented path")
		}
	}
	body := []byte(`{"model":"m","input":"hello","previous_interaction_id":"stored","background":true,"unknown":9007199254740993}`)
	ex, err := r.NewExchange(gemini.Interactions, gemini.Interactions, apix.ExchangeOptions{})
	if err != nil {
		t.Fatal(err)
	}
	out, err := ex.ConvertRequest(apix.Payload{Body: body}, "m")
	if err != nil || !bytes.Equal(body, out.Body) {
		t.Fatalf("native %s %v", out.Body, err)
	}
	agent := []byte(`{"agent":"deep-research","input":"hello","background":true}`)
	out, err = ex.ConvertRequest(apix.Payload{Body: agent}, "")
	if err != nil || !bytes.Equal(out.Body, agent) {
		t.Fatal(err)
	}
	if _, err = ex.ConvertRequest(apix.Payload{Body: agent}, "another-model"); err == nil {
		t.Fatal("rewrote agent")
	}
	if _, err = r.NewExchange(gemini.Interactions, gemini.Interactions, apix.ExchangeOptions{Output: openai.Responses}); err == nil {
		t.Fatal("converted Interactions output")
	}
	wire := "event: interaction.created\ndata: {\"event_type\":\"interaction.created\",\"interaction\":{\"id\":\"i\",\"status\":\"in_progress\"}}\n\nevent: interaction.completed\ndata: {\"event_type\":\"interaction.completed\",\"interaction\":{\"id\":\"i\",\"status\":\"completed\"}}\n\n"
	sc, _ := ex.NewStreamConverter()
	events, _, err := sc.Feed([]byte(wire))
	tail, ferr := sc.Finish()
	if err != nil || ferr != nil || string(bytes.Join(append(events, tail...), nil)) != wire {
		t.Fatalf("stream %v %v", err, ferr)
	}
	sc, _ = ex.NewStreamConverter()
	sc.Feed([]byte("data: {\"event_type\":\"interaction.created\"}\n\n"))
	if _, err = sc.Finish(); err == nil {
		t.Fatal("accepted incomplete interaction")
	}
}
