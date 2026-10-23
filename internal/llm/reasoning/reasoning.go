// Package reasoning maps reasoning levels and budgets and rewrites the
// reasoning control of converted requests. The engine, the codecs and the
// public thinking package share it, so every path renders a level the same
// way.
package reasoning

import (
	"encoding/json"
	"strings"

	"github.com/dillonzq/apix"
	"github.com/dillonzq/apix/internal/errclass"
	"github.com/dillonzq/apix/internal/llm/ids"
)

var levelBudgetTable = map[string]int64{
	"none":    0,
	"auto":    -1,
	"minimal": 512,
	"low":     1024,
	"medium":  8192,
	"high":    24576,
	"xhigh":   32768,
	"max":     128000,
}

// BudgetFromEffort converts a known level without capability filtering or
// budget clamping. Unknown levels have no numeric equivalent.
func BudgetFromEffort(effort string) (int64, bool) {
	budget, ok := levelBudgetTable[strings.ToLower(strings.TrimSpace(effort))]
	return budget, ok
}

// EffortFromBudget preserves the zero/off and -1/auto sentinels. Positive
// budgets use fixed thresholds; max is never derived from a budget.
func EffortFromBudget(budget int64) (string, bool) {
	switch {
	case budget < -1:
		return "", false
	case budget == -1:
		return "auto", true
	case budget == 0:
		return "none", true
	case budget <= 512:
		return "minimal", true
	case budget <= 1024:
		return "low", true
	case budget <= 8192:
		return "medium", true
	case budget <= 24576:
		return "high", true
	default:
		return "xhigh", true
	}
}

// GeminiLevel returns the GenerateContent thinkingLevel for a level. Gemini
// has no level above high, so xhigh and max become high and exact is false.
// Other values are returned unchanged for the upstream to judge.
func GeminiLevel(level string) (gemini string, exact bool) {
	switch level {
	case "xhigh", "max":
		return "high", false
	}
	return level, true
}

// Options select how Apply renders a level for an upstream.
type Options struct {
	// MessagesManual renders Messages controls as enabled thinking with
	// budget_tokens instead of adaptive thinking.
	MessagesManual bool
	// GeminiMode is "budget" or "level" to force that GenerateContent control;
	// empty renders numeric requests as budgets and levels as levels.
	GeminiMode string
}

// Validate checks an override and fills Effort from Budget when only the
// budget is set.
func Validate(r apix.Reasoning) (apix.Reasoning, *errclass.Error) {
	if r.Budget != nil {
		effort, ok := EffortFromBudget(*r.Budget)
		if !ok {
			return r, errclass.Translation("reasoning budget must be -1 or greater")
		}
		if r.Effort == "" {
			r.Effort = effort
		}
	}
	if r.Effort == "" {
		return r, errclass.Translation("reasoning override needs an effort or a budget")
	}
	r.Effort = strings.ToLower(strings.TrimSpace(r.Effort))
	if _, ok := levelBudgetTable[r.Effort]; !ok {
		return r, &errclass.Error{Class: errclass.ClassUnsupported, Message: "unknown reasoning effort: " + errclass.Redact(r.Effort)}
	}
	return r, nil
}

// Apply rewrites the reasoning control of a request body already converted to
// the target format, so the override takes priority over the control the
// client sent. It returns the paths whose value had to be approximated.
// Targets without a reasoning control are returned unchanged.
func Apply(target string, body []byte, r apix.Reasoning, opts Options) ([]byte, []string, *errclass.Error) {
	r, err := Validate(r)
	if err != nil {
		return nil, nil, err
	}
	var req map[string]json.RawMessage
	if err := json.Unmarshal(body, &req); err != nil {
		return nil, nil, errclass.Translation("malformed upstream request JSON: " + err.Error())
	}
	if req == nil {
		return nil, nil, errclass.Translation("malformed request body: JSON null is not a valid request")
	}
	var approximated []string
	switch target {
	case ids.FormatChatCompletions:
		applyChatEffort(req, r.Effort)
	case ids.FormatMessages:
		applyMessagesEffort(req, r, opts.MessagesManual)
	case ids.FormatResponses:
		applyResponsesEffort(req, r.Effort)
	case ids.FormatGemini:
		approximated = applyGemini(req, r, opts.GeminiMode)
	default:
		return body, nil, nil
	}
	out, mErr := json.Marshal(req)
	if mErr != nil {
		return nil, nil, errclass.Translation("reasoning control cannot be represented as JSON")
	}
	return out, approximated, nil
}

// Strip removes the client's own reasoning control from a source body when an
// override supersedes it. Only the (target, source) pairs whose conversion
// derives the target control from that field are touched, because only those
// can reject a value the override already replaces. Everything else keeps
// its body untouched: a native Messages request passes the control through
// without validating it, and its siblings (display) are preserved by the
// override.
func Strip(target, source string, body []byte) []byte {
	dropThinking := source == ids.FormatMessages && (target == ids.FormatChatCompletions || target == ids.FormatResponses || target == ids.FormatGemini)
	dropEffort := (target == ids.FormatMessages || target == ids.FormatGemini) && source == ids.FormatChatCompletions
	dropNestedEffort := (target == ids.FormatMessages || target == ids.FormatGemini) && source == ids.FormatResponses
	dropGemini := source == ids.FormatGemini && (target == ids.FormatChatCompletions || target == ids.FormatResponses || target == ids.FormatMessages)
	if !dropThinking && !dropEffort && !dropNestedEffort && !dropGemini {
		return body
	}
	var req map[string]json.RawMessage
	if err := json.Unmarshal(body, &req); err != nil || req == nil {
		// Malformed input stays the adapter's to classify and report.
		return body
	}
	switch {
	case dropGemini:
		cfg := nestedObject(req, "generationConfig")
		tc := nestedObject(cfg, "thinkingConfig")
		for _, k := range []string{"thinkingLevel", "thinkingBudget", "thinking_level", "thinking_budget"} {
			delete(tc, k)
		}
		cfg["thinkingConfig"] = jsonObject(tc)
		req["generationConfig"] = jsonObject(cfg)
	case dropThinking:
		// Chat Completions and Responses derive their control from the
		// Messages thinking object and reject an unknown type.
		delete(req, "thinking")
	case dropEffort:
		delete(req, "reasoning_effort")
	default:
		// Messages converts the effort to adaptive or manual thinking and
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
// thinking and auto removes the control so the upstream picks its default.
// Adaptive mode sends the level as output_config.effort, deriving it from a
// numeric budget with the fixed thresholds; manual mode sends an enabled
// budget from the fixed table, or the exact numeric budget. Sibling fields the
// client sent (display) survive an active override; display is dropped when
// thinking is disabled, because it only applies to an active block.
func applyMessagesEffort(req map[string]json.RawMessage, r apix.Reasoning, manual bool) {
	budget, ok := messagesBudget(r)
	if r.Effort != "auto" && !ok {
		return
	}
	deleteNested(req, "output_config", "effort")
	if r.Effort == "auto" {
		delete(req, "thinking")
		return
	}
	thinkingObj := nestedObject(req, "thinking")
	delete(thinkingObj, "budget_tokens")
	switch {
	case budget == 0:
		delete(thinkingObj, "display")
		thinkingObj["type"] = jsonRaw("disabled")
	case manual:
		thinkingObj["type"] = jsonRaw("enabled")
		thinkingObj["budget_tokens"] = jsonRaw(budget)
	default:
		effort := r.Effort
		if r.Budget != nil {
			effort, _ = EffortFromBudget(budget)
		}
		thinkingObj["type"] = jsonRaw("adaptive")
		cfg := nestedObject(req, "output_config")
		cfg["effort"] = jsonRaw(effort)
		req["output_config"] = jsonObject(cfg)
	}
	req["thinking"] = jsonObject(thinkingObj)
}

// messagesBudget resolves the Messages budget: an exact numeric value keeps
// its number, a level uses the fixed table.
func messagesBudget(r apix.Reasoning) (int64, bool) {
	if r.Budget != nil {
		return *r.Budget, true
	}
	return BudgetFromEffort(r.Effort)
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

// applyGemini renders the override as thinkingBudget or thinkingLevel and
// keeps includeThoughts, which controls visibility independently. The auto
// and none sentinels are always budgets. Budget mode resolves levels through
// the fixed table; level mode resolves positive budgets through the fixed
// thresholds; the default renders numeric requests as budgets.
func applyGemini(req map[string]json.RawMessage, r apix.Reasoning, mode string) []string {
	cfg := nestedObject(req, "generationConfig")
	tc := nestedObject(cfg, "thinkingConfig")
	for _, k := range []string{"thinkingBudget", "thinkingLevel", "thinking_budget", "thinking_level"} {
		delete(tc, k)
	}
	var approximated []string
	budget, _ := BudgetFromEffort(r.Effort)
	if r.Budget != nil {
		budget = *r.Budget
	}
	switch {
	case r.Effort == "auto" || r.Effort == "none" || mode == "budget" || (mode == "" && r.Budget != nil):
		tc["thinkingBudget"] = jsonRaw(budget)
	default:
		level, exact := GeminiLevel(r.Effort)
		if !exact {
			approximated = append(approximated, "generationConfig.thinkingConfig.thinkingLevel")
		}
		tc["thinkingLevel"] = jsonRaw(level)
	}
	cfg["thinkingConfig"] = jsonObject(tc)
	req["generationConfig"] = jsonObject(cfg)
	return approximated
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
