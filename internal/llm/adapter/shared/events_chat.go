package shared

import (
	"encoding/json"
	"time"
)

// ChatChunkBuilder synthesizes chat.completion.chunk frames with one
// shared shape so every openai-target stream synthesizer renders
// identical envelopes: role chunks carry role plus empty
// content, an empty finish renders null, and usage is attached only when
// provided. A zero Created defaults to render-time now.
type ChatChunkBuilder struct {
	ID      string
	Model   string
	Created int64
}

// RoleChunk renders the leading assistant-role chunk.
func (b ChatChunkBuilder) RoleChunk() []byte {
	return b.frame(map[string]any{"role": "assistant", "content": ""}, nil, nil)
}

// Delta renders an intermediate delta chunk (null finish_reason).
func (b ChatChunkBuilder) Delta(delta any) []byte {
	return b.frame(delta, nil, nil)
}

// Finish renders the terminal chunk; an empty finish renders null.
func (b ChatChunkBuilder) Finish(finish string, usage map[string]any) []byte {
	var fr any
	if finish != "" {
		fr = finish
	}
	return b.frame(map[string]any{}, fr, usage)
}

func (b ChatChunkBuilder) frame(delta, finish any, usage map[string]any) []byte {
	created := b.Created
	if created == 0 {
		created = time.Now().Unix()
	}
	frame := map[string]any{
		"id":      b.ID,
		"object":  "chat.completion.chunk",
		"created": created,
		"model":   b.Model,
		"choices": []any{map[string]any{"index": 0, "delta": delta, "finish_reason": finish}},
	}
	if usage != nil {
		frame["usage"] = usage
	}
	raw, _ := json.Marshal(frame)
	return raw
}

// CCToolCallOpeningEntry renders the canonical OPENING tool_calls delta
// entry for a Chat Completions stream: OpenAI puts type on the FIRST
// delta entry only, so it carries index/id/type plus the function name
// and initial arguments fragment; continuation deltas for the same call
// are minimal ({index, function:{arguments}}) and must be built by the
// caller, not through this kernel.
func CCToolCallOpeningEntry(index int, callID, name, argsDelta string) map[string]any {
	return map[string]any{
		"index": index,
		"id":    callID,
		"type":  "function",
		"function": map[string]any{
			"name":      name,
			"arguments": argsDelta,
		},
	}
}
