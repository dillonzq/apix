package shared

import (
	"encoding/json"
	"fmt"
	"sort"
	"strings"

	"github.com/dillonzq/apix/internal/errclass"
	"github.com/dillonzq/apix/internal/llm/ids"
)

const (
	FormatChatCompletions = ids.FormatChatCompletions
	FormatResponses       = ids.FormatResponses
	FormatMessages        = ids.FormatMessages
	FormatGemini          = ids.FormatGemini
)

// LossPolicy controls request conversions that cannot preserve every field.
type LossPolicy string

const (
	LossPolicyAllow  LossPolicy = "allow"
	LossPolicySafe   LossPolicy = "safe"
	LossPolicyStrict LossPolicy = "strict"
)

// Diagnostic describes a concrete field omitted or approximated by conversion.
// Messages contain field names, never request values or upstream content.
type Diagnostic struct {
	Code     string
	Path     string
	Message  string
	Semantic bool
	From     string
	To       string
}

// ConversionOptions belongs to one RequestState. Safe rejects semantic request
// loss; Strict rejects all reported request loss. Responses report losses but
// are never retried or rejected solely because of this option.
type ConversionOptions struct {
	LossPolicy LossPolicy
	// AllowUnsignedThinking opts into replaying cross-protocol reasoning as
	// unsigned Messages thinking blocks, for upstreams that accept them.
	// The default omits these blocks. Diagnostics and Safe/Strict rejection
	// still apply when enabled; native Messages and response display are unchanged.
	AllowUnsignedThinking bool
	// MessagesThinkingMode selects adaptive (empty, the default) or manual
	// budget thinking when a reasoning level converts to Messages.
	MessagesThinkingMode string
	// GeminiThinkingMode selects "budget" or "level" explicitly. Empty
	// preserves the kind of reasoning control requested by the client.
	GeminiThinkingMode             string
	GeminiIncludeThoughts          *bool
	GeminiRequireThoughtSignatures bool
}

// MessagesThinkingManual selects budget_tokens thinking for Claude models
// that predate adaptive thinking.
const MessagesThinkingManual = "manual"

func (s *ConversionContext) Report(from, to, path string, semantic bool) {
	s.Record(DiagnosticFor(from, to, path, semantic))
}

func (s *ConversionContext) RejectLoss(d Diagnostic) *errclass.Error {
	s.Record(d)
	if s != nil && (s.Options.LossPolicy == LossPolicyStrict || (s.Options.LossPolicy == LossPolicySafe && d.Semantic)) {
		return &errclass.Error{Class: errclass.ClassUnsupported, Message: d.Message}
	}
	return nil
}

func DiagnosticFor(from, to, path string, semantic bool) Diagnostic {
	path = errclass.Redact(path)
	return Diagnostic{Code: "unrepresentable_field", Path: path, Message: "conversion cannot preserve " + path, Semantic: semantic, From: from, To: to}
}

// requestDiagnostics identifies known request losses at protocol field paths
// without inspecting business data inside tool arguments or results.
func requestDiagnostics(from, to string, body []byte) []Diagnostic {
	if from == to {
		return nil
	}
	var doc map[string]json.RawMessage
	if json.Unmarshal(body, &doc) != nil {
		return nil
	}
	var out []Diagnostic
	add := func(path string, semantic bool) { out = append(out, DiagnosticFor(from, to, path, semantic)) }
	unsignedThinking := func(path string) {
		out = append(out, Diagnostic{Code: "unsigned_thinking", Path: path,
			Message:  "conversion cannot create a valid Messages thinking signature for " + path,
			Semantic: true, From: from, To: to})
	}
	// Top-level fields supported by the direct converters. Mapped controls are
	// handled separately; unknown extension fields are reported rather than hidden.
	supported := map[string]bool{"model": true, "stream": true, "temperature": true, "top_p": true, "tools": true, "tool_choice": true, "parallel_tool_calls": true}
	switch from {
	case FormatChatCompletions:
		for _, k := range []string{"messages", "max_tokens", "max_completion_tokens", "reasoning_effort", "response_format"} {
			supported[k] = true
		}
		supported["stop"] = to == FormatMessages
	case FormatResponses:
		for _, k := range []string{"input", "instructions", "max_output_tokens", "reasoning", "text"} {
			supported[k] = true
		}
	case FormatMessages:
		for _, k := range []string{"messages", "system", "max_tokens", "thinking", "output_config"} {
			supported[k] = true
		}
		supported["stop_sequences"] = to == FormatChatCompletions
	}
	for key, raw := range doc {
		if !supported[key] && string(raw) != "null" {
			add(key, key != "metadata" && key != "stream_options" && key != "user")
		}
	}
	var tools []map[string]json.RawMessage
	DecodeProbe(doc["tools"], &tools)
	var inspectTool func(map[string]json.RawMessage, string)
	inspectTool = func(tool map[string]json.RawMessage, path string) {
		var typ, name string
		DecodeProbe(tool["type"], &typ)
		DecodeProbe(tool["name"], &name)
		if from == FormatChatCompletions {
			if typ != "function" {
				add(path, true)
			}
			if HasContent(tool["cache_control"]) && to != FormatMessages {
				add(path+".cache_control", false)
			}
			var fn map[string]json.RawMessage
			DecodeProbe(tool["function"], &fn)
			tool = fn
		} else if from == FormatResponses {
			if typ == "namespace" {
				if HasContent(tool["cache_control"]) {
					add(path+".cache_control", false)
				}
				var children []map[string]json.RawMessage
				DecodeProbe(tool["tools"], &children)
				for i, child := range children {
					var childType string
					DecodeProbe(child["type"], &childType)
					if IsSearchTool(childType) {
						add(fmt.Sprintf("%s.tools[%d]", path, i), true)
					}
					inspectTool(child, fmt.Sprintf("%s.tools[%d]", path, i))
				}
				return
			}
			if typ != "function" && typ != "custom" && !localShell(tool) && !(to == FormatMessages && (typ == "web_search" || typ == "web_search_preview")) {
				add(path, true)
			}
			if typ == "custom" && len(tool["format"]) > 0 {
				add(path+".format", true)
			}
			if (typ == "web_search" || typ == "web_search_preview") && len(tool["search_context_size"]) > 0 {
				add(path+".search_context_size", true)
			}
		} else if typ != "" && typ != "custom" && !strings.HasPrefix(typ, "web_search_") {
			add(path, true)
		} else if strings.HasPrefix(typ, "web_search_") && to != FormatResponses {
			add(path, true)
		}
		if from == FormatMessages && strings.HasPrefix(typ, "web_search_") && to == FormatResponses {
			for _, key := range []string{"max_uses", "blocked_domains"} {
				if len(tool[key]) > 0 {
					add(path+"."+key, true)
				}
			}
		}
		known := map[string]bool{"type": true, "name": true, "description": true, "parameters": true, "input_schema": true, "strict": true, "cache_control": true, "format": true}
		if from == FormatResponses && localShell(tool) {
			known["environment"] = true
		}
		if (from == FormatResponses && (typ == "web_search" || typ == "web_search_preview")) || (from == FormatMessages && strings.HasPrefix(typ, "web_search_")) {
			for _, key := range []string{"user_location", "filters", "allowed_domains", "blocked_domains", "max_uses", "search_context_size"} {
				known[key] = true
			}
		}
		for key := range tool {
			if !known[key] {
				add(path+"."+key, true)
			}
		}
		if HasContent(tool["cache_control"]) && to != FormatMessages {
			add(path+".cache_control", false)
		}
	}
	for i, tool := range tools {
		inspectTool(tool, fmt.Sprintf("tools[%d]", i))
	}
	// Inspect only protocol envelopes and content blocks. Tool input, JSON
	// schemas, and tool result payloads are business data, never traversed.
	inspectBlock := func(block map[string]json.RawMessage, path string) {
		var typ string
		DecodeProbe(block["type"], &typ)
		if from == FormatMessages && typ == "document" {
			for _, key := range []string{"context", "citations"} {
				if HasContent(block[key]) {
					add(path+"."+key, true)
				}
			}
		}
		if from == FormatMessages && typ == "redacted_thinking" {
			add(path, true)
		}
		for _, key := range []string{"cache_control", "signature"} {
			lostCache := to != FormatMessages
			if key == "cache_control" && to == FormatMessages {
				var text string
				DecodeProbe(block["text"], &text)
				lostCache = (typ == "text" || typ == "input_text" || typ == "output_text") && text == ""
			}
			if HasContent(block[key]) && (key != "cache_control" || lostCache) {
				add(path+"."+key, key == "signature" && typ == "thinking")
			}
		}
	}
	inspectToolResult := func(parts []map[string]json.RawMessage, path string) {
		for j, part := range parts {
			partPath := fmt.Sprintf("%s[%d]", path, j)
			inspectBlock(part, partPath)
			if to == FormatMessages && j != len(parts)-1 && HasContent(part["cache_control"]) {
				add(partPath+".cache_control", false)
			}
		}
	}
	if from == FormatMessages || from == FormatChatCompletions {
		var messages []map[string]json.RawMessage
		DecodeProbe(doc["messages"], &messages)
		for i, message := range messages {
			path := fmt.Sprintf("messages[%d]", i)
			var role string
			DecodeProbe(message["role"], &role)
			if HasContent(message["cache_control"]) {
				cacheLost := to != FormatMessages
				if !cacheLost {
					parts, _ := DecodeStringOrParts(message["content"], to)
					var calls []json.RawMessage
					DecodeProbe(message["tool_calls"], &calls)
					cacheLost = len(parts) == 0 && role != "tool" && !(role == "assistant" && len(calls) > 0)
				}
				if cacheLost {
					add(path+".cache_control", false)
				}
			}
			if from == FormatChatCompletions {
				fields := ReasoningFields{ReasoningContent: message["reasoning_content"], Reasoning: message["reasoning"], ReasoningDetails: message["reasoning_details"]}
				selected := fields.ReplayText()
				selectedKey := ""
				for _, key := range []string{"reasoning_content", "reasoning", "reasoning_details"} {
					text, complete := ReplayReasoningText(message[key])
					if text != "" && selectedKey == "" {
						selectedKey = key
					}
					if HasContent(message[key]) && (!complete || (text != "" && text != selected) || role != "assistant") {
						add(path+"."+key, true)
					}
				}
				if role == "assistant" && selected != "" && to == FormatMessages {
					// Text survives, but an authentic Anthropic signature cannot.
					unsignedThinking(path + "." + selectedKey)
				}
			}
			var blocks []map[string]json.RawMessage
			DecodeProbe(message["content"], &blocks)
			if from == FormatChatCompletions && role == "tool" {
				inspectToolResult(blocks, path+".content")
			} else {
				for j, block := range blocks {
					inspectBlock(block, fmt.Sprintf("%s.content[%d]", path, j))
				}
			}
		}
		var system []map[string]json.RawMessage
		DecodeProbe(doc["system"], &system)
		for i, block := range system {
			inspectBlock(block, fmt.Sprintf("system[%d]", i))
		}
	} else if from == FormatResponses {
		var items []map[string]json.RawMessage
		DecodeProbe(doc["input"], &items)
		for i, item := range items {
			path := fmt.Sprintf("input[%d]", i)
			var typ string
			DecodeProbe(item["type"], &typ)
			if typ == "reasoning" {
				text := ReasoningContent(item["content"])
				if HasContent(item["content"]) {
					var parts []map[string]json.RawMessage
					valid := json.Unmarshal(item["content"], &parts) == nil
					for _, p := range parts {
						var kind, value string
						DecodeProbe(p["type"], &kind)
						valid = valid && kind == "reasoning_text" && json.Unmarshal(p["text"], &value) == nil && len(p) == 2
					}
					if !valid {
						add(path+".content", true)
					}
				}
				var summary []ReasoningSummary
				DecodeProbe(item["summary"], &summary)
				var summaryText strings.Builder
				for _, p := range summary {
					summaryText.WriteString(p.Text)
				}
				if summaryText.Len() > 0 && summaryText.String() != text {
					add(path+".summary", true)
				}
				if text != "" && to == FormatMessages {
					unsignedThinking(path + ".content")
				}
				if HasContent(item["encrypted_content"]) {
					add(path+".encrypted_content", true)
				}
			}
			if HasContent(item["cache_control"]) {
				cacheLost := to != FormatMessages || typ == "reasoning" || typ == "additional_tools"
				if !cacheLost && (typ == "message" || typ == "") {
					parts, _ := DecodeStringOrParts(item["content"], to)
					cacheLost = len(parts) == 0
				}
				if cacheLost {
					add(path+".cache_control", false)
				}
			}
			if typ == "message" || (typ == "" && HasContent(item["role"])) {
				var parts []map[string]json.RawMessage
				DecodeProbe(item["content"], &parts)
				for j, part := range parts {
					inspectBlock(part, fmt.Sprintf("%s.content[%d]", path, j))
				}
			}
			if typ == "function_call_output" || typ == "custom_tool_call_output" {
				var parts []map[string]json.RawMessage
				DecodeProbe(item["output"], &parts)
				inspectToolResult(parts, path+".output")
			}
			if typ == "additional_tools" {
				var extra []map[string]json.RawMessage
				DecodeProbe(item["tools"], &extra)
				for j, tool := range extra {
					inspectTool(tool, fmt.Sprintf("%s.tools[%d]", path, j))
				}
			}
		}
	}
	// Report protocol-specific reasoning and output configuration siblings.
	for _, pair := range []struct {
		parent string
		kept   []string
	}{{"reasoning", []string{"effort"}}, {"output_config", []string{"format", "effort"}}, {"text", []string{"format"}}} {
		var obj map[string]json.RawMessage
		DecodeProbe(doc[pair.parent], &obj)
		for k := range obj {
			keep := false
			for _, v := range pair.kept {
				if k == v {
					keep = true
				}
			}
			if !keep {
				add(pair.parent+"."+k, true)
			}
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Path < out[j].Path })
	return out
}

// ValidatePolicy rejects an unknown loss policy before any conversion runs.
func (state *ConversionContext) ValidatePolicy() *errclass.Error {
	if state != nil {
		switch state.Options.LossPolicy {
		case "", LossPolicyAllow, LossPolicySafe, LossPolicyStrict:
		default:
			return errclass.Translation("invalid conversion loss policy")
		}
	}
	return nil
}

func (state *ConversionContext) CheckRequestLosses(from, to string, body []byte) *errclass.Error {
	if err := state.ValidatePolicy(); err != nil {
		return err
	}
	diagnostics := requestDiagnostics(from, to, body)
	for _, d := range diagnostics {
		state.Record(d)
	}
	for _, d := range diagnostics {
		if state != nil && (state.Options.LossPolicy == LossPolicyStrict || (state.Options.LossPolicy == LossPolicySafe && d.Semantic)) {
			return &errclass.Error{Class: errclass.ClassUnsupported, Message: d.Message}
		}
	}
	return nil
}

func (state *ConversionContext) RecordResponseLosses(from, to string, body []byte) {
	var doc map[string]json.RawMessage
	DecodeProbe(body, &doc)
	for _, key := range []string{"metadata", "service_tier", "system_fingerprint", "incomplete_details"} {
		if len(doc[key]) > 0 {
			state.Record(DiagnosticFor(from, to, key, false))
		}
	}
	if from == FormatChatCompletions {
		var choices []map[string]json.RawMessage
		DecodeProbe(doc["choices"], &choices)
		if len(choices) > 1 {
			state.Record(DiagnosticFor(from, to, "choices[1:]", true))
		}
		for i, choice := range choices {
			if len(choice["logprobs"]) > 0 && string(choice["logprobs"]) != "null" {
				state.Record(DiagnosticFor(from, to, fmt.Sprintf("choices[%d].logprobs", i), false))
			}
		}
	}
}
