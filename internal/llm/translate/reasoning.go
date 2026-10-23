package translate

import (
	"github.com/dillonzq/apix"
	"github.com/dillonzq/apix/internal/llm/adapter/shared"
	"github.com/dillonzq/apix/internal/llm/reasoning"
)

// StripReasoning removes the client's reasoning control from a source body
// when an override will replace it, so conversion cannot reject it first.
func StripReasoning(targetFormat, sourceFormat string, body []byte) []byte {
	return reasoning.Strip(targetFormat, sourceFormat, body)
}

// ApplyReasoning replaces the reasoning control of a request already
// converted from sourceFormat to targetFormat, rendering it with the
// execution's Messages and Gemini thinking modes. Approximated values are
// request losses under the execution's loss policy.
func ApplyReasoning(targetFormat, sourceFormat string, body []byte, r apix.Reasoning, state *RequestState) ([]byte, *Error) {
	var opts reasoning.Options
	if state != nil {
		opts.MessagesManual = state.Options.MessagesThinkingMode == shared.MessagesThinkingManual
		opts.GeminiMode = state.Options.GeminiThinkingMode
	}
	out, approximated, err := reasoning.Apply(targetFormat, body, r, opts)
	if err != nil {
		return nil, err
	}
	if ctx := conversionContext(state); ctx != nil {
		for _, path := range approximated {
			if err := ctx.RejectLoss(shared.DiagnosticFor(sourceFormat, targetFormat, path, false)); err != nil {
				return nil, err
			}
		}
	}
	return out, nil
}
