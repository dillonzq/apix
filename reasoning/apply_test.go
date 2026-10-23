package reasoning_test

import (
	"strings"
	"testing"

	"github.com/dillonzq/apix"
	"github.com/dillonzq/apix/protocol/anthropic"
	"github.com/dillonzq/apix/protocol/openai"
	"github.com/dillonzq/apix/reasoning"
)

const (
	chatCompletions = openai.ChatCompletions
	responses       = openai.Responses
	messages        = anthropic.Messages
)

func TestApplySuffixSeamBranches(t *testing.T) {
	// Without a recognized suffix the body stays byte-identical: no suffix must
	// not rewrite a request at all.
	body := []byte(`{"model":"m","reasoning_effort":"low"}`)
	if got, eErr := reasoning.ApplySuffix(chatCompletions, body, reasoning.ParseSuffix("m")); eErr != nil || string(got) != string(body) {
		t.Fatalf("no suffix = %q, %v", got, eErr)
	}
	if got, eErr := reasoning.ApplySuffix(chatCompletions, body, reasoning.ParseSuffix("m(turbo)")); eErr != nil || string(got) != string(body) {
		t.Fatalf("unrecognized suffix = %q, %v", got, eErr)
	}
	// An unknown route has no reasoning control to override.
	if got, eErr := reasoning.ApplySuffix("weird", body, reasoning.ParseSuffix("m(high)")); eErr != nil || string(got) != string(body) {
		t.Fatalf("unknown route = %q, %v", got, eErr)
	}
	// Defensive parse failures are classified translation errors, never a send.
	for _, in := range [][]byte{[]byte("{bad"), []byte("null")} {
		if _, eErr := reasoning.ApplySuffix(chatCompletions, in, reasoning.ParseSuffix("m(high)")); eErr == nil || apix.AsError(eErr).Class != apix.ClassTranslation {
			t.Fatalf("malformed body %q = %v", in, eErr)
		}
	}
	// Containers with the wrong JSON shape are replaced or left alone, not fatal.
	if got, eErr := reasoning.ApplySuffix(responses, []byte(`{"reasoning":"auto"}`), reasoning.ParseSuffix("m(high)")); eErr != nil || !strings.Contains(string(got), `"effort":"high"`) {
		t.Fatalf("non-object reasoning = %s, %v", got, eErr)
	}
	if got, eErr := reasoning.ApplySuffix(messages, []byte(`{"output_config":"auto"}`), reasoning.ParseSuffix("m(none)")); eErr != nil || !strings.Contains(string(got), `"type":"disabled"`) {
		t.Fatalf("non-object output_config = %s, %v", got, eErr)
	}
	// Auto drops the control, including an emptied parent object.
	if got, eErr := reasoning.ApplySuffix(responses, []byte(`{"reasoning":{"effort":"low"}}`), reasoning.ParseSuffix("m(auto)")); eErr != nil || strings.Contains(string(got), "reasoning") {
		t.Fatalf("auto reasoning = %s, %v", got, eErr)
	}
	// Adaptive mode (the default) replaces the client's effort and drops a
	// manual budget, keeping sibling fields.
	got, eErr := reasoning.ApplySuffix(messages,
		[]byte(`{"thinking":{"type":"enabled","budget_tokens":1,"display":"omitted"},"output_config":{"effort":"low","other":1}}`), reasoning.ParseSuffix("m(high)"))
	if eErr != nil || string(got) != `{"output_config":{"effort":"high","other":1},"thinking":{"display":"omitted","type":"adaptive"}}` {
		t.Fatalf("messages adaptive override = %s, %v", got, eErr)
	}
	// A numeric suffix becomes the effort its budget maps to.
	if got, eErr := reasoning.ApplySuffix(messages, []byte(`{}`), reasoning.ParseSuffix("m(8192)")); eErr != nil || string(got) != `{"output_config":{"effort":"medium"},"thinking":{"type":"adaptive"}}` {
		t.Fatalf("messages numeric adaptive = %s, %v", got, eErr)
	}
	// Manual mode drops the adaptive effort, keeps every sibling field and
	// sends a fixed or exact budget.
	manual := reasoning.Options{MessagesManual: true}
	got, eErr = reasoning.ApplySuffixWith(messages,
		[]byte(`{"thinking":{"type":"adaptive","display":"omitted"},"output_config":{"effort":"low","other":1}}`), reasoning.ParseSuffix("m(high)"), manual)
	if eErr != nil || string(got) != `{"output_config":{"other":1},"thinking":{"budget_tokens":24576,"display":"omitted","type":"enabled"}}` {
		t.Fatalf("messages manual override = %s, %v", got, eErr)
	}
	if got, eErr := reasoning.ApplySuffixWith(messages, []byte(`{}`), reasoning.ParseSuffix("m(3000)"), manual); eErr != nil || string(got) != `{"thinking":{"budget_tokens":3000,"type":"enabled"}}` {
		t.Fatalf("messages numeric manual = %s, %v", got, eErr)
	}
	// None disables thinking in either mode and drops display.
	for _, opts := range []reasoning.Options{{}, manual} {
		got, eErr := reasoning.ApplySuffixWith(messages, []byte(`{"thinking":{"type":"adaptive","display":"omitted"},"output_config":{"effort":"low"}}`), reasoning.ParseSuffix("m(none)"), opts)
		if eErr != nil || string(got) != `{"thinking":{"type":"disabled"}}` {
			t.Fatalf("messages none (%+v) = %s, %v", opts, got, eErr)
		}
	}
	// A superseded control is dropped only for the conversions that read it.
	if got := reasoning.StripSupersededReasoning(messages, messages, body); string(got) != string(body) {
		t.Fatalf("native messages body rewritten: %s", got)
	}
	if got := reasoning.StripSupersededReasoning(chatCompletions, chatCompletions, body); string(got) != string(body) {
		t.Fatalf("native chat body rewritten: %s", got)
	}
	if got := reasoning.StripSupersededReasoning(messages, chatCompletions, body); strings.Contains(string(got), "reasoning_effort") {
		t.Fatalf("superseded chat effort kept: %s", got)
	}
	if got := reasoning.StripSupersededReasoning(messages, chatCompletions, []byte("{bad")); string(got) != "{bad" {
		t.Fatalf("malformed body rewritten: %s", got)
	}
}
