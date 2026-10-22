package shared

// CompletionEnvelope renders the top-level chat.completion envelope so
// every cross-format converter emits an identical shape: one
// choice entry plus prompt/completion usage.
func CompletionEnvelope(id, model string, created int64, choices []map[string]any, usage map[string]any) map[string]any {
	return map[string]any{
		"id":      id,
		"object":  "chat.completion",
		"created": created,
		"model":   model,
		"choices": choices,
		"usage":   usage,
	}
}

// NewClaudeResult renders the non-stream Anthropic Messages response
// envelope so every claude-target converter emits an identical shape
// regardless of serving route — the Messages sibling of
// CompletionEnvelope/ResponsesResult. One policy per field: content is
// always a JSON array (nil normalizes to []), usage always carries both
// counters, and stop_sequence is never emitted — neither cross-format
// source vocabulary (CC finish_reason, Responses status) carries a stop
// sequence, so the key stays absent instead of null, matching the
// streaming message_delta sibling that omits it entirely. Callers pass
// blocks whose builders already drop empty text parts, so no dead text
// block ships.
func NewClaudeResult(id, model, stopReason string, inputTokens, outputTokens int64, cacheRead, cacheCreation *int64, content []map[string]any) map[string]any {
	if content == nil {
		content = []map[string]any{}
	}
	return map[string]any{
		"id":          id,
		"type":        "message",
		"role":        "assistant",
		"model":       model,
		"content":     content,
		"stop_reason": stopReason,
		"usage":       ClaudeUsage(inputTokens, outputTokens, cacheRead, cacheCreation),
	}
}

// ResponsesResult is the non-stream Responses result body synthesized
// for Responses clients; one shared shape keeps the Chat
// Completions and Messages routes byte-identical in structure
// (including the model field).
type ResponsesResult struct {
	ID     string         `json:"id"`
	Object string         `json:"object"`
	Model  string         `json:"model"`
	Status string         `json:"status"`
	Output []any          `json:"output"`
	Usage  ResponsesUsage `json:"usage"`
}
