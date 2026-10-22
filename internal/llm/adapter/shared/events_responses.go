package shared

import (
	"encoding/json"
	"time"
)

// ResponsesEventEmitter renders canonical OpenAI Responses SSE frames so
// every Responses stream synthesizer emits byte-identical shapes
// regardless of serving route — the sibling of
// ClaudeEventEmitter/ChatChunkBuilder. Native Responses responses carry a
// creation timestamp synthesizers never observe, so Created falls back to
// render-time now (same policy as ChatChunkBuilder.Created); the terminal
// completed payload always carries the model, matching the non-stream
// ResponsesResult shape.
type ResponsesEventEmitter struct {
	ID    string // response identity carried by every event
	Model string // model rendered on the terminal completed payload
	Tools *ConversionContext
}

// Created renders the leading response.created announcement.
func (e ResponsesEventEmitter) Created() []byte {
	return SSEEvent("response.created", map[string]any{
		"type": "response.created",
		"response": map[string]any{
			"id": e.ID, "object": "response", "created_at": time.Now().Unix(), "status": "in_progress",
		},
	})
}

// ItemAdded announces one output item before its first delta; item carries
// the block-type-specific fields (message id/role/content, or function_call
// call_id/name/arguments per the canonical call_id-only shape).
func (e ResponsesEventEmitter) ItemAdded(outputIndex int, item map[string]any) []byte {
	if item["type"] == "function_call" {
		if name, ok := item["name"].(string); ok {
			id := e.Tools.Identity(name)
			if id.isShell {
				if itemID, _ := item["id"].(string); itemID == "" {
					callID, _ := item["call_id"].(string)
					item["id"] = ShellItemID(callID)
				}
				item["type"] = "shell_call"
				item["action"] = shellResponseAction(json.RawMessage(`{"commands":[]}`))
				item["environment"] = json.RawMessage(shellEnvironment)
				item["status"] = "in_progress"
				delete(item, "name")
				delete(item, "arguments")
				return SSEEvent("response.output_item.added", map[string]any{"type": "response.output_item.added", "output_index": outputIndex, "item": item})
			}
			if id.isCustom {
				item["type"] = "custom_tool_call"
				item["input"] = ""
				delete(item, "arguments")
			}
			item["name"] = id.name
			if id.namespace != "" {
				item["namespace"] = id.namespace
			}
		}
	}
	return SSEEvent("response.output_item.added", map[string]any{
		"type": "response.output_item.added", "output_index": outputIndex, "item": item,
	})
}

// TextDelta streams one partial output_text delta referencing the
// announced message item by item_id and output_index.
func (e ResponsesEventEmitter) TextDelta(itemID string, outputIndex int, delta string) []byte {
	return e.TextDeltaAt(itemID, outputIndex, 0, delta)
}

// ArgsDelta streams one partial function_call_arguments delta referencing
// the announced function_call item by item_id and output_index.
func (e ResponsesEventEmitter) ArgsDelta(itemID string, outputIndex int, delta string) []byte {
	return SSEEvent("response.function_call_arguments.delta", map[string]any{
		"type": "response.function_call_arguments.delta", "item_id": itemID, "output_index": outputIndex, "delta": delta,
	})
}

// InputDelta streams one partial custom_tool_call_input delta referencing
// the announced custom_tool_call item by item_id and output_index.
func (e ResponsesEventEmitter) InputDelta(itemID string, outputIndex int, delta string) []byte {
	return SSEEvent("response.custom_tool_call_input.delta", map[string]any{
		"type": "response.custom_tool_call_input.delta", "item_id": itemID, "output_index": outputIndex, "delta": delta,
	})
}

// InputDone renders the custom_tool_call_input completion for the
// announced custom_tool_call item, emitted before response.completed.
func (e ResponsesEventEmitter) InputDone(itemID string, outputIndex int, input string) []byte {
	return SSEEvent("response.custom_tool_call_input.done", map[string]any{
		"type": "response.custom_tool_call_input.done", "item_id": itemID, "output_index": outputIndex, "input": input,
	})
}

// TextDone renders the output_text completion for the announced message
// item, emitted before response.completed.
func (e ResponsesEventEmitter) TextDone(itemID string, outputIndex int, text string) []byte {
	return SSEEvent("response.output_text.done", map[string]any{
		"type": "response.output_text.done", "item_id": itemID, "output_index": outputIndex, "content_index": 0, "text": text,
	})
}

// ContentPartDone renders the content_part completion for the announced
// message item, emitted before response.completed.
func (e ResponsesEventEmitter) ContentPartDone(itemID string, outputIndex int, text string) []byte {
	return SSEEvent("response.content_part.done", map[string]any{
		"type": "response.content_part.done", "item_id": itemID, "output_index": outputIndex, "content_index": 0,
		"part": map[string]any{"type": "output_text", "text": text},
	})
}

// ArgsDone renders the function_call_arguments completion for the
// announced function_call item, emitted before response.completed; name
// is included only when non-empty.
func (e ResponsesEventEmitter) ArgsDone(itemID string, outputIndex int, name, args string) []byte {
	if name != "" {
		name = e.Tools.Identity(name).name
	}
	payload := map[string]any{
		"type": "response.function_call_arguments.done", "item_id": itemID, "output_index": outputIndex, "arguments": args,
	}
	if name != "" {
		payload["name"] = name
	}
	return SSEEvent("response.function_call_arguments.done", payload)
}

// ItemDone renders the output_item completion for one rendered output
// item, emitted before response.completed.
func (e ResponsesEventEmitter) ItemDone(outputIndex int, item any) []byte {
	return SSEEvent("response.output_item.done", map[string]any{
		"type": "response.output_item.done", "output_index": outputIndex, "item": item,
	})
}

// Completed renders the terminal response.completed event: status from the
// route's status mapping, usage always attached, and output items
// rendered by the caller through OutputAssembler.
func (e ResponsesEventEmitter) Completed(status string, usage ResponsesUsage, output []any) []byte {
	return SSEEvent("response.completed", map[string]any{
		"type": "response.completed",
		"response": map[string]any{
			"id": e.ID, "object": "response", "model": e.Model, "status": status,
			"usage": usage, "output": output,
		},
	})
}
