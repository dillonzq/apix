package codec_test

import (
	"encoding/json"
	"reflect"
	"strings"
	"testing"

	"github.com/dillonzq/apix"
	"github.com/dillonzq/apix/protocol/anthropic"
	"github.com/dillonzq/apix/protocol/google"
	"github.com/dillonzq/apix/protocol/openai"
)

func usageOf(t *testing.T, ex apix.Exchange) (apix.Usage, bool) {
	t.Helper()
	ur, ok := ex.(apix.UsageReporter)
	if !ok {
		t.Fatalf("%T does not report usage", ex)
	}
	u, ok := ur.Usage()
	u.Raw = nil
	return u, ok
}

// Every direction reports the upstream's own usage, read from the upstream
// response rather than the converted one.
func TestExchangeUsage(t *testing.T) {
	reg := registry(t)
	want := apix.Usage{InputTokens: 4, OutputTokens: 2, TotalTokens: 6}
	streams := map[apix.Protocol]string{
		openai.ChatCompletions: strings.Replace(fixtures[openai.ChatCompletions].stream, "data: [DONE]", "data: {\"choices\":[],\"usage\":{\"prompt_tokens\":4,\"completion_tokens\":2}}\n\ndata: [DONE]", 1),
		openai.Responses:       strings.Replace(fixtures[openai.Responses].stream, `"output":[]}`, `"output":[],"usage":{"input_tokens":4,"output_tokens":2}}`, 1),
		google.GenerateContent: strings.Replace(fixtures[google.GenerateContent].stream, `"finishReason":"STOP"}]}`, `"finishReason":"STOP"}],"usageMetadata":{"promptTokenCount":4,"candidatesTokenCount":2}}`, 1),
		anthropic.Messages: strings.NewReplacer(`"model":"m"}}`, `"model":"m","usage":{"input_tokens":4,"output_tokens":1}}}`,
			`"end_turn"}}`, `"end_turn"},"usage":{"output_tokens":2}}`).Replace(fixtures[anthropic.Messages].stream),
	}
	for upstream := range fixtures {
		if streams[upstream] == fixtures[upstream].stream {
			t.Fatalf("%s stream fixture changed; usage was not added", upstream)
		}
		for client := range fixtures {
			ex, err := reg.NewExchange(client, upstream, apix.ExchangeOptions{})
			if err != nil {
				t.Fatal(err)
			}
			if _, ok := usageOf(t, ex); ok {
				t.Fatalf("%s -> %s: usage before a response", client, upstream)
			}
			if _, err := ex.ConvertResponse(200, apix.Payload{Body: []byte(fixtures[upstream].response)}); err != nil {
				t.Fatalf("%s -> %s: %v", client, upstream, err)
			}
			if got, ok := usageOf(t, ex); !ok || !reflect.DeepEqual(got, want) {
				t.Fatalf("%s -> %s response usage %+v %v", client, upstream, got, ok)
			}

			ex, _ = reg.NewExchange(client, upstream, apix.ExchangeOptions{})
			sc, err := ex.NewStreamConverter()
			if err != nil {
				t.Fatal(err)
			}
			drain(t, sc, streams[upstream])
			if got, ok := usageOf(t, ex); !ok || !reflect.DeepEqual(got, want) {
				t.Fatalf("%s -> %s stream usage %+v %v", client, upstream, got, ok)
			}
		}
	}
}

func TestErrorResponseReportsNoUsage(t *testing.T) {
	ex, err := registry(t).NewExchange(anthropic.Messages, openai.ChatCompletions, apix.ExchangeOptions{})
	if err != nil {
		t.Fatal(err)
	}
	_, _ = ex.ConvertResponse(429, apix.Payload{Body: []byte(`{"error":{"message":"slow down"},"usage":{"prompt_tokens":4}}`)})
	if u, ok := usageOf(t, ex); ok {
		t.Fatalf("usage %+v", u)
	}
}

func TestInteractionsUsage(t *testing.T) {
	reg, err := apix.NewRegistry(google.InteractionsCodec())
	if err != nil {
		t.Fatal(err)
	}
	ex, err := reg.NewExchange(google.Interactions, google.Interactions, apix.ExchangeOptions{})
	if err != nil {
		t.Fatal(err)
	}
	sc, err := ex.NewStreamConverter()
	if err != nil {
		t.Fatal(err)
	}
	drain(t, sc, "event: interaction.created\ndata: {\"event_type\":\"interaction.created\",\"interaction\":{\"id\":\"i\",\"status\":\"in_progress\"}}\n\n"+
		"event: interaction.completed\ndata: {\"event_type\":\"interaction.completed\",\"interaction\":{\"id\":\"i\",\"status\":\"completed\",\"usage\":{\"total_input_tokens\":4,\"total_output_tokens\":2,\"total_tokens\":6}}}\n\n")
	if got, ok := usageOf(t, ex); !ok || !reflect.DeepEqual(got, apix.Usage{InputTokens: 4, OutputTokens: 2, TotalTokens: 6}) {
		t.Fatalf("usage %+v %v", got, ok)
	}
}

func TestChatStreamUsageOption(t *testing.T) {
	reg := registry(t)
	if err := reg.ValidateOptions(openai.ChatCompletions, apix.ExchangeOptions{Ext: &openai.ChatCompletionsOptions{StreamUsage: true}}); err != nil {
		t.Fatal(err)
	}
	if err := reg.ValidateOptions(openai.ChatCompletions, apix.ExchangeOptions{Ext: openai.ResponsesOptions{}}); err == nil {
		t.Fatal("accepted Responses options")
	}
	streaming := true
	tests := []struct {
		name   string
		client apix.Protocol
		body   string
		meta   apix.RequestMetadata
		want   string // stream_options after conversion; empty means absent
	}{
		{"cross-protocol stream", anthropic.Messages, `{"model":"m","max_tokens":5,"stream":true,"messages":[{"role":"user","content":"hi"}]}`, apix.RequestMetadata{}, `{"include_usage":true}`},
		{"native stream keeps other options", openai.ChatCompletions, `{"model":"m","stream":true,"stream_options":{"include_obfuscation":false},"messages":[{"role":"user","content":"hi"}]}`, apix.RequestMetadata{}, `{"include_obfuscation":false,"include_usage":true}`},
		{"URL-borne stream", google.GenerateContent, `{"contents":[{"role":"user","parts":[{"text":"hi"}]}]}`, apix.RequestMetadata{Model: "m", Stream: &streaming}, `{"include_usage":true}`},
		{"ordinary request", openai.ChatCompletions, `{"model":"m","messages":[{"role":"user","content":"hi"}]}`, apix.RequestMetadata{}, ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			ex, err := reg.NewExchange(tt.client, openai.ChatCompletions, apix.ExchangeOptions{Ext: openai.ChatCompletionsOptions{StreamUsage: true}})
			if err != nil {
				t.Fatal(err)
			}
			out, err := ex.ConvertRequest(apix.Payload{Body: []byte(tt.body), Metadata: tt.meta}, "")
			if err != nil {
				t.Fatal(err)
			}
			var doc struct {
				StreamOptions json.RawMessage `json:"stream_options"`
			}
			if err := json.Unmarshal(out.Body, &doc); err != nil {
				t.Fatal(err)
			}
			if string(doc.StreamOptions) != tt.want {
				t.Fatalf("stream_options %s in %s", doc.StreamOptions, out.Body)
			}
		})
	}
	// Without the option, streamed requests are unchanged.
	ex, _ := reg.NewExchange(anthropic.Messages, openai.ChatCompletions, apix.ExchangeOptions{})
	out, err := ex.ConvertRequest(apix.Payload{Body: []byte(tests[0].body)}, "")
	if err != nil || strings.Contains(string(out.Body), "stream_options") {
		t.Fatalf("default request %s, %v", out.Body, err)
	}
}
