package responses

import (
	"encoding/json"
	"strings"

	"github.com/dillonzq/apix/internal/errclass"
	"github.com/dillonzq/apix/internal/llm/adapter/shared"
)

// nativeOutput tracks restoration without sharing target block state.
type nativeOutput struct {
	// customItems retains both item and call IDs for later input-done events.
	customItems  map[string]struct{}
	customArgs   map[string]*strings.Builder
	customSent   map[string]int
	shellItems   map[string]bool
	shellItemIDs map[string]string
}

func newNativeOutput() nativeOutput {
	return nativeOutput{customItems: map[string]struct{}{}, customArgs: map[string]*strings.Builder{}, customSent: map[string]int{}, shellItems: map[string]bool{}, shellItemIDs: map[string]string{}}
}

// passthroughEvent echoes native Responses frames verbatim (the framer's
// raw block, byte-identical to the upstream bytes); terminal events flip
// done after the common terminal validation. Failure/error payloads are parsed
// best-effort, so an unparseable failure degrades to a retryable error rather
// than failing the passthrough contract. Items for tools originally
// declared as custom restore to custom_tool_call shapes so native clients
// can dispatch them.
func (sc *StreamConverter) passthroughEvent(eventType, payload string, raw []byte) ([][]byte, bool, *errclass.Error) {
	switch eventType {
	case "response.completed", "response.incomplete":
		var doc map[string]json.RawMessage
		if json.Unmarshal([]byte(payload), &doc) == nil {
			if response, ok := doc["response"]; ok {
				if err := shared.ValidateShellResponse(response, "responses", sc.context); err != nil {
					return nil, false, err
				}
				restored := shared.RestoreResponseTools(response, sc.context, sc.native.shellItemIDs)
				if string(restored) != string(response) {
					doc["response"] = restored
					return [][]byte{shared.SSEEvent(eventType, doc)}, true, nil
				}
			}
		}
		return [][]byte{raw}, true, nil
	case "response.failed", "error":
		var ev failureEvent
		shared.DecodeProbe([]byte(payload), &ev)
		return nil, false, failureError(&ev)
	case "response.output_item.added", "response.output_item.done":
		var shellEvent struct {
			Item struct {
				Name      string `json:"name"`
				CallID    string `json:"call_id"`
				Arguments string `json:"arguments"`
			} `json:"item"`
		}
		shared.DecodeProbe([]byte(payload), &shellEvent)
		if sc.context.IsShell(shellEvent.Item.Name) {
			if err := shared.ValidateShellCallID(shellEvent.Item.CallID); err != nil {
				return nil, false, err
			}
			if eventType == "response.output_item.done" {
				if _, err := shared.ShellAction(shellEvent.Item.Arguments); err != nil {
					return nil, false, err
				}
			}
		}
		if sc.context != nil {
			if rewritten, ok := sc.restoreCustomItem(eventType, payload); ok {
				return [][]byte{rewritten}, false, nil
			}
		}
		if doc, changed := shared.RestoreToolEventIdentity([]byte(payload), sc.context); changed && (eventType == "response.function_call_arguments.delta" || eventType == "response.function_call_arguments.done") {
			return [][]byte{shared.SSEEvent(eventType, doc)}, false, nil
		}
		return [][]byte{raw}, false, nil
	case "response.function_call_arguments.delta":
		var ev argsDeltaEvent
		if err := decodeEvent(eventType, payload, &ev); err != nil {
			return nil, false, err
		}
		if sc.native.shellItems[ev.ItemID] || sc.context.IsShell(ev.Name) {
			sc.native.shellItems[ev.ItemID] = true
			sc.customArguments(ev.ItemID, ev.Delta)
			return nil, false, nil
		}
		if ev.ItemID != "" && sc.context.IsCustom(ev.Name) {
			sc.native.customItems[ev.ItemID] = struct{}{}
		}
		if _, ok := sc.native.customItems[ev.ItemID]; ok {
			sc.customArguments(ev.ItemID, ev.Delta)
			sent := sc.native.customSent[ev.ItemID]
			tail := shared.CustomInputTail(sc.customArguments(ev.ItemID, ""), &sent)
			sc.native.customSent[ev.ItemID] = sent
			if tail == "" {
				return nil, false, nil
			}
			var doc map[string]json.RawMessage
			if json.Unmarshal([]byte(payload), &doc) != nil || doc == nil {
				return [][]byte{raw}, false, nil //nolint:nilerr // a malformed event passes through verbatim
			}
			doc["type"] = json.RawMessage(`"response.custom_tool_call_input.delta"`)
			doc["delta"], _ = json.Marshal(tail)
			return [][]byte{shared.SSEEvent("response.custom_tool_call_input.delta", doc)}, false, nil
		}
		if doc, changed := shared.RestoreToolEventIdentity([]byte(payload), sc.context); changed && (eventType == "response.function_call_arguments.delta" || eventType == "response.function_call_arguments.done") {
			return [][]byte{shared.SSEEvent(eventType, doc)}, false, nil
		}
		return [][]byte{raw}, false, nil
	case "response.function_call_arguments.done":
		var shellArgs argsDeltaEvent
		shared.DecodeProbe([]byte(payload), &shellArgs)
		if sc.native.shellItems[shellArgs.ItemID] || sc.context.IsShell(shellArgs.Name) {
			if err := decodeEvent(eventType, payload, &shellArgs); err != nil {
				return nil, false, err
			}
			args := shellArgs.Arguments
			if args == "" {
				args = sc.customArguments(shellArgs.ItemID, "")
			}
			if _, err := shared.ShellAction(args); err != nil {
				return nil, false, err
			}
			return nil, false, nil
		}
		if sc.context != nil {
			if rewritten, ok := sc.restoreCustomInputDone(payload); ok {
				return [][]byte{rewritten}, false, nil
			}
		}
		if doc, changed := shared.RestoreToolEventIdentity([]byte(payload), sc.context); changed && (eventType == "response.function_call_arguments.delta" || eventType == "response.function_call_arguments.done") {
			return [][]byte{shared.SSEEvent(eventType, doc)}, false, nil
		}
		return [][]byte{raw}, false, nil
	default:
		if doc, changed := shared.RestoreToolEventIdentity([]byte(payload), sc.context); changed && (eventType == "response.function_call_arguments.delta" || eventType == "response.function_call_arguments.done") {
			return [][]byte{shared.SSEEvent(eventType, doc)}, false, nil
		}
		return [][]byte{raw}, false, nil
	}
}

// restoreCustomItem rewrites a function_call output item for a custom tool
// into its custom_tool_call shape; non-custom items report false so the
// caller falls back to verbatim passthrough.
func (sc *StreamConverter) restoreCustomItem(eventType, payload string) ([]byte, bool) {
	var doc map[string]json.RawMessage
	if json.Unmarshal([]byte(payload), &doc) != nil {
		return nil, false
	}
	var original struct {
		ID     string `json:"id"`
		CallID string `json:"call_id"`
		Name   string `json:"name"`
	}
	shared.DecodeProbe(doc["item"], &original)
	if sc.context.IsShell(original.Name) {
		if original.ID != "" {
			sc.native.shellItems[original.ID] = true
		}
		if original.CallID != "" {
			sc.native.shellItems[original.CallID] = true
		}
		if eventType == "response.output_item.added" {
			var item map[string]json.RawMessage
			if json.Unmarshal(doc["item"], &item) != nil || item == nil {
				return nil, false // keep the upstream item verbatim
			}
			item["arguments"] = json.RawMessage(`""`)
			if !shared.HasContent(item["status"]) {
				item["status"] = json.RawMessage(`"in_progress"`)
			}
			doc["item"], _ = json.Marshal(item)
		}
	}
	restored, ok := shared.RestoreToolItem(doc["item"], sc.context, sc.native.shellItemIDs)
	if !ok {
		return nil, false
	}
	if sc.context.IsShell(original.Name) {
		var identity struct {
			ID string `json:"id"`
		}
		shared.DecodeProbe(restored, &identity)
		if identity.ID != "" {
			sc.native.shellItems[identity.ID] = true
		}
	}
	if sc.context.IsCustom(original.Name) {
		if original.ID != "" {
			sc.native.customItems[original.ID] = struct{}{}
		}
		if original.CallID != "" {
			sc.native.customItems[original.CallID] = struct{}{}
		}
	}
	doc["item"] = restored
	return shared.SSEEvent(eventType, doc), true
}

// restoreCustomInputDone rewrites a function_call_arguments.done event for
// a custom tool into its custom_tool_call_input.done shape; non-custom
// events report false so the caller falls back to verbatim passthrough.
func (sc *StreamConverter) restoreCustomInputDone(payload string) ([]byte, bool) {
	var doc map[string]json.RawMessage
	if json.Unmarshal([]byte(payload), &doc) != nil {
		return nil, false
	}
	var name, id string
	shared.DecodeProbe(doc["name"], &name)
	shared.DecodeProbe(doc["item_id"], &id)
	args, argsOK := shared.StringField(doc["arguments"])
	if args == "" {
		// Added events may omit arguments until the deltas arrive; the
		// accumulated stream is the real fallback for them.
		args = sc.customArguments(id, "")
	}
	if !argsOK && args == "" {
		// The event carries a non-string arguments value and nothing was
		// accumulated: rewriting it would invent an empty input, so the
		// caller keeps the upstream event verbatim.
		return nil, false
	}
	_, known := sc.native.customItems[id]
	if !known && !sc.context.IsCustom(name) {
		return nil, false
	}
	doc["type"] = json.RawMessage(`"response.custom_tool_call_input.done"`)
	doc["input"], _ = json.Marshal(shared.UnwrapCustomToolInput(args))
	delete(doc, "arguments")
	if name != "" {
		item, _ := json.Marshal(map[string]any{"type": "function_call", "name": name})
		restored, _ := shared.RestoreToolItem(item, sc.context)
		var identity map[string]json.RawMessage
		shared.DecodeProbe(restored, &identity)
		doc["name"] = identity["name"]
		if namespace := identity["namespace"]; len(namespace) > 0 {
			doc["namespace"] = namespace
		}
	}
	return shared.SSEEvent("response.custom_tool_call_input.done", doc), true
}
