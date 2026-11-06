package shared

import (
	"bytes"
	"encoding/json"
)

// The per-delta Responses events are structs rather than maps: a stream emits
// one per upstream chunk, and a struct encodes without boxing each value or
// sorting the keys. Fields are declared in key order, so each encodes exactly
// as the map it replaces, which encoding/json renders with sorted keys.
type (
	// contentDeltaEvent is response.output_text.delta or
	// response.refusal.delta.
	contentDeltaEvent struct {
		ContentIndex int    `json:"content_index"`
		Delta        string `json:"delta"`
		ItemID       string `json:"item_id"`
		OutputIndex  int    `json:"output_index"`
		Type         string `json:"type"`
	}
	// itemDeltaEvent is a delta of a tool call's input.
	itemDeltaEvent struct {
		Delta       string `json:"delta"`
		ItemID      string `json:"item_id"`
		OutputIndex int    `json:"output_index"`
		Type        string `json:"type"`
	}
	// summaryDeltaEvent is a delta of the first reasoning summary part.
	summaryDeltaEvent struct {
		Delta        string `json:"delta"`
		ItemID       string `json:"item_id"`
		OutputIndex  int    `json:"output_index"`
		SummaryIndex int    `json:"summary_index"`
		Type         string `json:"type"`
	}
)

// SSEEvent renders one named SSE frame.
func SSEEvent(name string, v any) []byte {
	b, _ := json.Marshal(v) // composed marshallable types only; cannot fail
	return SSEFrame(name, b)
}

// SSEFrame renders one named SSE frame around b, a JSON document. A data
// field ends at a line break, so a document that spans lines (an edited
// upstream payload keeps its whitespace) is compacted first; a line break
// can only be whitespace between JSON tokens.
func SSEFrame(name string, b []byte) []byte {
	if bytes.ContainsAny(b, "\r\n") {
		var compact bytes.Buffer
		if json.Compact(&compact, b) == nil {
			b = compact.Bytes()
		} else {
			// Not JSON: keep every line, each as its own data line.
			b = bytes.ReplaceAll(bytes.ReplaceAll(bytes.ReplaceAll(b, []byte("\r\n"), []byte("\n")), []byte("\r"), []byte("\n")), []byte("\n"), []byte("\ndata: "))
		}
	}
	out := make([]byte, 0, len("event: ")+len(name)+len("\ndata: ")+len(b)+2)
	out = append(out, "event: "...)
	out = append(out, name...)
	out = append(out, "\ndata: "...)
	out = append(out, b...)
	return append(out, '\n', '\n')
}
