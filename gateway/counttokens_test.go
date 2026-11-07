package gateway_test

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"testing"

	"github.com/dillonzq/apix"
	"github.com/dillonzq/apix/gateway"
	"github.com/dillonzq/apix/protocol/anthropic"
	"github.com/dillonzq/apix/protocol/google"
	"github.com/dillonzq/apix/protocol/openai"
	"github.com/dillonzq/apix/route"
)

func TestCountTokensGateway(t *testing.T) {
	reg, err := apix.NewRegistry(slices.Concat(openai.Codecs(), anthropic.Codecs(), google.Codecs())...)
	if err != nil {
		t.Fatal(err)
	}
	gem := newUpstream(t, respond(200, "application/json", `{"totalTokens":17,"promptTokensDetails":[{"modality":"TEXT","tokenCount":17}]}`))
	oai := newUpstream(t, respond(200, "application/json", `{"object":"response.input_tokens","input_tokens":9}`))
	// A generation endpoint of another Kind serves the same models and must
	// never receive a count.
	llm := newUpstream(t, respond(200, "application/json", `{}`))
	gw, err := gateway.New(gateway.Config{Registry: reg, Upstreams: []gateway.Upstream{
		{Endpoint: route.Endpoint{ID: "llm", Protocol: anthropic.Messages}, BaseURL: llm.URL},
		{Endpoint: route.Endpoint{ID: "google", Protocol: google.CountTokens, Models: map[string]string{"public": "gem-up"}}, BaseURL: gem.URL, APIKey: "gem-key"},
		{Endpoint: route.Endpoint{ID: "openai", Protocol: openai.ResponsesInputTokens, Models: map[string]string{"gpt": ""}}, BaseURL: oai.URL, APIKey: "oai-key"},
	}})
	if err != nil {
		t.Fatal(err)
	}
	mux := http.NewServeMux()
	mux.Handle("/v1/messages/count_tokens", gw.Handler(anthropic.MessagesCountTokens))
	mux.Handle("/v1beta/models/", gw.Handler(google.CountTokens))
	srv := httptest.NewServer(mux)
	defer srv.Close()
	post := func(path, body string) (int, string) {
		t.Helper()
		resp, err := http.Post(srv.URL+path, "application/json", strings.NewReader(body))
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		out, _ := io.ReadAll(resp.Body)
		return resp.StatusCode, string(out)
	}

	status, body := post("/v1/messages/count_tokens", `{"model":"public","system":"be brief","messages":[{"role":"user","content":"hi"}]}`)
	if status != 200 || body != `{"input_tokens":17}` {
		t.Fatalf("Messages via Gemini: %d %s", status, body)
	}
	got := gem.last()
	inner, _ := got.body["generateContentRequest"].(map[string]any)
	if got.path != "/v1beta/models/gem-up:countTokens" || got.header.Get("X-Goog-Api-Key") != "gem-key" || inner["model"] != "models/gem-up" || inner["systemInstruction"] == nil {
		t.Fatalf("Gemini request %+v", got)
	}

	status, body = post("/v1beta/models/gpt:countTokens", `{"contents":[{"role":"user","parts":[{"text":"hi"}]}]}`)
	if status != 200 || body != `{"totalTokens":9}` {
		t.Fatalf("Gemini via Responses: %d %s", status, body)
	}
	got = oai.last()
	if got.path != "/v1/responses/input_tokens" || got.header.Get("Authorization") != "Bearer oai-key" || got.body["model"] != "gpt" || got.body["input"] == nil {
		t.Fatalf("Responses request %+v", got)
	}

	status, body = post("/v1beta/models/gpt:generateContent", `{"contents":[{"role":"user","parts":[{"text":"hi"}]}]}`)
	if status != 400 || !strings.Contains(body, "INVALID_ARGUMENT") {
		t.Fatalf("generateContent on the count handler: %d %s", status, body)
	}

	status, body = post("/v1/messages/count_tokens", `{"model":"unknown","messages":[{"role":"user","content":"hi"}]}`)
	var envelope struct {
		Type  string
		Error struct{ Type string }
	}
	if status != 404 || json.Unmarshal([]byte(body), &envelope) != nil || envelope.Type != "error" || envelope.Error.Type != "not_found_error" {
		t.Fatalf("unknown model: %d %s", status, body)
	}
	if len(llm.requests) != 0 {
		t.Fatalf("a count reached the generation endpoint: %+v", llm.requests)
	}
}
