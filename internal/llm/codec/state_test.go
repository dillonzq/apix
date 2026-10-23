package codec_test

import (
	"slices"
	"strings"
	"testing"

	"github.com/dillonzq/apix"
	"github.com/dillonzq/apix/protocol/anthropic"
	"github.com/dillonzq/apix/protocol/gemini"
	"github.com/dillonzq/apix/protocol/openai"
)

func fullRegistry(t *testing.T) *apix.Registry {
	t.Helper()
	reg, err := apix.NewRegistry(slices.Concat(openai.Codecs(), anthropic.Codecs(), gemini.Codecs())...)
	if err != nil {
		t.Fatal(err)
	}
	return reg
}

func TestInspectStateReferences(t *testing.T) {
	reg := fullRegistry(t)
	for _, tc := range []struct {
		protocol apix.Protocol
		body     string
		affinity []string
		stateful bool
	}{
		{openai.Responses, `{"model":"m","input":"hi","previous_response_id":"resp_1"}`, []string{"response:resp_1"}, true},
		{openai.Responses, `{"model":"m","input":"hi","conversation":{"id":"conv_1"}}`, []string{"conversation:conv_1"}, true},
		{openai.Responses, `{"model":"m","input":[{"type":"item_reference","id":"msg_1"}]}`, []string{"item:msg_1"}, true},
		{openai.Responses, `{"model":"m","input":[{"role":"user","content":[{"type":"input_file","file_id":"file-1"},{"type":"input_image","file_id":"file-2"}]}]}`, []string{"file:file-1", "file:file-2"}, false},
		{openai.Responses, `{"model":"m","input":"hi","previous_response_id":null}`, nil, false},
		{openai.ChatCompletions, `{"model":"m","messages":[{"role":"user","content":[{"type":"file","file":{"file_id":"file-1"}}]}]}`, []string{"file:file-1"}, false},
		{anthropic.Messages, `{"model":"m","messages":[{"role":"user","content":[{"type":"document","source":{"type":"file","file_id":"file_1"}}]}]}`, []string{"file:file_1"}, false},
		{anthropic.Messages, `{"model":"m","container":"ctr_1","messages":[]}`, []string{"container:ctr_1"}, true},
		{gemini.GenerateContent, `{"cachedContent":"cachedContents/c1","contents":[{"role":"user","parts":[{"fileData":{"mimeType":"application/pdf","fileUri":"https://generativelanguage.googleapis.com/v1beta/files/f1"}},{"fileData":{"mimeType":"video/mp4","fileUri":"https://youtu.be/x"}}]}]}`,
			[]string{"cached-content:cachedContents/c1", "file:https://generativelanguage.googleapis.com/v1beta/files/f1"}, true},
		{gemini.Interactions, `{"model":"m","input":"hi","previous_interaction_id":"i1"}`, []string{"interaction:i1"}, true},
	} {
		info, err := reg.Inspect(tc.protocol, apix.Payload{Body: []byte(tc.body)})
		if err != nil || !slices.Equal(info.Affinity, tc.affinity) || info.Stateful != tc.stateful {
			t.Errorf("%s %s: affinity %v stateful %v err %v", tc.protocol, tc.body, info.Affinity, info.Stateful, err)
		}
	}
}

func TestCreatedState(t *testing.T) {
	reg := fullRegistry(t)
	created := func(ex apix.Exchange) []string {
		sr, ok := ex.(apix.StateReporter)
		if !ok {
			t.Fatalf("%T does not report state", ex)
		}
		return sr.CreatedState()
	}
	native, _ := reg.NewExchange(openai.Responses, openai.Responses, apix.ExchangeOptions{})
	if _, err := native.ConvertResponse(200, apix.Payload{Body: []byte(fixtures[openai.Responses].response)}); err != nil {
		t.Fatal(err)
	}
	if got := created(native); !slices.Equal(got, []string{"response:r"}) {
		t.Fatalf("native response state %v", got)
	}
	streamed, _ := reg.NewExchange(openai.Responses, openai.Responses, apix.ExchangeOptions{})
	sc, _ := streamed.NewStreamConverter()
	drain(t, sc, fixtures[openai.Responses].stream)
	if got := created(streamed); !slices.Equal(got, []string{"response:r"}) {
		t.Fatalf("native stream state %v", got)
	}
	// A synthesized Responses ID refers to nothing an upstream stored.
	converted, _ := reg.NewExchange(openai.Responses, openai.ChatCompletions, apix.ExchangeOptions{})
	if _, err := converted.ConvertResponse(200, apix.Payload{Body: []byte(fixtures[openai.ChatCompletions].response)}); err != nil {
		t.Fatal(err)
	}
	if got := created(converted); got != nil {
		t.Fatalf("converted state %v", got)
	}
	interaction, _ := reg.NewExchange(gemini.Interactions, gemini.Interactions, apix.ExchangeOptions{})
	if _, err := interaction.ConvertResponse(200, apix.Payload{Body: []byte(`{"id":"i1","status":"completed"}`)}); err != nil {
		t.Fatal(err)
	}
	if got := created(interaction); !slices.Equal(got, []string{"interaction:i1"}) {
		t.Fatalf("interaction state %v", got)
	}
	interaction, _ = reg.NewExchange(gemini.Interactions, gemini.Interactions, apix.ExchangeOptions{})
	sc, _ = interaction.NewStreamConverter()
	drain(t, sc, "event: interaction.created\ndata: {\"event_type\":\"interaction.created\",\"interaction\":{\"id\":\"i2\",\"status\":\"in_progress\"}}\n\nevent: interaction.completed\ndata: {\"event_type\":\"interaction.completed\",\"interaction\":{\"id\":\"i2\",\"status\":\"completed\"}}\n\n")
	if got := created(interaction); !slices.Equal(got, []string{"interaction:i2"}) {
		t.Fatalf("interaction stream state %v", got)
	}
}

func TestReasoningOverride(t *testing.T) {
	reg := fullRegistry(t)
	client := apix.Payload{Body: []byte(`{"model":"m","messages":[{"role":"user","content":"hi"}],"reasoning_effort":"low"}`)}
	for _, tc := range []struct {
		name     string
		upstream apix.Protocol
		opts     apix.ExchangeOptions
		want     string
	}{
		{"native chat", openai.ChatCompletions, apix.ExchangeOptions{Reasoning: &apix.Reasoning{Effort: "high"}}, `"reasoning_effort":"high"`},
		{"responses", openai.Responses, apix.ExchangeOptions{Reasoning: &apix.Reasoning{Effort: "high"}}, `"effort":"high"`},
		{"messages adaptive", anthropic.Messages, apix.ExchangeOptions{Reasoning: &apix.Reasoning{Effort: "high"}}, `"effort":"high"`},
		{"messages manual", anthropic.Messages, apix.ExchangeOptions{Reasoning: &apix.Reasoning{Effort: "high"}, Ext: anthropic.MessagesOptions{ThinkingMode: anthropic.ThinkingManual}}, `"budget_tokens":24576`},
		{"gemini budget", gemini.GenerateContent, apix.ExchangeOptions{Reasoning: &apix.Reasoning{Effort: "max"}, Ext: gemini.GenerateContentOptions{ThinkingMode: gemini.ThinkingBudget}}, `"thinkingBudget":128000`},
		{"gemini level", gemini.GenerateContent, apix.ExchangeOptions{Reasoning: &apix.Reasoning{Budget: ptr(3000)}, Ext: gemini.GenerateContentOptions{ThinkingMode: gemini.ThinkingLevel}}, `"thinkingLevel":"medium"`},
	} {
		ex, err := reg.NewExchange(openai.ChatCompletions, tc.upstream, tc.opts)
		if err != nil {
			t.Fatalf("%s: %v", tc.name, err)
		}
		out, err := ex.ConvertRequest(client, "up")
		if err != nil || !strings.Contains(string(out.Body), tc.want) || strings.Contains(string(out.Body), `"low"`) {
			t.Errorf("%s: %s %v", tc.name, out.Body, err)
		}
	}
	for _, r := range []apix.Reasoning{{}, {Effort: "turbo"}, {Budget: ptr(-2)}} {
		if _, err := reg.NewExchange(openai.ChatCompletions, anthropic.Messages, apix.ExchangeOptions{Reasoning: &r}); err == nil {
			t.Errorf("accepted reasoning %+v", r)
		}
	}
	if _, err := reg.NewExchange(gemini.Interactions, gemini.Interactions, apix.ExchangeOptions{Reasoning: &apix.Reasoning{Effort: "high"}}); err == nil {
		t.Error("Interactions accepted a reasoning override")
	}
}

func ptr(v int64) *int64 { return &v }

func TestValidateOptions(t *testing.T) {
	reg := fullRegistry(t)
	for _, tc := range []struct {
		upstream apix.Protocol
		opts     apix.ExchangeOptions
		ok       bool
	}{
		{anthropic.Messages, apix.ExchangeOptions{Ext: anthropic.MessagesOptions{ThinkingMode: anthropic.ThinkingManual}}, true},
		{anthropic.Messages, apix.ExchangeOptions{Ext: anthropic.MessagesOptions{ThinkingMode: "budget"}}, false},
		{openai.ChatCompletions, apix.ExchangeOptions{Ext: anthropic.MessagesOptions{}}, false},
		{gemini.GenerateContent, apix.ExchangeOptions{Ext: &gemini.GenerateContentOptions{ThinkingMode: "fast"}}, false},
		{gemini.GenerateContent, apix.ExchangeOptions{Reasoning: &apix.Reasoning{Effort: "turbo"}}, false},
		{gemini.Interactions, apix.ExchangeOptions{LossPolicy: apix.LossPolicyStrict}, true},
		{gemini.Interactions, apix.ExchangeOptions{Ext: gemini.GenerateContentOptions{}}, false},
		{gemini.Interactions, apix.ExchangeOptions{Output: openai.Responses}, false},
	} {
		if err := reg.ValidateOptions(tc.upstream, tc.opts); (err == nil) != tc.ok {
			t.Errorf("%s %+v: %v", tc.upstream, tc.opts, err)
		}
	}
}

func TestInteractionsEnvelopeAndDiagnostics(t *testing.T) {
	reg := fullRegistry(t)
	body := reg.EncodeError(gemini.Interactions, apix.FromStatus(429, "slow"))
	if !strings.Contains(string(body.Body), `"RESOURCE_EXHAUSTED"`) {
		t.Fatalf("envelope %s", body.Body)
	}
	ex, err := reg.NewExchange(gemini.Interactions, gemini.Interactions, apix.ExchangeOptions{})
	if err != nil || ex.Diagnostics() != nil {
		t.Fatalf("diagnostics %v %v", ex, err)
	}
	if _, err := reg.Inspect(gemini.Interactions, apix.Payload{Body: []byte(`{"model":"m","agent":"a"}`)}); err == nil {
		t.Fatal("model and agent accepted together")
	}
	if _, err := reg.Inspect(gemini.Interactions, apix.Payload{Body: []byte(`{"agent":7}`)}); err == nil {
		t.Fatal("non-string agent accepted")
	}
}
