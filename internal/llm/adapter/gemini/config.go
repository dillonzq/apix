package gemini

import (
	"strings"

	"github.com/dillonzq/apix/internal/llm/adapter/shared"
	"github.com/dillonzq/apix/thinking"
)

func (c *conversion) checkSourceFields(root object) {
	known := []string{"model", "stream", "temperature", "top_p", "tools", "tool_choice", "parallel_tool_calls"}
	switch c.from {
	case chat:
		known = append(known, "messages", "max_tokens", "max_completion_tokens", "stop", "reasoning_effort", "response_format", "n", "top_k")
	case messages:
		known = append(known, "messages", "system", "max_tokens", "stop_sequences", "thinking", "output_config", "top_k")
	case responses:
		known = append(known, "input", "instructions", "max_output_tokens", "reasoning", "text")
	}
	c.unknown(root, "", known...)
}
func (c *conversion) toConfig(root, out object) {
	cfg := object{}
	for from, to := range map[string]string{"temperature": "temperature", "top_p": "topP", "top_k": "topK"} {
		if v := root[from]; v != nil {
			cfg[to] = v
		}
	}
	for _, k := range []string{"max_completion_tokens", "max_output_tokens", "max_tokens"} {
		if v := root[k]; v != nil {
			cfg["maxOutputTokens"] = v
			break
		}
	}
	if n := root["n"]; n != nil {
		cfg["candidateCount"] = n
	}
	if stop := first(root, "stop_sequences", "stop"); stop != nil {
		if s, ok := stop.(string); ok {
			cfg["stopSequences"] = []any{s}
		} else {
			cfg["stopSequences"] = c.array(stop, "stop")
		}
	}
	var tc object
	switch c.from {
	case chat:
		if v := root["reasoning_effort"]; v != nil {
			tc = c.effort(c.text(v, "reasoning_effort"))
		}
	case responses:
		r := c.object(root["reasoning"], "reasoning")
		if r["effort"] != nil {
			tc = c.effort(c.text(r["effort"], "reasoning.effort"))
		}
		c.unknown(r, "reasoning.", "effort", "summary")
		if r["summary"] != nil {
			tc = ensure(tc)
			tc["includeThoughts"] = str(r["summary"]) != "none"
		}
	case messages:
		t := c.object(root["thinking"], "thinking")
		switch str(t["type"]) {
		case "disabled":
			tc = object{"thinkingBudget": 0}
		case "enabled":
			tc = object{"thinkingBudget": t["budget_tokens"]}
		case "adaptive":
			level := str(obj(root["output_config"])["effort"])
			if level != "" {
				tc = c.effort(level)
			}
		case "":
		default:
			c.reject("unsupported thinking type")
		}
		if t["display"] != nil {
			tc = ensure(tc)
			tc["includeThoughts"] = str(t["display"]) != "omitted"
		}
	}
	tc = c.thinkingMode(tc)
	if c.ctx.Options.GeminiIncludeThoughts != nil {
		tc = ensure(tc)
		tc["includeThoughts"] = *c.ctx.Options.GeminiIncludeThoughts
	}
	if len(tc) > 0 {
		cfg["thinkingConfig"] = tc
	}
	c.toSchema(root, cfg)
	if len(cfg) > 0 {
		out["generationConfig"] = cfg
	}
}
func ensure(o object) object {
	if o == nil {
		return object{}
	}
	return o
}
func (c *conversion) effort(e string) object {
	if e == "auto" {
		return object{"thinkingBudget": -1}
	}
	if e == "none" {
		return object{"thinkingBudget": 0}
	}
	if _, ok := thinking.BudgetFromEffort(e); !ok {
		c.reject("unsupported reasoning effort")
	}
	return object{"thinkingLevel": e}
}
func (c *conversion) thinkingMode(t object) object {
	if t == nil {
		return nil
	}
	mode := c.ctx.Options.GeminiThinkingMode
	if mode == "budget" && t["thinkingLevel"] != nil {
		budget, ok := thinking.BudgetFromEffort(str(t["thinkingLevel"]))
		if !ok {
			c.reject("reasoning level has no budget equivalent")
		}
		delete(t, "thinkingLevel")
		t["thinkingBudget"] = budget
	}
	if mode == "level" && t["thinkingBudget"] != nil {
		var budget int64
		switch v := t["thinkingBudget"].(type) {
		case int:
			budget = int64(v)
		case int64:
			budget = v
		default:
			budget = number(v)
		}
		if budget > 0 {
			level, ok := thinking.EffortFromBudget(budget)
			if !ok {
				c.reject("reasoning budget has no level equivalent")
			}
			delete(t, "thinkingBudget")
			t["thinkingLevel"] = level
		}
	}
	return t
}
func (c *conversion) toSchema(root, cfg object) {
	var format object
	switch c.from {
	case chat:
		format = c.object(root["response_format"], "response_format")
	case responses:
		format = c.object(obj(root["text"])["format"], "text.format")
	case messages:
		format = c.object(obj(root["output_config"])["format"], "output_config.format")
	}
	if format == nil {
		return
	}
	typ := str(format["type"])
	if typ == "text" {
		return
	}
	if typ != "json_schema" && typ != "json_object" {
		c.reject("unsupported output format")
		return
	}
	cfg["responseMimeType"] = "application/json"
	if typ == "json_object" {
		return
	}
	schema := format
	if c.from == chat {
		schema = c.object(format["json_schema"], "json_schema")
	}
	if schema["schema"] == nil {
		c.fail("structured output requires schema")
	}
	cfg["responseJsonSchema"] = schema["schema"]
	for _, k := range []string{"name", "description"} {
		if schema[k] != nil {
			c.loss("output_format."+k, false)
		}
	}
	if schema["strict"] != nil {
		c.loss("output_format.strict", true)
	}
}
func (c *conversion) fromConfig(root, out object) {
	cfg := c.object(first(root, "generationConfig", "generation_config"), "generationConfig")
	c.unknown(cfg, "generationConfig.", "temperature", "topP", "top_p", "topK", "top_k", "maxOutputTokens", "max_output_tokens", "stopSequences", "stop_sequences", "candidateCount", "candidate_count", "responseMimeType", "response_mime_type", "responseJsonSchema", "response_json_schema", "responseSchema", "response_schema", "thinkingConfig", "thinking_config")
	for _, pair := range [][3]string{{"temperature", "temperature", "temperature"}, {"topP", "top_p", "top_p"}, {"topK", "top_k", "top_k"}} {
		if v := first(cfg, pair[0], pair[1]); v != nil {
			if pair[0] == "topK" && c.to == responses {
				c.loss("generationConfig.topK", true)
			} else {
				out[pair[2]] = v
			}
		}
	}
	maxKey := "max_tokens"
	if c.to == responses {
		maxKey = "max_output_tokens"
	}
	if v := first(cfg, "maxOutputTokens", "max_output_tokens"); v != nil {
		out[maxKey] = v
	} else if c.to == messages {
		out[maxKey] = 4096
	}
	if stop := first(cfg, "stopSequences", "stop_sequences"); stop != nil {
		if c.to == responses {
			c.loss("generationConfig.stopSequences", true)
		} else {
			key := "stop"
			if c.to == messages {
				key = "stop_sequences"
			}
			out[key] = stop
		}
	}
	if n := first(cfg, "candidateCount", "candidate_count"); n != nil {
		if c.to == chat {
			out["n"] = n
		} else if number(n) > 1 {
			c.loss("generationConfig.candidateCount", true)
		}
	}
	tc := c.object(first(cfg, "thinkingConfig", "thinking_config"), "thinkingConfig")
	level := str(first(tc, "thinkingLevel", "thinking_level"))
	budget := first(tc, "thinkingBudget", "thinking_budget")
	if level != "" && budget != nil {
		c.fail("thinkingLevel and thinkingBudget are mutually exclusive")
	}
	if level == "" && budget != nil {
		var ok bool
		level, ok = thinking.EffortFromBudget(number(budget))
		if !ok {
			c.reject("unsupported thinking budget")
		}
	}
	if level != "" {
		switch c.to {
		case chat:
			if level != "auto" {
				out["reasoning_effort"] = level
			}
		case responses:
			if level != "auto" {
				out["reasoning"] = object{"effort": level}
			}
		case messages:
			if level == "none" {
				out["thinking"] = object{"type": "disabled"}
			} else if level != "auto" {
				// Adaptive thinking (the default) carries the level as effort; manual
				// mode keeps an explicit budget numeric or resolves the level to one.
				if c.ctx.Options.MessagesThinkingMode != shared.MessagesThinkingManual {
					out["thinking"] = object{"type": "adaptive"}
					cfg := ensure(obj(out["output_config"]))
					cfg["effort"] = level
					out["output_config"] = cfg
				} else if budget != nil {
					out["thinking"] = object{"type": "enabled", "budget_tokens": budget}
				} else {
					value, _ := thinking.BudgetFromEffort(level)
					out["thinking"] = object{"type": "enabled", "budget_tokens": value}
				}
			}
		}
	}
	c.unknown(tc, "generationConfig.thinkingConfig.", "thinkingLevel", "thinking_level", "thinkingBudget", "thinking_budget", "includeThoughts", "include_thoughts")
	if visibility := first(tc, "includeThoughts", "include_thoughts"); visibility != nil {
		if _, ok := visibility.(bool); !ok {
			c.fail("includeThoughts must be a boolean")
		}
		switch c.to {
		case responses:
			r := ensure(obj(out["reasoning"]))
			if boolean(visibility) {
				r["summary"] = "auto"
			} else {
				r["summary"] = "none"
			}
			out["reasoning"] = r
		case messages:
			if t := obj(out["thinking"]); t != nil && str(t["type"]) != "disabled" {
				if boolean(visibility) {
					t["display"] = "summarized"
				} else {
					t["display"] = "omitted"
				}
			}
		default:
			c.loss("generationConfig.thinkingConfig.includeThoughts", false)
		}
	}
	mime := str(first(cfg, "responseMimeType", "response_mime_type"))
	schema := first(cfg, "responseJsonSchema", "response_json_schema", "responseSchema", "response_schema")
	if mime != "" && mime != "text/plain" && mime != "application/json" {
		c.reject("unsupported generated MIME type")
	}
	if mime == "application/json" || schema != nil {
		format := object{"type": "json_object"}
		if schema != nil {
			format = object{"type": "json_schema", "schema": c.schema(schema, "generationConfig.responseSchema")}
		}
		switch c.to {
		case chat:
			if schema != nil {
				format = object{"type": "json_schema", "json_schema": object{"name": "response", "schema": c.schema(schema, "generationConfig.responseSchema")}}
			}
			out["response_format"] = format
		case responses:
			if schema != nil {
				format["name"] = "response"
			}
			out["text"] = object{"format": format}
		case messages:
			if schema == nil {
				c.reject("Messages structured output requires a schema")
			} else {
				oc := ensure(obj(out["output_config"]))
				oc["format"] = format
				out["output_config"] = oc
			}
		}
	}
}
func (c *conversion) toTools(root, out object) {
	var declarations, tools []any
	for _, tv := range c.array(root["tools"], "tools") {
		t := c.object(tv, "tools[]")
		typ := str(t["type"])
		fn := t
		if c.from == chat {
			fn = c.object(t["function"], "function")
		}
		if typ == "web_search" || typ == "web_search_preview" || strings.HasPrefix(typ, "web_search_") {
			c.unknown(t, "tools.web_search.", "type", "name")
			tools = append(tools, object{"googleSearch": object{}})
			continue
		}
		if typ != "function" && typ != "" && !(c.from == messages && typ == "custom") {
			c.loss("tools."+typ, true)
			continue
		}
		name := c.text(fn["name"], "function.name")
		if strings.TrimSpace(name) == "" {
			c.fail("function declaration requires a name")
		}
		decl := object{"name": name}
		if fn["description"] != nil {
			decl["description"] = c.text(fn["description"], "description")
		}
		schema := first(fn, "parameters", "input_schema")
		if schema == nil {
			schema = object{"type": "object", "properties": object{}}
		}
		decl["parametersJsonSchema"] = schema
		c.unknown(fn, "tools.function.", "type", "name", "description", "parameters", "input_schema")
		declarations = append(declarations, decl)
	}
	if len(declarations) > 0 {
		tools = append(tools, object{"functionDeclarations": declarations})
	}
	if len(tools) > 0 {
		out["tools"] = tools
	}
	choice := root["tool_choice"]
	mode := ""
	var allowed []any
	if s, ok := choice.(string); ok {
		switch s {
		case "auto":
			mode = "AUTO"
		case "none":
			mode = "NONE"
		case "required":
			mode = "ANY"
		default:
			c.reject("unsupported tool_choice")
		}
	} else if choice != nil {
		o := c.object(choice, "tool_choice")
		switch str(o["type"]) {
		case "auto":
			mode = "AUTO"
		case "none":
			mode = "NONE"
		case "any", "required":
			mode = "ANY"
		case "function", "tool":
			mode = "ANY"
			name := str(o["name"])
			if name == "" {
				name = str(obj(o["function"])["name"])
			}
			if name == "" {
				c.fail("named tool choice requires a name")
			}
			allowed = []any{name}
		default:
			c.loss("tool_choice", true)
		}
		if o["disable_parallel_tool_use"] != nil && boolean(o["disable_parallel_tool_use"]) {
			c.loss("tool_choice.disable_parallel_tool_use", true)
		}
	}
	if v := root["parallel_tool_calls"]; v != nil && !boolean(v) {
		c.loss("parallel_tool_calls", true)
	}
	if mode != "" {
		f := object{"mode": mode}
		if len(allowed) > 0 {
			f["allowedFunctionNames"] = allowed
		}
		out["toolConfig"] = object{"functionCallingConfig": f}
	}
}
func (c *conversion) fromTools(root, out object) {
	var tools []any
	for _, tv := range c.array(root["tools"], "tools") {
		t := c.object(tv, "tools[]")
		c.unknown(t, "tools.", "functionDeclarations", "function_declarations", "googleSearch", "google_search")
		if search := first(t, "googleSearch", "google_search"); search != nil {
			c.unknown(c.object(search, "googleSearch"), "tools.googleSearch.")
			switch c.to {
			case responses:
				tools = append(tools, object{"type": "web_search"})
			case messages:
				tools = append(tools, object{"type": "web_search_20250305", "name": "web_search"})
			default:
				c.loss("tools.googleSearch", true)
			}
		}
		for _, dv := range c.array(first(t, "functionDeclarations", "function_declarations"), "functionDeclarations") {
			d := c.object(dv, "functionDeclarations[]")
			name := c.text(d["name"], "function.name")
			if name == "" {
				c.fail("function declaration requires a name")
			}
			schema := first(d, "parametersJsonSchema", "parameters_json_schema", "parameters")
			if schema == nil {
				schema = object{"type": "object", "properties": object{}}
			}
			f := object{"name": name}
			if d["description"] != nil {
				f["description"] = d["description"]
			}
			key := "parameters"
			if c.to == messages {
				key = "input_schema"
			}
			f[key] = c.schema(schema, "tools.functionDeclarations.parameters")
			switch c.to {
			case chat:
				tools = append(tools, object{"type": "function", "function": f})
			case responses:
				f["type"] = "function"
				tools = append(tools, f)
			case messages:
				tools = append(tools, f)
			}
			c.unknown(d, "tools.functionDeclarations.", "name", "description", "parameters", "parametersJsonSchema", "parameters_json_schema")
		}
	}
	if len(tools) > 0 {
		out["tools"] = tools
	}
	config := c.object(first(root, "toolConfig", "tool_config"), "toolConfig")
	f := c.object(first(config, "functionCallingConfig", "function_calling_config"), "functionCallingConfig")
	mode := str(f["mode"])
	names := c.array(first(f, "allowedFunctionNames", "allowed_function_names"), "allowedFunctionNames")
	if len(names) > 1 {
		c.loss("toolConfig.allowedFunctionNames", true)
	}
	c.strings(first(f, "allowedFunctionNames", "allowed_function_names"), "allowedFunctionNames")
	if len(names) > 0 && mode != "ANY" {
		c.loss("toolConfig.allowedFunctionNames", true)
	}
	if len(names) == 1 && mode == "ANY" {
		switch c.to {
		case chat:
			out["tool_choice"] = object{"type": "function", "function": object{"name": names[0]}}
		case responses:
			out["tool_choice"] = object{"type": "function", "name": names[0]}
		case messages:
			out["tool_choice"] = object{"type": "tool", "name": names[0]}
		}
	} else if mode != "" {
		value := ""
		switch mode {
		case "AUTO":
			value = "auto"
		case "ANY":
			value = "required"
		case "NONE":
			value = "none"
		case "VALIDATED":
			value = "auto"
			c.loss("toolConfig.mode", true)
		default:
			c.reject("unsupported Gemini function calling mode")
		}
		if c.to == messages {
			if value == "required" {
				value = "any"
			}
			out["tool_choice"] = object{"type": value}
		} else {
			out["tool_choice"] = value
		}
	}
	c.unknown(config, "toolConfig.", "functionCallingConfig", "function_calling_config")
	c.unknown(f, "toolConfig.functionCallingConfig.", "mode", "allowedFunctionNames", "allowed_function_names")
}
