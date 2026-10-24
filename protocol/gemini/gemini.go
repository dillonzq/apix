// Package gemini provides codecs for Google Gemini wire protocols.
// GenerateContent belongs to apix.KindLLM and converts directly to and from
// the other generation protocols in this module. Interactions is native only.
// GenerateContentImage and Imagen belong to apix.KindImage; see images.go.
package gemini

import (
	"github.com/dillonzq/apix"
	"github.com/dillonzq/apix/internal/llm/codec"
	"github.com/dillonzq/apix/internal/llm/ids"
)

const (
	// GenerateContent is "gemini.generate-content":
	// POST /v1beta/models/{model}:generateContent, or :streamGenerateContent
	// for streams. The model and mode live in the URL; pass them as
	// apix.Payload.Metadata.
	GenerateContent = ids.GenerateContent
	// Interactions is "gemini.interactions": POST /v1beta/interactions. A
	// native exchange reports created interaction IDs through
	// apix.StateReporter.
	Interactions = ids.Interactions
)

// ThinkingMode selects how reasoning becomes GenerateContent thinkingConfig.
// The host chooses it from model capabilities; no model-name heuristics run.
type ThinkingMode string

const (
	// ThinkingBudget maps reasoning levels to a numeric thinkingBudget.
	ThinkingBudget ThinkingMode = "budget"
	// ThinkingLevel maps positive numeric budgets to a thinkingLevel.
	ThinkingLevel ThinkingMode = "level"
)

// GenerateContentOptions configures an upstream Gemini exchange. Pass it, or
// a pointer to it, as apix.ExchangeOptions.Ext. Zero options preserve the
// client's kind of reasoning control.
type GenerateContentOptions struct {
	// ThinkingMode forces budget or level controls; it also decides how
	// apix.ExchangeOptions.Reasoning is rendered.
	ThinkingMode ThinkingMode
	// IncludeThoughts sets thinkingConfig.includeThoughts when non-nil.
	IncludeThoughts *bool
	// RequireThoughtSignatures rejects historical function calls without a
	// Gemini signature, for models that require signed function-call replay.
	RequireThoughtSignatures bool
}

// GenerateContentCodec returns the GenerateContent codec. It accepts
// GenerateContentOptions.
func GenerateContentCodec() apix.Codec {
	return codec.New(GenerateContent, func(ext any) (codec.Options, bool) {
		var o GenerateContentOptions
		switch v := ext.(type) {
		case GenerateContentOptions:
			o = v
		case *GenerateContentOptions:
			if v != nil {
				o = *v
			}
		default:
			return codec.Options{}, false
		}
		if o.ThinkingMode != "" && o.ThinkingMode != ThinkingBudget && o.ThinkingMode != ThinkingLevel {
			return codec.Options{}, false
		}
		return codec.Options{GeminiThinkingMode: string(o.ThinkingMode), GeminiIncludeThoughts: o.IncludeThoughts, GeminiRequireThoughtSignatures: o.RequireThoughtSignatures}, true
	})
}

// InteractionsCodec supports native request/response/SSE passthrough. Stateful
// IDs, agent requests and background mode remain upstream-owned. It does not
// advertise conversion to another protocol or resolve stored interactions.
func InteractionsCodec() apix.Codec { return codec.NewInteractions() }

// Codecs returns every Gemini codec, for apix.NewRegistry.
func Codecs() []apix.Codec {
	return []apix.Codec{GenerateContentCodec(), InteractionsCodec(), GenerateContentImageCodec(), ImagenCodec()}
}
