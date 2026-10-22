package shared

// ClaudeEventEmitter renders canonical Anthropic Messages SSE frames so
// every claude-target stream synthesizer emits byte-identical shapes
// regardless of serving route: every frame carries a named
// event: line, message_start usage always includes output_tokens:0
// beside input_tokens, and message_delta omits stop_sequence entirely
// (a null member breaks event-line-dispatching clients). Native
// Anthropic frames carry no created/timestamp value, so none is stored.
type ClaudeEventEmitter struct {
	id    string
	model string
}

// NewClaudeEventEmitter binds the emitter to the stream identity.
func NewClaudeEventEmitter(id, model string) ClaudeEventEmitter {
	return ClaudeEventEmitter{id: id, model: model}
}

// MessageStart opens the stream with an empty content list.
func (e ClaudeEventEmitter) MessageStart(inputTokens int64) []byte {
	return SSEEvent("message_start", map[string]any{
		"type": "message_start",
		"message": map[string]any{
			"id": e.id, "type": "message", "role": "assistant",
			"model": e.model, "content": []any{},
			"usage": map[string]any{"input_tokens": inputTokens, "output_tokens": 0},
		},
	})
}

// ContentBlockStart announces one content block; extra carries the
// block-type-specific fields (text, or id/name/input for tool_use).
func (e ClaudeEventEmitter) ContentBlockStart(index int, blockType string, extra map[string]any) []byte {
	block := make(map[string]any, len(extra)+1)
	block["type"] = blockType
	for k, v := range extra {
		block[k] = v
	}
	return SSEEvent("content_block_start", map[string]any{
		"type": "content_block_start", "index": index, "content_block": block,
	})
}

// ContentBlockDelta streams one partial delta (text_delta or
// input_json_delta).
func (e ClaudeEventEmitter) ContentBlockDelta(index int, delta map[string]any) []byte {
	return SSEEvent("content_block_delta", map[string]any{
		"type": "content_block_delta", "index": index, "delta": delta,
	})
}

// ContentBlockStop closes one content block.
func (e ClaudeEventEmitter) ContentBlockStop(index int) []byte {
	return SSEEvent("content_block_stop", map[string]any{
		"type": "content_block_stop", "index": index,
	})
}

// MessageDelta renders the terminal delta; a nil stopReason omits
// stop_reason, and stop_sequence is never emitted. A nil usage map omits
// usage; both serving routes always attach it.
func (e ClaudeEventEmitter) MessageDelta(stopReason *string, usage map[string]any) []byte {
	delta := map[string]any{}
	if stopReason != nil {
		delta["stop_reason"] = *stopReason
	}
	payload := map[string]any{"type": "message_delta", "delta": delta}
	if usage != nil {
		payload["usage"] = usage
	}
	return SSEEvent("message_delta", payload)
}

// MessageStop terminates the stream.
func (e ClaudeEventEmitter) MessageStop() []byte {
	return SSEEvent("message_stop", map[string]any{"type": "message_stop"})
}
