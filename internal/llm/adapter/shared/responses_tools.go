package shared

import (
	"encoding/json"
	"fmt"
	"strings"

	"github.com/dillonzq/apix/internal/errclass"
)

// normalizeCustomTool omits hosted tools from function-only adapters. The
// public boundary maps representable hosted search declarations and reports
// other losses. Custom tools use a required string input wrapper.
func normalizeCustomTool(tool *RespTool, target string) bool {
	if tool.Type == "tool_search" || tool.Type == "image_generation" {
		return false
	}
	if target == "/v1/chat/completions" &&
		(tool.Type == "web_search" || tool.Type == "web_search_preview") {
		return false
	}
	if tool.Type == "custom" {
		tool.Type = "function"
		if len(tool.Parameters) == 0 {
			tool.Parameters = json.RawMessage(customToolParameters)
		}
	}
	return true
}

// Normalize merges additional_tools input items into r.Tools, unrolls
// namespace tools to flattened function tools, and rewrites historical
// function_call items plus a namespaced tool_choice to wire names. It
// returns the conversation items with additional_tools stripped. r.Tools
// and r.ToolChoice are rewritten in place.
func (t *ConversionContext) Normalize(r *ResponsesRequest, target string) ([]RespItem, *errclass.Error) {
	if t.Tools.names == nil {
		t.Tools.names = make(map[string]responseToolIdentity)
	}
	items, eErr := r.DecodeInputItems()
	if eErr != nil {
		return nil, eErr
	}
	// Keep original declaration paths even after additional_tools are merged.
	toolPaths := make([]string, len(r.Tools))
	for i := range r.Tools {
		toolPaths[i] = fmt.Sprintf("tools[%d]", i)
	}
	var conv []RespItem
	for i, item := range items {
		if item.Type == "additional_tools" {
			if item.Tools == nil {
				return nil, errclass.Translation("additional_tools requires a tools array")
			}
			r.Tools = append(r.Tools, item.Tools...)
			for j := range item.Tools {
				toolPaths = append(toolPaths, fmt.Sprintf("input[%d].tools[%d]", i, j))
			}
			continue
		}
		conv = append(conv, item)
	}
	var flat []RespTool
	declared := make(map[string]bool)
	for i, tool := range r.Tools {
		if tool.Type != "namespace" {
			wasCustom := tool.Type == "custom"
			if !normalizeCustomTool(&tool, target) {
				continue
			}
			// Hosted tools retain their own protocol-specific validation.
			if tool.Type == "function" {
				_, first, err := t.Tools.declareTool(declared, responseToolIdentity{name: tool.Name, isCustom: wasCustom})
				if err != nil {
					return nil, err
				}
				if !first {
					if target == "/v1/messages" && HasContent(tool.CacheControl) {
						if err := t.RejectLoss(DiagnosticFor(FormatResponses, FormatMessages, toolPaths[i]+".cache_control", false)); err != nil {
							return nil, err
						}
					}
					continue
				}
			}
			flat = append(flat, tool)
			continue
		}
		if strings.TrimSpace(tool.Name) == "" || tool.Tools == nil {
			return nil, errclass.Translation("namespace tool requires a name and tools array")
		}
		for j, child := range tool.Tools {
			if IsSearchTool(child.Type) && target != "/v1/responses" {
				continue
			}
			wasCustom := child.Type == "custom"
			if !normalizeCustomTool(&child, target) {
				continue
			}
			if eErr := FunctionTool(child.Type, target); eErr != nil {
				return nil, eErr
			}
			qualified, first, err := t.Tools.declareTool(declared, responseToolIdentity{name: child.Name, namespace: tool.Name, isCustom: wasCustom})
			if err != nil {
				return nil, err
			}
			if !first {
				// The discarded declaration's hint has no outbound block. Report
				// it where normalization actually removes it, not in preflight.
				if target == "/v1/messages" && HasContent(child.CacheControl) {
					path := fmt.Sprintf("%s.tools[%d].cache_control", toolPaths[i], j)
					if err := t.RejectLoss(DiagnosticFor(FormatResponses, FormatMessages, path, false)); err != nil {
						return nil, err
					}
				}
				continue // duplicate identical declaration: first wins
			}
			child.Name = qualified
			flat = append(flat, child)
		}
	}
	r.Tools = flat
	for i := range conv {
		if conv[i].Type != "function_call" && conv[i].Type != "custom_tool_call" {
			continue
		}
		wire, err := t.Tools.referenceTool(responseToolIdentity{name: conv[i].Name, namespace: conv[i].Namespace, isCustom: conv[i].Type == "custom_tool_call"})
		if err != nil {
			return nil, err
		}
		conv[i].Name = wire
		conv[i].Namespace = ""
	}
	if HasContent(r.ToolChoice) {
		var choice rawObject
		if json.Unmarshal(r.ToolChoice, &choice) == nil {
			typ := rawString(choice, "type")
			if typ == "function" || typ == "custom" || typ == "" && rawString(choice, "namespace") != "" {
				wire, err := t.Tools.referenceTool(responseToolIdentity{name: rawString(choice, "name"), namespace: rawString(choice, "namespace"), isCustom: typ == "custom"})
				if err != nil {
					return nil, err
				}
				if rawString(choice, "namespace") != "" || typ == "custom" {
					putString(choice, "name", wire)
					delete(choice, "namespace")
					if typ == "custom" {
						putString(choice, "type", "function")
					}
					r.ToolChoice, _ = json.Marshal(choice)
				}
			}
		}
	}
	return conv, nil
}
