package gemini

import (
	"fmt"
	"strings"

	"github.com/dillonzq/apix/internal/errclass"
	"github.com/dillonzq/apix/internal/llm/adapter/shared"
)

// ConvertRequest converts a direct path with GenerateContent at one end.
func ConvertRequest(from, to, model string, body []byte, ctx *shared.ConversionContext) ([]byte, *errclass.Error) {
	ctx = shared.ContextOrNew([]*shared.ConversionContext{ctx})
	return ctx.WithToolTransaction(func(work *shared.ConversionContext) ([]byte, *errclass.Error) {
		c := &conversion{from: from, to: to, ctx: work, native: from == to}
		if err := work.ValidatePolicy(); err != nil {
			return nil, err
		}
		if from == responses {
			var err *errclass.Error
			body, err = shared.NormalizeResponsesRequest(body, work)
			if err != nil {
				return nil, err
			}
		}
		root, err := decode(body)
		if err != nil {
			return nil, err
		}
		if from == Format {
			c.normalizeGemini(root)
		}
		c.validateConfig(root)
		if c.err != nil {
			return nil, c.err
		}
		if from == Format && to == Format {
			c.nativeRequest(root)
			options := work.Options
			if options.GeminiThinkingMode != "" || options.GeminiIncludeThoughts != nil {
				cfg := ensure(c.object(first(root, "generationConfig", "generation_config"), "generationConfig"))
				tc := ensure(c.object(first(cfg, "thinkingConfig", "thinking_config"), "thinkingConfig"))
				for _, pair := range [][2]string{{"thinking_budget", "thinkingBudget"}, {"thinking_level", "thinkingLevel"}, {"include_thoughts", "includeThoughts"}} {
					if value, ok := tc[pair[0]]; ok {
						tc[pair[1]] = value
						delete(tc, pair[0])
					}
				}
				tc = c.thinkingMode(tc)
				if options.GeminiIncludeThoughts != nil {
					tc["includeThoughts"] = *options.GeminiIncludeThoughts
				}
				delete(cfg, "thinking_config")
				cfg["thinkingConfig"] = tc
				delete(root, "generation_config")
				root["generationConfig"] = cfg
				body = encode(root)
			}
			if c.err != nil {
				return nil, c.err
			}
			return body, nil
		}
		var out object
		if to == Format {
			out = c.toGemini(root)
		} else if from == Format {
			out = c.fromGemini(root, model)
		} else {
			return nil, unsupported("expected a Gemini conversion path")
		}
		if c.err != nil {
			return nil, c.err
		}
		return encode(out), nil
	})
}

func (c *conversion) nativeRequest(root object) {
	contents := c.array(root["contents"], "contents")
	if len(contents) == 0 {
		c.fail("Gemini request requires contents")
	}
	for _, v := range contents {
		ct := c.object(v, "contents[]")
		parts := c.array(ct["parts"], "contents[].parts")
		if len(parts) == 0 {
			c.fail("Gemini content requires parts")
		}
		c.validateParts(parts)
		// Native fields are preserved. Validate structure without loss reporting.
		for _, pv := range parts {
			p := c.object(pv, "parts[]")
			if p == nil {
				c.fail("parts must contain objects")
			}
			if c.ctx.Options.GeminiRequireThoughtSignatures && p["functionCall"] != nil && signature(p) == "" {
				c.reject("Gemini function call history requires thoughtSignature")
			}
		}
	}
}

func (c *conversion) toGemini(root object) object {
	out := object{}
	var contents, system []any
	calls := map[string]string{}
	add := func(role string, parts []any) {
		if len(parts) == 0 {
			return
		}
		contents = append(contents, object{"role": role, "parts": parts})
	}
	content := func(role string, v any) []any { return c.toParts(v, role) }
	if c.from == responses {
		if root["instructions"] != nil {
			system = append(system, object{"text": c.text(root["instructions"], "instructions")})
		}
		input := root["input"]
		if s, ok := input.(string); ok {
			add("user", []any{object{"text": s}})
		} else {
			for _, v := range c.array(input, "input") {
				item := c.object(v, "input[]")
				typ := str(item["type"])
				role := str(item["role"])
				if p := carrierParts(item); p != nil {
					c.validateParts(p)
					add("model", p)
					for _, pv := range p {
						if fc := obj(obj(pv)["functionCall"]); fc != nil {
							calls[str(item["call_id"])] = str(fc["name"])
						}
					}
					continue
				}
				switch typ {
				case "message", "":
					if role == "system" || role == "developer" {
						system = append(system, content(role, item["content"])...)
					} else if role == "user" || role == "assistant" {
						if role == "assistant" {
							role = "model"
						}
						add(role, content(role, item["content"]))
					} else {
						c.reject("unsupported Responses message role")
					}
				case "function_call":
					id, name := c.text(item["call_id"], "call_id"), c.text(item["name"], "name")
					if id == "" || name == "" {
						c.fail("function_call requires call_id and name")
					}
					calls[id] = name
					p := object{"functionCall": object{"id": id, "name": name, "args": c.args(item["arguments"])}}
					if sig := signatureFrom(item); sig != "" {
						p["thoughtSignature"] = sig
					}
					add("model", []any{p})
				case "function_call_output":
					id := c.text(item["call_id"], "call_id")
					name := calls[id]
					if name == "" {
						c.fail("function output has no matching call")
					}
					add("user", []any{object{"functionResponse": object{"id": id, "name": name, "response": c.toolResult(item["output"])}}})
				case "reasoning":
					for _, pv := range c.array(item["content"], "reasoning.content") {
						part := c.object(pv, "reasoning.content[]")
						if str(part["type"]) == "reasoning_text" {
							add("model", []any{object{"text": c.text(part["text"], "reasoning.text"), "thought": true}})
						} else {
							c.loss("input.reasoning.content", true)
						}
					}
					if item["encrypted_content"] != nil {
						c.loss("input.reasoning.encrypted_content", true)
					}
				case "web_search_call":
					c.loss("input.web_search_call", true)
				default:
					c.reject("unsupported Responses input item for Gemini")
				}
			}
		}
	} else {
		if c.from == messages && root["system"] != nil {
			system = append(system, content("system", root["system"])...)
		}
		for _, v := range c.array(root["messages"], "messages") {
			m := c.object(v, "messages[]")
			role := c.text(m["role"], "message.role")
			if role == "system" || role == "developer" {
				system = append(system, content(role, m["content"])...)
				continue
			}
			if role == "tool" && c.from == chat {
				id := c.text(m["tool_call_id"], "tool_call_id")
				name := calls[id]
				if name == "" {
					c.fail("tool reply has no matching call")
				}
				add("user", []any{object{"functionResponse": object{"id": id, "name": name, "response": c.toolResult(m["content"])}}})
				continue
			}
			if role != "user" && role != "assistant" {
				c.reject("unsupported message role for Gemini")
				continue
			}
			if role == "assistant" {
				role = "model"
			}
			var parts []any
			if p := carrierParts(m); p != nil {
				c.validateParts(p)
				parts = p
			} else {
				parts = content(role, m["content"])
				if reasoning := str(m["reasoning_content"]); reasoning != "" {
					parts = append([]any{object{"text": reasoning, "thought": true}}, parts...)
				}
			}
			for _, cv := range c.array(m["tool_calls"], "tool_calls") {
				call := c.object(cv, "tool_calls[]")
				fn := c.object(call["function"], "tool_call.function")
				id, name := c.text(call["id"], "tool_call.id"), c.text(fn["name"], "function.name")
				if name == "" || id == "" {
					c.fail("tool call requires id and name")
				}
				calls[id] = name
				if carrierParts(m) == nil {
					p := object{"functionCall": object{"id": id, "name": name, "args": c.args(fn["arguments"])}}
					if sig := signatureFrom(call); sig != "" {
						p["thoughtSignature"] = sig
					}
					parts = append(parts, p)
				}
			}
			for _, pv := range parts {
				p := obj(pv)
				if fc := obj(p["functionCall"]); fc != nil {
					calls[str(fc["id"])] = str(fc["name"])
				}
				if fr := obj(p["functionResponse"]); fr != nil && str(fr["name"]) == "" {
					fr["name"] = calls[str(fr["id"])]
					if str(fr["name"]) == "" {
						c.fail("tool result has no matching call")
					}
				}
			}
			add(role, parts)
		}
	}
	if len(contents) == 0 {
		c.fail("conversion produced no Gemini contents")
	}
	out["contents"] = contents
	if len(system) > 0 {
		out["systemInstruction"] = object{"parts": system}
	}
	c.toConfig(root, out)
	c.toTools(root, out)
	c.checkSourceFields(root)
	if c.ctx.Options.GeminiRequireThoughtSignatures {
		for _, cv := range contents {
			for _, pv := range list(obj(cv)["parts"]) {
				part := obj(pv)
				if part["functionCall"] != nil && signature(part) == "" {
					c.reject("Gemini function call history requires thoughtSignature")
				}
			}
		}
	}
	return out
}

func (c *conversion) toolResult(v any) object {
	if v == nil {
		return object{"result": ""}
	}
	if _, ok := v.(string); ok {
		return object{"result": v}
	}
	if o := obj(v); o != nil {
		return o
	}
	if a, ok := v.([]any); ok {
		var texts []string
		for _, pv := range a {
			p := c.object(pv, "tool result part")
			switch str(p["type"]) {
			case "text", "input_text", "output_text":
				texts = append(texts, c.text(p["text"], "text"))
			default:
				c.reject("Gemini tool result media is not representable by this path")
			}
		}
		return object{"result": strings.Join(texts, "\n")}
	}
	c.fail("tool result must be text, an object or content parts")
	return nil
}

func (c *conversion) fromGemini(root object, model string) object {
	out := object{"model": model}
	var msgs, items []any
	if c.to != chat && c.to != messages && c.to != responses {
		c.reject("unsupported Gemini output request protocol")
		return out
	}
	c.unknown(root, "", "model", "stream", "contents", "systemInstruction", "system_instruction", "tools", "toolConfig", "tool_config", "generationConfig", "generation_config")
	if sys := c.object(first(root, "systemInstruction", "system_instruction"), "systemInstruction"); sys != nil {
		var text strings.Builder
		for _, v := range c.array(sys["parts"], "systemInstruction.parts") {
			p := c.object(v, "parts[]")
			if p["text"] == nil {
				c.reject("system instruction media is unsupported")
			}
			if text.Len() > 0 {
				text.WriteString("\n")
			}
			text.WriteString(c.text(p["text"], "text"))
		}
		switch c.to {
		case chat:
			msgs = append(msgs, object{"role": "system", "content": text.String()})
		case messages:
			out["system"] = text.String()
		case responses:
			out["instructions"] = text.String()
		}
	}
	callIndex := 0
	pending := map[string][]string{}
	for _, cv := range c.array(root["contents"], "contents") {
		ct := c.object(cv, "contents[]")
		role := str(ct["role"])
		if role == "" {
			role = "user"
		}
		if role != "user" && role != "model" {
			c.reject("unsupported Gemini content role")
		}
		assistant := role == "model"
		if assistant {
			role = "assistant"
		}
		parts := c.array(ct["parts"], "parts")
		c.validateParts(parts)
		var content, calls []any
		flush := func() {
			if len(content) == 0 {
				return
			}
			if c.to == responses {
				items = append(items, object{"type": "message", "role": role, "content": content})
			} else {
				msgs = append(msgs, object{"role": role, "content": content})
			}
			content = nil
		}
		for _, pv := range parts {
			p := obj(pv)
			if p["text"] != nil {
				if boolean(p["thought"]) {
					switch c.to {
					case chat:
						flush()
						msgs = append(msgs, object{"role": "assistant", "content": nil, "reasoning_content": str(p["text"])})
					case responses:
						flush()
						items = append(items, object{"type": "reasoning", "content": []any{object{"type": "reasoning_text", "text": str(p["text"])}}, "summary": []any{object{"type": "summary_text", "text": str(p["text"])}}})
					case messages:
						c.loss("contents.parts.thought", true)
						if c.ctx.Options.AllowUnsignedThinking {
							content = append(content, object{"type": "thinking", "thinking": str(p["text"])})
						}
					}
					continue
				}
				typ := "text"
				if c.to == responses {
					typ = "input_text"
					if assistant {
						typ = "output_text"
					}
				}
				content = append(content, object{"type": typ, "text": str(p["text"])})
				continue
			}
			if fc := obj(p["functionCall"]); fc != nil {
				if !assistant {
					c.fail("functionCall requires model role")
				}
				id := callID(fc, callIndex)
				callIndex++
				name := str(fc["name"])
				pending[name] = append(pending[name], id)
				switch c.to {
				case chat:
					calls = append(calls, object{"id": id, "type": "function", "function": object{"name": name, "arguments": string(encode(c.args(fc["args"])))}})
				case messages:
					content = append(content, object{"type": "tool_use", "id": id, "name": name, "input": c.args(fc["args"])})
				case responses:
					flush()
					items = append(items, object{"type": "function_call", "call_id": id, "name": name, "arguments": string(encode(c.args(fc["args"])))})
				}
				continue
			}
			if fr := obj(p["functionResponse"]); fr != nil {
				if assistant {
					c.fail("functionResponse requires user role")
				}
				name := str(fr["name"])
				id := str(fr["id"])
				queue := pending[name]
				if id == "" && len(queue) > 0 {
					id = queue[0]
				}
				match := -1
				for i, known := range queue {
					if known == id {
						match = i
						break
					}
				}
				if id == "" || match < 0 {
					c.fail("functionResponse has no matching functionCall")
				} else {
					pending[name] = append(queue[:match], queue[match+1:]...)
				}
				val := fr["response"]
				text := stringOrJSON(val)
				switch c.to {
				case chat:
					flush()
					msgs = append(msgs, object{"role": "tool", "tool_call_id": id, "content": text})
				case messages:
					content = append(content, object{"type": "tool_result", "tool_use_id": id, "content": text})
				case responses:
					flush()
					items = append(items, object{"type": "function_call_output", "call_id": id, "output": text})
				}
				continue
			}
			if signature(p) != "" && len(p) == 1 {
				c.loss("contents.parts.thoughtSignature", true)
				continue
			}
			if v := c.fromMedia(p); v != nil {
				content = append(content, v)
			}
		}
		if c.to == chat && len(calls) > 0 {
			msgs = append(msgs, object{"role": role, "content": content, "tool_calls": calls})
			content = nil
		}
		flush()
		// Signed source history cannot be used by a different provider. Do not
		// present Gemini metadata as a Claude/OpenAI native signature.
		for i, pv := range parts {
			if signature(obj(pv)) != "" {
				c.loss(fmt.Sprintf("contents.parts[%d].thoughtSignature", i), true)
			}
		}
	}
	if c.to == responses {
		out["input"] = items
	} else {
		out["messages"] = msgs
	}
	c.fromConfig(root, out)
	c.fromTools(root, out)
	return out
}
