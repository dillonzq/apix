// Package anthropic provides codecs for Anthropic wire protocols. Messages
// belongs to apix.KindLLM and converts directly to and from every other LLM
// protocol in this module.
package anthropic

import (
	"github.com/dillonzq/apix"
	"github.com/dillonzq/apix/internal/llm/codec"
)

// Messages is POST /v1/messages.
const Messages apix.Protocol = "anthropic.messages"

// ThinkingMode selects how a cross-protocol reasoning level becomes Messages
// thinking. The host chooses it from model capabilities; no model-name
// heuristics run.
type ThinkingMode string

const (
	// ThinkingAdaptive sends thinking.type "adaptive" with the level as
	// output_config.effort. It is the default, required by current Claude models.
	ThinkingAdaptive ThinkingMode = "adaptive"
	// ThinkingManual sends thinking.type "enabled" with a fixed budget_tokens,
	// for Claude models that predate adaptive thinking.
	ThinkingManual ThinkingMode = "manual"
)

// MessagesOptions configures exchanges whose upstream speaks Messages. Pass
// it, or a pointer to it, as apix.ExchangeOptions.Ext.
type MessagesOptions struct {
	// AllowUnsignedThinking replays cross-protocol reasoning history as
	// unsigned thinking blocks, for upstreams that accept them. By default
	// these blocks are omitted. Diagnostics and Safe/Strict loss policies
	// still apply.
	AllowUnsignedThinking bool
	// ThinkingMode defaults to ThinkingAdaptive. Native Messages thinking
	// controls pass through unchanged.
	ThinkingMode ThinkingMode
}

// MessagesCodec returns the Messages codec. It accepts MessagesOptions.
func MessagesCodec() apix.Codec {
	return codec.New(codec.Messages, func(ext any) (codec.Options, bool) {
		var o MessagesOptions
		switch v := ext.(type) {
		case MessagesOptions:
			o = v
		case *MessagesOptions:
			if v != nil {
				o = *v
			}
		default:
			return codec.Options{}, false
		}
		mode := ""
		switch o.ThinkingMode {
		case "", ThinkingAdaptive:
		case ThinkingManual:
			mode = string(ThinkingManual)
		default:
			return codec.Options{}, false
		}
		return codec.Options{AllowUnsignedThinking: o.AllowUnsignedThinking, MessagesThinkingMode: mode}, true
	})
}

// Codecs returns every Anthropic codec, for apix.NewRegistry.
func Codecs() []apix.Codec { return []apix.Codec{MessagesCodec()} }
