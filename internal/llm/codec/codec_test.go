package codec_test

import (
	"bytes"
	"encoding/json"
	"slices"
	"testing"

	"github.com/dillonzq/apix"
	"github.com/dillonzq/apix/internal/llm/translate"
	"github.com/dillonzq/apix/protocol/anthropic"
	"github.com/dillonzq/apix/protocol/gemini"
	"github.com/dillonzq/apix/protocol/openai"
)

// The fixtures are the conversion golden inputs in internal/llm/translate.
var fixtures = map[apix.Protocol]struct{ request, response, stream string }{
	openai.ChatCompletions: {
		`{"model":"m","messages":[{"role":"user","content":"hello"}],"tools":[{"type":"function","function":{"name":"f","strict":true,"parameters":{"type":"object"}}}],"response_format":{"type":"json_schema","json_schema":{"name":"answer","strict":true,"schema":{"type":"object"}}}}`,
		`{"id":"r","model":"m","choices":[{"message":{"role":"assistant","content":"hello","reasoning_content":"plan","tool_calls":[{"id":"c1","type":"function","function":{"name":"f","arguments":"{}"}}]},"finish_reason":"tool_calls"}],"usage":{"prompt_tokens":4,"completion_tokens":2}}`,
		"data: {\"id\":\"r\",\"model\":\"m\",\"choices\":[{\"delta\":{\"content\":\"hello\"}}]}\n\ndata: {\"choices\":[{\"delta\":{},\"finish_reason\":\"stop\"}]}\n\ndata: [DONE]\n\n",
	},
	openai.Responses: {
		`{"model":"m","input":"hello","tools":[{"type":"function","name":"f","strict":true,"parameters":{"type":"object"}}],"text":{"format":{"type":"json_schema","name":"answer","strict":true,"schema":{"type":"object"}}}}`,
		`{"id":"r","object":"response","model":"m","status":"completed","output":[{"type":"reasoning","summary":[{"type":"summary_text","text":"plan"}]},{"type":"message","role":"assistant","content":[{"type":"output_text","text":"hello"}]},{"type":"function_call","call_id":"c1","name":"f","arguments":"{}"}],"usage":{"input_tokens":4,"output_tokens":2}}`,
		"event: response.created\ndata: {\"response\":{\"id\":\"r\",\"model\":\"m\"}}\n\nevent: response.output_text.delta\ndata: {\"item_id\":\"m1\",\"content_index\":0,\"delta\":\"hello\"}\n\nevent: response.completed\ndata: {\"type\":\"response.completed\",\"response\":{\"id\":\"r\",\"model\":\"m\",\"object\":\"response\",\"status\":\"completed\",\"output\":[]}}\n\n",
	},
	gemini.GenerateContent: {
		`{"contents":[{"role":"user","parts":[{"text":"hello"}]}],"tools":[{"functionDeclarations":[{"name":"f","parametersJsonSchema":{"type":"object"}}]}]}`,
		`{"responseId":"r","modelVersion":"m","candidates":[{"index":0,"content":{"role":"model","parts":[{"text":"plan","thought":true},{"text":"hello"},{"functionCall":{"id":"c1","name":"f","args":{}}}]},"finishReason":"STOP"}],"usageMetadata":{"promptTokenCount":4,"candidatesTokenCount":2,"totalTokenCount":6}}`,
		"data: {\"responseId\":\"r\",\"modelVersion\":\"m\",\"candidates\":[{\"index\":0,\"content\":{\"parts\":[{\"text\":\"hello\"}]}}]}\n\ndata: {\"candidates\":[{\"index\":0,\"finishReason\":\"STOP\"}]}\n\n",
	},

	anthropic.Messages: {
		`{"model":"m","messages":[{"role":"user","content":"hello"}],"max_tokens":100,"tools":[{"name":"f","strict":true,"input_schema":{"type":"object"}}],"output_config":{"format":{"type":"json_schema","schema":{"type":"object"}}}}`,
		`{"id":"r","model":"m","content":[{"type":"thinking","thinking":"plan"},{"type":"text","text":"hello"},{"type":"tool_use","id":"c1","name":"f","input":{}}],"stop_reason":"tool_use","usage":{"input_tokens":4,"output_tokens":2}}`,
		"event: message_start\ndata: {\"type\":\"message_start\",\"message\":{\"id\":\"r\",\"model\":\"m\"}}\n\nevent: content_block_start\ndata: {\"index\":0,\"content_block\":{\"type\":\"text\",\"text\":\"\"}}\n\nevent: content_block_delta\ndata: {\"index\":0,\"delta\":{\"type\":\"text_delta\",\"text\":\"hello\"}}\n\nevent: content_block_stop\ndata: {\"index\":0}\n\nevent: message_delta\ndata: {\"delta\":{\"stop_reason\":\"end_turn\"}}\n\nevent: message_stop\ndata: {\"type\":\"message_stop\"}\n\n",
	},
}

var internalFormat = map[apix.Protocol]string{
	openai.ChatCompletions: translate.FormatChatCompletions,
	openai.Responses:       translate.FormatResponses,
	anthropic.Messages:     translate.FormatMessages,
	gemini.GenerateContent: translate.FormatGemini,
}

func registry(t *testing.T) *apix.Registry {
	t.Helper()
	reg, err := apix.NewRegistry(slices.Concat(openai.Codecs(), anthropic.Codecs(), []apix.Codec{gemini.GenerateContentCodec()})...)
	if err != nil {
		t.Fatal(err)
	}
	return reg
}

func TestSpecs(t *testing.T) {
	reg := registry(t)
	for p, want := range map[apix.Protocol]string{openai.ChatCompletions: "/v1/chat/completions", openai.Responses: "/v1/responses", anthropic.Messages: "/v1/messages"} {
		spec, ok := reg.Spec(p)
		if !ok || spec.Kind != apix.KindLLM || spec.Stream != apix.FramingSSE || spec.Method != "POST" || spec.Path(apix.RequestInfo{}, "m") != want {
			t.Fatalf("%s: %+v", p, spec)
		}
	}
	if spec, _ := reg.Spec(anthropic.Messages); spec.Auth.Header != "x-api-key" || spec.Headers["anthropic-version"] == "" {
		t.Fatalf("messages auth: %+v", spec)
	}
	if spec, _ := reg.Spec(openai.Responses); spec.Auth != apix.AuthBearer {
		t.Fatalf("responses auth: %+v", spec)
	}
}

func drainEngine(t *testing.T, sc translate.StreamConverter, wire string) []byte {
	t.Helper()
	events, _, err := sc.Feed([]byte(wire))
	if err != nil {
		t.Fatal(err)
	}
	tail, err := sc.Finish()
	if err != nil {
		t.Fatal(err)
	}
	return bytes.Join(append(events, tail...), nil)
}

func drain(t *testing.T, sc apix.StreamConverter, wire string) []byte {
	t.Helper()
	events, _, err := sc.Feed([]byte(wire))
	if err != nil {
		t.Fatal(err)
	}
	tail, err := sc.Finish()
	if err != nil {
		t.Fatal(err)
	}
	return bytes.Join(append(events, tail...), nil)
}

// Every exchange must produce exactly what the engine produces for the same
// directions, so the public API inherits the golden snapshots. An empty output
// is the default: responses in the client protocol.
func TestExchangeMatchesEngine(t *testing.T) {
	reg := registry(t)
	for _, client := range reg.Protocols() {
		for _, upstream := range reg.Protocols() {
			for _, output := range append([]apix.Protocol{""}, reg.Protocols()...) {
				t.Run(string(client)+"/"+string(upstream)+"/"+string(output), func(t *testing.T) {
					exchangeMatchesEngine(t, reg, client, upstream, output)
				})
			}
		}
	}
}

func exchangeMatchesEngine(t *testing.T, reg *apix.Registry, client, upstream, output apix.Protocol) {
	t.Helper()
	from, to, out := internalFormat[client], internalFormat[upstream], internalFormat[output]
	if output == "" {
		out = from
	}
	state := translate.NewRequestState()
	ex, xerr := reg.NewExchange(client, upstream, apix.ExchangeOptions{Output: output})
	if xerr != nil {
		t.Fatal(xerr)
	}

	wantReq, err := translate.ConvertRequest(to, "up", from, []byte(fixtures[client].request), state)
	if err != nil {
		t.Fatal(err)
	}
	gotReq, gerr := ex.ConvertRequest(apix.Payload{Body: []byte(fixtures[client].request)}, "up")
	if gerr != nil {
		t.Fatal(gerr)
	}
	if !bytes.Equal(gotReq.Body, wantReq) || gotReq.ContentType != "application/json" {
		t.Fatalf("request:\n got %s\nwant %s", gotReq.Body, wantReq)
	}

	wantResp, err := translate.ConvertResponse(to, out, 200, []byte(fixtures[upstream].response), state)
	if err != nil {
		t.Fatal(err)
	}
	gotResp, gerr := ex.ConvertResponse(200, apix.Payload{Body: []byte(fixtures[upstream].response)})
	if gerr != nil {
		t.Fatal(gerr)
	}
	if normalizeTimes(gotResp.Body) != normalizeTimes(wantResp) {
		t.Fatalf("response:\n got %s\nwant %s", gotResp.Body, wantResp)
	}

	wantSC, err := translate.NewStreamConverter(to, out, state)
	if err != nil {
		t.Fatal(err)
	}
	gotSC, gerr := ex.NewStreamConverter()
	if gerr != nil {
		t.Fatal(gerr)
	}
	want, got := drainEngine(t, wantSC, fixtures[upstream].stream), drain(t, gotSC, fixtures[upstream].stream)
	if normalizeTimes(got) != normalizeTimes(want) {
		t.Fatalf("stream:\n got %s\nwant %s", got, want)
	}
}

// Synthesized responses carry creation times; compare everything else.
func normalizeTimes(b []byte) string {
	var out bytes.Buffer
	for i := 0; i < len(b); i++ {
		for _, key := range []string{`"created":`, `"created_at":`} {
			if bytes.HasPrefix(b[i:], []byte(key)) {
				out.WriteString(key + "0")
				i += len(key)
				for i < len(b) && b[i] >= '0' && b[i] <= '9' {
					i++
				}
			}
		}
		if i < len(b) {
			out.WriteByte(b[i])
		}
	}
	return out.String()
}

func TestEmptyUpstreamModelKeepsClientModel(t *testing.T) {
	ex, err := registry(t).NewExchange(anthropic.Messages, openai.ChatCompletions, apix.ExchangeOptions{})
	if err != nil {
		t.Fatal(err)
	}
	out, err := ex.ConvertRequest(apix.Payload{Body: []byte(`{"model":"claude-x","max_tokens":5,"messages":[{"role":"user","content":"hi"}]}`)}, "")
	if err != nil {
		t.Fatal(err)
	}
	var doc struct{ Model string }
	if json.Unmarshal(out.Body, &doc) != nil || doc.Model != "claude-x" {
		t.Fatalf("model: %s", out.Body)
	}
}

func TestLossPolicyAndDiagnostics(t *testing.T) {
	reg := registry(t)
	body := apix.Payload{Body: []byte(`{"model":"m","messages":[],"seed":42}`)}
	ex, err := reg.NewExchange(openai.ChatCompletions, anthropic.Messages, apix.ExchangeOptions{LossPolicy: apix.LossPolicySafe})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := ex.ConvertRequest(body, "m"); err == nil || apix.AsError(err).Class != apix.ClassUnsupported {
		t.Fatalf("safe policy accepted a semantic loss: %v", err)
	}
	ex, _ = reg.NewExchange(openai.ChatCompletions, anthropic.Messages, apix.ExchangeOptions{})
	if _, err := ex.ConvertRequest(body, "m"); err != nil {
		t.Fatal(err)
	}
	diags := ex.Diagnostics()
	if len(diags) == 0 {
		t.Fatal("no diagnostics recorded")
	}
	for _, d := range diags {
		if d.From != openai.ChatCompletions || d.To != anthropic.Messages {
			t.Fatalf("diagnostic protocols not public IDs: %+v", d)
		}
	}
}

func TestMessagesOptions(t *testing.T) {
	reg := registry(t)
	body := `{"model":"m","messages":[{"role":"assistant","reasoning_content":"raw"}]}`
	state := translate.NewRequestState()
	state.Options.AllowUnsignedThinking = true
	want, err := translate.ConvertRequest(translate.FormatMessages, "m", translate.FormatChatCompletions, []byte(body), state)
	if err != nil {
		t.Fatal(err)
	}
	for _, ext := range []any{anthropic.MessagesOptions{AllowUnsignedThinking: true}, &anthropic.MessagesOptions{AllowUnsignedThinking: true}} {
		ex, err := reg.NewExchange(openai.ChatCompletions, anthropic.Messages, apix.ExchangeOptions{Ext: ext})
		if err != nil {
			t.Fatal(err)
		}
		got, err := ex.ConvertRequest(apix.Payload{Body: []byte(body)}, "m")
		if err != nil || !bytes.Equal(got.Body, want) {
			t.Fatalf("%T: %s %v\nwant %s", ext, got.Body, err, want)
		}
	}
	plain, _ := reg.NewExchange(openai.ChatCompletions, anthropic.Messages, apix.ExchangeOptions{})
	if got, _ := plain.ConvertRequest(apix.Payload{Body: []byte(body)}, "m"); bytes.Equal(got.Body, want) {
		t.Fatal("unsigned thinking replayed without the option")
	}
}

func TestResponsesNormalizeTools(t *testing.T) {
	reg := registry(t)
	body := `{"model":"client","input":"hi","tools":[{"type":"namespace","name":"ns","tools":[{"type":"function","name":"f","parameters":{"type":"object"}}]}]}`
	state := translate.NewRequestState()
	normalized, err := translate.NormalizeResponsesRequest([]byte(body), state)
	if err != nil {
		t.Fatal(err)
	}
	want, err := translate.ConvertRequest(translate.FormatResponses, "up", translate.FormatResponses, normalized, state)
	if err != nil {
		t.Fatal(err)
	}
	ex, gerr := reg.NewExchange(openai.Responses, openai.Responses, apix.ExchangeOptions{Ext: openai.ResponsesOptions{NormalizeTools: true}})
	if gerr != nil {
		t.Fatal(gerr)
	}
	got, gerr := ex.ConvertRequest(apix.Payload{Body: []byte(body)}, "up")
	if gerr != nil || !bytes.Equal(got.Body, want) {
		t.Fatalf("got %s %v\nwant %s", got.Body, gerr, want)
	}
	plain, _ := reg.NewExchange(openai.Responses, openai.Responses, apix.ExchangeOptions{})
	if got, _ := plain.ConvertRequest(apix.Payload{Body: []byte(body)}, "up"); bytes.Contains(got.Body, []byte("ns__f")) || !bytes.Contains(got.Body, []byte(`"namespace"`)) {
		t.Fatalf("passthrough normalized tools: %s", got.Body)
	}
}

func TestRejectsForeignOptions(t *testing.T) {
	reg := registry(t)
	for _, tc := range []struct {
		upstream apix.Protocol
		ext      any
	}{
		{openai.ChatCompletions, anthropic.MessagesOptions{}},
		{anthropic.Messages, openai.ResponsesOptions{}},
		{openai.Responses, "normalize"},
	} {
		if _, err := reg.NewExchange(openai.ChatCompletions, tc.upstream, apix.ExchangeOptions{Ext: tc.ext}); err == nil || apix.AsError(err).Class != apix.ClassUnsupported {
			t.Fatalf("%s accepted %T: %v", tc.upstream, tc.ext, err)
		}
	}
}

func TestInspect(t *testing.T) {
	reg := registry(t)
	for _, tc := range []struct {
		protocol apix.Protocol
		body     string
		model    string
		stream   bool
		features []string
		fail     bool
	}{
		{openai.ChatCompletions, `{"model":"gpt","messages":[]}`, "gpt", false, nil, false},
		{openai.ChatCompletions, `{"model":"gpt","stream":true,"tools":[{"type":"function"}]}`, "gpt", true, []string{apix.FeatureStream, apix.FeatureTools}, false},
		{openai.Responses, `{"model":"o","stream":null,"tools":[]}`, "o", false, nil, false},
		{anthropic.Messages, `{"model":"claude","stream":true}`, "claude", true, []string{apix.FeatureStream}, false},
		{anthropic.Messages, `{"messages":[]}`, "", false, nil, false},
		{anthropic.Messages, `{"model":1}`, "", false, nil, true},
		{openai.Responses, `{"model":"o","stream":"yes"}`, "", false, nil, true},
		{openai.ChatCompletions, `not json`, "", false, nil, true},
	} {
		info, err := reg.Inspect(tc.protocol, apix.Payload{Body: []byte(tc.body)})
		if tc.fail {
			if err == nil {
				t.Fatalf("%s %s: no error", tc.protocol, tc.body)
			}
			continue
		}
		if err != nil || info.Kind != apix.KindLLM || info.Protocol != tc.protocol || info.Model != tc.model || info.Stream != tc.stream || !slices.Equal(info.Features, tc.features) {
			t.Fatalf("%s %s: %+v %v", tc.protocol, tc.body, info, err)
		}
	}
}

func TestEncodeError(t *testing.T) {
	reg := registry(t)
	err := apix.FromStatus(429, "slow down")
	var openAI struct {
		Error struct{ Message, Type, Code string }
	}
	if json.Unmarshal(reg.EncodeError(openai.ChatCompletions, err).Body, &openAI) != nil || openAI.Error.Type != "rate_limit_error" || openAI.Error.Message != "slow down" || openAI.Error.Code != string(apix.ClassRateLimit) {
		t.Fatalf("openai: %+v", openAI)
	}
	var claude struct {
		Type  string
		Error struct{ Type, Message string }
	}
	if json.Unmarshal(reg.EncodeError(anthropic.Messages, apix.FromStatus(404, "no model")).Body, &claude) != nil || claude.Type != "error" || claude.Error.Type != "not_found_error" || claude.Error.Message != "no model" {
		t.Fatalf("anthropic: %+v", claude)
	}
	if !json.Valid(reg.EncodeError(openai.Responses, nil).Body) {
		t.Fatal("nil error body is not JSON")
	}
}
