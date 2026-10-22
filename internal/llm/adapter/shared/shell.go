package shared

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"strings"

	"github.com/dillonzq/apix/internal/errclass"
)

// ShellFunctionName is reserved only when a request uses a local shell.
const ShellFunctionName = "shell"

// ShellItemID separates the item identity from call_id and stays stable across
// synthesized added/done events and the final response snapshot.
func ShellItemID(callID string) string {
	digest := sha256.Sum256([]byte(callID))
	return "sh_" + hex.EncodeToString(digest[:12])
}

const shellParameters = `{"type":"object","properties":{"commands":{"type":"array","items":{"type":"string"},"minItems":1},"timeout_ms":{"type":["integer","null"]},"max_output_length":{"type":["integer","null"]}},"required":["commands"],"additionalProperties":false}`

const shellEnvironment = `{"type":"local"}`

// shellResponseAction fills nullable fields required on response items without
// changing the optional fields accepted in requests and historical calls.
func shellResponseAction(action json.RawMessage) json.RawMessage {
	var fields rawObject
	if json.Unmarshal(action, &fields) != nil || fields == nil {
		return action
	}
	for _, key := range []string{"timeout_ms", "max_output_length"} {
		if _, ok := fields[key]; !ok {
			fields[key] = json.RawMessage("null")
		}
	}
	result, _ := json.Marshal(fields)
	return result
}

func localShell(tool rawObject) bool {
	if rawString(tool, "type") != "shell" {
		return false
	}
	if !HasContent(tool["environment"]) {
		return true
	}
	var env rawObject
	return json.Unmarshal(tool["environment"], &env) == nil && rawString(env, "type") == "local"
}

func validateLocalShellEnvironment(raw json.RawMessage) *errclass.Error {
	if !HasContent(raw) {
		return nil
	}
	var env rawObject
	if json.Unmarshal(raw, &env) != nil {
		return errclass.Translation("shell environment must be an object")
	}
	for key := range env {
		if key != "type" && key != "skills" {
			return &errclass.Error{Class: errclass.ClassUnsupported, Message: "local shell environment field cannot be represented by function tools"}
		}
	}
	if rawSkills, present := env["skills"]; present {
		var skills []json.RawMessage
		if json.Unmarshal(rawSkills, &skills) != nil || skills == nil {
			return errclass.Translation("local shell skills must be an array")
		}
		if len(skills) != 0 {
			return &errclass.Error{Class: errclass.ClassUnsupported, Message: "local shell skills cannot be represented by function tools"}
		}
	}
	return nil
}

// ShellAction validates the function arguments without rounding JSON numbers.
func ShellAction(args string) (json.RawMessage, *errclass.Error) {
	var action rawObject
	if json.Unmarshal([]byte(args), &action) != nil || action == nil {
		return nil, errclass.Translation("shell action must be an object")
	}
	for key := range action {
		if key != "commands" && key != "timeout_ms" && key != "max_output_length" {
			return nil, errclass.Translation("shell action contains unsupported properties")
		}
	}
	var commands []string
	if json.Unmarshal(action["commands"], &commands) != nil || len(commands) == 0 {
		return nil, errclass.Translation("shell action requires a nonempty commands array")
	}
	for _, command := range commands {
		if strings.TrimSpace(command) == "" {
			return nil, errclass.Translation("shell commands must be nonempty strings")
		}
	}
	for _, key := range []string{"timeout_ms", "max_output_length"} {
		if HasContent(action[key]) {
			var n int64
			if json.Unmarshal(action[key], &n) != nil || n < 0 {
				return nil, errclass.Translation("shell limits must be nonnegative integers or null")
			}
		}
	}
	raw, _ := json.Marshal(action)
	return raw, nil
}

// ValidateShellCallID rejects incomplete identities before an item is restored.
func ValidateShellCallID(callID string) *errclass.Error {
	if strings.TrimSpace(callID) == "" {
		return errclass.Translation("shell call requires a nonblank call_id")
	}
	return nil
}

func shellHistory(item rawObject) *errclass.Error {
	if err := ValidateShellCallID(rawString(item, "call_id")); err != nil {
		return err
	}
	switch rawString(item, "type") {
	case "shell_call":
		if err := validateLocalShellEnvironment(item["environment"]); err != nil {
			return err
		}
		action, err := ShellAction(string(item["action"]))
		if err != nil {
			return err
		}
		putString(item, "type", "function_call")
		putString(item, "name", ShellFunctionName)
		putString(item, "arguments", string(action))
		delete(item, "action")
		delete(item, "environment")
	case "shell_call_output":
		var chunks []rawObject
		if json.Unmarshal(item["output"], &chunks) != nil || chunks == nil {
			return errclass.Translation("shell output must be an array")
		}
		for _, chunk := range chunks {
			if chunk == nil {
				return errclass.Translation("shell output must contain objects")
			}
			for _, key := range []string{"stdout", "stderr"} {
				var s string
				if !strings.HasPrefix(strings.TrimSpace(string(chunk[key])), `"`) || json.Unmarshal(chunk[key], &s) != nil {
					return errclass.Translation("shell output requires stdout and stderr strings")
				}
			}
			var outcome rawObject
			if json.Unmarshal(chunk["outcome"], &outcome) != nil {
				return errclass.Translation("shell output requires an outcome")
			}
			switch rawString(outcome, "type") {
			case "timeout":
			case "exit":
				var n int64
				if !HasContent(outcome["exit_code"]) || json.Unmarshal(outcome["exit_code"], &n) != nil {
					return errclass.Translation("shell exit outcome requires exit_code")
				}
			default:
				return errclass.Translation("invalid shell output outcome")
			}
		}
		// Encode the full result as text: preserve stdout, stderr, order and outcomes.
		var result any = chunks
		if limit, ok := item["max_output_length"]; ok {
			result = rawObject{"output": item["output"], "max_output_length": limit}
			delete(item, "max_output_length")
		}
		output, _ := json.Marshal(result)
		putString(item, "type", "function_call_output")
		putString(item, "output", string(output))
	}
	return nil
}

// normalizeShellDocument only emulates local shell. Hosted environments stay
// native so cross-protocol adapters reject them instead of executing locally.
func normalizeShellDocument(doc rawObject, state *ConversionContext) (bool, *errclass.Error) {
	var tools []rawObject
	if HasContent(doc["tools"]) && json.Unmarshal(doc["tools"], &tools) != nil {
		return false, errclass.Translation("tools must be an array of objects")
	}
	var items []rawObject
	array := strings.HasPrefix(strings.TrimSpace(string(doc["input"])), "[")
	if array && json.Unmarshal(doc["input"], &items) != nil {
		return false, errclass.Translation("input must contain objects")
	}
	used := false
	hosted := false
	localDeclared := false
	var inspect func([]rawObject)
	inspect = func(ts []rawObject) {
		for _, tool := range ts {
			if localShell(tool) {
				used = true
				localDeclared = true
			} else if rawString(tool, "type") == "shell" {
				hosted = true
			}
			if rawString(tool, "type") == "namespace" {
				var children []rawObject
				DecodeProbe(tool["tools"], &children)
				inspect(children)
			}
		}
	}
	inspect(tools)
	for _, item := range items {
		switch rawString(item, "type") {
		case "shell_call", "shell_call_output":
			used = true
			if HasContent(item["environment"]) {
				if !localShell(rawObject{"type": json.RawMessage(`"shell"`), "environment": item["environment"]}) {
					hosted = true
				}
			}
		case "additional_tools":
			var extra []rawObject
			DecodeProbe(item["tools"], &extra)
			inspect(extra)
		}
	}
	if hosted {
		if localDeclared {
			return false, errclass.Translation("local and hosted shell environments cannot be combined")
		}
		return false, nil
	}
	if !used {
		return false, nil
	}
	// A custom call uses a different input contract even if its name matches
	// the function used to emulate shell. Namespaced custom calls stay distinct.
	for _, item := range items {
		if rawString(item, "type") == "custom_tool_call" && rawString(item, "name") == ShellFunctionName && rawString(item, "namespace") == "" {
			return false, errclass.Translation("shell function name conflicts with custom tool call history")
		}
	}
	id := responseToolIdentity{name: ShellFunctionName, isShell: true}
	if prev, ok := state.Tools.names[ShellFunctionName]; ok && prev != id {
		return false, errclass.Translation("shell function name collides with another tool")
	}
	rewriteTools := func(ts []rawObject) *errclass.Error {
		for _, tool := range ts {
			if (rawString(tool, "type") == "function" || rawString(tool, "type") == "custom") && rawString(tool, "name") == ShellFunctionName {
				return errclass.Translation("shell function name collides with another tool")
			}
			if rawString(tool, "type") == "namespace" {
				var children []rawObject
				DecodeProbe(tool["tools"], &children)
				for _, child := range children {
					if localShell(child) {
						return errclass.Translation("shell tools cannot be namespaced")
					}
				}
			}
			if localShell(tool) {
				putString(tool, "type", "function")
				if err := validateLocalShellEnvironment(tool["environment"]); err != nil {
					return err
				}
				putString(tool, "name", ShellFunctionName)
				tool["parameters"] = json.RawMessage(shellParameters)
				if !HasContent(tool["description"]) {
					putString(tool, "description", "Execute shell commands in the local environment.")
				}
				delete(tool, "environment")
			}
		}
		return nil
	}
	if err := rewriteTools(tools); err != nil {
		return false, err
	}
	for _, item := range items {
		typ := rawString(item, "type")
		if typ == "additional_tools" {
			var extra []rawObject
			DecodeProbe(item["tools"], &extra)
			if err := rewriteTools(extra); err != nil {
				return false, err
			}
			item["tools"], _ = json.Marshal(extra)
		}
		if typ == "shell_call" || typ == "shell_call_output" {
			if err := shellHistory(item); err != nil {
				return false, err
			}
		}
	}
	rewriteChoice := func(choice rawObject) {
		if rawString(choice, "type") == "shell" {
			putString(choice, "type", "function")
			putString(choice, "name", ShellFunctionName)
		}
	}
	if HasContent(doc["tool_choice"]) {
		var choice rawObject
		if json.Unmarshal(doc["tool_choice"], &choice) == nil && choice != nil {
			rewriteChoice(choice)
			if rawString(choice, "type") == "allowed_tools" {
				var allowed []rawObject
				if json.Unmarshal(choice["tools"], &allowed) != nil || allowed == nil {
					return false, errclass.Translation("allowed_tools requires a tools array")
				}
				for _, tool := range allowed {
					rewriteChoice(tool)
				}
				choice["tools"], _ = json.Marshal(allowed)
			}
			doc["tool_choice"], _ = json.Marshal(choice)
		}
	}
	if HasContent(doc["tools"]) {
		doc["tools"], _ = json.Marshal(tools)
	}
	if array {
		doc["input"], _ = json.Marshal(items)
	}
	if state.Tools.names == nil {
		state.Tools.names = make(map[string]responseToolIdentity)
	}
	state.Tools.names[ShellFunctionName] = id
	return true, nil
}

// NormalizeShellRequest adapts shell declarations and history before the
// existing typed cross-protocol converters decode the request.
func NormalizeShellRequest(body []byte, state *ConversionContext) ([]byte, *errclass.Error) {
	var doc rawObject
	if json.Unmarshal(body, &doc) != nil || doc == nil {
		return nil, errclass.Translation("Responses request requires a JSON object")
	}
	changed, err := normalizeShellDocument(doc, state)
	if err != nil {
		return nil, err
	}
	if !changed {
		return body, nil
	}
	out, _ := json.Marshal(doc)
	return out, nil
}

// ValidateShellResponse ensures incomplete or malformed function arguments
// cannot silently escape as ordinary function calls on the restoration path.
func ValidateShellResponse(body []byte, format string, state *ConversionContext) *errclass.Error {
	var doc rawObject
	if json.Unmarshal(body, &doc) != nil {
		return nil //nolint:nilerr // an undecodable body has no shell calls to validate
	}
	var calls []rawObject
	switch format {
	case FormatResponses:
		DecodeProbe(doc["output"], &calls)
	case FormatMessages:
		DecodeProbe(doc["content"], &calls)
	case FormatChatCompletions:
		var choices []rawObject
		DecodeProbe(doc["choices"], &choices)
		for _, choice := range choices {
			var message rawObject
			DecodeProbe(choice["message"], &message)
			var tools []rawObject
			DecodeProbe(message["tool_calls"], &tools)
			calls = append(calls, tools...)
		}
	}
	for _, call := range calls {
		name := rawString(call, "name")
		args := rawString(call, "arguments")
		callID := rawString(call, "call_id")
		if format == FormatMessages {
			args = string(call["input"])
			callID = rawString(call, "id")
		}
		if format == FormatChatCompletions {
			callID = rawString(call, "id")
			var fn rawObject
			DecodeProbe(call["function"], &fn)
			name = rawString(fn, "name")
			args = rawString(fn, "arguments")
		}
		if state.IsShell(name) {
			if err := ValidateShellCallID(callID); err != nil {
				return err
			}
			if _, err := ShellAction(args); err != nil {
				return err
			}
		}
	}
	return nil
}
