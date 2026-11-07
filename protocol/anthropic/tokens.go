package anthropic

import (
	"github.com/dillonzq/apix"
	"github.com/dillonzq/apix/internal/tokens"
)

// MessagesCountTokens is "anthropic.messages.count-tokens":
// POST /v1/messages/count_tokens. It belongs to apix.KindTokens and
// converts directly to and from openai.ResponsesInputTokens and
// gemini.CountTokens. Its body is a Messages request without generation
// controls, and the reply is {"input_tokens": n}.
const MessagesCountTokens = tokens.MessagesCountTokens

// MessagesCountTokensCodec returns the count_tokens codec. It defines no
// exchange options.
func MessagesCountTokensCodec() apix.Codec { return tokens.New(MessagesCountTokens) }
