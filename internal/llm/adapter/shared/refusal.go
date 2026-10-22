package shared

// FinishWithRefusal preserves truncation/tool signals while classifying a
// normally completed refusal as content_filter.
func FinishWithRefusal(finish string, refused bool) string {
	if refused && (finish == "" || finish == "stop") {
		return "content_filter"
	}
	return finish
}

func (a *OutputAssembler) AddRefusal(fragment string) {
	if fragment == "" {
		return
	}
	if a.text.Len() == 0 {
		a.refusalFirst = true
	}
	a.refusal.WriteString(fragment)
}

func (e ResponsesEventEmitter) RefusalDelta(id string, index, contentIndex int, text string) []byte {
	return SSEEvent("response.refusal.delta", map[string]any{"type": "response.refusal.delta", "item_id": id, "output_index": index, "content_index": contentIndex, "delta": text})
}

// MessageDone completes every rendered text/refusal part with the same
// content indexes used for deltas, then completes the message item.
func (e ResponsesEventEmitter) MessageDone(item RespItem, index int) [][]byte {
	var parts []struct {
		Type        string           `json:"type"`
		Text        string           `json:"text"`
		Refusal     string           `json:"refusal"`
		Annotations []map[string]any `json:"annotations"`
	}
	DecodeProbe(item.Content, &parts)
	var events [][]byte
	for i, part := range parts {
		content := map[string]any{"type": part.Type}
		payload := map[string]any{"item_id": item.ID, "output_index": index, "content_index": i}
		kind := "response.output_text.done"
		if part.Type == "refusal" {
			kind = "response.refusal.done"
			payload["refusal"] = part.Refusal
			content["refusal"] = part.Refusal
		} else {
			payload["text"] = part.Text
			content["text"] = part.Text
			if len(part.Annotations) > 0 {
				content["annotations"] = part.Annotations
			}
		}
		payload["type"] = kind
		events = append(events, SSEEvent(kind, payload))
		events = append(events, SSEEvent("response.content_part.done", map[string]any{"type": "response.content_part.done", "item_id": item.ID, "output_index": index, "content_index": i, "part": content}))
	}
	return append(events, e.ItemDone(index, item))
}

func (e ResponsesEventEmitter) TextDeltaAt(id string, index, contentIndex int, text string) []byte {
	return SSEEvent("response.output_text.delta", map[string]any{"type": "response.output_text.delta", "item_id": id, "output_index": index, "content_index": contentIndex, "delta": text})
}
