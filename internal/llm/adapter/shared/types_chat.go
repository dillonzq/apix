package shared

import (
	"encoding/json"
)

// CCFunction is one Chat Completions tool function definition (decode and
// emit; omitempty keeps absent descriptions/parameters off the wire).
type CCFunction struct {
	CacheControl json.RawMessage `json:"cache_control,omitempty"`
	Name         string          `json:"name"`
	Description  string          `json:"description,omitempty"`
	Parameters   json.RawMessage `json:"parameters,omitempty"`
	Strict       *bool           `json:"strict,omitempty"`
}

// CCToolCall is one Chat Completions tool call (decode and emit).
type CCToolCall struct {
	ID       string `json:"id"`
	Type     string `json:"type"`
	Function struct {
		Name      string `json:"name"`
		Arguments string `json:"arguments"`
	} `json:"function"`
}

// CCMessage is one Chat Completions message.
type CCMessage struct {
	ReasoningFields
	CacheControl json.RawMessage `json:"cache_control,omitempty"`
	Role         string          `json:"role"`
	Content      json.RawMessage `json:"content"` // string or typed parts
	ToolCalls    []CCToolCall    `json:"tool_calls"`
	ToolCallID   string          `json:"tool_call_id"`
}

// CCTool is one Chat Completions tool definition.
type CCTool struct {
	CacheControl json.RawMessage `json:"cache_control,omitempty"`
	Type         string          `json:"type"`
	Function     CCFunction      `json:"function"`
}

// ChatCompletionsRequest decodes an inbound Chat Completions request
// (superset of what each adapter reads).
type ChatCompletionsRequest struct {
	Messages            []CCMessage     `json:"messages"`
	MaxTokens           *int64          `json:"max_tokens"`
	MaxCompletionTokens *int64          `json:"max_completion_tokens"`
	Stop                json.RawMessage `json:"stop"`
	Tools               []CCTool        `json:"tools"`
	ToolChoice          json.RawMessage `json:"tool_choice"`
	ParallelToolCalls   *bool           `json:"parallel_tool_calls"`
	Stream              bool            `json:"stream"`
	Temperature         *float64        `json:"temperature"`
	TopP                *float64        `json:"top_p"`
	ReasoningEffort     string          `json:"reasoning_effort"`
	ResponseFormat      json.RawMessage `json:"response_format"`
}
