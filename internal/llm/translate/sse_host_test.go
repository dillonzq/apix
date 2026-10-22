package translate

import (
	"testing"

	"github.com/dillonzq/apix/sse"
)

func TestSSEDecoderHostFraming(t *testing.T) {
	stream, err := NewStreamConverter(FormatChatCompletions, FormatChatCompletions, nil)
	if err != nil {
		t.Fatal(err)
	}
	frames, done, err := stream.Feed([]byte("data: {\"choices\":[{\"delta\":{\"content\":\"hi\"},\"finish_reason\":null}]}\n\ndata: [DONE]\n\n"))
	if err != nil || !done {
		t.Fatalf("done=%v err=%v", done, err)
	}
	var decoder sse.Decoder
	var payloads [][]byte
	markers := 0
	for _, frame := range frames {
		for _, event := range decoder.Feed(frame) {
			if event.Done {
				markers++
				continue
			}
			payloads = append(payloads, event.Data)
		}
	}
	if markers != 1 || len(payloads) != 1 || payloads[0][0] != '{' {
		t.Fatalf("markers=%d payloads=%q", markers, payloads)
	}
	var eof sse.Decoder
	if len(eof.Feed([]byte("data: tail"))) != 0 {
		t.Fatal("premature frame")
	}
	if got := eof.Finish(); len(got) != 1 || string(got[0].Data) != "tail" {
		t.Fatalf("EOF=%v", got)
	}
}
