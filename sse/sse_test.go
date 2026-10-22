package sse

import (
	"reflect"
	"testing"
)

func TestDecoderChunkBoundaries(t *testing.T) {
	wire := []byte(": heartbeat\r\nevent: chunk\r\ndata: {\r\ndata: \"x\":1}\r\n\r\ndata: [DONE]\r\n\r\n")
	want := []Event{{Event: "chunk", Data: []byte("{\n\"x\":1}")}, {Data: []byte("[DONE]"), Done: true}}
	for split := 0; split <= len(wire); split++ {
		var decoder Decoder
		got := decoder.Feed(wire[:split])
		got = append(got, decoder.Feed(wire[split:])...)
		got = append(got, decoder.Finish()...)
		if !reflect.DeepEqual(got, want) {
			t.Fatalf("split %d: %+v", split, got)
		}
		if len(decoder.Finish()) != 0 || len(decoder.Feed(wire)) != 0 {
			t.Fatal("closed decoder emitted events")
		}
	}
}

func TestDecoderEmptyData(t *testing.T) {
	for _, wire := range []string{"data:\n\n", "data: \r\n\r\n", "data:\r\r", "data:"} {
		for split := 0; split <= len(wire); split++ {
			var decoder Decoder
			got := decoder.Feed([]byte(wire[:split]))
			got = append(got, decoder.Feed([]byte(wire[split:]))...)
			got = append(got, decoder.Finish()...)
			if len(got) != 1 || len(got[0].Data) != 0 || got[0].Done || got[0].Event != "" {
				t.Fatalf("wire=%q split=%d events=%+v", wire, split, got)
			}
		}
	}
	var decoder Decoder
	got := decoder.Feed([]byte(": comment\n\n\n\nevent: ping\ndata:\n\n"))
	if len(got) != 1 || got[0].Event != "ping" || len(got[0].Data) != 0 {
		t.Fatalf("events=%+v", got)
	}
}
