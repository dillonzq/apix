package translate

import (
	"github.com/dillonzq/apix/internal/llm/adapter/chatcompletions"
	"github.com/dillonzq/apix/internal/llm/adapter/gemini"
	"github.com/dillonzq/apix/internal/llm/adapter/messages"
	"github.com/dillonzq/apix/internal/llm/adapter/responses"
	"github.com/dillonzq/apix/internal/llm/adapter/shared"
)

// ConvertRequest converts a request directly and applies the execution loss policy.
func ConvertRequest(targetFormat, upstreamModel, sourceFormat string, payload []byte, state *RequestState) ([]byte, *Error) {
	if err := validateFormats(targetFormat, sourceFormat); err != nil {
		return nil, err
	}
	if sourceFormat == FormatGemini || targetFormat == FormatGemini {
		return gemini.ConvertRequest(sourceFormat, targetFormat, upstreamModel, payload, conversionContext(state))
	}
	if err := conversionContext(state).CheckRequestLosses(sourceFormat, targetFormat, payload); err != nil {
		return nil, err
	}
	if sourceFormat == targetFormat {
		return shared.RewriteModelID(upstreamModel, payload, sourceFormat)
	}
	if sourceFormat == FormatResponses && state == nil {
		return nil, Translation("Responses cross-protocol conversion requires request state")
	}
	ctx := conversionContext(state)
	convert := func(work *shared.ConversionContext) ([]byte, *Error) {
		body := payload
		if sourceFormat == FormatResponses {
			var err *Error
			body, err = shared.NormalizeShellRequest(body, work)
			if err != nil {
				return nil, err
			}
		}
		switch targetFormat {
		case FormatChatCompletions:
			return chatcompletions.BuildRequest(upstreamModel, sourceFormat, body, work)
		case FormatMessages:
			return messages.BuildRequest(upstreamModel, sourceFormat, body, work)
		default:
			return responses.BuildRequest(upstreamModel, sourceFormat, body, work)
		}
	}
	if sourceFormat == FormatResponses {
		return ctx.WithToolTransaction(convert)
	}
	return convert(ctx)
}
