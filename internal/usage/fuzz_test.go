package usage

import (
	"reflect"
	"strings"
	"testing"

	"github.com/dillonzq/apix/sse"
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

// The tracker's line splitting and key filter must record what framing the
// stream with sse.Framer and decoding every frame records, including for
// keys that differ in case. Keys spelled with \u escapes are outside the
// filter's reach.
func FuzzTrackerMatchesFramer(f *testing.F) {
	f.Add(uint8(Messages), "event: message_start\ndata: {\"type\":\"message\",\"message\":{\"Usage\":{\"input_tokens\":5}}}\n\n", uint8(3))
	f.Add(uint8(Gemini), "data: {\"usageMetadata\":{\"promptTokenCount\":5}}\r\rdata: {\"usage_metadata\":{\"candidatesTokenCount\":7}}\r\n", uint8(1))
	f.Add(uint8(ChatCompletions), ": c\ndata: {\"usage\":\ndata:{\"prompt_tokens\":5}}\n\nevent: x\n\ndata: [DONE]\n\n", uint8(7))
	f.Add(uint8(Messages), "data: {\"Message\":{\"usage\":{\"input_tokens\":1}},\"uſage\":{},\"usage\":{\"output_tokens\":2},\"Usage\":{}}\n\n", uint8(16))
	f.Add(uint8(Responses), "data: {\"text\":\"\\\"usage\\\": passage\"}\n\ndata: {\"response\":{\"USAGE\":{\"input_tokens\":2}}}", uint8(5))
	f.Fuzz(func(t *testing.T, dialect uint8, wire string, step uint8) {
		if len(wire) > 4096 || strings.Contains(wire, `\u`) {
			t.Skip()
		}
		d := Dialect(dialect%uint8(Interactions)) + 1
		got := New(d)
		n := int(step%16) + 1
		for i := 0; i < len(wire); i += n {
			got.Feed([]byte(wire[i:min(i+n, len(wire))]))
		}
		got.End()
		want := New(d)
		framer := sse.NewFramer(false)
		framer.Push([]byte(wire))
		framer.End()
		for {
			_, data, _, ok := framer.Next()
			if !ok {
				break
			}
			want.merge([]byte(data))
		}
		a, aok := got.Usage()
		b, bok := want.Usage()
		if aok != bok || !reflect.DeepEqual(a, b) {
			t.Fatalf("tracker %+v %v, framer %+v %v", a, aok, b, bok)
		}
	})
}
