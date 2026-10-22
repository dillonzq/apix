package thinking

import (
	"encoding/json"

	"github.com/dillonzq/apix"
	"github.com/dillonzq/apix/internal/errclass"
)

// Error reports reasoning-control translation failures.
type Error = apix.Error

// The LLM protocols this package rewrites. They equal openai.ChatCompletions,
// openai.Responses, and anthropic.Messages, which import this package
// indirectly and so cannot be imported here.
const (
	chatCompletions apix.Protocol = "openai.chat-completions"
	responses       apix.Protocol = "openai.responses"
	messages        apix.Protocol = "anthropic.messages"
)

// StripSupersededReasoning removes the client's own reasoning control from the
// source payload when a recognized suffix supersedes it. Only the (route,
// source protocol) pairs whose conversion derives the target control from that
// field are touched, because only those can reject a value the suffix already
// replaced. Everything else keeps its body untouched: a native Messages
// request passes the control through without validating it, and its siblings
// (display) are preserved by the override.
func StripSupersededReasoning(target, source apix.Protocol, body []byte) []byte {
	dropThinking := source == messages && (target == chatCompletions || target == responses)
	dropEffort := target == messages && source == chatCompletions
	dropNestedEffort := target == messages && source == responses
	if !dropThinking && !dropEffort && !dropNestedEffort {
		return body
	}
	var req map[string]json.RawMessage
	if err := json.Unmarshal(body, &req); err != nil || req == nil {
		// Malformed input stays the adapter's to classify and report.
		return body
	}
	switch {
	case dropThinking:
		// Chat Completions and Responses derive their control from the
		// Messages thinking object and reject an unknown type.
		delete(req, "thinking")
	case dropEffort:
		delete(req, "reasoning_effort")
	default:
		// Messages converts the effort through the fixed budget table and
		// reports a level with no equivalent; the other reasoning fields
		// (summary, and any future sibling) stay untouched.
		deleteNested(req, "reasoning", "effort")
	}
	out, err := json.Marshal(req)
	if err != nil {
		return body
	}
	return out
}

// ApplySuffix rewrites the reasoning control of an upstream body the
// route adapter already built, so the requested model's suffix takes priority
// over the control the client sent. It runs only for a recognized suffix value:
// absent or unrecognized suffixes leave the body byte-identical.
func ApplySuffix(target apix.Protocol, body []byte, suffix Suffix) ([]byte, *Error) {
	if suffix.Effort == "" {
		return body, nil
	}
	var req map[string]json.RawMessage
	if err := json.Unmarshal(body, &req); err != nil {
		return nil, errclass.Translation("malformed upstream request JSON: " + err.Error())
	}
	if req == nil {
		return nil, errclass.Translation("malformed request body: JSON null is not a valid request")
	}
	switch target {
	case chatCompletions:
		applyChatEffort(req, suffix.Effort)
	case messages:
		applyMessagesEffort(req, suffix)
	case responses:
		applyResponsesEffort(req, suffix.Effort)
	default:
		return body, nil
	}
	out, err := json.Marshal(req)
	if err != nil {
		return nil, errclass.Translation("reasoning control cannot be represented as JSON")
	}
	return out, nil
}

// applyChatEffort overrides reasoning_effort. Auto removes the control so the
// upstream applies its own default, which is how every adapter renders auto for
// a protocol without an auto wire value.
func applyChatEffort(req map[string]json.RawMessage, effort string) {
	if effort == "auto" {
		delete(req, "reasoning_effort")
		return
	}
	req["reasoning_effort"] = jsonRaw(effort)
}

// applyMessagesEffort overrides the Anthropic thinking control: none disables
// thinking, a level becomes an enabled budget from the fixed table, a numeric
// suffix keeps its exact budget, and auto removes the
// control so the upstream picks its default. Sibling fields the client sent
// (display) survive the enabled override; display is dropped when thinking is
// disabled, because display only applies to an active block. An adaptive
// output_config.effort is always superseded.
func applyMessagesEffort(req map[string]json.RawMessage, suffix Suffix) {
	deleteNested(req, "output_config", "effort")
	if suffix.Effort == "auto" {
		delete(req, "thinking")
		return
	}
	budget, ok := messagesBudget(suffix)
	if !ok {
		return
	}
	thinkingObj := nestedObject(req, "thinking")
	if budget == 0 {
		delete(thinkingObj, "budget_tokens")
		delete(thinkingObj, "display")
		thinkingObj["type"] = jsonRaw("disabled")
	} else {
		thinkingObj["type"] = jsonRaw("enabled")
		thinkingObj["budget_tokens"] = jsonRaw(budget)
	}
	req["thinking"] = jsonObject(thinkingObj)
}

// messagesBudget resolves the Messages budget for a recognized suffix: an
// exact numeric value keeps its number, a level uses the fixed table.
func messagesBudget(suffix Suffix) (int64, bool) {
	if suffix.Budget != nil {
		return *suffix.Budget, true
	}
	return BudgetFromEffort(suffix.Effort)
}

// applyResponsesEffort overrides reasoning.effort inside an existing reasoning
// object so sibling fields (summary, encrypted_content) survive. Auto removes
// the effort, and an emptied reasoning object is dropped.
func applyResponsesEffort(req map[string]json.RawMessage, effort string) {
	if effort == "auto" {
		deleteNested(req, "reasoning", "effort")
		return
	}
	reasoning := nestedObject(req, "reasoning")
	reasoning["effort"] = jsonRaw(effort)
	req["reasoning"] = jsonObject(reasoning)
}

// deleteNested removes parent.child, dropping the parent object when it holds
// nothing else. A parent that is absent or not a JSON object is left alone.
func deleteNested(req map[string]json.RawMessage, parent, child string) {
	if _, ok := req[parent]; !ok {
		return
	}
	obj := nestedObject(req, parent)
	if _, ok := obj[child]; !ok {
		return
	}
	delete(obj, child)
	if len(obj) == 0 {
		delete(req, parent)
		return
	}
	req[parent] = jsonObject(obj)
}

// nestedObject reads a parent field as a mutable object; an absent parent, a
// non-object parent, and a JSON null all start empty.
func nestedObject(req map[string]json.RawMessage, parent string) map[string]json.RawMessage {
	obj := map[string]json.RawMessage{}
	if raw, ok := req[parent]; ok {
		if err := json.Unmarshal(raw, &obj); err != nil || obj == nil {
			obj = map[string]json.RawMessage{}
		}
	}
	return obj
}

// jsonObject re-encodes a composed object. Its values came from a successful
// unmarshal and jsonRaw encodes Go builtins, so marshaling cannot fail.
func jsonObject(obj map[string]json.RawMessage) json.RawMessage {
	b, err := json.Marshal(obj)
	if err != nil {
		return json.RawMessage(`{}`)
	}
	return b
}

// jsonRaw encodes a JSON scalar (a canonical effort string or an int64 budget).
func jsonRaw(v any) json.RawMessage {
	b, _ := json.Marshal(v)
	return b
}
