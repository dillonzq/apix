// Package ids is the single definition of the built-in LLM protocol IDs and
// of the engine's internal format names. The vendor packages re-export the
// IDs; the codec, engine and reasoning packages share the mapping.
package ids

import "github.com/dillonzq/apix"

// Public protocol IDs.
const (
	ChatCompletions apix.Protocol = "openai.chat-completions"
	Responses       apix.Protocol = "openai.responses"
	Messages        apix.Protocol = "anthropic.messages"
	GenerateContent apix.Protocol = "gemini.generate-content"
	Interactions    apix.Protocol = "gemini.interactions"
)

// Internal format names used by the adapters and snapshots.
const (
	FormatChatCompletions = "chat-completions"
	FormatResponses       = "responses"
	FormatMessages        = "messages"
	FormatGemini          = "gemini"
	FormatInteractions    = "interactions"
)

var formats = map[apix.Protocol]string{
	ChatCompletions: FormatChatCompletions,
	Responses:       FormatResponses,
	Messages:        FormatMessages,
	GenerateContent: FormatGemini,
	Interactions:    FormatInteractions,
}

// Format returns the internal format name of a public protocol ID.
func Format(p apix.Protocol) (string, bool) {
	f, ok := formats[p]
	return f, ok
}

// Protocol returns the public protocol ID of an internal format name. Unknown
// names are returned unchanged.
func Protocol(format string) apix.Protocol {
	for p, f := range formats {
		if f == format {
			return p
		}
	}
	return apix.Protocol(format)
}
