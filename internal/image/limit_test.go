package image_test

import (
	"errors"
	"strings"
	"testing"

	"github.com/dillonzq/apix"
	"github.com/dillonzq/apix/protocol/openai"
	"github.com/dillonzq/apix/sse"
)

func TestImageStreamFrameLimit(test *testing.T) {
	wire := "event: image_generation.completed\ndata: {\"type\":\"image_generation.completed\",\"b64_json\":\"" + strings.Repeat("QUJD", 1024) + "\"}\n\n"
	for _, maximum := range []int{128, len(wire), -1} {
		exchange, err := registry(test).NewExchange(openai.ImagesGenerations, openai.ImagesGenerations, apix.ExchangeOptions{MaxStreamFrameBytes: maximum})
		if err != nil {
			test.Fatal(err)
		}
		if _, err := exchange.ConvertRequest(apix.Payload{Body: []byte(`{"model":"image","prompt":"test","stream":true}`)}, "image"); err != nil {
			test.Fatal(err)
		}
		converter, err := exchange.NewStreamConverter()
		if err != nil {
			test.Fatal(err)
		}
		var failure error
		var frames [][]byte
		for offset := 0; offset < len(wire); offset += 31 {
			events, _, err := converter.Feed([]byte(wire[offset:min(offset+31, len(wire))]))
			frames = append(frames, events...)
			if err != nil {
				failure = err
				break
			}
		}
		if maximum == 128 {
			if !errors.Is(failure, sse.ErrFrameTooLarge) || len(frames) != 0 {
				test.Fatalf("oversized image: %d frames, %v", len(frames), failure)
			}
			if _, err := converter.Finish(); !errors.Is(err, sse.ErrFrameTooLarge) {
				test.Fatal("image stream recovered after a size error")
			}
		} else if failure != nil || len(frames) != 1 || string(frames[0]) != wire {
			test.Fatalf("valid large image frame was changed: %d frames, %v", len(frames), failure)
		}
	}
}
