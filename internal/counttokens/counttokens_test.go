package counttokens_test

import (
	"bytes"
	"encoding/json"
	"errors"
	"slices"
	"strings"
	"testing"

	"github.com/dillonzq/apix"
	"github.com/dillonzq/apix/internal/counttokens"
)

var all = []apix.Protocol{counttokens.MessagesCountTokens, counttokens.ResponsesInputTokens, counttokens.CountTokens}

func registry(t testing.TB) *apix.Registry {
	t.Helper()
	reg, err := apix.NewRegistry(counttokens.New(counttokens.MessagesCountTokens), counttokens.New(counttokens.ResponsesInputTokens), counttokens.New(counttokens.CountTokens))
	if err != nil {
		t.Fatal(err)
	}
	return reg
}

// requests carry a system prompt, a user turn, a tool and a generation-only
// field each, so every direction shows what an upstream count body keeps.
var requests = map[apix.Protocol]apix.Payload{
	counttokens.MessagesCountTokens: {Body: []byte(`{"model":"claude","system":"be brief","messages":[{"role":"user","content":"hi"}],` +
		`"tools":[{"name":"f","description":"d","input_schema":{"type":"object"}}],"thinking":{"type":"enabled","budget_tokens":2048}}`)},
	counttokens.ResponsesInputTokens: {Body: []byte(`{"model":"gpt","instructions":"be brief","input":"hi","max_output_tokens":64,"stream":true,"temperature":0.5,` +
		`"tools":[{"type":"function","name":"f","parameters":{"type":"object"}}],"reasoning":{"effort":"high"}}`)},
	counttokens.CountTokens: {Body: []byte(`{"generateContentRequest":{"model":"models/gem","systemInstruction":{"parts":[{"text":"be brief"}]},` +
		`"contents":[{"role":"user","parts":[{"text":"hi"}]}],"tools":[{"functionDeclarations":[{"name":"f","parameters":{"type":"object"}}]}],` +
		`"generationConfig":{"maxOutputTokens":64}}}`), Metadata: apix.RequestMetadata{Model: "gem"}},
}

func decode(t *testing.T, body []byte) map[string]any {
	t.Helper()
	var m map[string]any
	if err := json.Unmarshal(body, &m); err != nil {
		t.Fatalf("%v: %s", err, body)
	}
	return m
}

func keys(m map[string]any) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	slices.Sort(out)
	return out
}

func TestRequestDirections(t *testing.T) {
	reg := registry(t)
	want := map[apix.Protocol][]string{
		counttokens.MessagesCountTokens:  {"messages", "model", "system", "thinking", "tools"},
		counttokens.ResponsesInputTokens: {"input", "instructions", "model", "reasoning", "tools"},
	}
	for _, client := range all {
		for _, up := range all {
			t.Run(string(client)+"->"+string(up), func(t *testing.T) {
				ex, err := reg.NewExchange(client, up, apix.ExchangeOptions{})
				if err != nil {
					t.Fatal(err)
				}
				out, err := ex.ConvertRequest(requests[client], "up")
				if err != nil {
					t.Fatal(err)
				}
				body := decode(t, out.Body)
				if up == counttokens.CountTokens {
					if out.Metadata.Model != "up" || out.Metadata.Stream == nil || *out.Metadata.Stream {
						t.Fatalf("metadata %+v", out.Metadata)
					}
					inner, _ := body["generateContentRequest"].(map[string]any)
					if len(body) != 1 || inner["model"] != "models/up" || inner["contents"] == nil || inner["systemInstruction"] == nil || inner["tools"] == nil {
						t.Fatalf("Gemini body %s", out.Body)
					}
					// Generation controls are dropped on converted requests;
					// native bodies keep theirs.
					if client != up && inner["generationConfig"] != nil {
						t.Fatalf("Gemini body keeps generationConfig: %s", out.Body)
					}
					return
				}
				if out.Metadata != (apix.RequestMetadata{}) {
					t.Fatalf("metadata %+v", out.Metadata)
				}
				if body["model"] != "up" {
					t.Fatalf("model %s", out.Body)
				}
				if client == up {
					return // native bodies are checked in TestNativeKeepsBytes
				}
				got := keys(body)
				if up == counttokens.ResponsesInputTokens && client == counttokens.CountTokens {
					// GenerateContent carries no reasoning in this request.
					want := []string{"input", "instructions", "model", "tools"}
					if !slices.Equal(got, want) {
						t.Fatalf("fields %v, want %v: %s", got, want, out.Body)
					}
					return
				}
				if up == counttokens.MessagesCountTokens && client == counttokens.CountTokens {
					want := []string{"messages", "model", "system", "tools"}
					if !slices.Equal(got, want) {
						t.Fatalf("fields %v, want %v: %s", got, want, out.Body)
					}
					return
				}
				if !slices.Equal(got, want[up]) {
					t.Fatalf("fields %v, want %v: %s", got, want[up], out.Body)
				}
			})
		}
	}
}

func TestNativeKeepsBytes(t *testing.T) {
	reg := registry(t)
	for _, p := range []apix.Protocol{counttokens.MessagesCountTokens, counttokens.ResponsesInputTokens} {
		ex, _ := reg.NewExchange(p, p, apix.ExchangeOptions{})
		out, err := ex.ConvertRequest(requests[p], "")
		if err != nil || !bytes.Equal(out.Body, requests[p].Body) {
			t.Fatalf("%s: %v %s", p, err, out.Body)
		}
	}

	ex, _ := reg.NewExchange(counttokens.CountTokens, counttokens.CountTokens, apix.ExchangeOptions{})
	plain := apix.Payload{Body: []byte(`{"contents":[{"parts":[{"text":"hi"}]}]}`), Metadata: apix.RequestMetadata{Model: "gem"}}
	out, err := ex.ConvertRequest(plain, "up")
	if err != nil || !bytes.Equal(out.Body, plain.Body) || out.Metadata.Model != "up" {
		t.Fatalf("contents form: %v %s %+v", err, out.Body, out.Metadata)
	}
	out, err = ex.ConvertRequest(requests[counttokens.CountTokens], "up")
	want := bytes.Replace(requests[counttokens.CountTokens].Body, []byte(`"models/gem"`), []byte(`"models/up"`), 1)
	if err != nil || !bytes.Equal(out.Body, want) {
		t.Fatalf("wrapped form: %v %s", err, out.Body)
	}
	// The URL model is used when the route keeps the client's model.
	out, err = ex.ConvertRequest(plain, "")
	if err != nil || out.Metadata.Model != "gem" {
		t.Fatalf("client model: %v %+v", err, out.Metadata)
	}
}

func TestMessagesOutputConfig(t *testing.T) {
	ex, _ := registry(t).NewExchange(counttokens.ResponsesInputTokens, counttokens.MessagesCountTokens, apix.ExchangeOptions{})
	out, err := ex.ConvertRequest(apix.Payload{Body: []byte(`{"model":"gpt","input":"hi","reasoning":{"effort":"high"},` +
		`"text":{"format":{"type":"json_schema","name":"r","schema":{"type":"object"}}}}`)}, "claude")
	if err != nil {
		t.Fatal(err)
	}
	cfg, _ := decode(t, out.Body)["output_config"].(map[string]any)
	if cfg["format"] == nil || cfg["effort"] != nil {
		t.Fatalf("output_config keeps the format and drops the effort: %s", out.Body)
	}
}

func TestGeminiOutputSchema(t *testing.T) {
	ex, _ := registry(t).NewExchange(counttokens.ResponsesInputTokens, counttokens.CountTokens, apix.ExchangeOptions{})
	out, err := ex.ConvertRequest(apix.Payload{Body: []byte(`{"model":"gpt","input":"hi","max_output_tokens":64,"temperature":0.5,` +
		`"text":{"format":{"type":"json_schema","name":"r","schema":{"type":"object"}}}}`)}, "gem")
	if err != nil {
		t.Fatal(err)
	}
	inner, _ := decode(t, out.Body)["generateContentRequest"].(map[string]any)
	cfg, _ := inner["generationConfig"].(map[string]any)
	for k := range cfg {
		if !strings.HasPrefix(k, "response") {
			t.Fatalf("generationConfig keeps %s: %s", k, out.Body)
		}
	}
	if cfg["responseMimeType"] == nil || (cfg["responseSchema"] == nil && cfg["responseJsonSchema"] == nil) {
		t.Fatalf("generationConfig drops the schema: %s", out.Body)
	}
}

func TestResponseDirections(t *testing.T) {
	reg := registry(t)
	replies := map[apix.Protocol]string{
		counttokens.MessagesCountTokens:  `{"input_tokens":42}`,
		counttokens.ResponsesInputTokens: `{"object":"response.input_tokens","input_tokens":42}`,
		counttokens.CountTokens:          `{"totalTokens":42,"promptTokensDetails":[{"modality":"TEXT","tokenCount":42}]}`,
	}
	for _, client := range all {
		for _, up := range all {
			ex, _ := reg.NewExchange(client, up, apix.ExchangeOptions{})
			out, err := ex.ConvertResponse(200, apix.Payload{Body: []byte(replies[up])})
			if err != nil {
				t.Fatalf("%s->%s: %v", client, up, err)
			}
			if client == up {
				if string(out.Body) != replies[up] {
					t.Fatalf("%s native reply changed: %s", up, out.Body)
				}
				continue
			}
			want := map[apix.Protocol]string{
				counttokens.MessagesCountTokens:  `{"input_tokens":42}`,
				counttokens.ResponsesInputTokens: `{"object":"response.input_tokens","input_tokens":42}`,
				counttokens.CountTokens:          `{"totalTokens":42}`,
			}[client]
			if string(out.Body) != want || out.ContentType != "application/json" {
				t.Fatalf("%s->%s: %s", client, up, out.Body)
			}
		}
	}
}

func TestOutputOption(t *testing.T) {
	ex, err := registry(t).NewExchange(counttokens.MessagesCountTokens, counttokens.MessagesCountTokens, apix.ExchangeOptions{Output: counttokens.CountTokens})
	if err != nil {
		t.Fatal(err)
	}
	out, err := ex.ConvertResponse(200, apix.Payload{Body: []byte(`{"input_tokens":7}`)})
	if err != nil || string(out.Body) != `{"totalTokens":7}` {
		t.Fatalf("%v %s", err, out.Body)
	}
}

func TestGeminiCounts(t *testing.T) {
	ex, _ := registry(t).NewExchange(counttokens.MessagesCountTokens, counttokens.CountTokens, apix.ExchangeOptions{})
	for body, want := range map[string]string{
		`{}`:                      `{"input_tokens":0}`, // proto JSON omits a zero count
		`{"totalTokens":"12"}`:    `{"input_tokens":12}`,
		`{"total_tokens":5}`:      `{"input_tokens":5}`,
		`{"totalTokens":3,"x":1}`: `{"input_tokens":3}`,
	} {
		out, err := ex.ConvertResponse(200, apix.Payload{Body: []byte(body)})
		if err != nil || string(out.Body) != want {
			t.Fatalf("%s: %v %s", body, err, out.Body)
		}
	}
}

func TestHostileResponses(t *testing.T) {
	reg := registry(t)
	for up, bodies := range map[apix.Protocol][]string{
		counttokens.MessagesCountTokens:  {``, `null`, `[]`, `{}`, `{"input_tokens":"x"}`, `{"input_tokens":-1}`, `{"input_tokens":1.5}`, `{"input_tokens":null}`},
		counttokens.ResponsesInputTokens: {`{"object":"response.input_tokens"}`, `{"input_tokens":1e40}`, `{"input_tokens":"12"}`},
		counttokens.CountTokens:          {`null`, `{"totalTokens":-2}`, `{"totalTokens":"many"}`, `{"totalTokens":{}}`},
	} {
		for _, client := range all {
			ex, _ := reg.NewExchange(client, up, apix.ExchangeOptions{})
			for _, body := range bodies {
				_, err := ex.ConvertResponse(200, apix.Payload{Body: []byte(body)})
				if class(err) != apix.ClassUpstream {
					t.Fatalf("%s->%s %q: %v", client, up, body, err)
				}
			}
		}
	}
}

func TestUpstreamErrors(t *testing.T) {
	ex, _ := registry(t).NewExchange(counttokens.CountTokens, counttokens.MessagesCountTokens, apix.ExchangeOptions{})
	_, err := ex.ConvertResponse(429, apix.Payload{Body: []byte(`{"type":"error","error":{"type":"rate_limit_error","message":"slow down"}}`)})
	var e *apix.Error
	if !errors.As(err, &e) || e.Class != apix.ClassRateLimit || e.Message != "slow down" {
		t.Fatalf("%v", err)
	}
}

func TestGeminiRequestForms(t *testing.T) {
	reg := registry(t)
	ex, _ := reg.NewExchange(counttokens.CountTokens, counttokens.MessagesCountTokens, apix.ExchangeOptions{})
	out, err := ex.ConvertRequest(apix.Payload{Body: []byte(`{"generate_content_request":{"model":"models/gem","contents":[{"parts":[{"text":"hi"}]}]}}`)}, "")
	if err != nil || decode(t, out.Body)["model"] != "gem" {
		t.Fatalf("snake_case wrapper and its model: %v %s", err, out.Body)
	}
	for _, body := range []string{
		`not json`, `null`, `{}`, `{"contents":null}`,
		`{"contents":[{"parts":[{"text":"hi"}]}],"generateContentRequest":{"contents":[{"parts":[{"text":"hi"}]}]}}`,
		`{"generateContentRequest":{"contents":[]},"generate_content_request":{"contents":[]}}`,
		`{"generateContentRequest":[]}`,
		`{"generateContentRequest":{"model":1,"contents":[{"parts":[{"text":"hi"}]}]}}`,
		`{"generateContentRequest":{"contents":[]}}`,
	} {
		p := apix.Payload{Body: []byte(body), Metadata: apix.RequestMetadata{Model: "gem"}}
		if _, err := reg.Inspect(counttokens.CountTokens, p); err == nil {
			t.Errorf("Inspect accepted %s", body)
		}
		if _, err := ex.ConvertRequest(p, "up"); err == nil {
			t.Errorf("ConvertRequest accepted %s", body)
		}
	}
	toGemini, _ := reg.NewExchange(counttokens.MessagesCountTokens, counttokens.CountTokens, apix.ExchangeOptions{})
	if _, err := toGemini.ConvertRequest(apix.Payload{Body: []byte(`{"messages":[{"role":"user","content":"hi"}]}`)}, ""); class(err) != apix.ClassTranslation {
		t.Fatalf("a Gemini upstream needs a model: %v", err)
	}
}

func TestSpecAndPath(t *testing.T) {
	reg := registry(t)
	for p, path := range map[apix.Protocol]string{
		counttokens.MessagesCountTokens:  "/v1/messages/count_tokens",
		counttokens.ResponsesInputTokens: "/v1/responses/input_tokens",
		counttokens.CountTokens:          "/v1beta/models/m%2F1:countTokens",
	} {
		spec, _ := reg.Spec(p)
		if spec.Kind != apix.KindCountTokens || spec.Stream != apix.FramingNone || spec.Path(apix.RequestInfo{}, "models/m/1") != path {
			t.Fatalf("%s spec %+v", p, spec)
		}
	}
	spec, _ := reg.Spec(counttokens.MessagesCountTokens)
	if spec.Headers["anthropic-version"] == "" || spec.Auth.Header != "x-api-key" {
		t.Fatalf("Messages spec %+v", spec)
	}
	spec, _ = reg.Spec(counttokens.CountTokens)
	md, err := spec.ParsePath("/v1/models/gem:countTokens")
	if err != nil || md.Model != "gem" || md.Stream == nil || *md.Stream {
		t.Fatalf("%+v %v", md, err)
	}
	for _, path := range []string{"/v1beta/models/gem:generateContent", "/v1beta/models/:countTokens", "/v1beta/models/a/b:countTokens", "/v1beta/gem:countTokens"} {
		if _, err := spec.ParsePath(path); class(err) != apix.ClassUnsupported {
			t.Fatalf("%s: %v", path, err)
		}
	}
	ex, _ := reg.NewExchange(counttokens.CountTokens, counttokens.CountTokens, apix.ExchangeOptions{})
	if _, err := ex.NewStreamConverter(); class(err) != apix.ClassUnsupported {
		t.Fatalf("stream: %v", err)
	}
}

func TestInspect(t *testing.T) {
	reg := registry(t)
	for p, req := range requests {
		info, err := reg.Inspect(p, req)
		if err != nil || info.Kind != apix.KindCountTokens || info.Protocol != p || info.Stream || info.Needs(apix.FeatureStream) || !info.Needs(apix.FeatureTools) {
			t.Fatalf("%s: %+v %v", p, info, err)
		}
	}
	info, _ := reg.Inspect(counttokens.CountTokens, apix.Payload{Body: requests[counttokens.CountTokens].Body})
	if info.Model != "gem" {
		t.Fatalf("model from generateContentRequest: %+v", info)
	}
	info, err := reg.Inspect(counttokens.ResponsesInputTokens, apix.Payload{Body: []byte(`{"model":"gpt","previous_response_id":"resp_1",` +
		`"input":[{"role":"user","content":[{"type":"input_image","image_url":"https://x/y.png"}]}]}`)})
	if err != nil || !info.Stateful || !slices.Contains(info.Affinity, "response:resp_1") || !info.Needs(apix.FeatureImageInput) {
		t.Fatalf("%+v %v", info, err)
	}
}

func TestOptions(t *testing.T) {
	reg := registry(t)
	effort := &apix.Reasoning{Effort: "high"}
	for _, tc := range []struct {
		opts apix.ExchangeOptions
		ok   bool
	}{
		{apix.ExchangeOptions{}, true},
		{apix.ExchangeOptions{LossPolicy: apix.LossPolicyStrict, Output: counttokens.ResponsesInputTokens, Reasoning: effort}, true},
		{apix.ExchangeOptions{LossPolicy: "lossy"}, false},
		{apix.ExchangeOptions{Ext: struct{}{}}, false},
		{apix.ExchangeOptions{Reasoning: &apix.Reasoning{Effort: "extreme"}}, false},
		{apix.ExchangeOptions{Output: "anthropic.messages"}, false},
	} {
		err := reg.ValidateOptions(counttokens.MessagesCountTokens, tc.opts)
		_, nerr := reg.NewExchange(counttokens.CountTokens, counttokens.MessagesCountTokens, tc.opts)
		if (err == nil) != tc.ok || (nerr == nil) != tc.ok {
			t.Fatalf("%+v: %v %v", tc.opts, err, nerr)
		}
	}
}

func TestLossPolicy(t *testing.T) {
	reg := registry(t)
	body := apix.Payload{Body: []byte(`{"model":"gpt","input":"hi","text":{"verbosity":"low"}}`)}
	ex, _ := reg.NewExchange(counttokens.ResponsesInputTokens, counttokens.MessagesCountTokens, apix.ExchangeOptions{})
	if _, err := ex.ConvertRequest(body, ""); err != nil {
		t.Fatal(err)
	}
	d := ex.Diagnostics()
	if len(d) != 1 || d[0].Path != "text.verbosity" || d[0].From != counttokens.ResponsesInputTokens || d[0].To != counttokens.MessagesCountTokens {
		t.Fatalf("%+v", d)
	}
	ex, _ = reg.NewExchange(counttokens.ResponsesInputTokens, counttokens.MessagesCountTokens, apix.ExchangeOptions{LossPolicy: apix.LossPolicyStrict})
	if _, err := ex.ConvertRequest(body, ""); err == nil {
		t.Fatal("strict policy accepted a lossy request")
	}
}

func class(err error) apix.Class {
	if e := apix.AsError(err); e != nil {
		return e.Class
	}
	return ""
}
