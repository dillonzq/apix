package shared

import "encoding/json"

// RestoreToolItem preserves unmodeled fields and numeric JSON values while
// restoring a flattened function's namespace, original name, and custom input.
// An optional stream registry retains the first shell item ID per call_id.
func RestoreToolItem(raw json.RawMessage, tools *ConversionContext, shellItemIDs ...map[string]string) (json.RawMessage, bool) {
	var item map[string]json.RawMessage
	if json.Unmarshal(raw, &item) != nil {
		return raw, false
	}
	var typ, name string
	DecodeProbe(item["type"], &typ)
	DecodeProbe(item["name"], &name)
	if typ != "function_call" && typ != "custom_tool_call" {
		return raw, false
	}
	id := tools.Identity(name)
	if id.isShell && typ == "function_call" {
		callID := rawString(item, "call_id")
		if ValidateShellCallID(callID) != nil {
			return raw, false
		}
		itemID := rawString(item, "id")
		// A stream may omit id on later snapshots. Retain its first identity.
		if len(shellItemIDs) > 0 && shellItemIDs[0][callID] != "" {
			itemID = shellItemIDs[0][callID]
		}
		if itemID == "" {
			itemID = ShellItemID(callID)
		}
		putString(item, "id", itemID)
		args, argsOK := StringField(item["arguments"])
		if !argsOK {
			// A non-string arguments value cannot be validated as a shell
			// action; rewriting it would emit an empty action instead.
			return raw, false
		}
		// Added events announce an empty action before arguments arrive.
		action := json.RawMessage(`{"commands":[]}`)
		if args != "" {
			validated, actionErr := ShellAction(args)
			if actionErr != nil {
				return raw, false
			}
			action = validated
		}
		item["type"] = json.RawMessage(`"shell_call"`)
		item["action"] = shellResponseAction(action)
		item["environment"] = json.RawMessage(shellEnvironment)
		if rawString(item, "status") == "" {
			putString(item, "status", "completed")
		}
		delete(item, "name")
		delete(item, "arguments")
		delete(item, "namespace")
		if len(shellItemIDs) > 0 && shellItemIDs[0] != nil {
			shellItemIDs[0][callID] = itemID
		}
		out, _ := json.Marshal(item)
		return out, true
	}
	changed := id.name != name || id.namespace != "" || id.isCustom
	if !changed {
		return raw, false
	}
	item["name"], _ = json.Marshal(id.name)
	if id.namespace != "" {
		item["namespace"], _ = json.Marshal(id.namespace)
	}
	if id.isCustom && typ == "function_call" {
		args, argsOK := StringField(item["arguments"])
		if !argsOK {
			// The upstream value is not the string this shape requires.
			// Converting it would drop the original bytes and emit an empty
			// custom input, so leave the item untouched.
			return raw, false
		}
		item["type"] = json.RawMessage(`"custom_tool_call"`)
		item["input"], _ = json.Marshal(UnwrapCustomToolInput(args))
		delete(item, "arguments")
	}
	out, err := json.Marshal(item)
	if err != nil {
		return raw, false
	}
	return out, true
}
func RestoreResponseTools(body []byte, tools *ConversionContext, shellItemIDs ...map[string]string) []byte {
	var doc map[string]json.RawMessage
	if json.Unmarshal(body, &doc) != nil {
		return body
	}
	var items []json.RawMessage
	if json.Unmarshal(doc["output"], &items) != nil {
		return body
	}
	changed := false
	for i, item := range items {
		restored, ok := RestoreToolItem(item, tools, shellItemIDs...)
		if ok {
			items[i] = restored
			changed = true
		}
	}
	if !changed {
		return body
	}
	doc["output"], _ = json.Marshal(items)
	out, err := json.Marshal(doc)
	if err != nil {
		return body
	}
	return out
}

// RestoreToolEventIdentity rewrites optional event names without changing the
// event's argument delta, sequence number, or other unmodeled fields.
func RestoreToolEventIdentity(payload []byte, tools *ConversionContext) (map[string]json.RawMessage, bool) {
	var doc map[string]json.RawMessage
	if json.Unmarshal(payload, &doc) != nil {
		return nil, false
	}
	var name string
	DecodeProbe(doc["name"], &name)
	if name == "" {
		return doc, false
	}
	identity := tools.Identity(name)
	if identity.name == name && identity.namespace == "" {
		return doc, false
	}
	doc["name"], _ = json.Marshal(identity.name)
	if identity.namespace != "" {
		doc["namespace"], _ = json.Marshal(identity.namespace)
	}
	return doc, true
}
