package codec

import (
	"encoding/json"
	"strings"

	"github.com/dillonzq/apix"
)

type featureSet map[string]bool
type featureObject map[string]json.RawMessage

func featureMap(raw json.RawMessage) featureObject {
	var out featureObject
	_ = json.Unmarshal(raw, &out)
	return out
}
func featureList(raw json.RawMessage) []json.RawMessage {
	var out []json.RawMessage
	_ = json.Unmarshal(raw, &out)
	return out
}
func featureString(raw json.RawMessage) string {
	var out string
	_ = json.Unmarshal(raw, &out)
	return out
}
func featurePresent(raw json.RawMessage) bool { return len(raw) > 0 && string(raw) != "null" }
func featureAlias(o featureObject, keys ...string) json.RawMessage {
	for _, k := range keys {
		if raw, ok := o[k]; ok {
			return raw
		}
	}
	return nil
}

// requiredFeatures follows protocol content paths, not arbitrary JSON keys.
// A tool's schema or result JSON can contain identical words without requiring
// media support. The fixed ordering makes Inspect output reproducible.
func requiredFeatures(id apix.Protocol, body []byte, stream bool) []string {
	root := featureMap(body)
	set := featureSet{}
	if stream {
		set[apix.FeatureStream] = true
	}
	set.tools(root["tools"])
	switch id {
	case ChatCompletions, Messages:
		if id == Messages {
			set.content(root["system"])
		}
		for _, raw := range featureList(root["messages"]) {
			message := featureMap(raw)
			set.content(message["content"])
			if len(featureList(message["images"])) > 0 {
				set[apix.FeatureImageInput] = true
			}
			if featurePresent(message["audio"]) {
				set[apix.FeatureAudioInput] = true
			}
			set.carrier(message)
		}
		if id == ChatCompletions {
			set.outputModalities(root["modalities"])
			if featurePresent(root["audio"]) {
				set[apix.FeatureAudioOutput] = true
			}
			set.structured(root["response_format"])
		} else {
			set.structured(featureMap(root["output_config"])["format"])
		}
	case Responses:
		set.content(root["input"])
		set.structured(featureMap(root["text"])["format"])
	case Gemini:
		set.geminiContent(featureAlias(root, "systemInstruction", "system_instruction"))
		for _, v := range featureList(root["contents"]) {
			set.geminiContent(v)
		}
		cfg := featureMap(featureAlias(root, "generationConfig", "generation_config"))
		set.outputModalities(featureAlias(cfg, "responseModalities", "response_modalities"))
		if featurePresent(featureAlias(cfg, "imageConfig", "image_config")) {
			set[apix.FeatureImageOutput] = true
		}
		if featurePresent(featureAlias(cfg, "speechConfig", "speech_config")) {
			set[apix.FeatureAudioOutput] = true
		}
		if featureString(featureAlias(cfg, "responseMimeType", "response_mime_type")) == "application/json" || featurePresent(featureAlias(cfg, "responseJsonSchema", "response_json_schema", "responseSchema", "response_schema")) {
			set[apix.FeatureStructuredOutput] = true
		}
	case Interactions:
		set.content(root["input"])
		set.outputModalities(root["response_modalities"])
		set.structured(root["response_format"])
	}
	var out []string
	for _, f := range []string{apix.FeatureStream, apix.FeatureTools, apix.FeatureImageInput, apix.FeatureFileInput, apix.FeatureAudioInput, apix.FeatureVideoInput, apix.FeatureImageOutput, apix.FeatureAudioOutput, apix.FeatureVideoOutput, apix.FeatureStructuredOutput, apix.FeatureWebSearch} {
		if set[f] {
			out = append(out, f)
		}
	}
	return out
}
func (s featureSet) content(raw json.RawMessage) {
	if parts := featureList(raw); parts != nil {
		for _, part := range parts {
			s.content(part)
		}
		return
	}
	part := featureMap(raw)
	if part == nil {
		return
	}
	s.carrier(part)
	typ := featureString(part["type"])
	switch typ {
	case "image", "image_url", "input_image":
		s[apix.FeatureImageInput] = true
	case "file", "input_file", "document":
		s[apix.FeatureFileInput] = true
	case "input_audio", "audio":
		s[apix.FeatureAudioInput] = true
	case "input_video", "video_url", "video":
		s[apix.FeatureVideoInput] = true
	case "message", "user_input", "model_output":
		s.content(part["content"])
	case "tool_result", "function_result":
		s.content(part["content"])
	case "function_call_output", "custom_tool_call_output":
		s.content(part["output"])
	case "thought":
		s.content(part["summary"])
	case "additional_tools":
		s.tools(part["tools"])
	case "":
		if featureString(part["role"]) != "" {
			s.content(part["content"])
		}
	}
}
func (s featureSet) carrier(o featureObject) {
	google := featureMap(featureMap(o["extra_content"])["google"])
	for _, raw := range featureList(google["parts"]) {
		s.geminiPart(raw)
	}
}
func (s featureSet) geminiContent(raw json.RawMessage) {
	for _, part := range featureList(featureMap(raw)["parts"]) {
		s.geminiPart(part)
	}
}
func (s featureSet) geminiPart(raw json.RawMessage) {
	part := featureMap(raw)
	for _, key := range []string{"inlineData", "inline_data", "fileData", "file_data"} {
		if featurePresent(part[key]) {
			media := featureMap(part[key])
			s.mime(featureString(featureAlias(media, "mimeType", "mime_type")))
		}
	}
	// Function response attachments are content; the response/args objects are
	// arbitrary tool data and must never be traversed.
	response := featureMap(featureAlias(part, "functionResponse", "function_response"))
	for _, attachment := range featureList(response["parts"]) {
		s.geminiPart(attachment)
	}
}
func (s featureSet) mime(mime string) {
	switch mime = strings.ToLower(strings.TrimSpace(mime)); {
	case strings.HasPrefix(mime, "image/"):
		s[apix.FeatureImageInput] = true
	case strings.HasPrefix(mime, "audio/"):
		s[apix.FeatureAudioInput] = true
	case strings.HasPrefix(mime, "video/"):
		s[apix.FeatureVideoInput] = true
	default:
		s[apix.FeatureFileInput] = true
	}
}
func (s featureSet) tools(raw json.RawMessage) {
	tools := featureList(raw)
	if len(tools) > 0 {
		s[apix.FeatureTools] = true
	}
	for _, raw := range tools {
		tool := featureMap(raw)
		typ := featureString(tool["type"])
		if typ == "namespace" {
			s.tools(featureAlias(tool, "tools", "children"))
		}
		if typ == "web_search" || typ == "web_search_preview" || strings.HasPrefix(typ, "web_search_") || typ == "google_search" {
			s[apix.FeatureWebSearch] = true
		}
		if typ == "image_generation" {
			s[apix.FeatureImageOutput] = true
		}
		for _, key := range []string{"googleSearch", "google_search", "googleSearchRetrieval", "google_search_retrieval"} {
			if featurePresent(tool[key]) {
				s[apix.FeatureWebSearch] = true
			}
		}
	}
}
func (s featureSet) structured(raw json.RawMessage) {
	format := featureMap(raw)
	typ := featureString(format["type"])
	if typ == "json_schema" || typ == "json_object" {
		s[apix.FeatureStructuredOutput] = true
	}
}
func (s featureSet) outputModalities(raw json.RawMessage) {
	for _, v := range featureList(raw) {
		switch strings.ToLower(featureString(v)) {
		case "image":
			s[apix.FeatureImageOutput] = true
		case "audio":
			s[apix.FeatureAudioOutput] = true
		case "video":
			s[apix.FeatureVideoOutput] = true
		}
	}
}
