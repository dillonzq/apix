package shared

import (
	"encoding/json"
	"strings"

	"github.com/dillonzq/apix/sse"
)

// SSEFramer is the public SSE framer; adapters keep the historical name.
type SSEFramer = sse.Framer

// NewSSEFramer prepares a framer; wantRaw enables verbatim block capture.
func NewSSEFramer(wantRaw bool) *SSEFramer { return sse.NewFramer(wantRaw) }

// IsSSEDone reports whether a framer-stripped SSE data payload is the
// terminal [DONE] sentinel; surrounding whitespace and CR are ignored.
func IsSSEDone(payload string) bool { return sse.IsDone(payload) }

// IsJSONObject checks EOF payloads before a missing SSE separator is filled
// in. An event name alone or a partial JSON value cannot establish completion.
func IsJSONObject(payload string) bool {
	payload = strings.TrimSpace(payload)
	return strings.HasPrefix(payload, "{") && json.Valid([]byte(payload))
}
