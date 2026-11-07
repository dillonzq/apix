package openai

import (
	"github.com/dillonzq/apix"
	"github.com/dillonzq/apix/internal/counttokens"
)

// ResponsesInputTokens is "openai.responses.input-tokens":
// POST /v1/responses/input_tokens. It belongs to apix.KindCountTokens and
// converts directly to and from anthropic.MessagesCountTokens and
// gemini.CountTokens. Its body is a Responses request without generation
// controls, and the reply is {"object": "response.input_tokens",
// "input_tokens": n}. Chat Completions has no count endpoint.
const ResponsesInputTokens = counttokens.ResponsesInputTokens

// ResponsesInputTokensCodec returns the input_tokens codec. It defines no
// exchange options.
func ResponsesInputTokensCodec() apix.Codec { return counttokens.New(ResponsesInputTokens) }
