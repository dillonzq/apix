package codec_test

import (
	"slices"
	"testing"

	"github.com/dillonzq/apix"
	"github.com/dillonzq/apix/protocol/anthropic"
	"github.com/dillonzq/apix/protocol/gemini"
	"github.com/dillonzq/apix/protocol/openai"
)

func TestRequiredContentFeatures(t *testing.T) {
	reg, err := apix.NewRegistry(slices.Concat(openai.Codecs(), anthropic.Codecs(), gemini.Codecs())...)
	if err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		name     string
		protocol apix.Protocol
		body     string
		want     []string
	}{
		{"chat image", openai.ChatCompletions, `{"messages":[{"role":"user","content":[{"type":"image_url","image_url":{"url":"https://example.com/a.png"}}]}]}`, []string{"image-input"}},
		{"responses media", openai.Responses, `{"input":[{"role":"user","content":[{"type":"input_file","file_id":"f"},{"type":"input_audio","input_audio":{"data":"YQ==","format":"wav"}},{"type":"input_video","video_url":"https://example.com/v"}]}]}`, []string{"file-input", "audio-input", "video-input"}},
		{"messages tool result", anthropic.Messages, `{"messages":[{"role":"user","content":[{"type":"tool_result","tool_use_id":"c","content":[{"type":"image","source":{"type":"base64","media_type":"image/png","data":"YQ=="}},{"type":"document","source":{"type":"url","url":"https://example.com/doc"}}]}]}]}`, []string{"image-input", "file-input"}},
		{"gemini media and output", gemini.GenerateContent, `{"contents":[{"parts":[{"inlineData":{"mimeType":"image/png","data":"YQ=="}},{"file_data":{"mime_type":"audio/wav","file_uri":"https://example.com/a"}}]}],"generationConfig":{"responseModalities":["IMAGE","AUDIO"],"responseJsonSchema":{"type":"object"}}}`, []string{"image-input", "audio-input", "image-output", "audio-output", "structured-output"}},
		{"interactions", gemini.Interactions, `{"model":"m","input":[{"type":"user_input","content":[{"type":"image","uri":"https://example.com/a"},{"type":"video","uri":"https://example.com/v"}]}],"response_modalities":["audio"]}`, []string{"image-input", "video-input", "audio-output"}},
		{"structured and search", openai.Responses, `{"input":"hi","tools":[{"type":"web_search"}],"text":{"format":{"type":"json_schema","schema":{"type":"object"}}}}`, []string{"tools", "structured-output", "web-search"}},
		{"business data is not media", openai.ChatCompletions, `{"messages":[{"role":"tool","content":"{\"type\":\"image_url\"}"},{"role":"assistant","tool_calls":[{"function":{"arguments":"{\"type\":\"input_audio\"}"}}]}],"tools":[{"type":"function","function":{"parameters":{"properties":{"image":{"type":"image_url"}}}}}]}`, []string{"tools"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			for _, stream := range []*bool{nil, new(true), new(false)} {
				req := apix.Payload{Body: []byte(tc.body), Metadata: apix.RequestMetadata{Model: "uri-model", Stream: stream}}
				info, err := reg.Inspect(tc.protocol, req)
				if err != nil {
					t.Fatal(err)
				}
				want := append([]string(nil), tc.want...)
				if stream != nil && *stream {
					want = append([]string{"stream"}, want...)
				}
				if !slices.Equal(info.Features, want) {
					t.Fatalf("features %v want %v", info.Features, want)
				}
			}
		})
	}
}
