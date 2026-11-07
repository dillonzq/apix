package codec_test

import (
	"errors"
	"strings"
	"testing"

	"github.com/dillonzq/apix"
	"github.com/dillonzq/apix/protocol/google"
	"github.com/dillonzq/apix/protocol/openai"
	"github.com/dillonzq/apix/sse"
)

func TestStreamFrameLimitsAllDirections(test *testing.T) {
	reg := registry(test)
	for _, upstream := range reg.Protocols() {
		for _, output := range reg.Protocols() {
			test.Run(string(upstream)+"/"+string(output), func(test *testing.T) {
				for _, step := range []int{1, 7, 300} {
					exchange, err := reg.NewExchange(output, upstream, apix.ExchangeOptions{MaxStreamFrameBytes: 128})
					if err != nil {
						test.Fatal(err)
					}
					converter, err := exchange.NewStreamConverter()
					if err != nil {
						test.Fatal(err)
					}
					wire := ":" + strings.Repeat("x", 256) + "\n\n"
					var failure error
					for offset := 0; offset < len(wire); offset += step {
						_, done, err := converter.Feed([]byte(wire[offset:min(offset+step, len(wire))]))
						failure = err
						if done {
							test.Fatal("oversized frame completed the stream")
						}
						if failure != nil {
							break
						}
					}
					if !errors.Is(failure, sse.ErrFrameTooLarge) {
						test.Fatalf("limit error lost: %v", failure)
					}
					if _, err := converter.Finish(); !errors.Is(err, sse.ErrFrameTooLarge) {
						test.Fatalf("Finish lost the limit error: %v", err)
					}
					if _, done, err := converter.Feed(nil); done || !errors.Is(err, sse.ErrFrameTooLarge) {
						test.Fatalf("limit failure was not terminal: %v", err)
					}
				}
			})
		}
	}
}

func TestInteractionsStreamFrameLimit(test *testing.T) {
	reg, err := apix.NewRegistry(google.InteractionsCodec())
	if err != nil {
		test.Fatal(err)
	}
	exchange, err := reg.NewExchange(google.Interactions, google.Interactions, apix.ExchangeOptions{MaxStreamFrameBytes: 32})
	if err != nil {
		test.Fatal(err)
	}
	converter, err := exchange.NewStreamConverter()
	if err != nil {
		test.Fatal(err)
	}
	if _, _, err := converter.Feed([]byte(strings.Repeat("x", 33))); !errors.Is(err, sse.ErrFrameTooLarge) {
		test.Fatalf("Interactions limit: %v", err)
	}
}

func TestExactFrameLimitAllDirections(test *testing.T) {
	reg := registry(test)
	for upstream, fixture := range fixtures {
		wire := fixture.stream
		if upstream == openai.ChatCompletions {
			wire = strings.ReplaceAll(wire, "data: ", "data:")
		}
		maximum := 0
		for _, frame := range strings.SplitAfter(wire, "\n\n") {
			maximum = max(maximum, len(frame))
		}
		for _, output := range reg.Protocols() {
			test.Run(string(upstream)+"/"+string(output), func(test *testing.T) {
				for _, step := range []int{1, len(wire)} {
					exchange, err := reg.NewExchange(output, upstream, apix.ExchangeOptions{MaxStreamFrameBytes: maximum})
					if err != nil {
						test.Fatal(err)
					}
					converter, err := exchange.NewStreamConverter()
					if err != nil {
						test.Fatal(err)
					}
					for offset := 0; offset < len(wire); offset += step {
						if _, _, err := converter.Feed([]byte(wire[offset:min(offset+step, len(wire))])); err != nil {
							test.Fatalf("valid frame at the limit: %v", err)
						}
					}
					if _, err := converter.Finish(); err != nil {
						test.Fatal(err)
					}
				}
			})
		}
	}
}
