package usage

import (
	"encoding/json"
	"reflect"
	"testing"

	"github.com/dillonzq/apix"
)

func counts(u apix.Usage) apix.Usage {
	u.Raw = nil
	return u
}

func TestBody(t *testing.T) {
	tests := []struct {
		name    string
		dialect Dialect
		body    string
		want    apix.Usage
	}{
		{"chat", ChatCompletions,
			`{"id":"c","usage":{"prompt_tokens":100,"completion_tokens":40,"total_tokens":140,"prompt_tokens_details":{"cached_tokens":60},"completion_tokens_details":{"reasoning_tokens":25}}}`,
			apix.Usage{InputTokens: 100, OutputTokens: 40, TotalTokens: 140, CacheReadTokens: 60, ReasoningTokens: 25}},
		{"responses", Responses,
			`{"id":"r","usage":{"input_tokens":100,"output_tokens":40,"total_tokens":140,"input_tokens_details":{"cached_tokens":60},"output_tokens_details":{"reasoning_tokens":25}}}`,
			apix.Usage{InputTokens: 100, OutputTokens: 40, TotalTokens: 140, CacheReadTokens: 60, ReasoningTokens: 25}},
		{"messages adds cache counts to input", Messages,
			`{"id":"m","usage":{"input_tokens":10,"cache_read_input_tokens":60,"cache_creation_input_tokens":30,"output_tokens":40}}`,
			apix.Usage{InputTokens: 100, OutputTokens: 40, TotalTokens: 140, CacheReadTokens: 60, CacheWriteTokens: 30}},
		{"gemini adds thoughts and tool prompts", Gemini,
			`{"candidates":[],"usageMetadata":{"promptTokenCount":90,"toolUsePromptTokenCount":10,"candidatesTokenCount":15,"thoughtsTokenCount":25,"cachedContentTokenCount":60,"totalTokenCount":140}}`,
			apix.Usage{InputTokens: 100, OutputTokens: 40, TotalTokens: 140, CacheReadTokens: 60, ReasoningTokens: 25}},
		{"gemini snake case", Gemini,
			`{"usage_metadata":{"prompt_token_count":100,"candidates_token_count":40}}`,
			apix.Usage{InputTokens: 100, OutputTokens: 40, TotalTokens: 140}},
		{"interactions", Interactions,
			`{"id":"i","usage":{"total_input_tokens":100,"total_output_tokens":15,"total_thought_tokens":25,"total_cached_tokens":60,"total_tokens":140}}`,
			apix.Usage{InputTokens: 100, OutputTokens: 40, TotalTokens: 140, CacheReadTokens: 60, ReasoningTokens: 25}},
		{"total derived when absent", ChatCompletions,
			`{"usage":{"prompt_tokens":3,"completion_tokens":4}}`,
			apix.Usage{InputTokens: 3, OutputTokens: 4, TotalTokens: 7}},
		{"invalid counts read as zero", ChatCompletions,
			`{"usage":{"prompt_tokens":-3,"completion_tokens":"4","total_tokens":1.5}}`,
			apix.Usage{}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			tr := New(tt.dialect)
			tr.Body([]byte(tt.body))
			got, ok := tr.Usage()
			if !ok || !reflect.DeepEqual(counts(got), tt.want) {
				t.Fatalf("got %+v %v, want %+v", got, ok, tt.want)
			}
			if !json.Valid(got.Raw) {
				t.Fatalf("raw %s", got.Raw)
			}
		})
	}
}

func TestNoUsage(t *testing.T) {
	for _, body := range []string{`{"id":"x"}`, `{"usage":null}`, `{"usage":[1]}`, `not json`, `{"response":{"usage":null}}`} {
		tr := New(Responses)
		tr.Body([]byte(body))
		if u, ok := tr.Usage(); ok {
			t.Fatalf("%s: got %+v", body, u)
		}
	}
	var nilTracker *Tracker
	nilTracker.Body([]byte(`{"usage":{"input_tokens":1}}`))
	nilTracker.Feed([]byte("data: {}\n\n"))
	nilTracker.End()
	if _, ok := nilTracker.Usage(); ok || New(0) != nil {
		t.Fatal("a nil tracker reported usage")
	}
}

func TestRawIsCopied(t *testing.T) {
	body := []byte(`{"usage":{"prompt_tokens":1}}`)
	tr := New(ChatCompletions)
	tr.Body(body)
	copy(body, "xxxxxxxxxxxxxxxxxxxxxxxxxxxx")
	if u, _ := tr.Usage(); string(u.Raw) != `{"prompt_tokens":1}` {
		t.Fatalf("raw %s", u.Raw)
	}
}

func TestAmbiguousKeysFallBack(t *testing.T) {
	tr := New(ChatCompletions)
	tr.Body([]byte(`{"usage":{"prompt_tokens":1},"usage":{"prompt_tokens":2}}`))
	if u, ok := tr.Usage(); !ok || u.InputTokens != 2 {
		t.Fatalf("got %+v %v", u, ok)
	}
}

// feed runs wire through a tracker whole and byte by byte.
func feed(t *testing.T, d Dialect, wire string) []apix.Usage {
	t.Helper()
	whole := New(d)
	whole.Feed([]byte(wire))
	whole.End()
	bytewise := New(d)
	for i := range len(wire) {
		bytewise.Feed([]byte{wire[i]})
	}
	bytewise.End()
	var out []apix.Usage
	for _, tr := range []*Tracker{whole, bytewise} {
		u, ok := tr.Usage()
		if !ok {
			t.Fatal("no usage")
		}
		out = append(out, u)
	}
	if string(out[0].Raw) != string(out[1].Raw) || !reflect.DeepEqual(counts(out[0]), counts(out[1])) {
		t.Fatalf("whole %+v, bytewise %+v", out[0], out[1])
	}
	return out
}

func TestStreams(t *testing.T) {
	tests := []struct {
		name    string
		dialect Dialect
		wire    string
		want    apix.Usage
		raw     string
	}{
		{"chat trailer", ChatCompletions,
			"data: {\"choices\":[{\"delta\":{\"content\":\"usage\"}}],\"usage\":null}\n\n" +
				"data: {\"choices\":[],\"usage\":{\"prompt_tokens\":5,\"completion_tokens\":7,\"total_tokens\":12}}\n\ndata: [DONE]\n\n",
			apix.Usage{InputTokens: 5, OutputTokens: 7, TotalTokens: 12}, `{"prompt_tokens":5,"completion_tokens":7,"total_tokens":12}`},
		{"responses completed", Responses,
			"event: response.created\ndata: {\"type\":\"response.created\",\"response\":{\"usage\":null}}\n\n" +
				"event: response.completed\ndata: {\"type\":\"response.completed\",\"response\":{\"usage\":{\"input_tokens\":5,\"output_tokens\":7}}}\n\n",
			apix.Usage{InputTokens: 5, OutputTokens: 7, TotalTokens: 12}, `{"input_tokens":5,"output_tokens":7}`},
		{"messages merges start and delta", Messages,
			"event: message_start\ndata: {\"type\":\"message_start\",\"message\":{\"usage\":{\"input_tokens\":5,\"cache_read_input_tokens\":3,\"output_tokens\":1}}}\n\n" +
				"event: message_delta\ndata: {\"type\":\"message_delta\",\"usage\":{\"output_tokens\":7}}\n\n",
			apix.Usage{InputTokens: 8, OutputTokens: 7, TotalTokens: 15, CacheReadTokens: 3}, `{"input_tokens":5,"cache_read_input_tokens":3,"output_tokens":7}`},
		{"gemini keeps the latest cumulative counts", Gemini,
			"data: {\"usageMetadata\":{\"promptTokenCount\":5,\"totalTokenCount\":5}}\n\n" +
				"data: {\"usageMetadata\":{\"promptTokenCount\":5,\"candidatesTokenCount\":7,\"totalTokenCount\":12}}",
			apix.Usage{InputTokens: 5, OutputTokens: 7, TotalTokens: 12}, `{"promptTokenCount":5,"totalTokenCount":12,"candidatesTokenCount":7}`},
		{"interactions completed", Interactions,
			"event: interaction.completed\ndata: {\"event_type\":\"interaction.completed\",\"interaction\":{\"id\":\"i\",\"usage\":{\"total_input_tokens\":5,\"total_output_tokens\":7}}}\n\n",
			apix.Usage{InputTokens: 5, OutputTokens: 7, TotalTokens: 12}, `{"total_input_tokens":5,"total_output_tokens":7}`},
		{"multi-line data", ChatCompletions,
			"data: {\"usage\":\ndata: {\"prompt_tokens\":5}}\n\n",
			apix.Usage{InputTokens: 5, TotalTokens: 5}, `{"prompt_tokens":5}`},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := feed(t, tt.dialect, tt.wire)[0]
			if !reflect.DeepEqual(counts(got), tt.want) || string(got.Raw) != tt.raw {
				t.Fatalf("got %+v raw %s, want %+v raw %s", counts(got), got.Raw, tt.want, tt.raw)
			}
		})
	}
}
