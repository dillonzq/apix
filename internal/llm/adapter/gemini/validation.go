package gemini

import (
	"bytes"
	"encoding/json"
	"strings"
)

func (c *conversion) numeric(v any, path string, integer bool) {
	if v == nil {
		return
	}
	n, ok := v.(json.Number)
	if !ok {
		c.fail(path + " must be a number")
		return
	}
	if integer {
		if _, err := n.Int64(); err != nil {
			c.fail(path + " must be an integer")
		}
	} else {
		if _, err := n.Float64(); err != nil {
			c.fail(path + " must be a finite number")
		}
	}
}
func (c *conversion) flags(v any, path string) {
	if v != nil {
		if _, ok := v.(bool); !ok {
			c.fail(path + " must be a boolean")
		}
	}
}
func (c *conversion) strings(v any, path string) {
	for _, entry := range c.array(v, path) {
		c.text(entry, path+"[]")
	}
}
func (c *conversion) aliases(o object, pairs ...string) {
	for i := 0; i < len(pairs); i += 2 {
		source, target := pairs[i], pairs[i+1]
		if v, ok := o[source]; ok {
			if prior, exists := o[target]; exists && !bytes.Equal(encode(v), encode(prior)) {
				c.fail("conflicting Gemini field aliases: " + target)
			} else {
				o[target] = v
			}
			delete(o, source)
		}
	}
}

// Normalize only protocol envelopes. Tool args, results, schema properties and
// other user data are never searched for field names or rewritten.
func (c *conversion) normalizeGemini(root object) {
	c.aliases(root, "system_instruction", "systemInstruction", "generation_config", "generationConfig", "tool_config", "toolConfig", "response_id", "responseId", "model_version", "modelVersion", "usage_metadata", "usageMetadata", "prompt_feedback", "promptFeedback")
	normalizeParts := func(content object) {
		for _, pv := range list(content["parts"]) {
			p := obj(pv)
			c.aliases(p, "function_call", "functionCall", "function_response", "functionResponse", "inline_data", "inlineData", "file_data", "fileData", "thought_signature", "thoughtSignature", "video_metadata", "videoMetadata")
			for _, key := range []string{"inlineData", "fileData"} {
				c.aliases(obj(p[key]), "mime_type", "mimeType", "file_uri", "fileUri", "display_name", "displayName")
			}
		}
	}
	normalizeParts(obj(root["systemInstruction"]))
	for _, cv := range list(root["contents"]) {
		normalizeParts(obj(cv))
	}
	for _, cv := range list(root["candidates"]) {
		cand := obj(cv)
		c.aliases(cand, "finish_reason", "finishReason", "grounding_metadata", "groundingMetadata")
		normalizeParts(obj(cand["content"]))
	}
}
func (c *conversion) validateConfig(root object) {
	if c.from != Format {
		for _, k := range []string{"temperature", "top_p"} {
			c.numeric(root[k], k, false)
		}
		for _, k := range []string{"top_k", "n", "max_tokens", "max_completion_tokens", "max_output_tokens"} {
			c.numeric(root[k], k, true)
		}
		c.flags(root["stream"], "stream")
		c.flags(root["parallel_tool_calls"], "parallel_tool_calls")
		t := c.object(root["thinking"], "thinking")
		c.numeric(t["budget_tokens"], "thinking.budget_tokens", true)
		return
	}
	cfg := c.object(first(root, "generationConfig", "generation_config"), "generationConfig")
	for _, keys := range [][2]string{{"temperature", "temperature"}, {"topP", "top_p"}} {
		c.numeric(first(cfg, keys[0], keys[1]), "generationConfig."+keys[0], false)
	}
	for _, keys := range [][2]string{{"topK", "top_k"}, {"maxOutputTokens", "max_output_tokens"}, {"candidateCount", "candidate_count"}} {
		c.numeric(first(cfg, keys[0], keys[1]), "generationConfig."+keys[0], true)
	}
	c.strings(first(cfg, "stopSequences", "stop_sequences"), "stopSequences")
	tc := c.object(first(cfg, "thinkingConfig", "thinking_config"), "thinkingConfig")
	budget := first(tc, "thinkingBudget", "thinking_budget")
	level := first(tc, "thinkingLevel", "thinking_level")
	c.numeric(budget, "thinkingBudget", true)
	c.text(level, "thinkingLevel")
	c.flags(first(tc, "includeThoughts", "include_thoughts"), "includeThoughts")
	if budget != nil && number(budget) < -1 {
		c.reject("unsupported negative thinking budget")
	}
	if budget != nil && level != nil {
		c.fail("thinkingBudget and thinkingLevel are mutually exclusive")
	}
}
func (c *conversion) schema(value any, path string) any {
	if value == nil {
		return object{"type": "object", "properties": object{}}
	}
	o := c.object(value, path)
	if o == nil {
		return nil
	}
	o = clone(o)
	if typ, ok := o["type"].(string); ok {
		o["type"] = strings.ToLower(typ)
	}

	if nullable, exists := o["nullable"]; exists {
		c.flags(nullable, path+".nullable")
		delete(o, "nullable")
		if boolean(nullable) {
			if typ, ok := o["type"].(string); ok {
				o["type"] = []any{typ, "null"}
			} else {
				base := clone(o)
				o = object{"anyOf": []any{base, object{"type": "null"}}}
			}
		}
	}

	if o["propertyOrdering"] != nil {
		c.loss(path+".propertyOrdering", false)
		delete(o, "propertyOrdering")
	}
	if props := c.object(o["properties"], path+".properties"); props != nil {
		converted := object{}
		for k, v := range props {
			converted[k] = c.schema(v, path+".properties."+k)
		}
		o["properties"] = converted
	}
	if o["items"] != nil {
		o["items"] = c.schema(o["items"], path+".items")
	}
	for _, k := range []string{"anyOf", "oneOf", "allOf"} {
		if o[k] != nil {
			var children []any
			for _, v := range c.array(o[k], path+"."+k) {
				children = append(children, c.schema(v, path+"."+k))
			}
			o[k] = children
		}
	}
	return o
}
