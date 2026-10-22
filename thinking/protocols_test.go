package thinking_test

import (
	"strings"
	"testing"

	"github.com/dillonzq/apix/protocol/anthropic"
	"github.com/dillonzq/apix/protocol/openai"
	"github.com/dillonzq/apix/thinking"
)

// The package keeps private copies of the vendor protocol IDs; these calls
// fail if the copies drift from the public constants.
func TestVendorProtocolIDs(t *testing.T) {
	high := thinking.ParseSuffix("m(high)")
	for _, tc := range []struct {
		name string
		got  func() ([]byte, *thinking.Error)
		want string
	}{
		{"chat", func() ([]byte, *thinking.Error) {
			return thinking.ApplySuffix(openai.ChatCompletions, []byte(`{}`), high)
		}, `"reasoning_effort":"high"`},
		{"responses", func() ([]byte, *thinking.Error) { return thinking.ApplySuffix(openai.Responses, []byte(`{}`), high) }, `"effort":"high"`},
		{"messages", func() ([]byte, *thinking.Error) { return thinking.ApplySuffix(anthropic.Messages, []byte(`{}`), high) }, `"thinking"`},
	} {
		got, err := tc.got()
		if err != nil || !strings.Contains(string(got), tc.want) {
			t.Fatalf("%s: %s %v", tc.name, got, err)
		}
	}
	body := []byte(`{"reasoning_effort":"low"}`)
	if got := thinking.StripSupersededReasoning(anthropic.Messages, openai.ChatCompletions, body); strings.Contains(string(got), "reasoning_effort") {
		t.Fatalf("strip: %s", got)
	}
}
