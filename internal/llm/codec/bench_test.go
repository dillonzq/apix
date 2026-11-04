package codec_test

import (
	"encoding/base64"
	"strings"
	"testing"

	"github.com/dillonzq/apix"
	"github.com/dillonzq/apix/protocol/anthropic"
	"github.com/dillonzq/apix/protocol/gemini"
	"github.com/dillonzq/apix/protocol/openai"
)

// benchMedia is 768 KiB of binary data; base64 makes it 1 MiB on the wire,
// the size of a typical inline screenshot or PDF.
var benchMedia = base64.StdEncoding.EncodeToString([]byte(strings.Repeat("\x89PNG\r\n\x1a\n\x00\x01\x02\x03\xfe\xff\x7f\x80", 768<<10/16)))

const benchHistory = `{"role":"system","content":"You are a careful assistant."},{"role":"user","content":"Summarize the attached report and list three risks."},{"role":"assistant","content":"Sure. Which section matters most?"}`

var (
	benchChatSmall = []byte(`{"model":"gpt-5.6-luna","stream":true,"temperature":0.2,"max_tokens":4096,"messages":[` + benchHistory + `,{"role":"user","content":"The finance section."}],"tools":[{"type":"function","function":{"name":"lookup","parameters":{"type":"object","properties":{"q":{"type":"string"}}}}}]}`)
	benchChatImage = []byte(`{"model":"gpt-5.6-luna","stream":true,"max_tokens":4096,"messages":[` + benchHistory + `,{"role":"user","content":[{"type":"text","text":"What is in this image?"},{"type":"image_url","image_url":{"url":"data:image/png;base64,` + benchMedia + `"}}]}]}`)
	benchChatPDF   = []byte(`{"model":"gpt-5.6-luna","max_tokens":4096,"messages":[` + benchHistory + `,{"role":"user","content":[{"type":"text","text":"Read this."},{"type":"file","file":{"filename":"r.pdf","file_data":"data:application/pdf;base64,` + benchMedia + `"}}]}]}`)
	benchMsgImage  = []byte(`{"model":"claude-x","max_tokens":4096,"messages":[{"role":"user","content":[{"type":"text","text":"Describe."},{"type":"image","source":{"type":"base64","media_type":"image/png","data":"` + benchMedia + `"}}]}]}`)
)

func benchRegistry(b *testing.B) *apix.Registry {
	b.Helper()
	reg, err := apix.NewRegistry(openai.ChatCompletionsCodec(), openai.ResponsesCodec(), anthropic.MessagesCodec(), gemini.GenerateContentCodec())
	if err != nil {
		b.Fatal(err)
	}
	return reg
}

func BenchmarkInspect(b *testing.B) {
	reg := benchRegistry(b)
	for _, c := range []struct {
		name string
		body []byte
	}{{"small", benchChatSmall}, {"image1MiB", benchChatImage}} {
		b.Run(c.name, func(b *testing.B) {
			b.SetBytes(int64(len(c.body)))
			b.ReportAllocs()
			for b.Loop() {
				if _, err := reg.Inspect(openai.ChatCompletions, apix.Payload{Body: c.body}); err != nil {
					b.Fatal(err)
				}
			}
		})
	}
}

func BenchmarkConvertRequest(b *testing.B) {
	reg := benchRegistry(b)
	effort := apix.Reasoning{Effort: "high"}
	stream := true
	for _, c := range []struct {
		name             string
		client, upstream apix.Protocol
		opts             apix.ExchangeOptions
		meta             apix.RequestMetadata
		body             []byte
	}{
		{"passthrough-rewrite/small", openai.ChatCompletions, openai.ChatCompletions, apix.ExchangeOptions{}, apix.RequestMetadata{}, benchChatSmall},
		{"passthrough-rewrite/image1MiB", openai.ChatCompletions, openai.ChatCompletions, apix.ExchangeOptions{}, apix.RequestMetadata{}, benchChatImage},
		{"passthrough-reasoning/image1MiB", openai.ChatCompletions, openai.ChatCompletions, apix.ExchangeOptions{Reasoning: &effort}, apix.RequestMetadata{}, benchChatImage},
		{"passthrough-stream-meta/image1MiB", openai.ChatCompletions, openai.ChatCompletions, apix.ExchangeOptions{}, apix.RequestMetadata{Stream: &stream}, benchChatImage},
		{"chat-to-messages-pdf/1MiB", openai.ChatCompletions, anthropic.Messages, apix.ExchangeOptions{}, apix.RequestMetadata{}, benchChatPDF},
		{"messages-to-gemini-image/1MiB", anthropic.Messages, gemini.GenerateContent, apix.ExchangeOptions{}, apix.RequestMetadata{}, benchMsgImage},
	} {
		b.Run(c.name, func(b *testing.B) {
			b.SetBytes(int64(len(c.body)))
			b.ReportAllocs()
			for b.Loop() {
				ex, err := reg.NewExchange(c.client, c.upstream, c.opts)
				if err != nil {
					b.Fatal(err)
				}
				if _, err := ex.ConvertRequest(apix.Payload{Body: c.body, Metadata: c.meta}, "upstream-model"); err != nil {
					b.Fatal(err)
				}
			}
		})
	}
}

// benchChatStream is a Chat Completions stream converted to Responses, which
// numbers every output event.
var benchChatStream = func() []byte {
	var sb strings.Builder
	sb.WriteString("data: {\"id\":\"r\",\"model\":\"m\",\"choices\":[{\"index\":0,\"delta\":{\"role\":\"assistant\"}}]}\n\n")
	for range 200 {
		sb.WriteString("data: {\"id\":\"r\",\"model\":\"m\",\"choices\":[{\"index\":0,\"delta\":{\"content\":\"The quick brown fox jumps over the lazy dog. \"}}]}\n\n")
	}
	sb.WriteString("data: {\"id\":\"r\",\"model\":\"m\",\"choices\":[{\"index\":0,\"delta\":{},\"finish_reason\":\"stop\"}]}\n\ndata: [DONE]\n\n")
	return []byte(sb.String())
}()

func BenchmarkStreamChatToResponses(b *testing.B) {
	reg := benchRegistry(b)
	b.SetBytes(int64(len(benchChatStream)))
	b.ReportAllocs()
	for b.Loop() {
		ex, err := reg.NewExchange(openai.Responses, openai.ChatCompletions, apix.ExchangeOptions{})
		if err != nil {
			b.Fatal(err)
		}
		sc, err := ex.NewStreamConverter()
		if err != nil {
			b.Fatal(err)
		}
		if _, _, err := sc.Feed(benchChatStream); err != nil {
			b.Fatal(err)
		}
		if _, err := sc.Finish(); err != nil {
			b.Fatal(err)
		}
	}
}
