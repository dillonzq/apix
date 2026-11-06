package image_test

import (
	"encoding/base64"
	"strings"
	"testing"

	"github.com/dillonzq/apix"
	"github.com/dillonzq/apix/protocol/openai"
)

// benchImage is 3 MiB of Base64, the size of a 1024x1024 PNG.
var benchImage = base64.StdEncoding.EncodeToString([]byte(strings.Repeat("\x89PNG\r\n\x1a\n\x00\x01\x02\x03\xfe\xff\x7f\x80", 3<<20/4*3/16)))

var benchImageStream = []byte(
	"event: image_generation.partial_image\ndata: {\"type\":\"image_generation.partial_image\",\"b64_json\":\"" + benchImage + "\",\"partial_image_index\":0}\n\n" +
		"event: image_generation.partial_image\ndata: {\"type\":\"image_generation.partial_image\",\"b64_json\":\"" + benchImage + "\",\"partial_image_index\":1}\n\n" +
		"event: image_generation.completed\ndata: {\"type\":\"image_generation.completed\",\"b64_json\":\"" + benchImage + "\",\"usage\":{\"input_tokens\":50,\"output_tokens\":4160,\"total_tokens\":4210}}\n\n")

// BenchmarkStreamNative passes two partial images and the final image
// through in 16 KiB chunks, the size of a TLS record.
func BenchmarkStreamNative(b *testing.B) {
	reg, err := apix.NewRegistry(openai.ImagesGenerationsCodec())
	if err != nil {
		b.Fatal(err)
	}
	const chunk = 16 << 10
	b.SetBytes(int64(len(benchImageStream)))
	b.ReportAllocs()
	for b.Loop() {
		ex, err := reg.NewExchange(openai.ImagesGenerations, openai.ImagesGenerations, apix.ExchangeOptions{})
		if err != nil {
			b.Fatal(err)
		}
		if _, err := ex.ConvertRequest(apix.Payload{Body: []byte(`{"model":"gpt-image-1","prompt":"p","stream":true,"partial_images":2}`)}, ""); err != nil {
			b.Fatal(err)
		}
		sc, err := ex.NewStreamConverter()
		if err != nil {
			b.Fatal(err)
		}
		for i := 0; i < len(benchImageStream); i += chunk {
			if _, _, err := sc.Feed(benchImageStream[i:min(i+chunk, len(benchImageStream))]); err != nil {
				b.Fatal(err)
			}
		}
		if _, err := sc.Finish(); err != nil {
			b.Fatal(err)
		}
		if _, ok := ex.(apix.UsageReporter).Usage(); !ok {
			b.Fatal("no usage")
		}
	}
}
