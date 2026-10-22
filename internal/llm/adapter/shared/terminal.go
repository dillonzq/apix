package shared

// ClaudeStopToFinish maps Anthropic Messages stop reasons to Chat
// Completions finish reasons. One table serves the messages
// adapter's streaming and non-stream paths so they cannot diverge:
// refusal maps to content_filter, and unmapped reasons fall back to stop
// by the best-effort policy. The pair with FinishToClaudeStop is
// a symmetric round-trip (refusal ↔ content_filter).
func ClaudeStopToFinish(stop string) string {
	switch stop {
	case "tool_use":
		return "tool_calls"
	case "max_tokens":
		return "length"
	case "refusal":
		return "content_filter"
	default: // end_turn, stop_sequence, unknown
		return "stop"
	}
}

// FinishToClaudeStop is the inverse of ClaudeStopToFinish: Chat
// Completions finish reasons to Messages stop reasons, served from this
// one shared table so the chatcompletions adapter's streaming and
// non-stream paths match it exactly. content_filter maps to refusal so a
// mapped reason round-trips; unmapped reasons fall back to end_turn by
// the best-effort policy.
func FinishToClaudeStop(finish string) string {
	switch finish {
	case "tool_calls":
		return "tool_use"
	case "length":
		return "max_tokens"
	case "content_filter":
		return "refusal"
	default: // stop, stop_sequence, unknown
		return "end_turn"
	}
}

// TerminalReason resolves a converted result's terminal stop/finish
// reason when two signals exist: observed tool calls outrank the
// status-derived reason, so a terminal "incomplete" cannot downgrade
// tool_calls to length. toolReason/statusReason carry the target
// protocol's vocabulary ("tool_calls"/"length" for Chat Completions,
// "tool_use"/"max_tokens" for Messages). Responses-target STATUS fields
// are exempt — status derives positionally (truncation→incomplete) because
// tool calls are represented by output items, not status vocabulary;
// applies to CC finish_reason and Claude stop_reason vocabularies only.
func TerminalReason(toolCallsSeen bool, toolReason, statusReason string) string {
	if toolCallsSeen {
		return toolReason
	}
	return statusReason
}

// ResponseStatusFromCCFinish maps a Chat Completions finish_reason into
// the Responses status vocabulary: only "length" is truncation
// and maps to incomplete; every other reason — including unknown or
// absent values — completes the response. Shared by the non-streaming
// converter and the stream terminal so the mapping cannot diverge across
// the sibling routes (consistency).
func ResponseStatusFromCCFinish(finish string) string {
	if finish == "length" {
		return "incomplete"
	}
	return "completed"
}

// ResponseStatusFromClaudeStop maps a Messages stop_reason into the
// Responses status vocabulary: only "max_tokens" is truncation
// and maps to incomplete; every other reason — including unknown or
// absent values — completes the response. Shared by the non-streaming
// converter and the stream message_stop handler so the mapping cannot
// diverge across the sibling routes (consistency).
func ResponseStatusFromClaudeStop(stop string) string {
	if stop == "max_tokens" {
		return "incomplete"
	}
	return "completed"
}

// CCFinishFromResponseStatus maps a Responses result status back to the
// Chat Completions finish_reason vocabulary (inverse of
// ResponseStatusFromCCFinish) so synthesized non-Responses output cannot
// drift from the forward mapping.
func CCFinishFromResponseStatus(status string) string {
	if status == "incomplete" {
		return "length"
	}
	return "stop"
}

// ClaudeStopFromResponseStatus maps a Responses result status back to the
// Anthropic stop_reason vocabulary (inverse of
// ResponseStatusFromClaudeStop) so synthesized non-Responses output cannot
// drift from the forward mapping.
func ClaudeStopFromResponseStatus(status string) string {
	if status == "incomplete" {
		return "max_tokens"
	}
	return "end_turn"
}

// ClaudeMaxTokens applies the max_tokens policy shared by every
// Claude-source converter: a positive value passes through, and an
// absent, zero, or negative value defaults to 4096 instead of failing
// translation or silently dropping the cap. One kernel serves all legs
// so identical Anthropic requests cannot diverge between rejecting,
// unbounding, and defaulting depending on the routed model.
func ClaudeMaxTokens(maxTokens int64) int64 {
	if maxTokens > 0 {
		return maxTokens
	}
	return 4096
}
