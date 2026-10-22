package shared

import (
	"encoding/json"
	"fmt"
	"strings"

	"github.com/dillonzq/apix/internal/errclass"
)

// DefaultArgs applies the absent-tool-arguments policy:
// whitespace-only or JSON-null arguments become an empty JSON object so
// every tool call carries valid arguments.
func DefaultArgs(args string) string {
	s := strings.TrimSpace(args)
	if s == "" || s == "null" {
		return "{}"
	}
	return args
}

// emptyObjectSchema is the normalized parameter schema for tools declared
// without one.
var emptyObjectSchema = json.RawMessage(`{"type":"object","properties":{}}`)

// ObjectSchema normalizes an absent or null parameter schema to the empty
// object schema; a present schema passes through untouched.
func ObjectSchema(raw json.RawMessage) json.RawMessage {
	if !HasContent(raw) {
		return emptyObjectSchema
	}
	return raw
}

// ToolChoice kinds returned by DecodeToolChoice: "absent" (field missing
// or null), "auto", "any" (any/required), "none", "named" (forced single
// tool, name carries the tool name).
const (
	ToolChoiceAbsent = "absent"
	ToolChoiceAuto   = "auto"
	ToolChoiceAny    = "any"
	ToolChoiceNone   = "none"
	ToolChoiceNamed  = "named"
)

// AllToolChoiceKinds enumerates every ToolChoice* kind DecodeToolChoice
// returns, in kind order (absent/auto/any/none/named). Consumers'
// exhaustiveness tests must iterate this slice; add a kind here first.
var AllToolChoiceKinds = []string{
	ToolChoiceAbsent,
	ToolChoiceAuto,
	ToolChoiceAny,
	ToolChoiceNone,
	ToolChoiceNamed,
}

// DecodeToolChoice classifies a tool_choice value across the Chat
// Completions ({type:function,function:{name}}), Anthropic
// ({type:tool,name}), and Responses (string or {type:function,name})
// shapes into a normalized kind plus forced-tool name. Unknown types are
// a translation failure — tool_choice is never silently dropped.
func DecodeToolChoice(raw json.RawMessage) (kind, name string, eErr *errclass.Error) {
	if !HasContent(raw) {
		return ToolChoiceAbsent, "", nil
	}
	var tc struct {
		Type     string `json:"type"`
		Name     string `json:"name"`
		Function struct {
			Name string `json:"name"`
		} `json:"function"`
	}
	if err := json.Unmarshal(raw, &tc); err != nil {
		if s := strings.TrimSpace(string(raw)); s == `"auto"` || s == `"any"` ||
			s == `"required"` || s == `"none"` {
			switch strings.Trim(s, `"`) {
			case "auto":
				return ToolChoiceAuto, "", nil
			case "any", "required":
				return ToolChoiceAny, "", nil
			default:
				return ToolChoiceNone, "", nil
			}
		}
		return "", "", errclass.Translation("malformed tool_choice: " + err.Error())
	}
	name = tc.Function.Name
	if name == "" {
		name = tc.Name
	}
	switch tc.Type {
	case "auto":
		return ToolChoiceAuto, "", nil
	case "any", "required":
		return ToolChoiceAny, "", nil
	case "none":
		return ToolChoiceNone, "", nil
	case "tool", "function":
		if name == "" {
			return "", "", errclass.Translation("malformed tool_choice: named tool requires a name")
		}
		return ToolChoiceNamed, name, nil
	}
	return "", "", errclass.Translation(fmt.Sprintf("unsupported tool_choice type %q", tc.Type))
}

// DecodeArgs parses tool-call arguments JSON; empty arguments decode to an
// empty object so every tool call carries valid input.
//
// Accepted ceiling: cross-format legs round-trip args through float64;
// integers > 2^53 lose precision — Responses passthrough preserves bytes.
// Upgrade path: carry json.RawMessage end-to-end.
func DecodeArgs(args string) (any, *errclass.Error) {
	args = DefaultArgs(args)
	var v any
	if err := json.Unmarshal([]byte(args), &v); err != nil {
		return nil, errclass.Translation("malformed tool call arguments JSON: " + err.Error())
	}
	return v, nil
}

// CustomInputTail derives the newly-available custom tool input from the
// accumulated wrapped arguments: nothing is reportable until the
// accumulation unwraps past its JSON envelope, at which point only the
// tail beyond already-reported bytes returns. Callers advance *emitted
// past the returned tail, so raw {"input":" wrapper tokens never reach
// client input deltas while complete inputs still stream incrementally.
func CustomInputTail(accumulated string, emitted *int) string {
	if !strings.HasSuffix(strings.TrimSpace(accumulated), "}") {
		return ""
	}
	unwrapped := UnwrapCustomToolInput(accumulated)
	if unwrapped == accumulated {
		return ""
	}
	if *emitted >= len(unwrapped) {
		return ""
	}
	tail := unwrapped[*emitted:]
	*emitted = len(unwrapped)
	return tail
}

// FunctionTool validates one tool definition for translation. toolType
// must be "function" — anything else is unsupported_protocol_or_parameter
// (a non-function tool type is an unsupported parameter) naming
// the target endpoint. Callers normalize or preserve the parameter schema
// themselves from their own raw field.
func FunctionTool(toolType, targetLabel string) *errclass.Error {
	if toolType != "function" {
		return &errclass.Error{
			Class:   errclass.ClassUnsupported,
			Message: fmt.Sprintf("unsupported tool type %q; only function tools translate to %s", toolType, targetLabel),
		}
	}
	return nil
}

func wrapCustomInput(input string) string {
	args, _ := json.Marshal(map[string]string{"input": input})
	return string(args)
}

// UnwrapCustomToolInput extracts the input string from custom tool call
// arguments: a JSON object with properties like "input", "code", or "arguments"
// unwraps to that raw string value, otherwise the arguments pass through verbatim.
func UnwrapCustomToolInput(args string) string {
	var m map[string]json.RawMessage
	if json.Unmarshal([]byte(args), &m) == nil {
		for _, key := range []string{"input", "code", "arguments", "cmd", "command"} {
			if raw, ok := m[key]; ok {
				var s string
				if json.Unmarshal(raw, &s) == nil {
					return s
				}
				return string(raw)
			}
		}
		if len(m) == 1 {
			for _, raw := range m {
				var s string
				if json.Unmarshal(raw, &s) == nil {
					return s
				}
			}
		}
	}
	return args
}
