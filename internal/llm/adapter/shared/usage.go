package shared

// ResponsesUsage is the token-usage block of synthesized Responses
// results and terminal response.completed payloads.
type ResponsesUsage struct {
	InputTokens   int64                   `json:"input_tokens"`
	OutputTokens  int64                   `json:"output_tokens"`
	TotalTokens   int64                   `json:"total_tokens"`
	InputDetails  *ResponsesInputDetails  `json:"input_tokens_details,omitempty"`
	OutputDetails *ResponsesOutputDetails `json:"output_tokens_details,omitempty"`
}

type ResponsesInputDetails struct {
	CachedTokens     *int64 `json:"cached_tokens,omitempty"`
	CacheWriteTokens *int64 `json:"cache_write_tokens,omitempty"`
}

type ResponsesOutputDetails struct {
	ReasoningTokens *int64 `json:"reasoning_tokens,omitempty"`
}

// UsageDetails carries only fields that were present in the source usage.
// Pointer values preserve an explicit zero from an absent field.
type UsageDetails struct {
	CachedTokens     *int64
	CacheWriteTokens *int64
	ReasoningTokens  *int64
}

func ClampSubtract(value int64, parts ...*int64) int64 {
	for _, part := range parts {
		if part != nil {
			value -= *part
		}
	}
	if value < 0 {
		return 0
	}
	return value
}

// NewResponsesUsageFrom builds the struct form of the Responses usage
// block with the majority sum rule: total_tokens is always input+output
// (consistency), never taken from or left zero beside
// nonzero counts. Shared by every adapter emitting a ResponsesUsage struct
// so the computation cannot diverge.
func NewResponsesUsageFrom(inputTokens, outputTokens int64, details UsageDetails) ResponsesUsage {
	usage := ResponsesUsage{
		InputTokens:  inputTokens,
		OutputTokens: outputTokens,
		TotalTokens:  inputTokens + outputTokens,
	}
	if details.CachedTokens != nil || details.CacheWriteTokens != nil {
		usage.InputDetails = &ResponsesInputDetails{
			CachedTokens: details.CachedTokens, CacheWriteTokens: details.CacheWriteTokens,
		}
	}
	if details.ReasoningTokens != nil {
		usage.OutputDetails = &ResponsesOutputDetails{ReasoningTokens: details.ReasoningTokens}
	}
	return usage
}

// CCUsageFrom renders the Chat Completions usage map with total_tokens
// computed as prompt+completion (consistency): the same
// majority sum rule as NewResponsesUsageFrom applied to the Chat
// Completions vocabulary. Shared by every adapter emitting a Chat
// Completions usage block so the computation cannot diverge.
func CCUsageFrom(promptTokens, completionTokens int64, details UsageDetails) map[string]any {
	usage := map[string]any{
		"prompt_tokens":     promptTokens,
		"completion_tokens": completionTokens,
		"total_tokens":      promptTokens + completionTokens,
	}
	if details.CachedTokens != nil || details.CacheWriteTokens != nil {
		promptDetails := map[string]any{}
		if details.CachedTokens != nil {
			promptDetails["cached_tokens"] = *details.CachedTokens
		}
		if details.CacheWriteTokens != nil {
			promptDetails["cache_write_tokens"] = *details.CacheWriteTokens
		}
		usage["prompt_tokens_details"] = promptDetails
	}
	if details.ReasoningTokens != nil {
		usage["completion_tokens_details"] = map[string]any{"reasoning_tokens": *details.ReasoningTokens}
	}
	return usage
}

func ClaudeUsage(input, output int64, cacheRead, cacheWrite *int64) map[string]any {
	usage := map[string]any{"input_tokens": input, "output_tokens": output}
	if cacheRead != nil {
		usage["cache_read_input_tokens"] = *cacheRead
	}
	if cacheWrite != nil {
		usage["cache_creation_input_tokens"] = *cacheWrite
	}
	return usage
}
