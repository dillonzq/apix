package shared

import (
	"encoding/json"
	"fmt"
	"strings"

	"github.com/dillonzq/apix/internal/errclass"
)

type rawObject map[string]json.RawMessage

func rawString(obj rawObject, key string) string {
	var value string
	DecodeProbe(obj[key], &value)
	return value
}

func putString(obj rawObject, key, value string) { obj[key], _ = json.Marshal(value) }

// NormalizeResponsesRequest preserves extension fields and exact JSON numbers.
// Identity changes are committed only after the entire request succeeds.
func NormalizeResponsesRequest(body []byte, state *ConversionContext) ([]byte, *errclass.Error) {
	return state.WithToolTransaction(func(work *ConversionContext) ([]byte, *errclass.Error) {
		return normalizeResponsesRequest(body, work)
	})
}

func normalizeResponsesRequest(body []byte, work *ConversionContext) ([]byte, *errclass.Error) {
	var doc rawObject
	if json.Unmarshal(body, &doc) != nil || doc == nil {
		return nil, errclass.Translation("Responses normalization requires a JSON object")
	}
	if _, err := normalizeShellDocument(doc, work); err != nil {
		return nil, err
	}
	var tools []rawObject
	if HasContent(doc["tools"]) && json.Unmarshal(doc["tools"], &tools) != nil {
		return nil, errclass.Translation("tools must be an array of objects")
	}
	var items []rawObject
	inputArray := HasContent(doc["input"]) && strings.HasPrefix(strings.TrimSpace(string(doc["input"])), "[")
	if inputArray {
		if json.Unmarshal(doc["input"], &items) != nil {
			return nil, errclass.Translation("input must contain objects")
		}
	} else if HasContent(doc["input"]) {
		var text string
		if json.Unmarshal(doc["input"], &text) != nil {
			return nil, errclass.Translation("input must be a string or an array of items")
		}
	}
	var history []rawObject
	for _, item := range items {
		if item == nil {
			return nil, errclass.Translation("input must contain objects")
		}
		if rawString(item, "type") == "additional_tools" {
			var extra []rawObject
			if !HasContent(item["tools"]) || json.Unmarshal(item["tools"], &extra) != nil {
				return nil, errclass.Translation("additional_tools requires a tools array")
			}
			tools = append(tools, extra...)
		} else {
			history = append(history, item)
		}
	}
	var flat []rawObject
	declared := make(map[string]bool)
	var flatten func(rawObject, string, string) *errclass.Error
	flatten = func(tool rawObject, namespace, path string) *errclass.Error {
		if tool == nil {
			return errclass.Translation("tools must contain objects")
		}
		typ, name := rawString(tool, "type"), rawString(tool, "name")
		if typ == "namespace" {
			if namespace != "" || strings.TrimSpace(name) == "" {
				return errclass.Translation("namespace requires a name and cannot be nested")
			}
			var children []rawObject
			if !HasContent(tool["tools"]) || json.Unmarshal(tool["tools"], &children) != nil {
				return errclass.Translation("namespace requires a tools array")
			}
			for i, child := range children {
				if err := flatten(child, name, fmt.Sprintf("%s.tools[%d]", path, i)); err != nil {
					return err
				}
			}
			return nil
		}
		if typ != "function" && typ != "custom" {
			if namespace != "" {
				return errclass.Translation("namespace normalization supports function and custom tools only")
			}
			flat = append(flat, tool) // hosted tools stay under caller control
			return nil
		}
		wire, first, err := work.Tools.declareTool(declared, responseToolIdentity{name: name, namespace: namespace, isCustom: typ == "custom"})
		if err != nil {
			return err
		}
		if !first {
			return nil
		}
		putString(tool, "name", wire)
		if typ == "custom" {
			if HasContent(tool["format"]) {
				if err := work.RejectLoss(DiagnosticFor(FormatResponses, FormatResponses, path+".format", true)); err != nil {
					return err
				}
			}
			delete(tool, "format")
			putString(tool, "type", "function")
			tool["parameters"] = json.RawMessage(customToolParameters)
		}
		flat = append(flat, tool)
		return nil
	}
	for i, tool := range tools {
		if err := flatten(tool, "", fmt.Sprintf("tools[%d]", i)); err != nil {
			return nil, err
		}
	}
	rewrite := func(item rawObject) *errclass.Error {
		typ := rawString(item, "type")
		if typ != "function" && typ != "custom" && typ != "function_call" && typ != "custom_tool_call" {
			return nil
		}
		name, ns := rawString(item, "name"), rawString(item, "namespace")
		wire, err := work.Tools.referenceTool(responseToolIdentity{name: name, namespace: ns, isCustom: typ == "custom" || typ == "custom_tool_call"})
		if err != nil {
			return err
		}
		putString(item, "name", wire)
		delete(item, "namespace")
		if typ == "custom_tool_call" {
			var input string
			if json.Unmarshal(item["input"], &input) != nil {
				return errclass.Translation("custom_tool_call input must be a string")
			}
			putString(item, "type", "function_call")
			putString(item, "arguments", wrapCustomInput(input))
			delete(item, "input")
		} else if typ == "custom" {
			putString(item, "type", "function")
		}
		return nil
	}
	for _, item := range history {
		if rawString(item, "type") == "custom_tool_call_output" {
			putString(item, "type", "function_call_output")
		}
		if err := rewrite(item); err != nil {
			return nil, err
		}
	}
	if inputArray {
		if history == nil {
			history = []rawObject{}
		}
		doc["input"], _ = json.Marshal(history)
	}
	if len(tools) > 0 || HasContent(doc["tools"]) {
		if flat == nil {
			flat = []rawObject{}
		}
		doc["tools"], _ = json.Marshal(flat)
	}
	if HasContent(doc["tool_choice"]) && strings.HasPrefix(strings.TrimSpace(string(doc["tool_choice"])), "{") {
		var choice rawObject
		if json.Unmarshal(doc["tool_choice"], &choice) != nil {
			return nil, errclass.Translation("invalid tool_choice")
		}
		if err := rewrite(choice); err != nil {
			return nil, err
		}
		if rawString(choice, "type") == "allowed_tools" {
			var allowed []rawObject
			if json.Unmarshal(choice["tools"], &allowed) != nil {
				return nil, errclass.Translation("allowed_tools requires a tools array")
			}
			for _, item := range allowed {
				if item == nil {
					return nil, errclass.Translation("allowed_tools must contain objects")
				}
				if err := rewrite(item); err != nil {
					return nil, err
				}
			}
			choice["tools"], _ = json.Marshal(allowed)
		}
		doc["tool_choice"], _ = json.Marshal(choice)
	}
	out, err := json.Marshal(doc)
	if err != nil {
		return nil, errclass.Translation("cannot encode normalized Responses request")
	}
	return out, nil
}
