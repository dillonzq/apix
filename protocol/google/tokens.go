package google

import (
	"github.com/dillonzq/apix"
	"github.com/dillonzq/apix/internal/tokens"
)

// CountTokens is "google.count-tokens": POST
// /v1beta/models/{model}:countTokens. It belongs to apix.KindTokens and
// converts directly to and from anthropic.MessagesCountTokens and
// openai.ResponsesInputTokens. The model lives in the URL; pass it as
// apix.Payload.Metadata. The body holds either contents or a full
// generateContentRequest; converted requests use generateContentRequest so
// system instructions and tools are counted. The reply is {"totalTokens": n}.
const CountTokens = tokens.CountTokens

// CountTokensCodec returns the countTokens codec. It defines no exchange
// options.
func CountTokensCodec() apix.Codec { return tokens.New(CountTokens) }
