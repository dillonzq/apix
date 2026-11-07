package sse

import (
	"errors"
	"reflect"
	"strings"
	"testing"
)

func limitedFrames(wire string, step, maximum int, raw bool) ([]frame, error) {
	framer := NewFramerWithLimit(raw, maximum)
	var frames []frame
	drain := func() {
		for {
			name, data, block, ok := framer.Next()
			if !ok {
				return
			}
			frames = append(frames, frame{name, data, string(block)})
		}
	}
	for offset := 0; offset < len(wire); offset += step {
		framer.Push([]byte(wire[offset:min(offset+step, len(wire))]))
		drain()
	}
	framer.End()
	drain()
	return frames, framer.Err()
}

func TestFrameSizeLimit(test *testing.T) {
	for _, fixture := range []struct {
		name, wire string
		maximum    int
		frames     int
		fails      bool
	}{
		{"unterminated line", "data:" + strings.Repeat("x", 64), 32, 0, true},
		{"multi-line frame", strings.Repeat("data:x\n", 8), 32, 0, true},
		{"comments", strings.Repeat(":x\n", 20), 32, 0, true},
		{"unknown lines", strings.Repeat("ignored\n", 8), 32, 0, true},
		{"preceding frame", "data: ok\n\n" + strings.Repeat("x", 64), 32, 1, true},
		{"many bounded frames", strings.Repeat("data:x\n\n", 100), 8, 100, false},
		{"CRLF at boundary", "data:x\r\n\r\n", 10, 1, false},
		{"CRLF over boundary", "data:x\r\n\r\n", 9, 0, true},
		{"CR at boundary", "data:x\r\r", 8, 1, false},
		{"EOF at boundary", "data:x", 6, 1, false},
		{"disabled limit", "data:" + strings.Repeat("x", 128) + "\n\n", -1, 1, false},
	} {
		test.Run(fixture.name, func(test *testing.T) {
			for _, raw := range []bool{false, true} {
				whole, wholeErr := limitedFrames(fixture.wire, len(fixture.wire), fixture.maximum, raw)
				if len(whole) != fixture.frames || errors.Is(wholeErr, ErrFrameTooLarge) != fixture.fails {
					test.Fatalf("frames=%d error=%v", len(whole), wholeErr)
				}
				for step := 1; step <= len(fixture.wire); step++ {
					chunked, chunkErr := limitedFrames(fixture.wire, step, fixture.maximum, raw)
					if !reflect.DeepEqual(whole, chunked) || !errors.Is(chunkErr, wholeErr) {
						test.Fatalf("step=%d: frames=%v err=%v; want %v %v", step, chunked, chunkErr, whole, wholeErr)
					}
				}
			}
		})
	}
}

func TestFrameLimitIsTerminalAndBoundsBuffer(test *testing.T) {
	framer := NewFramerWithLimit(true, 32)
	framer.Push([]byte(strings.Repeat("x", 4096)))
	if len(framer.buf) != 32 || !errors.Is(framer.Err(), ErrFrameTooLarge) {
		test.Fatal("oversized input was buffered past the limit")
	}
	for range 10 {
		framer.Push([]byte("data: valid\n\n"))
		framer.End()
		if _, _, _, ok := framer.Next(); ok || !errors.Is(framer.Err(), ErrFrameTooLarge) {
			test.Fatal("oversized frame recovered")
		}
	}
	if len(framer.buf) != 0 || framer.data.Len() != 0 || len(framer.raw) != 0 {
		test.Fatal("terminal framer retained partial input")
	}
	var limiter FrameLimiter
	limiter.bytes = DefaultMaxFrameBytes
	if accepted, err := limiter.Feed([]byte("x")); len(accepted) != 0 || !errors.Is(err, ErrFrameTooLarge) {
		test.Fatal("zero-value limiter did not apply the default")
	}
}

func TestTrailingCRLimitErrorReleasesPartialFrame(test *testing.T) {
	for _, prefix := range []string{"", "event: a\ndata:y\n"} {
		for _, captureRaw := range []bool{false, true} {
			wire := prefix + "data:x\r"
			framer := NewFramerWithLimit(captureRaw, len(wire))
			framer.Push([]byte(wire))
			if _, _, _, ok := framer.Next(); ok || framer.Err() != nil {
				test.Fatal("trailing CR should wait for more input before an error")
			}
			if string(framer.buf) != "data:x\r" || framer.scanned != len("data:x") {
				test.Fatal("non-terminal trailing CR state was not preserved")
			}
			framer.Push([]byte("\n"))
			for range 2 {
				if _, _, _, ok := framer.Next(); ok || !errors.Is(framer.Err(), ErrFrameTooLarge) {
					test.Fatal("oversized trailing CR frame did not remain terminal")
				}
				if framer.buf != nil || framer.raw != nil || framer.data.Len() != 0 || framer.scanned != 0 {
					test.Fatal("terminal trailing CR retained partial frame buffers")
				}
				if framer.eventType != "" || framer.dataLines != 0 || framer.sawFrame {
					test.Fatal("terminal trailing CR retained partial frame metadata")
				}
				framer.Push([]byte("data: later\n\n"))
				framer.End()
			}
		}
	}
}

func TestDecoderFrameLimit(test *testing.T) {
	decoder := Decoder{MaxFrameBytes: 16}
	events := decoder.Feed([]byte("data: ok\n\n" + strings.Repeat("x", 17)))
	if len(events) != 1 || string(events[0].Data) != "ok" || !errors.Is(decoder.Err(), ErrFrameTooLarge) {
		test.Fatalf("decoder events=%v error=%v", events, decoder.Err())
	}
	if len(decoder.Finish()) != 0 || len(decoder.Feed([]byte("data: later\n\n"))) != 0 {
		test.Fatal("decoder recovered after a limit error")
	}
}

func FuzzFrameLimitChunkBoundaries(fuzzer *testing.F) {
	for _, wire := range []string{"data:x\r\n\r\ndata:y\n\n", ":a\r\rdata:x", "data:x\n" + strings.Repeat("x", 300)} {
		fuzzer.Add(wire, uint8(13), uint8(1))
	}
	fuzzer.Fuzz(func(test *testing.T, wire string, budget, chunk uint8) {
		if len(wire) > 4096 {
			test.Skip()
		}
		maximum := int(budget) + 1
		whole, wholeErr := limitedFrames(wire, max(1, len(wire)), maximum, true)
		chunked, chunkErr := limitedFrames(wire, int(chunk)+1, maximum, true)
		if !reflect.DeepEqual(whole, chunked) || !errors.Is(chunkErr, wholeErr) {
			test.Fatalf("frames or limit error depend on chunks: %v %v; %v %v", whole, wholeErr, chunked, chunkErr)
		}
	})
}
