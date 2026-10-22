package adapters_test

import (
	"bytes"
	"testing"

	"github.com/dillonzq/apix/internal/llm/adapter/messages"
	"github.com/dillonzq/apix/internal/llm/translate"
)

// Internal native entry points must follow the public model-only contract.
func TestNativeMessagesRequestEntryParity(t *testing.T) {
	body := []byte(`{"model":"original","system":"base","max_tokens":10,"messages":[{"role":"system","content":"history"},{"role":"user","content":"hi"}],"unknown":9007199254740993}`)
	for _, model := range []string{"original", "replacement"} {
		public, err := translate.ConvertRequest(translate.FormatMessages, model, translate.FormatMessages, body, nil)
		if err != nil {
			t.Fatal(err)
		}
		internal, err := messages.BuildRequest(model, translate.FormatMessages, body)
		if err != nil {
			t.Fatal(err)
		}
		if !bytes.Equal(public, internal) {
			t.Fatalf("native entry points diverged: %s vs %s", public, internal)
		}
	}
}
