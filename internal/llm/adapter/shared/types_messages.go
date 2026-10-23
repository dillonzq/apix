package shared

import (
	"encoding/json"
	"fmt"
	"strings"

	"github.com/dillonzq/apix/internal/errclass"
	"github.com/dillonzq/apix/internal/llm/reasoning"
)

// ToolResultErrorPrefix marks tool results that arrived with is_error set;
// every Claude-source translator applies it so the flag is never silently
// erased.
const ToolResultErrorPrefix = "[error] "

// ClaudeTool is one Anthropic Messages tool definition (decode-only).
type ClaudeTool struct {
	Name           string          `json:"name"`
	Description    string          `json:"description"`
	InputSchema    json.RawMessage `json:"input_schema"`
	Strict         *bool           `json:"strict,omitempty"`
	Type           string          `json:"type,omitempty"`
	UserLocation   json.RawMessage `json:"user_location,omitempty"`
	AllowedDomains json.RawMessage `json:"allowed_domains,omitempty"`
}

// ClaudeThinking preserves whether a manual budget was explicitly supplied.
type ClaudeThinking struct {
	Type         string `json:"type"`
	BudgetTokens *int64 `json:"budget_tokens,omitempty"`
}

// ClaudeReasoningEffort translates explicit Claude controls without consulting
// model capabilities. Missing enabled/adaptive budgets mean automatic thinking.
func ClaudeReasoningEffort(r *ClaudeRequestRecord) (string, *errclass.Error) {
	if r.Thinking == nil {
		return "", nil
	}
	switch r.Thinking.Type {
	case "disabled":
		return "none", nil
	case "adaptive", "auto":
		if effort := strings.ToLower(strings.TrimSpace(r.ThinkingEffort)); effort != "" {
			return effort, nil
		}
		return "auto", nil
	case "enabled":
		if r.Thinking.BudgetTokens == nil {
			return "auto", nil
		}
		if effort, ok := reasoning.EffortFromBudget(*r.Thinking.BudgetTokens); ok {
			return effort, nil
		}
		return "", &errclass.Error{Class: errclass.ClassUnsupported,
			Message: "thinking.budget_tokens below -1 cannot be converted to a reasoning effort"}
	default:
		return "", &errclass.Error{Class: errclass.ClassUnsupported,
			Message: fmt.Sprintf("thinking.type %q cannot be converted to a reasoning effort", r.Thinking.Type)}
	}
}

// ClaudeBlock is one normalized Anthropic Messages content block produced
// by DecodeClaudeMessages: Kind carries the wire "type" and only the
// fields that kind uses are populated. Image URLs arrive pre-resolved
// through ClaudeImageURL; tool_use Input and tool_result Result stay raw
// so each target renderer applies its own vocabulary and policies.
type ClaudeBlock struct {
	Thinking string // readable thinking, separate from visible text
	Media    *MediaPart
	Kind     string          // text, image, document, tool_use, tool_result, thinking, redacted_thinking, or an unknown wire type
	Text     string          // text blocks
	URL      string          // image blocks: resolved http(s)/data URL
	CallID   string          // tool_use "id" / tool_result "tool_use_id"
	Name     string          // tool_use name
	Input    json.RawMessage // tool_use arguments (absent → nil)
	Result   json.RawMessage // tool_result payload (string or block array)
	IsError  bool            // tool_result is_error flag
}

// ClaudeMessageRecord is one Anthropic Messages message turn: Content is
// set when the wire content was a JSON string, Blocks when it was a block
// array; absent/null content sets neither.
type ClaudeMessageRecord struct {
	Role    string
	Content string
	Blocks  []ClaudeBlock
}

// ClaudeRequestRecord is the normalized decode of an inbound Anthropic
// Messages request body: envelope fields resolved once — max
// tokens defaulted per ClaudeMaxTokens, system flattened per
// ClaudeSystemText, tool_choice classified per DecodeToolChoice — so
// every Claude-source translator decodes identically and keeps only
// target-shape rendering local.
type ClaudeRequestRecord struct {
	MaxTokens      int64
	System         string
	Messages       []ClaudeMessageRecord
	StopSequences  []string
	Tools          []ClaudeTool
	ToolChoiceKind string
	ToolChoiceName string
	Thinking       *ClaudeThinking
	ThinkingEffort string
	Stream         bool
	Temperature    *float64
	TopP           *float64
	OutputConfig   json.RawMessage
}

// claudeWireMessage mirrors the inbound Anthropic message shape (decode-only).
type claudeWireMessage struct {
	Role    string          `json:"role"`
	Content json.RawMessage `json:"content"` // string or block array
}

// claudeWireBlock mirrors the inbound Anthropic block shape (decode-only).
type claudeWireBlock struct {
	Thinking string `json:"thinking"`
	Type     string `json:"type"`
	Text     string `json:"text"`
	Source   *struct {
		Type      string `json:"type"` // base64 | url
		MediaType string `json:"media_type"`
		Data      string `json:"data"`
		URL       string `json:"url"`
	} `json:"source"`
	ID        string          `json:"id"`
	Name      string          `json:"name"`
	Input     json.RawMessage `json:"input"`
	ToolUseID string          `json:"tool_use_id"`
	Content   json.RawMessage `json:"content"` // tool_result payload: string or blocks
	IsError   bool            `json:"is_error"`
}

// DecodeClaudeMessages decodes an inbound Anthropic Messages request body
// into a ClaudeRequestRecord. It is THE Claude-request kernel for every
// adapter translating claude source: one decoder owns malformed-
// input classification (all ClassTranslation) and wording — broken JSON,
// non-string/non-array message content, undecodable block elements, image
// sources (via ClaudeImageURL validation) — so the sibling routes cannot
// diverge again. Unknown block types and roles are recorded verbatim:
// rejecting them names the TARGET endpoint and stays with the renderer.
func DecodeClaudeMessages(body json.RawMessage) (*ClaudeRequestRecord, *errclass.Error) {
	var env struct {
		MaxTokens     int64               `json:"max_tokens"`
		System        json.RawMessage     `json:"system"`
		Messages      []claudeWireMessage `json:"messages"`
		StopSequences []string            `json:"stop_sequences"`
		Tools         []ClaudeTool        `json:"tools"`
		ToolChoice    json.RawMessage     `json:"tool_choice"`
		Thinking      *ClaudeThinking     `json:"thinking"`
		OutputConfig  *struct {
			Effort string          `json:"effort"`
			Format json.RawMessage `json:"format"`
		} `json:"output_config"`
		Stream      bool     `json:"stream"`
		Temperature *float64 `json:"temperature"`
		TopP        *float64 `json:"top_p"`
	}
	if err := json.Unmarshal(body, &env); err != nil {
		return nil, errclass.Translation("malformed claude request JSON: " + err.Error())
	}
	kind, name, eErr := DecodeToolChoice(env.ToolChoice)
	if eErr != nil {
		return nil, eErr
	}
	system, eErr := ClaudeSystemText(env.System)
	if eErr != nil {
		return nil, eErr
	}
	rec := &ClaudeRequestRecord{
		MaxTokens:      ClaudeMaxTokens(env.MaxTokens),
		System:         system,
		StopSequences:  env.StopSequences,
		Tools:          env.Tools,
		ToolChoiceKind: kind,
		ToolChoiceName: name,
		Thinking:       env.Thinking,
		Stream:         env.Stream,
		Temperature:    env.Temperature,
		TopP:           env.TopP,
	}
	if env.OutputConfig != nil {
		rec.ThinkingEffort = env.OutputConfig.Effort
		if len(env.OutputConfig.Format) > 0 {
			rec.OutputConfig, _ = json.Marshal(map[string]json.RawMessage{"format": env.OutputConfig.Format})
		}
	}
	for _, wm := range env.Messages {
		m := ClaudeMessageRecord{Role: wm.Role}
		if HasContent(wm.Content) {
			var s string
			if json.Unmarshal(wm.Content, &s) == nil {
				m.Content = s
			} else {
				blocks, eErr := decodeClaudeBlocks(wm.Content)
				if eErr != nil {
					return nil, eErr
				}
				m.Blocks = blocks
			}
		}
		rec.Messages = append(rec.Messages, m)
	}
	return rec, nil
}

// decodeClaudeBlocks normalizes one block-array message content field.
// A well-formed JSON value that is neither string nor array fails with
// the canonical shape error; an undecodable element fails with the
// canonical block error — never silently dropped.
func decodeClaudeBlocks(raw json.RawMessage) ([]ClaudeBlock, *errclass.Error) {
	var elems []json.RawMessage
	if err := json.Unmarshal(raw, &elems); err != nil {
		return nil, errclass.Translation("message content must be a string or an array of blocks")
	}
	blocks := make([]ClaudeBlock, 0, len(elems))
	for _, elem := range elems {
		var wb claudeWireBlock
		if err := json.Unmarshal(elem, &wb); err != nil {
			return nil, errclass.Translation("malformed content block")
		}
		blk := ClaudeBlock{
			Thinking: wb.Thinking,
			Kind:     wb.Type, Text: wb.Text,
			Name: wb.Name, Input: wb.Input,
			Result: wb.Content, IsError: wb.IsError,
		}
		switch wb.Type {
		case "document":
			m, err := decodeClaudeDocument(elem)
			if err != nil {
				return nil, err
			}
			blk.Media = m
		case "image":
			if wb.Source == nil {
				return nil, errclass.Translation("image block missing source")
			}
			url, eErr := ClaudeImageURL(wb.Source.Type, wb.Source.URL, wb.Source.MediaType, wb.Source.Data)
			if eErr != nil {
				return nil, eErr
			}
			blk.URL = url
		case "tool_use":
			blk.CallID = wb.ID
		case "tool_result":
			blk.CallID = wb.ToolUseID
		}
		blocks = append(blocks, blk)
	}
	return blocks, nil
}
