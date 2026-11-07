package image_test

import (
	"reflect"
	"testing"

	"github.com/dillonzq/apix"
	"github.com/dillonzq/apix/protocol/google"
	"github.com/dillonzq/apix/protocol/openai"
)

func imageUsage(t *testing.T, ex apix.Exchange) (apix.Usage, bool) {
	t.Helper()
	u, ok := ex.(apix.UsageReporter).Usage()
	u.Raw = nil
	return u, ok
}

func TestUsage(t *testing.T) {
	openaiReply := `{"created":1,"data":[{"b64_json":"` + b64 + `"}],"usage":{"input_tokens":10,"output_tokens":1290,"total_tokens":1300}}`
	googleReply := `{"candidates":[{"content":{"parts":[{"inlineData":{"mimeType":"image/png","data":"` + b64 + `"}}]},"finishReason":"STOP"}],"usageMetadata":{"promptTokenCount":10,"candidatesTokenCount":1290,"totalTokenCount":1300}}`
	want := apix.Usage{InputTokens: 10, OutputTokens: 1290, TotalTokens: 1300}
	for _, tt := range []struct {
		client, upstream apix.Protocol
		request, reply   string
		model            string
	}{
		{openai.ImagesGenerations, openai.ImagesGenerations, `{"prompt":"a cat"}`, openaiReply, ""},
		{google.Imagen, openai.ImagesGenerations, `{"instances":[{"prompt":"a cat"}]}`, openaiReply, "gpt-image-1"},
		{openai.ImagesGenerations, google.GenerateContentImage, `{"prompt":"a cat"}`, googleReply, "gemini-2.5-flash-image"},
		{google.GenerateContentImage, google.GenerateContentImage, `{"contents":[{"role":"user","parts":[{"text":"a cat"}]}]}`, googleReply, "gemini-2.5-flash-image"},
	} {
		ex := exchange(t, tt.client, tt.upstream, apix.ExchangeOptions{})
		if _, err := ex.ConvertRequest(apix.Payload{Body: []byte(tt.request)}, tt.model); err != nil {
			t.Fatal(err)
		}
		if _, err := ex.ConvertResponse(200, apix.Payload{Body: []byte(tt.reply)}); err != nil {
			t.Fatalf("%s -> %s: %v", tt.client, tt.upstream, err)
		}
		if got, ok := imageUsage(t, ex); !ok || !reflect.DeepEqual(got, want) {
			t.Fatalf("%s -> %s: %+v %v", tt.client, tt.upstream, got, ok)
		}
	}

	// Imagen reports no usage.
	ex := exchange(t, openai.ImagesGenerations, google.Imagen, apix.ExchangeOptions{})
	if _, err := ex.ConvertRequest(apix.Payload{Body: []byte(`{"prompt":"a cat"}`)}, "imagen-4.0-generate-001"); err != nil {
		t.Fatal(err)
	}
	if _, err := ex.ConvertResponse(200, apix.Payload{Body: []byte(`{"predictions":[{"bytesBase64Encoded":"` + b64 + `","mimeType":"image/png"}]}`)}); err != nil {
		t.Fatal(err)
	}
	if u, ok := imageUsage(t, ex); ok {
		t.Fatalf("imagen usage %+v", u)
	}
}

func TestNativeStreamUsage(t *testing.T) {
	wire := "event: image_generation.partial_image\ndata: {\"type\":\"image_generation.partial_image\",\"b64_json\":\"p\",\"partial_image_index\":0}\n\n" +
		"event: image_generation.completed\ndata: {\"type\":\"image_generation.completed\",\"b64_json\":\"a\",\"usage\":{\"input_tokens\":1,\"output_tokens\":2,\"total_tokens\":3}}\n\n"
	for _, bytewise := range []bool{false, true} {
		ex := exchange(t, openai.ImagesGenerations, openai.ImagesGenerations, apix.ExchangeOptions{})
		if _, err := ex.ConvertRequest(apix.Payload{Body: []byte(`{"prompt":"p","stream":true,"partial_images":1}`)}, ""); err != nil {
			t.Fatal(err)
		}
		sc, err := ex.NewStreamConverter()
		if err != nil {
			t.Fatal(err)
		}
		if _, _, err := feedAll(sc, wire, bytewise); err != nil {
			t.Fatal(err)
		}
		if got, ok := imageUsage(t, ex); !ok || !reflect.DeepEqual(got, apix.Usage{InputTokens: 1, OutputTokens: 2, TotalTokens: 3}) {
			t.Fatalf("bytewise=%v: %+v %v", bytewise, got, ok)
		}
	}
}
