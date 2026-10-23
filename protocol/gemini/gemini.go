// Package gemini provides the GenerateContent and native Interactions codecs.
package gemini

import (
	"github.com/dillonzq/apix"
	"github.com/dillonzq/apix/internal/llm/codec"
)

const (
	GenerateContent apix.Protocol = "gemini.generate-content"
	Interactions    apix.Protocol = "gemini.interactions"
)

type ThinkingMode string

const (
	// ThinkingBudget maps reasoning levels to a numeric thinkingBudget.
	ThinkingBudget ThinkingMode = "budget"
	// ThinkingLevel maps positive numeric budgets to a thinkingLevel.
	ThinkingLevel ThinkingMode = "level"
)

// GenerateContentOptions configures an upstream Gemini exchange. The host
// chooses thinking mode from model capabilities; no model-name heuristics run.
// Zero options preserve the client's kind of reasoning control.
type GenerateContentOptions struct {
	ThinkingMode    ThinkingMode
	IncludeThoughts *bool
	// RequireThoughtSignatures rejects historical function calls without a
	// Gemini signature, for models that require signed function-call replay.
	RequireThoughtSignatures bool
}

func GenerateContentCodec() apix.Codec {
	return codec.New(codec.Gemini, func(ext any) (codec.Options, bool) {
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
func InteractionsCodec() apix.Codec { return codec.New(codec.Interactions, nil) }
func Codecs() []apix.Codec          { return []apix.Codec{GenerateContentCodec(), InteractionsCodec()} }
