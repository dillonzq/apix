package shared

import (
	"encoding/json"
	"strings"

	"github.com/dillonzq/apix/internal/errclass"
)

// RespTool is one Responses function tool.
type RespTool struct {
	CacheControl json.RawMessage `json:"cache_control,omitempty"`
	Type         string          `json:"type"` // "function" or "namespace"
	Name         string          `json:"name"`
	Description  string          `json:"description,omitempty"`
	Parameters   json.RawMessage `json:"parameters,omitempty"`
	Tools        []RespTool      `json:"tools,omitempty"`
	Strict       *bool           `json:"strict,omitempty"`
	UserLocation json.RawMessage `json:"user_location,omitempty"`
	Filters      json.RawMessage `json:"filters,omitempty"`
}

// ResponsesRequest decodes an inbound OpenAI Responses request body
// (superset of what each adapter reads). Instructions is a JSON string,
// Input a JSON string or item array.
type ResponsesRequest struct {
	Instructions      json.RawMessage `json:"instructions"`
	Input             json.RawMessage `json:"input"`
	MaxOutputTokens   *int64          `json:"max_output_tokens"`
	Tools             []RespTool      `json:"tools"`
	ToolChoice        json.RawMessage `json:"tool_choice"`
	ParallelToolCalls *bool           `json:"parallel_tool_calls"`
	Stream            bool            `json:"stream"`
	Temperature       *float64        `json:"temperature"`
	TopP              *float64        `json:"top_p"`
	Reasoning         *struct {
		Effort string `json:"effort"`
	} `json:"reasoning"`
	Text json.RawMessage `json:"text"`
}

// DecodeInstructions resolves the instructions field: absent or
// null yields "", a JSON string decodes to its value, and any other
// shape is a translation failure. One kernel serves both Responses-source
// builders so the wording and policy cannot diverge again.
func (r *ResponsesRequest) DecodeInstructions() (string, *errclass.Error) {
	if !HasContent(r.Instructions) {
		return "", nil
	}
	var instr string
	if err := json.Unmarshal(r.Instructions, &instr); err != nil {
		return "", errclass.Translation("instructions must be a string")
	}
	return instr, nil
}

// DecodeInputItems decodes the input field into item form: an item array
// passes through, a plain JSON string surfaces as one synthesized
// user-message item carrying the raw string bytes. Builders drop empty
// strings, and absent or null input decodes
// to zero items. Any other shape is a translation failure. One kernel owns
// the string-or-items discrimination for both Responses-source builders so
// both use the same validation.
func (r *ResponsesRequest) DecodeInputItems() ([]RespItem, *errclass.Error) {
	if !HasContent(r.Input) {
		return nil, nil
	}
	var s string
	if err := json.Unmarshal(r.Input, &s); err == nil {
		return []RespItem{{Type: "message", Role: "user", Content: r.Input}}, nil
	}
	var items []RespItem
	if err := json.Unmarshal(r.Input, &items); err != nil {
		return nil, errclass.Translation("input must be a string or an array of items: " + err.Error())
	}
	for i := range items {
		if items[i].Type == "" && strings.TrimSpace(items[i].Role) != "" {
			items[i].Type = "message"
		}
	}
	return items, nil
}

// RespItem is one Responses input/output item (message, function_call,
// or function_call_output). ID carries the item identity of synthesized
// terminal message items (sibling parity: the Chat Completions
// and Messages routes emit the same "id" the streaming
// response.output_item.added announcement used); function_call items stay
// call_id-only per the documented canonical shape.
type RespItem struct {
	CacheControl json.RawMessage    `json:"cache_control,omitempty"`
	Type         string             `json:"type"`
	ID           string             `json:"id,omitempty"`
	Role         string             `json:"role,omitempty"`
	Content      json.RawMessage    `json:"content,omitempty"`
	CallID       string             `json:"call_id,omitempty"`
	Name         string             `json:"name,omitempty"`
	Namespace    string             `json:"namespace,omitempty"`
	Arguments    string             `json:"arguments,omitempty"`
	Input        string             `json:"input,omitempty"`
	Output       json.RawMessage    `json:"output,omitempty"`
	Summary      []ReasoningSummary `json:"summary,omitempty"`
	Tools        []RespTool         `json:"tools,omitempty"`
	Action       json.RawMessage    `json:"action,omitempty"`
	Status       string             `json:"status,omitempty"`
	Environment  json.RawMessage    `json:"environment,omitempty"`
}
