package gemini_test

import (
	"bytes"
	"encoding/json"
	"strings"
	"testing"

	"github.com/dillonzq/apix"
	"github.com/dillonzq/apix/protocol/anthropic"
	"github.com/dillonzq/apix/protocol/gemini"
	"github.com/dillonzq/apix/protocol/openai"
	"github.com/dillonzq/apix/thinking"
)

func TestMediaRequestsAndSchema(t *testing.T) {
	r := registry(t)
	body := `{"model":"m","input":[{"role":"user","content":[{"type":"input_text","text":"inspect"},{"type":"input_image","image_url":"data:image/png;base64,YQ=="},{"type":"input_file","file_data":"data:application/pdf;base64,Yg=="},{"type":"input_audio","input_audio":{"data":"Yw==","format":"wav"}},{"type":"input_video","video_url":{"url":"https://example.com/video.mp4"}}]}],"text":{"format":{"type":"json_schema","schema":{"type":"object","properties":{"answer":{"type":"integer"}},"required":["answer"]}}}}`
	ex, _ := r.NewExchange(openai.Responses, gemini.GenerateContent, apix.ExchangeOptions{LossPolicy: apix.LossPolicySafe})
	out, err := ex.ConvertRequest(apix.Payload{Body: []byte(body)}, "m")
	if err != nil {
		t.Fatal(err)
	}
	doc := decode(t, out.Body)
	parts := doc["contents"].([]any)[0].(map[string]any)["parts"].([]any)
	if len(parts) != 5 {
		t.Fatalf("parts %s", out.Body)
	}
	cfg := doc["generationConfig"].(map[string]any)
	if cfg["responseMimeType"] != "application/json" || cfg["responseJsonSchema"] == nil {
		t.Fatalf("schema %s", out.Body)
	}
	back, _ := r.NewExchange(gemini.GenerateContent, openai.Responses, apix.ExchangeOptions{})
	req, err := back.ConvertRequest(apix.Payload{Body: out.Body}, "m")
	if err != nil {
		t.Fatal(err)
	}
	for _, token := range []string{"input_image", "input_file", "input_audio", "input_video", "json_schema"} {
		if !bytes.Contains(req.Body, []byte(token)) {
			t.Fatalf("missing %s: %s", token, req.Body)
		}
	}
	pdf := []byte(`{"contents":[{"role":"user","parts":[{"inlineData":{"mimeType":"application/pdf","data":"Yg=="}}]}]}`)
	ex, _ = r.NewExchange(gemini.GenerateContent, anthropic.Messages, apix.ExchangeOptions{})
	out, err = ex.ConvertRequest(apix.Payload{Body: pdf}, "claude")
	if err != nil || !bytes.Contains(out.Body, []byte(`"document"`)) {
		t.Fatalf("PDF %s %v", out.Body, err)
	}
}
func TestToolHistoryWithRepeatedNames(t *testing.T) {
	r := registry(t)
	body := []byte(`{"contents":[{"role":"model","parts":[{"functionCall":{"id":"c1","name":"f","args":{"n":9007199254740993}}},{"functionCall":{"id":"c2","name":"f","args":{}}}]},{"role":"user","parts":[{"functionResponse":{"id":"c2","name":"f","response":{"value":2}}},{"functionResponse":{"id":"c1","name":"f","response":{"value":1}}}]}],"tools":[{"functionDeclarations":[{"name":"f","parametersJsonSchema":{"type":"object"}}]}]}`)
	for _, p := range []apix.Protocol{openai.ChatCompletions, openai.Responses, anthropic.Messages} {
		t.Run(string(p), func(t *testing.T) {
			ex, _ := r.NewExchange(gemini.GenerateContent, p, apix.ExchangeOptions{})
			out, err := ex.ConvertRequest(apix.Payload{Body: body}, "m")
			if err != nil {
				t.Fatal(err)
			}
			reverse, _ := r.NewExchange(p, gemini.GenerateContent, apix.ExchangeOptions{})
			back, err := reverse.ConvertRequest(out, "m")
			if err != nil {
				t.Fatal(err)
			}
			if !bytes.Contains(back.Body, []byte("9007199254740993")) {
				t.Fatal("integer rounded")
			}
			doc := decode(t, back.Body)
			var order []string
			for _, cv := range doc["contents"].([]any) {
				for _, pv := range cv.(map[string]any)["parts"].([]any) {
					if f, ok := pv.(map[string]any)["functionResponse"].(map[string]any); ok {
						order = append(order, f["id"].(string))
					}
				}
			}
			if strings.Join(order, ",") != "c2,c1" {
				t.Fatalf("tool pairing changed %v", order)
			}
		})
	}
}
func TestGeminiCustomAndShellRestore(t *testing.T) {
	r := registry(t)
	for _, tc := range []struct{ tool, args, want string }{
		{`{"type":"namespace","name":"edit","tools":[{"type":"custom","name":"patch"}]}`, `{"input":"patch text"}`, "custom_tool_call"},
		{`{"type":"shell"}`, `{"commands":["pwd"]}`, "shell_call"},
	} {
		ex, _ := r.NewExchange(openai.Responses, gemini.GenerateContent, apix.ExchangeOptions{})
		req, err := ex.ConvertRequest(apix.Payload{Body: []byte(`{"input":"hello","tools":[` + tc.tool + `]}`)}, "m")
		if err != nil {
			t.Fatal(err)
		}
		doc := decode(t, req.Body)
		name := doc["tools"].([]any)[0].(map[string]any)["functionDeclarations"].([]any)[0].(map[string]any)["name"].(string)
		response := `{"responseId":"r","candidates":[{"content":{"parts":[{"functionCall":{"id":"c","name":"` + name + `","args":` + tc.args + `}}]},"finishReason":"STOP"}]}`
		out, err := ex.ConvertResponse(200, apix.Payload{Body: []byte(response)})
		if err != nil || !bytes.Contains(out.Body, []byte(tc.want)) {
			t.Fatalf("restoration %s %v", out.Body, err)
		}
		sc, _ := ex.NewStreamConverter()
		events := drain(t, sc, "data: "+response+"\n\n", 1)
		found := false
		for _, e := range events {
			if bytes.Contains(e.Data, []byte(tc.want)) {
				found = true
			}
		}
		if !found {
			t.Fatal("stream identity not restored")
		}
	}
}
func TestLossPoliciesAndInvalidInput(t *testing.T) {
	r := registry(t)
	for _, body := range []string{
		`{"contents":[{"parts":[{"text":3}]}]}`,
		`{"contents":[{"parts":[{"functionCall":{"name":"f","args":"not json"}}]}]}`,
		`{"contents":[{"parts":[{"inlineData":{"mimeType":"image/png","data":"!!!"}}]}]}`,
		`{"contents":[{"parts":[{"unknown":{"payload":"content"}}]}]}`,
		`{"contents":[{"parts":[{"functionResponse":{"name":"missing","response":{}}}]}]}`,
	} {
		ex, _ := r.NewExchange(gemini.GenerateContent, openai.ChatCompletions, apix.ExchangeOptions{})
		if _, err := ex.ConvertRequest(apix.Payload{Body: []byte(body)}, "m"); err == nil {
			t.Fatalf("accepted %s", body)
		}
	}
	body := []byte(`{"contents":[{"parts":[{"text":"hi"}]}],"cachedContent":"cachedContents/provider-specific"}`)
	for _, policy := range []apix.LossPolicy{apix.LossPolicyAllow, apix.LossPolicySafe, apix.LossPolicyStrict} {
		ex, _ := r.NewExchange(gemini.GenerateContent, openai.Responses, apix.ExchangeOptions{LossPolicy: policy})
		_, err := ex.ConvertRequest(apix.Payload{Body: body}, "m")
		if (err != nil) != (policy != apix.LossPolicyAllow) || len(ex.Diagnostics()) == 0 {
			t.Fatalf("policy %s: %v %v", policy, err, ex.Diagnostics())
		}
	}
	ex, _ := r.NewExchange(openai.ChatCompletions, gemini.GenerateContent, apix.ExchangeOptions{Ext: gemini.GenerateContentOptions{RequireThoughtSignatures: true}})
	if _, err := ex.ConvertRequest(apix.Payload{Body: []byte(`{"messages":[{"role":"assistant","tool_calls":[{"id":"c","type":"function","function":{"name":"f","arguments":"{}"}}]}]}`)}, "m"); err == nil {
		t.Fatal("unsigned history accepted")
	}
}
func TestStreamErrorIsTerminal(t *testing.T) {
	r := registry(t)
	for _, p := range []apix.Protocol{openai.ChatCompletions, openai.Responses, anthropic.Messages, gemini.GenerateContent} {
		ex, _ := r.NewExchange(p, gemini.GenerateContent, apix.ExchangeOptions{})
		sc, _ := ex.NewStreamConverter()
		wire := strings.Split(fixtures[gemini.GenerateContent].stream, "\n\n")[0] + "\n\ndata: {broken\n\n"
		events, done, err := sc.Feed([]byte(wire))
		if err == nil || done || len(events) == 0 {
			t.Fatalf("%s lost prefix or accepted error: %v %v %d", p, err, done, len(events))
		}
		if _, err2 := sc.Finish(); err2 != err {
			t.Fatal("error changed at EOF")
		}
		if events, done, err2 := sc.Feed([]byte(fixtures[gemini.GenerateContent].stream)); err2 != err || done || len(events) > 0 {
			t.Fatal("failed stream recovered")
		}
		sc, _ = ex.NewStreamConverter()
		sc.Feed([]byte(strings.Split(wire, "\n\n")[0] + "\n\n"))
		if _, err = sc.Finish(); err == nil {
			t.Fatal("accepted truncated Gemini stream")
		}
	}
}
func TestThinkingAndTransportMetadata(t *testing.T) {
	r := registry(t)
	stream := true
	req := apix.Payload{Body: []byte(fixtures[gemini.GenerateContent].request), Metadata: apix.RequestMetadata{Model: "gemini-test", Stream: &stream}}
	info, err := r.Inspect(gemini.GenerateContent, req)
	if err != nil || info.Model != "gemini-test" || !info.Stream || !info.Needs(apix.FeatureStream) {
		t.Fatalf("info %+v %v", info, err)
	}
	ex, _ := r.NewExchange(gemini.GenerateContent, openai.ChatCompletions, apix.ExchangeOptions{})
	out, err := ex.ConvertRequest(req, "")
	if err != nil || !bytes.Contains(out.Body, []byte(`"stream":true`)) || !bytes.Contains(out.Body, []byte(`"model":"gemini-test"`)) {
		t.Fatalf("metadata %s %v", out.Body, err)
	}
	for _, s := range []string{"m(high)", "m(8192)", "m(none)", "m(auto)"} {
		out, err := thinking.ApplySuffix(gemini.GenerateContent, []byte(`{"generationConfig":{"thinkingConfig":{"includeThoughts":false}}}`), thinking.ParseSuffix(s))
		if err != nil {
			t.Fatal(err)
		}
		doc := decode(t, out)
		tc := doc["generationConfig"].(map[string]any)["thinkingConfig"].(map[string]any)
		if tc["includeThoughts"] != false || tc["thinkingBudget"] == nil && tc["thinkingLevel"] == nil {
			t.Fatalf("thinking %s", out)
		}
	}
}
func TestChunkedToolArguments(t *testing.T) {
	r := registry(t)
	wire := "data: {\"id\":\"r\",\"model\":\"m\",\"choices\":[{\"delta\":{\"tool_calls\":[{\"index\":0,\"id\":\"c\",\"function\":{\"arguments\":\"{\\\"n\\\":\"}}]}}]}\n\n" +
		"data: {\"choices\":[{\"delta\":{\"tool_calls\":[{\"index\":0,\"function\":{\"name\":\"lookup\",\"arguments\":\"9007199254740993}\"}}]},\"finish_reason\":\"tool_calls\"}]}\n\ndata: [DONE]\n\n"
	ex, _ := r.NewExchange(gemini.GenerateContent, openai.ChatCompletions, apix.ExchangeOptions{})
	sc, _ := ex.NewStreamConverter()
	events := drain(t, sc, wire, 1)
	calls := 0
	for _, ev := range events {
		var root struct {
			Candidates []struct {
				Content struct {
					Parts []struct {
						FunctionCall json.RawMessage `json:"functionCall"`
					} `json:"parts"`
				} `json:"content"`
			} `json:"candidates"`
		}
		json.Unmarshal(ev.Data, &root)
		for _, cand := range root.Candidates {
			for _, part := range cand.Content.Parts {
				if len(part.FunctionCall) > 0 {
					calls++
					if !bytes.Contains(part.FunctionCall, []byte("9007199254740993")) {
						t.Fatal("rounded arguments")
					}
				}
			}
		}
	}
	if calls != 1 {
		t.Fatalf("calls=%d", calls)
	}
}

func TestGeminiAliasesSchemaAndCitationOffsets(t *testing.T) {
	r := registry(t)
	ex, _ := r.NewExchange(gemini.GenerateContent, openai.Responses, apix.ExchangeOptions{})
	body := []byte(`{"contents":[{"role":"user","parts":[{"text":"hi"}]}],"generation_config":{"response_mime_type":"application/json","response_schema":{"type":"OBJECT","properties":{"n":{"type":"INTEGER","nullable":true}}}},"tools":[{"function_declarations":[{"name":"f","parameters":{"type":"OBJECT"}}]}]}`)
	out, err := ex.ConvertRequest(apix.Payload{Body: body}, "m")
	if err != nil || !bytes.Contains(out.Body, []byte(`"type":["integer","null"]`)) {
		t.Fatalf("schema %s %v", out.Body, err)
	}
	response := `{"response_id":"r","model_version":"m","candidates":[{"content":{"parts":[{"text":"你好 world"}]},"finish_reason":"STOP","grounding_metadata":{"groundingChunks":[{"web":{"uri":"https://example.com","title":"source"}}],"groundingSupports":[{"segment":{"partIndex":0,"startIndex":0,"endIndex":6,"text":"你好"},"groundingChunkIndices":[0]}]}}]}`
	for _, target := range []apix.Protocol{openai.ChatCompletions, openai.Responses, anthropic.Messages} {
		t.Run(string(target), func(t *testing.T) {
			ex, _ := r.NewExchange(target, gemini.GenerateContent, apix.ExchangeOptions{})
			out, err := ex.ConvertResponse(200, apix.Payload{Body: []byte(response)})
			if err != nil {
				t.Fatal(err)
			}
			if !bytes.Contains(out.Body, []byte("https://example.com")) {
				t.Fatalf("citation lost %s", out.Body)
			}
			if target != anthropic.Messages && !bytes.Contains(out.Body, []byte(`"end_index":2`)) {
				t.Fatalf("byte offsets not converted to runes: %s", out.Body)
			}
			sc, _ := ex.NewStreamConverter()
			events := drain(t, sc, "data: "+response+"\n\n", 1)
			found := false
			for _, ev := range events {
				if bytes.Contains(ev.Data, []byte("https://example.com")) {
					found = true
				}
			}
			if !found {
				t.Fatal("stream citation lost")
			}
		})
	}
}
func TestInvalidReasoningTypesAndSourceTerminals(t *testing.T) {
	r := registry(t)
	for _, config := range []string{`{"thinkingConfig":{"thinkingBudget":"bad"}}`, `{"thinkingConfig":{"thinkingLevel":3}}`, `{"candidateCount":"two"}`, `{"thinkingConfig":{"thinkingLevel":"high","thinkingBudget":1024}}`} {
		ex, _ := r.NewExchange(gemini.GenerateContent, openai.ChatCompletions, apix.ExchangeOptions{})
		if _, err := ex.ConvertRequest(apix.Payload{Body: []byte(`{"contents":[{"parts":[{"text":"hi"}]}],"generationConfig":` + config + `}`)}, "m"); err == nil {
			t.Fatalf("accepted invalid config: %s", config)
		}
	}
	ex, _ := r.NewExchange(gemini.GenerateContent, openai.Responses, apix.ExchangeOptions{})
	sc, _ := ex.NewStreamConverter()
	wire := "event: response.output_text.delta\ndata: {\"item_id\":\"m\",\"delta\":\"hello\"}\n\nevent: response.completed\ndata: {\"type\":\"response.completed\",\"response\":{\"id\":\"r\",\"object\":\"response\",\"status\":\"failed\",\"output\":[]}}\n\n"
	events, done, err := sc.Feed([]byte(wire))
	if err == nil || done {
		t.Fatal("invalid source terminal accepted")
	}
	if bytes.Contains(bytes.Join(events, nil), []byte("finishReason")) {
		t.Fatal("source validation failure emitted a successful Gemini terminal")
	}
}

func TestIndependentGeminiOutputWithNativeToolNormalization(t *testing.T) {
	r := registry(t)
	ex, err := r.NewExchange(openai.Responses, openai.Responses, apix.ExchangeOptions{Output: gemini.GenerateContent, Ext: openai.ResponsesOptions{NormalizeTools: true}})
	if err != nil {
		t.Fatal(err)
	}
	req, err := ex.ConvertRequest(apix.Payload{Body: []byte(`{"model":"m","input":"hi","tools":[{"type":"namespace","name":"editor","tools":[{"type":"custom","name":"patch"}]}]}`)}, "m")
	if err != nil {
		t.Fatal(err)
	}
	doc := decode(t, req.Body)
	name := doc["tools"].([]any)[0].(map[string]any)["name"].(string)
	item := map[string]any{"type": "function_call", "id": "fc", "call_id": "c", "name": name, "arguments": `{"input":"text"}`}
	response := map[string]any{"id": "r", "object": "response", "status": "completed", "output": []any{item}}
	wire := "event: response.output_item.added\n" + frame(map[string]any{"type": "response.output_item.added", "output_index": 0, "item": item}) + "event: response.completed\n" + frame(map[string]any{"type": "response.completed", "response": response})
	sc, err := ex.NewStreamConverter()
	if err != nil {
		t.Fatal(err)
	}
	events := drain(t, sc, wire, 1)
	found := false
	for _, e := range events {
		if bytes.Contains(e.Data, []byte(name)) {
			found = true
		}
	}
	if !found {
		t.Fatal("native validator rewrote tool identity for a different output protocol")
	}
}

func TestMalformedGeminiStreamShapes(t *testing.T) {
	r := registry(t)
	for _, p := range []apix.Protocol{gemini.GenerateContent, openai.ChatCompletions, openai.Responses, anthropic.Messages} {
		for _, body := range []string{`{"candidates":[{"content":"invalid","finishReason":"STOP"}]}`, `{"candidates":[{"content":{"parts":"invalid"},"finishReason":"STOP"}]}`, `{"candidates":[{"index":"invalid","finishReason":"STOP"}]}`, `{"candidates":[{"content":{"parts":[{"text":123}]},"finishReason":"STOP"}]}`, `{"candidates":[{"finishReason":"STOP"}],"error":"failed"}`} {
			ex, _ := r.NewExchange(p, gemini.GenerateContent, apix.ExchangeOptions{})
			sc, _ := ex.NewStreamConverter()
			_, done, err := sc.Feed([]byte("data: " + body + "\n\n"))
			if err == nil || done {
				t.Fatalf("%s accepted malformed chunk %s", p, body)
			}
		}
	}
}

func TestGeminiThinkingUsageRoundTrip(t *testing.T) {
	r := registry(t)
	wire := `{"responseId":"r","candidates":[{"content":{"parts":[{"text":"answer"}]},"finishReason":"STOP"}],"usageMetadata":{"promptTokenCount":7,"cachedContentTokenCount":4,"candidatesTokenCount":3,"thoughtsTokenCount":2,"totalTokenCount":12}}`
	for _, p := range []apix.Protocol{openai.ChatCompletions, openai.Responses, anthropic.Messages} {
		ex, _ := r.NewExchange(p, gemini.GenerateContent, apix.ExchangeOptions{})
		out, err := ex.ConvertResponse(200, apix.Payload{Body: []byte(wire)})
		if err != nil {
			t.Fatal(err)
		}
		back, _ := r.NewExchange(gemini.GenerateContent, p, apix.ExchangeOptions{})
		restored, err := back.ConvertResponse(200, out)
		if err != nil {
			t.Fatal(err)
		}
		doc := decode(t, restored.Body)
		usage := doc["usageMetadata"].(map[string]any)
		if usage["promptTokenCount"] != float64(7) || usage["cachedContentTokenCount"] != float64(4) || usage["candidatesTokenCount"] != float64(3) || usage["thoughtsTokenCount"] != float64(2) || usage["totalTokenCount"] != float64(12) {
			t.Fatalf("%s usage %v", p, usage)
		}
	}
}
