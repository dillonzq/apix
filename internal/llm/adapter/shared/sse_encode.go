package shared

import (
	"encoding/json"
)

// SSEEvent renders one named SSE frame.
func SSEEvent(name string, v any) []byte {
	b, _ := json.Marshal(v) // composed marshallable types only; cannot fail
	return SSEFrame(name, b)
}

// SSEFrame renders one named SSE frame around data, a JSON document on one
// line.
func SSEFrame(name string, b []byte) []byte {
	out := make([]byte, 0, len("event: ")+len(name)+len("\ndata: ")+len(b)+2)
	out = append(out, "event: "...)
	out = append(out, name...)
	out = append(out, "\ndata: "...)
	out = append(out, b...)
	return append(out, '\n', '\n')
}
