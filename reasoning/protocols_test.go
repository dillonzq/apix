package reasoning_test

import (
	"strings"
	"testing"

	"github.com/dillonzq/apix/protocol/anthropic"
	"github.com/dillonzq/apix/protocol/gemini"
	"github.com/dillonzq/apix/protocol/openai"
	"github.com/dillonzq/apix/reasoning"
)

func TestVendorProtocols(t *testing.T) {
	high := reasoning.ParseSuffix("m(high)")
	for _, tc := range []struct {
		name string
		got  func() ([]byte, error)
		want string
	}{
		{"chat", func() ([]byte, error) { return reasoning.ApplySuffix(openai.ChatCompletions, []byte(`{}`), high) }, `"reasoning_effort":"high"`},
		{"chat auto", func() ([]byte, error) {
			return reasoning.ApplySuffix(openai.ChatCompletions, []byte(`{"reasoning_effort":"low"}`), reasoning.ParseSuffix("m(auto)"))
		}, `{}`},
		{"responses", func() ([]byte, error) { return reasoning.ApplySuffix(openai.Responses, []byte(`{}`), high) }, `"effort":"high"`},
		{"messages", func() ([]byte, error) { return reasoning.ApplySuffix(anthropic.Messages, []byte(`{}`), high) }, `"thinking"`},
		{"gemini", func() ([]byte, error) { return reasoning.ApplySuffix(gemini.GenerateContent, []byte(`{}`), high) }, `"thinkingLevel":"high"`},
	} {
		got, err := tc.got()
		if err != nil || !strings.Contains(string(got), tc.want) {
			t.Fatalf("%s: %s %v", tc.name, got, err)
		}
	}
	body := []byte(`{"reasoning_effort":"low"}`)
	if got := reasoning.StripSupersededReasoning(anthropic.Messages, openai.ChatCompletions, body); strings.Contains(string(got), "reasoning_effort") {
		t.Fatalf("strip: %s", got)
	}
	geminiBody := []byte(`{"contents":[],"generationConfig":{"thinkingConfig":{"thinkingLevel":"low","includeThoughts":true}}}`)
	if got := reasoning.StripSupersededReasoning(openai.ChatCompletions, gemini.GenerateContent, geminiBody); string(got) != `{"contents":[],"generationConfig":{"thinkingConfig":{"includeThoughts":true}}}` {
		t.Fatalf("strip gemini: %s", got)
	}
	if got := reasoning.StripSupersededReasoning(gemini.GenerateContent, openai.Responses, []byte(`{"reasoning":{"effort":"low","summary":"auto"}}`)); string(got) != `{"reasoning":{"summary":"auto"}}` {
		t.Fatalf("strip responses: %s", got)
	}
	if got := reasoning.StripSupersededReasoning(gemini.Interactions, openai.ChatCompletions, body); string(got) != string(body) {
		t.Fatalf("strip without a reasoning path: %s", got)
	}
	if got, err := reasoning.ApplySuffix(gemini.Interactions, body, high); err != nil || string(got) != string(body) {
		t.Fatalf("apply without a reasoning control: %s %v", got, err)
	}
}

// Gemini suffixes follow the exchange's ThinkingMode and never send a level
// GenerateContent does not define.
func TestGeminiModes(t *testing.T) {
	for _, tc := range []struct {
		model string
		mode  gemini.ThinkingMode
		want  string
	}{
		{"m(high)", "", `{"generationConfig":{"thinkingConfig":{"includeThoughts":true,"thinkingLevel":"high"}}}`},
		{"m(max)", "", `{"generationConfig":{"thinkingConfig":{"includeThoughts":true,"thinkingLevel":"high"}}}`},
		{"m(3000)", "", `{"generationConfig":{"thinkingConfig":{"includeThoughts":true,"thinkingBudget":3000}}}`},
		{"m(high)", gemini.ThinkingBudget, `{"generationConfig":{"thinkingConfig":{"includeThoughts":true,"thinkingBudget":24576}}}`},
		{"m(max)", gemini.ThinkingBudget, `{"generationConfig":{"thinkingConfig":{"includeThoughts":true,"thinkingBudget":128000}}}`},
		{"m(3000)", gemini.ThinkingLevel, `{"generationConfig":{"thinkingConfig":{"includeThoughts":true,"thinkingLevel":"medium"}}}`},
		{"m(none)", gemini.ThinkingLevel, `{"generationConfig":{"thinkingConfig":{"includeThoughts":true,"thinkingBudget":0}}}`},
		{"m(auto)", gemini.ThinkingLevel, `{"generationConfig":{"thinkingConfig":{"includeThoughts":true,"thinkingBudget":-1}}}`},
	} {
		body := []byte(`{"generationConfig":{"thinkingConfig":{"includeThoughts":true,"thinkingLevel":"low"}}}`)
		got, err := reasoning.ApplySuffixWith(gemini.GenerateContent, body, reasoning.ParseSuffix(tc.model), reasoning.Options{GeminiMode: tc.mode})
		if err != nil || string(got) != tc.want {
			t.Errorf("%s mode %q = %s %v, want %s", tc.model, tc.mode, got, err, tc.want)
		}
	}
}

func TestSuffixReasoning(t *testing.T) {
	if r := reasoning.ParseSuffix("m").Reasoning(); r != nil {
		t.Fatalf("no suffix = %+v", r)
	}
	if r := reasoning.ParseSuffix("m(turbo)").Reasoning(); r != nil {
		t.Fatalf("unrecognized suffix = %+v", r)
	}
	r := reasoning.ParseSuffix("m(3000)").Reasoning()
	if r == nil || r.Effort != "medium" || r.Budget == nil || *r.Budget != 3000 {
		t.Fatalf("numeric suffix = %+v", r)
	}
}
