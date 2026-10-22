package gateway_test

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/http/httptest"
	"slices"

	"github.com/dillonzq/apix"
	"github.com/dillonzq/apix/gateway"
	"github.com/dillonzq/apix/protocol/anthropic"
	"github.com/dillonzq/apix/protocol/openai"
	"github.com/dillonzq/apix/route"
)

// A Messages client is served by an OpenAI Chat Completions upstream; here a
// local server stands in for the provider.
func Example() {
	provider := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"id":"c1","object":"chat.completion","model":"gpt-4.1","choices":[{"index":0,"message":{"role":"assistant","content":"Hi!"},"finish_reason":"stop"}]}`)
	}))

	reg, err := apix.NewRegistry(slices.Concat(openai.Codecs(), anthropic.Codecs())...)
	if err != nil {
		log.Fatal(err)
	}
	g, err := gateway.New(gateway.Config{
		Registry: reg,
		Upstreams: []gateway.Upstream{{
			Endpoint: route.Endpoint{ID: "openai", Protocol: openai.ChatCompletions, Models: map[string]string{"claude-sonnet-4-5": "gpt-4.1"}},
			BaseURL:  provider.URL,
			APIKey:   "sk-test",
		}},
	})
	if err != nil {
		log.Fatal(err)
	}
	// g.Handler(anthropic.Messages) serves the same flow over HTTP.
	resp, aerr := g.Do(context.Background(), gateway.Request{
		Protocol: anthropic.Messages,
		Payload:  apix.Payload{Body: []byte(`{"model":"claude-sonnet-4-5","max_tokens":64,"messages":[{"role":"user","content":"Hello"}]}`)},
	})
	if aerr != nil {
		log.Fatal(aerr)
	}
	var msg struct {
		Type    string
		Content []struct{ Text string }
	}
	err = json.NewDecoder(resp.Body).Decode(&msg)
	_ = resp.Body.Close()
	provider.Close()
	if err != nil {
		log.Fatal(err)
	}
	fmt.Println(resp.Endpoint, msg.Type, msg.Content[0].Text)
	// Output: openai message Hi!
}
