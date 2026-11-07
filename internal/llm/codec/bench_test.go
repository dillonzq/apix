package codec_test

import (
	"encoding/base64"
	"strings"
	"testing"

	"github.com/dillonzq/apix"
	"github.com/dillonzq/apix/protocol/anthropic"
	"github.com/dillonzq/apix/protocol/google"
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
	reg, err := apix.NewRegistry(openai.ChatCompletionsCodec(), openai.ResponsesCodec(), anthropic.MessagesCodec(), google.GenerateContentCodec())
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
		{"messages-to-google-image/1MiB", anthropic.Messages, google.GenerateContent, apix.ExchangeOptions{}, apix.RequestMetadata{}, benchMsgImage},
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

var benchMessagesStream = func() []byte {
	var sb strings.Builder
	sb.WriteString("event: message_start\ndata: {\"type\":\"message_start\",\"message\":{\"id\":\"m\",\"type\":\"message\",\"role\":\"assistant\",\"model\":\"c\",\"content\":[],\"stop_reason\":null,\"usage\":{\"input_tokens\":10,\"output_tokens\":1}}}\n\n")
	sb.WriteString("event: content_block_start\ndata: {\"type\":\"content_block_start\",\"index\":0,\"content_block\":{\"type\":\"text\",\"text\":\"\"}}\n\n")
	for range 200 {
		sb.WriteString("event: content_block_delta\ndata: {\"type\":\"content_block_delta\",\"index\":0,\"delta\":{\"type\":\"text_delta\",\"text\":\"The quick brown fox jumps over the lazy dog. \"}}\n\n")
	}
	sb.WriteString("event: content_block_stop\ndata: {\"type\":\"content_block_stop\",\"index\":0}\n\n")
	sb.WriteString("event: message_delta\ndata: {\"type\":\"message_delta\",\"delta\":{\"stop_reason\":\"end_turn\"},\"usage\":{\"output_tokens\":200}}\n\n")
	sb.WriteString("event: message_stop\ndata: {\"type\":\"message_stop\"}\n\n")
	return []byte(sb.String())
}()

// BenchmarkStreamNative measures same-protocol streams, where the converter
// does little beyond validating and framing, so per-event overhead such as
// usage tracking shows most.
func BenchmarkStreamNative(b *testing.B) {
	for _, tt := range []struct {
		name string
		p    apix.Protocol
		wire []byte
	}{
		{"ChatCompletions", openai.ChatCompletions, benchChatStream},
		{"Messages", anthropic.Messages, benchMessagesStream},
	} {
		b.Run(tt.name, func(b *testing.B) {
			reg := benchRegistry(b)
			b.SetBytes(int64(len(tt.wire)))
			b.ReportAllocs()
			for b.Loop() {
				ex, err := reg.NewExchange(tt.p, tt.p, apix.ExchangeOptions{})
				if err != nil {
					b.Fatal(err)
				}
				sc, err := ex.NewStreamConverter()
				if err != nil {
					b.Fatal(err)
				}
				if _, _, err := sc.Feed(tt.wire); err != nil {
					b.Fatal(err)
				}
				if _, err := sc.Finish(); err != nil {
					b.Fatal(err)
				}
				ex.(apix.UsageReporter).Usage()
			}
		})
	}
}
