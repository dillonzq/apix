package gemini

import (
	"fmt"
	"strings"
	"time"

	"github.com/dillonzq/apix/internal/errclass"
	"github.com/dillonzq/apix/internal/llm/adapter/shared"
)

// ConvertResponse converts ordinary responses directly to or from Gemini.
func ConvertResponse(from, to string, status int, body []byte, ctx *shared.ConversionContext) ([]byte, *errclass.Error) {
	if status >= 400 {
		return nil, shared.UpstreamStatusError(status, body)
	}
	root, err := decode(body)
	if err != nil {
		return nil, err
	}
	if err := responseError(root, body); err != nil {
		return nil, err
	}
	ctx = shared.ContextOrNew([]*shared.ConversionContext{ctx})
	c := &conversion{from: from, to: to, ctx: ctx, response: true}
	if from == Format {
		c.normalizeGemini(root)
		c.validateResponse(root)
		if c.err != nil {
			return nil, c.err
		}
		if to == Format {
			return body, nil
		}
		out := c.renderResponse(root)
		if c.err != nil {
			return nil, c.err
		}
		return encode(out), nil
	}
	if err := shared.CheckProtocolFailure(body, from); err != nil {
		return nil, err
	}
	out := c.responseToGemini(root)
	if c.err != nil {
		return nil, c.err
	}
	return encode(out), nil
}

func responseError(root object, body []byte) *errclass.Error {
	e := obj(root["error"])
	if e == nil {
		if root["error"] != nil {
			return errclass.UpstreamFallback("malformed Gemini error envelope")
		}
		return nil
	}
	status := int(number(e["code"]))
	if status < 400 || status > 599 {
		switch str(e["status"]) {
		case "INVALID_ARGUMENT":
			status = 400
		case "UNAUTHENTICATED":
			status = 401
		case "PERMISSION_DENIED":
			status = 403
		case "NOT_FOUND":
			status = 404
		case "RESOURCE_EXHAUSTED":
			status = 429
		case "UNAVAILABLE":
			status = 503
		default:
			status = 502
		}
	}
	return shared.UpstreamStatusError(status, body)
}

func (c *conversion) validateResponse(root object) {
	candidates := c.array(root["candidates"], "candidates")
	if len(candidates) == 0 && str(obj(root["promptFeedback"])["blockReason"]) == "" {
		c.fail("Gemini response has no candidates or prompt feedback")
	}
	for _, cv := range candidates {
		candidate := c.object(cv, "candidates[]")
		content := c.object(candidate["content"], "candidate.content")
		if content != nil {
			c.validateParts(c.array(content["parts"], "candidate.parts"))
		}
		if str(candidate["finishReason"]) == "MALFORMED_FUNCTION_CALL" {
			c.err = errclass.UpstreamFallback("Gemini produced a malformed function call")
		}
	}
}

func normalizedID(root object) string {
	if id := str(root["responseId"]); id != "" {
		return id
	}
	return "apix"
}

func finish(reason string, hasTools bool) string {
	switch reason {
	case "MAX_TOKENS":
		return "length"
	case "SAFETY", "RECITATION", "BLOCKLIST", "PROHIBITED_CONTENT", "SPII", "IMAGE_SAFETY":
		return "content_filter"
	case "CONTINUATION":
		return "length"
	}
	if hasTools {
		return "tool_calls"
	}
	return "stop"
}

func geminiFinish(reason string) string {
	switch reason {
	case "length", "max_tokens":
		return "MAX_TOKENS"
	case "content_filter", "refusal":
		return "SAFETY"
	default:
		return "STOP"
	}
}

func (c *conversion) renderedParts(parts []any, offset ...int) (content, tools, items []any, text, reasoning string) {
	for i, pv := range parts {
		if len(offset) > 0 {
			i += offset[0]
		}
		p := obj(pv)
		if p["text"] != nil {
			s := str(p["text"])
			if boolean(p["thought"]) {
				reasoning += s
				switch c.to {
				case messages:
					content = append(content, object{"type": "thinking", "thinking": s, "extra_content": carrier([]any{p})})
				case responses:
					items = append(items, object{"type": "reasoning", "id": fmt.Sprintf("reasoning_%d", i), "content": []any{object{"type": "reasoning_text", "text": s}}, "summary": []any{object{"type": "summary_text", "text": s}}, "extra_content": carrier([]any{p})})
				}
			} else {
				text += s
				switch c.to {
				case messages:
					content = append(content, object{"type": "text", "text": s, "extra_content": carrier([]any{p})})
				case responses:
					items = append(items, object{"type": "message", "id": fmt.Sprintf("message_%d", i), "role": "assistant", "status": "completed", "content": []any{object{"type": "output_text", "text": s, "annotations": []any{}}}, "extra_content": carrier([]any{p})})
				}
			}
			continue
		}
		if fc := obj(p["functionCall"]); fc != nil {
			id, name := callID(fc, i), str(fc["name"])
			args := c.args(fc["args"])
			switch c.to {
			case chat:
				tools = append(tools, object{"id": id, "type": "function", "function": object{"name": name, "arguments": string(encode(args))}})
			case messages:
				content = append(content, object{"type": "tool_use", "id": id, "name": name, "input": args, "extra_content": carrier([]any{p})})
				tools = append(tools, fc)
			case responses:
				item := object{"type": "function_call", "id": "fc_" + id, "call_id": id, "name": name, "arguments": string(encode(args)), "status": "completed", "extra_content": carrier([]any{p})}
				if c.ctx.IsShell(name) {
					if _, err := shared.ShellAction(str(item["arguments"])); err != nil {
						c.err = err
					}
				}
				if c.ctx.IsCustom(name) {
					if _, ok := args["input"].(string); !ok {
						c.fail("custom tool arguments require string input")
					}
				}
				raw, _ := shared.RestoreToolItem(encode(item), c.ctx)
				restored, err := decode(raw)
				if err != nil {
					c.err = err
				}
				items = append(items, restored)
				tools = append(tools, fc)
			}
			continue
		}
		if len(p) == 1 && signature(p) != "" {
			continue
		}
		// Image output is representable by Messages and the Chat image extension.
		// Other actual output content must fail rather than becoming empty success.
		media := first(p, "inlineData", "inline_data", "fileData", "file_data")
		if media != nil {
			mime := str(first(obj(media), "mimeType", "mime_type"))
			if !strings.HasPrefix(mime, "image/") {
				c.reject("non-image Gemini output media is not supported")
				continue
			}
			if c.to == responses {
				c.reject("Gemini image output cannot be represented by this Responses path")
				continue
			}
			v := c.fromMedia(p)
			if v != nil {
				content = append(content, v)
			}
			continue
		}
		c.reject("unsupported actual Gemini output part")
	}
	return
}

func (c *conversion) renderResponse(root object) object {
	id, model := normalizedID(root), str(root["modelVersion"])
	usage := obj(root["usageMetadata"])
	candidates := list(root["candidates"])
	if len(candidates) == 0 {
		candidates = []any{object{"content": object{"parts": []any{object{"text": "Request blocked by Gemini."}}}, "finishReason": "SAFETY"}}
	}
	var choices []any
	for ci, cv := range candidates {
		candidate := obj(cv)
		parts := list(obj(candidate["content"])["parts"])
		content, tools, items, text, reasoning := c.renderedParts(parts)
		reason := finish(str(candidate["finishReason"]), len(tools) > 0)
		switch c.to {
		case chat:
			msg := object{"role": "assistant", "content": text, "extra_content": carrier(parts)}
			if reasoning != "" {
				msg["reasoning_content"] = reasoning
			}
			if len(tools) > 0 {
				msg["tool_calls"] = tools
			}
			if len(content) > 0 {
				msg["images"] = content
			}
			if reason == "content_filter" {
				msg["refusal"] = text
			}
			if annotations := c.applyCitations(candidate, content, items); len(annotations) > 0 {
				msg["annotations"] = annotations
			}
			index := any(ci)
			if candidate["index"] != nil {
				index = candidate["index"]
			}
			choices = append(choices, object{"index": index, "message": msg, "finish_reason": reason})
		case messages:
			if len(candidates) > 1 {
				c.ctx.Report(Format, c.to, "candidates[1:]", true)
			}
			stop := "end_turn"
			switch reason {
			case "length":
				stop = "max_tokens"
			case "tool_calls":
				stop = "tool_use"
			case "content_filter":
				stop = "refusal"
			}
			c.applyCitations(candidate, content, items)
			return object{"id": id, "type": "message", "role": "assistant", "model": model, "content": nonNil(content), "stop_reason": stop, "stop_sequence": nil, "usage": renderUsage(usage, messages)}
		case responses:
			if len(candidates) > 1 {
				c.ctx.Report(Format, c.to, "candidates[1:]", true)
			}
			status := "completed"
			out := object{"id": id, "object": "response", "model": model, "status": status, "output": nonNil(items), "usage": renderUsage(usage, responses)}
			if reason == "length" {
				out["status"] = "incomplete"
				out["incomplete_details"] = object{"reason": "max_output_tokens"}
			}
			c.applyCitations(candidate, content, items)
			return out
		default:
			c.reject("unsupported Gemini response target")
		}
	}
	return object{"id": id, "object": "chat.completion", "created": time.Now().Unix(), "model": model, "choices": choices, "usage": renderUsage(usage, chat)}
}

func nonNil(a []any) []any {
	if a == nil {
		return []any{}
	}
	return a
}

func renderUsage(u object, to string) object {
	input, output, thought, cached := number(u["promptTokenCount"]), number(u["candidatesTokenCount"]), number(u["thoughtsTokenCount"]), number(u["cachedContentTokenCount"])
	total := number(u["totalTokenCount"])
	if total == 0 {
		total = input + output + thought
	}
	switch to {
	case chat:
		r := object{"prompt_tokens": input, "completion_tokens": output + thought, "total_tokens": total}
		if cached > 0 {
			r["prompt_tokens_details"] = object{"cached_tokens": cached}
		}
		if thought > 0 {
			r["completion_tokens_details"] = object{"reasoning_tokens": thought}
		}
		return r
	case responses:
		r := object{"input_tokens": input, "output_tokens": output + thought, "total_tokens": total}
		if cached > 0 {
			r["input_tokens_details"] = object{"cached_tokens": cached}
		}
		if thought > 0 {
			r["output_tokens_details"] = object{"reasoning_tokens": thought}
		}
		return r
	default:
		r := object{"input_tokens": max(input-cached, 0), "output_tokens": output + thought}
		if thought > 0 {
			r["output_tokens_details"] = object{"thinking_tokens": thought}
		}
		if cached > 0 {
			r["cache_read_input_tokens"] = cached
		}
		return r
	}
}

func usageToGemini(u object, from string) object {
	in, out := number(u["input_tokens"]), number(u["output_tokens"])
	cached, thought := int64(0), int64(0)
	switch from {
	case chat:
		in, out = number(u["prompt_tokens"]), number(u["completion_tokens"])
		cached = number(obj(u["prompt_tokens_details"])["cached_tokens"])
		thought = number(obj(u["completion_tokens_details"])["reasoning_tokens"])
	case responses:
		cached = number(obj(u["input_tokens_details"])["cached_tokens"])
		thought = number(obj(u["output_tokens_details"])["reasoning_tokens"])
	case messages:
		thought = number(obj(u["output_tokens_details"])["thinking_tokens"])
		cached = number(u["cache_read_input_tokens"])
		in += cached + number(u["cache_creation_input_tokens"])
	}
	r := object{"promptTokenCount": in, "candidatesTokenCount": max(out-thought, 0), "totalTokenCount": in + out}
	if cached > 0 {
		r["cachedContentTokenCount"] = cached
	}
	if thought > 0 {
		r["thoughtsTokenCount"] = thought
	}
	return r
}

func (c *conversion) responseToGemini(root object) object {
	id, model := str(root["id"]), str(root["model"])
	var candidates []any
	add := func(parts []any, reason string, index int) {
		candidates = append(candidates, object{"index": index, "content": object{"role": "model", "parts": nonNil(parts)}, "finishReason": geminiFinish(reason)})
	}
	switch c.from {
	case chat:
		choices := c.array(root["choices"], "choices")
		if len(choices) == 0 {
			c.fail("Chat response requires choices")
		}
		for ci, cv := range choices {
			ch := c.object(cv, "choices[]")
			m := c.object(ch["message"], "message")
			parts := carrierParts(m)
			if parts == nil {
				parts = c.toParts(m["content"], "model")
				if thought := str(m["reasoning_content"]); thought != "" {
					parts = append([]any{object{"text": thought, "thought": true}}, parts...)
				}
				if refusal := str(m["refusal"]); refusal != "" {
					parts = append(parts, object{"text": refusal})
				}
				for _, tv := range c.array(m["tool_calls"], "tool_calls") {
					t := c.object(tv, "tool_calls[]")
					f := c.object(t["function"], "function")
					part := object{"functionCall": object{"id": str(t["id"]), "name": c.text(f["name"], "name"), "args": c.args(f["arguments"])}}
					parts = append(parts, part)
				}
			}
			add(parts, str(ch["finish_reason"]), ci)
		}
	case messages:
		if root["content"] == nil {
			c.fail("Messages response requires content")
		}
		add(c.toParts(root["content"], "model"), str(root["stop_reason"]), 0)
	case responses:
		if root["output"] == nil {
			c.fail("Responses response requires output")
		}
		var parts []any
		for _, iv := range c.array(root["output"], "output") {
			item := c.object(iv, "output[]")
			if p := carrierParts(item); p != nil {
				parts = append(parts, p...)
				continue
			}
			switch str(item["type"]) {
			case "message":
				parts = append(parts, c.toParts(item["content"], "model")...)
			case "reasoning":
				values := list(item["content"])
				if values == nil {
					values = list(item["summary"])
				}
				for _, v := range values {
					parts = append(parts, object{"text": str(obj(v)["text"]), "thought": true})
				}
				if item["encrypted_content"] != nil {
					c.loss("output.reasoning.encrypted_content", true)
				}
			case "function_call":
				parts = append(parts, object{"functionCall": object{"id": str(item["call_id"]), "name": str(item["name"]), "args": c.args(item["arguments"])}})
			default:
				c.reject("unsupported actual Responses output for Gemini")
			}
		}
		reason := "stop"
		if str(root["status"]) == "incomplete" {
			reason = "length"
		}
		add(parts, reason, 0)
	default:
		c.reject("unsupported Gemini response source")
	}
	if len(candidates) > 0 {
		candidate := obj(candidates[0])
		if grounding := c.responseGrounding(root, list(obj(candidate["content"])["parts"])); grounding != nil {
			candidate["groundingMetadata"] = grounding
		}
	}
	return object{"responseId": id, "modelVersion": model, "candidates": candidates, "usageMetadata": usageToGemini(obj(root["usage"]), c.from)}
}
