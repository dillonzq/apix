// Package openai provides codecs for OpenAI wire protocols: Chat Completions
// and Responses. Both belong to apix.KindLLM and convert directly to and from
// the other generation protocols in this module (excluding Interactions).
package openai

import (
	"github.com/dillonzq/apix"
	"github.com/dillonzq/apix/internal/llm/codec"
	"github.com/dillonzq/apix/internal/llm/ids"
)

const (
	// ChatCompletions is "openai.chat-completions": POST /v1/chat/completions.
	ChatCompletions = ids.ChatCompletions
	// Responses is "openai.responses": POST /v1/responses. A native Responses
	// exchange reports stored response IDs through apix.StateReporter.
	Responses = ids.Responses
)

// ResponsesOptions configures exchanges whose upstream speaks Responses. Pass
// it, or a pointer to it, as apix.ExchangeOptions.Ext.
type ResponsesOptions struct {
	// NormalizeTools adapts native Responses requests for upstreams that only
	// accept function tools. It flattens namespaces, wraps custom tools,
	// adapts local shell calls, merges additional_tools, and records the
	// identities so responses restore the original tools. Hosted tools and
	// unrelated fields are preserved. It applies when the client also speaks
	// Responses; cross-protocol requests already use function tools.
	NormalizeTools bool
}

// ChatCompletionsCodec returns the Chat Completions codec.
func ChatCompletionsCodec() apix.Codec { return codec.New(ChatCompletions, nil) }

// ResponsesCodec returns the Responses codec. It accepts ResponsesOptions.
func ResponsesCodec() apix.Codec {
	return codec.New(Responses, func(ext any) (codec.Options, bool) {
		switch o := ext.(type) {
		case ResponsesOptions:
			return codec.Options{NormalizeResponsesTools: o.NormalizeTools}, true
		case *ResponsesOptions:
			return codec.Options{NormalizeResponsesTools: o != nil && o.NormalizeTools}, true
		}
		return codec.Options{}, false
	})
}

// Codecs returns every OpenAI codec, for apix.NewRegistry.
func Codecs() []apix.Codec { return []apix.Codec{ChatCompletionsCodec(), ResponsesCodec()} }
