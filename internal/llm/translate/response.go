package translate

import (
	"github.com/dillonzq/apix/internal/llm/adapter/chatcompletions"
	"github.com/dillonzq/apix/internal/llm/adapter/messages"
	"github.com/dillonzq/apix/internal/llm/adapter/responses"
)

// ConvertResponse converts an upstream response to the client protocol.
func ConvertResponse(upstreamFormat, outputFormat string, status int, body []byte, state *RequestState) ([]byte, *Error) {
	if err := validateFormats(upstreamFormat, outputFormat); err != nil {
		return nil, err
	}
	switch upstreamFormat {
	case FormatChatCompletions:
		return chatcompletions.ConvertNonStreamResponse(outputFormat, status, body, conversionContext(state))
	case FormatMessages:
		return messages.ConvertNonStreamResponse(outputFormat, status, body, conversionContext(state))
	default:
		return responses.ConvertNonStreamResponse(outputFormat, status, body, conversionContext(state))
	}
}
