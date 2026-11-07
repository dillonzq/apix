package reasoning_test

import (
	"strings"
	"testing"

	"github.com/dillonzq/apix/protocol/anthropic"
	"github.com/dillonzq/apix/protocol/google"
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
		{"google", func() ([]byte, error) { return reasoning.ApplySuffix(google.GenerateContent, []byte(`{}`), high) }, `"thinkingLevel":"high"`},
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
	googleBody := []byte(`{"contents":[],"generationConfig":{"thinkingConfig":{"thinkingLevel":"low","includeThoughts":true}}}`)
	if got := reasoning.StripSupersededReasoning(openai.ChatCompletions, google.GenerateContent, googleBody); string(got) != `{"contents":[],"generationConfig":{"thinkingConfig":{"includeThoughts":true}}}` {
		t.Fatalf("strip google: %s", got)
	}
	if got := reasoning.StripSupersededReasoning(google.GenerateContent, openai.Responses, []byte(`{"reasoning":{"effort":"low","summary":"auto"}}`)); string(got) != `{"reasoning":{"summary":"auto"}}` {
		t.Fatalf("strip responses: %s", got)
	}
	if got := reasoning.StripSupersededReasoning(google.Interactions, openai.ChatCompletions, body); string(got) != string(body) {
		t.Fatalf("strip without a reasoning path: %s", got)
	}
	if got, err := reasoning.ApplySuffix(google.Interactions, body, high); err != nil || string(got) != string(body) {
		t.Fatalf("apply without a reasoning control: %s %v", got, err)
	}
}

// Gemini suffixes follow the exchange's ThinkingMode and never send a level
// GenerateContent does not define.
func TestGoogleModes(t *testing.T) {
	for _, tc := range []struct {
		model string
		mode  google.ThinkingMode
		want  string
	}{
		{"m(high)", "", `{"generationConfig":{"thinkingConfig":{"includeThoughts":true,"thinkingLevel":"high"}}}`},
		{"m(max)", "", `{"generationConfig":{"thinkingConfig":{"includeThoughts":true,"thinkingLevel":"high"}}}`},
		{"m(3000)", "", `{"generationConfig":{"thinkingConfig":{"includeThoughts":true,"thinkingBudget":3000}}}`},
		{"m(high)", google.ThinkingBudget, `{"generationConfig":{"thinkingConfig":{"includeThoughts":true,"thinkingBudget":24576}}}`},
		{"m(max)", google.ThinkingBudget, `{"generationConfig":{"thinkingConfig":{"includeThoughts":true,"thinkingBudget":128000}}}`},
		{"m(3000)", google.ThinkingLevel, `{"generationConfig":{"thinkingConfig":{"includeThoughts":true,"thinkingLevel":"medium"}}}`},
		{"m(none)", google.ThinkingLevel, `{"generationConfig":{"thinkingConfig":{"includeThoughts":true,"thinkingBudget":0}}}`},
		{"m(auto)", google.ThinkingLevel, `{"generationConfig":{"thinkingConfig":{"includeThoughts":true,"thinkingBudget":-1}}}`},
	} {
		body := []byte(`{"generationConfig":{"thinkingConfig":{"includeThoughts":true,"thinkingLevel":"low"}}}`)
		got, err := reasoning.ApplySuffixWith(google.GenerateContent, body, reasoning.ParseSuffix(tc.model), reasoning.Options{GoogleMode: tc.mode})
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
