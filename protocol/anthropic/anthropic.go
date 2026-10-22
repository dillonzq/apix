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

// MessagesOptions configures exchanges whose upstream speaks Messages. Pass
// it, or a pointer to it, as apix.ExchangeOptions.Ext.
type MessagesOptions struct {
	// AllowUnsignedThinking replays cross-protocol reasoning history as
	// unsigned thinking blocks, for upstreams that accept them. By default
	// these blocks are omitted. Diagnostics and Safe/Strict loss policies
	// still apply.
	AllowUnsignedThinking bool
}

// MessagesCodec returns the Messages codec. It accepts MessagesOptions.
func MessagesCodec() apix.Codec {
	return codec.New(codec.Messages, func(ext any) (codec.Options, bool) {
		switch o := ext.(type) {
		case MessagesOptions:
			return codec.Options{AllowUnsignedThinking: o.AllowUnsignedThinking}, true
		case *MessagesOptions:
			return codec.Options{AllowUnsignedThinking: o != nil && o.AllowUnsignedThinking}, true
		}
		return codec.Options{}, false
	})
}

// Codecs returns every Anthropic codec, for apix.NewRegistry.
func Codecs() []apix.Codec { return []apix.Codec{MessagesCodec()} }
