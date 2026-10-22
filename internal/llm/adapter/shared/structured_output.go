package shared

import (
	"encoding/json"

	"github.com/dillonzq/apix/internal/errclass"
)

// ConvertOutputFormat parses one source control and renders the target field.
// It never reparses or mutates a converted request envelope.
func ConvertOutputFormat(from, to string, raw json.RawMessage, ctx *ConversionContext) (json.RawMessage, *errclass.Error) {
	key := "response_format"
	if from == FormatResponses {
		key = "text"
	} else if from == FormatMessages {
		key = "output_config"
	}
	format, err := readOutputFormat(from, map[string]json.RawMessage{key: raw})
	if err != nil || format == nil {
		return nil, err
	}
	target := map[string]json.RawMessage{}
	if err := writeOutputFormat(to, format, target, ctx, from); err != nil {
		return nil, err
	}
	key = "response_format"
	if to == FormatResponses {
		key = "text"
	} else if to == FormatMessages {
		key = "output_config"
	}
	return target[key], nil
}
func cloneRaw(source map[string]json.RawMessage) map[string]json.RawMessage {
	out := make(map[string]json.RawMessage, len(source))
	for k, v := range source {
		out[k] = v
	}
	return out
}
func readOutputFormat(from string, source map[string]json.RawMessage) (map[string]json.RawMessage, *errclass.Error) {
	raw := source["response_format"]
	if from != FormatChatCompletions {
		key := "text"
		if from == FormatMessages {
			key = "output_config"
		}
		var config map[string]json.RawMessage
		if len(source[key]) > 0 && string(source[key]) != "null" {
			if json.Unmarshal(source[key], &config) != nil {
				return nil, errclass.Translation(key + " must be an object")
			}
		}
		raw = config["format"]
	}
	if len(raw) == 0 || string(raw) == "null" {
		return nil, nil
	}
	var format map[string]json.RawMessage
	if json.Unmarshal(raw, &format) != nil || format == nil {
		return nil, errclass.Translation("output format must be an object")
	}
	var typ string
	DecodeProbe(format["type"], &typ)
	if typ != "text" && typ != "json_object" && typ != "json_schema" {
		return nil, &errclass.Error{Class: errclass.ClassUnsupported, Message: "unsupported structured output format"}
	}
	if typ == "json_schema" && from == FormatChatCompletions {
		var schema map[string]json.RawMessage
		if json.Unmarshal(format["json_schema"], &schema) != nil || schema == nil {
			return nil, errclass.Translation("json_schema must be an object")
		}
		format = schema
		format["type"] = json.RawMessage(`"json_schema"`)
	}
	if typ == "json_schema" && from == FormatMessages {
		format["strict"] = json.RawMessage("true")
	}
	if typ == "json_schema" && !json.Valid(format["schema"]) {
		return nil, errclass.Translation("structured output requires a JSON schema")
	}
	if strict, ok := format["strict"]; ok && string(strict) != "true" && string(strict) != "false" {
		return nil, errclass.Translation("structured output strict must be a boolean")
	}
	return format, nil
}
func writeOutputFormat(to string, format map[string]json.RawMessage, target map[string]json.RawMessage, state *ConversionContext, from string) *errclass.Error {
	var typ string
	DecodeProbe(format["type"], &typ)
	if typ == "text" {
		return nil
	}
	format = cloneRaw(format)
	formatPath := "response_format.json_schema"
	if from == FormatResponses {
		formatPath = "text.format"
	} else if from == FormatMessages {
		formatPath = "output_config.format"
	}
	switch to {
	case FormatChatCompletions:
		if typ == "json_schema" {
			delete(format, "type")
			if len(format["name"]) == 0 {
				format["name"] = json.RawMessage(`"response"`)
			}
			nested, _ := json.Marshal(format)
			target["response_format"], _ = json.Marshal(map[string]json.RawMessage{"type": json.RawMessage(`"json_schema"`), "json_schema": nested})
		} else {
			target["response_format"], _ = json.Marshal(format)
		}
	case FormatResponses:
		if typ == "json_schema" && len(format["name"]) == 0 {
			format["name"] = json.RawMessage(`"response"`)
		}
		target["text"], _ = json.Marshal(map[string]any{"format": format})
	case FormatMessages:
		if typ != "json_schema" {
			return &errclass.Error{Class: errclass.ClassUnsupported, Message: "Messages requires json_schema for structured output"}
		}
		// Anthropic's output_config.format carries type/schema, not OpenAI's
		// schema name, description, or optional strictness flag.
		for _, key := range []string{"name", "description"} {
			if len(format[key]) > 0 {
				d := DiagnosticFor(from, to, formatPath+"."+key, false)
				state.Record(d)
				if state != nil && state.Options.LossPolicy == LossPolicyStrict {
					return &errclass.Error{Class: errclass.ClassUnsupported, Message: d.Message}
				}
			}
		}
		if string(format["strict"]) == "false" {
			d := DiagnosticFor(from, to, formatPath+".strict", true)
			state.Record(d)
			if state != nil && (state.Options.LossPolicy == LossPolicySafe || state.Options.LossPolicy == LossPolicyStrict) {
				return &errclass.Error{Class: errclass.ClassUnsupported, Message: d.Message}
			}
		}
		config := map[string]any{"format": map[string]json.RawMessage{"type": format["type"], "schema": format["schema"]}}
		target["output_config"], _ = json.Marshal(config)
	}
	return nil
}
