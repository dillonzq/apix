// Package translate is the LLM conversion engine behind the public
// protocol/openai, protocol/anthropic and protocol/gemini codecs. It converts requests,
// responses, and SSE streams between Chat Completions, Responses, and
// Messages and Gemini using internal format names and per-execution state.
package translate

import (
	"github.com/dillonzq/apix/internal/errclass"
	"github.com/dillonzq/apix/internal/llm/adapter/shared"
)

// Protocol identifiers are independent of provider and host format names.
// Callers map external identifiers to these values.
const (
	FormatChatCompletions = shared.FormatChatCompletions
	FormatResponses       = shared.FormatResponses
	FormatMessages        = shared.FormatMessages
	FormatGemini          = shared.FormatGemini
)

// RequestState holds tool identities, options, and diagnostics for one execution. Its zero value is ready
// to use. Pass the same state through request and response conversion. Do not
// reuse it across executions or mutate it concurrently with response conversion.
type RequestState struct {
	Options ConversionOptions
	context shared.ConversionContext
}

// NewRequestState creates execution state with the default allow loss policy.
// Unsigned Messages thinking is omitted unless explicitly enabled in Options.
func NewRequestState() *RequestState { return &RequestState{} }

func conversionContext(state *RequestState) *shared.ConversionContext {
	if state == nil {
		return nil
	}
	state.context.Options = state.Options
	return &state.context
}

func validateFormats(formats ...string) *Error {
	for _, format := range formats {
		switch format {
		case FormatChatCompletions, FormatResponses, FormatMessages, FormatGemini:
		default:
			return &Error{Class: ClassUnsupported, Message: errclass.Redact("unsupported protocol format: " + format)}
		}
	}
	return nil
}
