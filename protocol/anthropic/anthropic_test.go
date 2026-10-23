package anthropic_test

import (
	"encoding/json"
	"slices"
	"testing"

	"github.com/dillonzq/apix"
	"github.com/dillonzq/apix/protocol/anthropic"
	"github.com/dillonzq/apix/protocol/gemini"
	"github.com/dillonzq/apix/protocol/openai"
)

func registry(t *testing.T) *apix.Registry {
	t.Helper()
	r, err := apix.NewRegistry(slices.Concat(openai.Codecs(), anthropic.Codecs(), gemini.Codecs())...)
	if err != nil {
		t.Fatal(err)
	}
	return r
}

func document(t *testing.T, body []byte) map[string]any {
	t.Helper()
	var out map[string]any
	if err := json.Unmarshal(body, &out); err != nil {
		t.Fatal(err)
	}
	return out
}

// Cross-protocol reasoning levels default to adaptive thinking with effort.
func TestAdaptiveReasoningDefault(t *testing.T) {
	for _, tc := range []struct {
		name         string
		protocol     apix.Protocol
		body, effort string
	}{
		{"chat", openai.ChatCompletions, `{"messages":[{"role":"user","content":"hi"}],"reasoning_effort":"high","response_format":{"type":"json_schema","json_schema":{"name":"answer","schema":{"type":"object"}}}}`, "high"},
		{"responses", openai.Responses, `{"input":"hi","reasoning":{"effort":"high"},"text":{"format":{"type":"json_schema","schema":{"type":"object"}}}}`, "high"},
		{"gemini level", gemini.GenerateContent, `{"contents":[{"parts":[{"text":"hi"}]}],"generationConfig":{"thinkingConfig":{"thinkingLevel":"high"},"responseMimeType":"application/json","responseJsonSchema":{"type":"object"}}}`, "high"},
		{"gemini budget", gemini.GenerateContent, `{"contents":[{"parts":[{"text":"hi"}]}],"generationConfig":{"thinkingConfig":{"thinkingBudget":8192},"responseMimeType":"application/json","responseJsonSchema":{"type":"object"}}}`, "medium"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ex, err := registry(t).NewExchange(tc.protocol, anthropic.Messages, apix.ExchangeOptions{})
			if err != nil {
				t.Fatal(err)
			}
			out, err := ex.ConvertRequest(apix.Payload{Body: []byte(tc.body)}, "deployment-alias")
			if err != nil {
				t.Fatal(err)
			}
			doc := document(t, out.Body)
			th := doc["thinking"].(map[string]any)
			if th["type"] != "adaptive" || th["budget_tokens"] != nil {
				t.Fatalf("new Claude request uses manual thinking: %s", out.Body)
			}
			cfg := doc["output_config"].(map[string]any)
			if cfg["effort"] != tc.effort || cfg["format"] == nil {
				t.Fatalf("effort or schema lost: %s", out.Body)
			}
			if doc["max_tokens"] != float64(4096) {
				t.Fatal("rewrote output limit")
			}
		})
	}
}

// Manual mode keeps budget thinking for Claude models without adaptive support.
func TestManualReasoningOption(t *testing.T) {
	for _, tc := range []struct {
		name     string
		protocol apix.Protocol
		body     string
		budget   float64
	}{
		{"chat", openai.ChatCompletions, `{"messages":[{"role":"user","content":"hi"}],"reasoning_effort":"high"}`, 24576},
		{"gemini level", gemini.GenerateContent, `{"contents":[{"parts":[{"text":"hi"}]}],"generationConfig":{"thinkingConfig":{"thinkingLevel":"low"}}}`, 1024},
		{"gemini budget", gemini.GenerateContent, `{"contents":[{"parts":[{"text":"hi"}]}],"generationConfig":{"thinkingConfig":{"thinkingBudget":3000}}}`, 3000},
	} {
		t.Run(tc.name, func(t *testing.T) {
			opts := apix.ExchangeOptions{Ext: &anthropic.MessagesOptions{ThinkingMode: anthropic.ThinkingManual}}
			ex, err := registry(t).NewExchange(tc.protocol, anthropic.Messages, opts)
			if err != nil {
				t.Fatal(err)
			}
			out, err := ex.ConvertRequest(apix.Payload{Body: []byte(tc.body)}, "claude-legacy")
			if err != nil {
				t.Fatal(err)
			}
			doc := document(t, out.Body)
			th := doc["thinking"].(map[string]any)
			if th["type"] != "enabled" || th["budget_tokens"] != tc.budget || doc["output_config"] != nil {
				t.Fatalf("manual thinking not applied: %s", out.Body)
			}
		})
	}
	opts := apix.ExchangeOptions{Ext: anthropic.MessagesOptions{ThinkingMode: "fixed"}}
	if _, err := registry(t).NewExchange(openai.ChatCompletions, anthropic.Messages, opts); err == nil {
		t.Fatal("unknown thinking mode accepted")
	}
}
