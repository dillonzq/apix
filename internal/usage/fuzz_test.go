package usage

import (
	"reflect"
	"testing"
)

// Usage must not depend on how a stream is split into chunks.
func FuzzTrackerChunkBoundaries(f *testing.F) {
	f.Add(uint8(Messages), "event: message_start\ndata: {\"message\":{\"usage\":{\"input_tokens\":5}}}\n\nevent: message_delta\ndata: {\"usage\":{\"output_tokens\":7}}\n\n", uint8(3))
	f.Add(uint8(Gemini), "data: {\"usageMetadata\":{\"promptTokenCount\":5}}\r\n\r\ndata: {\"usageMetadata\":{\"candidatesTokenCount\":7}}", uint8(1))
	f.Add(uint8(ChatCompletions), "data: {\"usage\":\ndata: {\"prompt_tokens\":5}}\n\ndata: [DONE]\n\n", uint8(7))
	f.Fuzz(func(t *testing.T, dialect uint8, wire string, step uint8) {
		d := Dialect(dialect%uint8(Interactions)) + 1
		whole := New(d)
		whole.Feed([]byte(wire))
		whole.End()
		split := New(d)
		n := int(step%16) + 1
		for i := 0; i < len(wire); i += n {
			split.Feed([]byte(wire[i:min(i+n, len(wire))]))
		}
		split.End()
		a, aok := whole.Usage()
		b, bok := split.Usage()
		if aok != bok || !reflect.DeepEqual(a, b) {
			t.Fatalf("whole %+v %v, split %+v %v", a, aok, b, bok)
		}
		if a.InputTokens < 0 || a.OutputTokens < 0 || a.TotalTokens < 0 {
			t.Fatalf("negative usage %+v", a)
		}
	})
}
