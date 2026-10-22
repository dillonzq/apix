package shared

import (
	"encoding/json"
	"strings"
)

// CCResponseMessage emits the canonical reasoning_content field.
type CCResponseMessage struct {
	CCMessage
	ReasoningContent string           `json:"reasoning_content,omitempty"`
	Refusal          string           `json:"refusal,omitempty"`
	Annotations      []map[string]any `json:"annotations,omitempty"`
}

// ReasoningFields accepts reasoning_content and provider aliases.
// The first field with readable text wins; encrypted metadata is never text.
type ReasoningFields struct {
	ReasoningContent json.RawMessage `json:"reasoning_content,omitempty"`
	Reasoning        json.RawMessage `json:"reasoning,omitempty"`
	ReasoningDetails json.RawMessage `json:"reasoning_details,omitempty"`
}

// Text selects the first readable reasoning alias for response display.
func (r ReasoningFields) Text() string {
	for _, raw := range []json.RawMessage{r.ReasoningContent, r.Reasoning, r.ReasoningDetails} {
		var node any
		if json.Unmarshal(raw, &node) == nil {
			if text := reasoningText(node); text != "" {
				return text
			}
		}
	}
	return ""
}

// ReplayText excludes summaries and opaque provider records. Response display
// parsing is intentionally more permissive than request replay parsing.
func (r ReasoningFields) ReplayText() string {
	for _, raw := range []json.RawMessage{r.ReasoningContent, r.Reasoning, r.ReasoningDetails} {
		if text, _ := ReplayReasoningText(raw); text != "" {
			return text
		}
	}
	return ""
}

// ReplayReasoningText also reports whether the entire field is representable.
// Metadata, encryption, signatures and unknown variants are never plain text.
func ReplayReasoningText(raw json.RawMessage) (string, bool) {
	if !HasContent(raw) {
		return "", true
	}
	var s string
	if json.Unmarshal(raw, &s) == nil {
		return s, true
	}
	var parts []json.RawMessage
	if json.Unmarshal(raw, &parts) == nil {
		var b strings.Builder
		complete := true
		for _, p := range parts {
			text, ok := ReplayReasoningText(p)
			b.WriteString(text)
			complete = complete && ok
		}
		return b.String(), complete
	}
	var obj map[string]json.RawMessage
	if json.Unmarshal(raw, &obj) != nil {
		return "", false
	}
	var typ string
	DecodeProbe(obj["type"], &typ)
	if typ != "" && typ != "text" && typ != "reasoning_text" && typ != "reasoning.text" {
		return "", false
	}
	if json.Unmarshal(obj["text"], &s) != nil {
		return "", false
	}
	complete := true
	for k := range obj {
		if k != "type" && k != "text" {
			complete = false
		}
	}
	return s, complete
}

// reasoningText flattens readable response fragments while excluding typed
// summaries and opaque provider records from replayable text.
func reasoningText(node any) string {
	switch v := node.(type) {
	case string:
		return v
	case []any:
		var b strings.Builder
		for _, part := range v {
			b.WriteString(reasoningText(part))
		}
		return b.String()
	case map[string]any:
		if typ, _ := v["type"].(string); typ != "" && typ != "text" && typ != "reasoning_text" && typ != "reasoning.text" {
			return ""
		}
		if text, ok := v["text"].(string); ok {
			return text
		}
	}
	return ""
}

// ReasoningContent decodes replayable Responses content. A display summary is
// deliberately not a fallback for missing original reasoning.
func ReasoningContent(raw json.RawMessage) string {
	var parts []ReasoningSummary
	if json.Unmarshal(raw, &parts) != nil {
		return ""
	}
	var b strings.Builder
	for _, p := range parts {
		if p.Type == "reasoning_text" {
			b.WriteString(p.Text)
		}
	}
	return b.String()
}

// ReasoningItem keeps the source text available for replay in content as well
// as display in summary. No encryption or provider signature is fabricated.
func ReasoningItem(id, text string) RespItem {
	content, _ := json.Marshal([]ReasoningSummary{{Type: "reasoning_text", Text: text}})
	return RespItem{Type: "reasoning", ID: id, Content: content, Summary: []ReasoningSummary{{Type: "summary_text", Text: text}}}
}

type ReasoningSummary struct {
	Type string `json:"type,omitempty"`
	Text string `json:"text"`
}

func (a *OutputAssembler) AppendReasoning(id, text string) {
	a.items = append(a.items, ReasoningItem(id, text))
}

func (e ResponsesEventEmitter) ReasoningAdded(id string, index int) [][]byte {
	return [][]byte{
		e.ItemAdded(index, map[string]any{"type": "reasoning", "id": id, "summary": []any{}}),
		SSEEvent("response.reasoning_summary_part.added", map[string]any{"type": "response.reasoning_summary_part.added", "item_id": id, "output_index": index, "summary_index": 0, "part": ReasoningSummary{Type: "summary_text"}}),
	}
}

func (e ResponsesEventEmitter) ReasoningDelta(id string, index int, text string) []byte {
	return SSEEvent("response.reasoning_summary_text.delta", map[string]any{"type": "response.reasoning_summary_text.delta", "item_id": id, "output_index": index, "summary_index": 0, "delta": text})
}

func (e ResponsesEventEmitter) ReasoningDone(item RespItem, index int) [][]byte {
	text := ""
	for _, p := range item.Summary {
		text += p.Text
	}
	return [][]byte{
		SSEEvent("response.reasoning_summary_text.done", map[string]any{"type": "response.reasoning_summary_text.done", "item_id": item.ID, "output_index": index, "summary_index": 0, "text": text}),
		SSEEvent("response.reasoning_summary_part.done", map[string]any{"type": "response.reasoning_summary_part.done", "item_id": item.ID, "output_index": index, "summary_index": 0, "part": ReasoningSummary{Type: "summary_text", Text: text}}),
		e.ItemDone(index, item),
	}
}
