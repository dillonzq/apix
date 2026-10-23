package codec

import (
	"encoding/json"
	"strings"

	"github.com/dillonzq/apix"
	"github.com/dillonzq/apix/internal/llm/ids"
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

// scan collects what Inspect reports from known protocol paths: required
// features, affinity keys of upstream-owned state, and whether the request
// continues upstream-owned context.
type scan struct {
	set      featureSet
	affinity []string
	stateful bool
}

func (s *scan) key(kind, id string) {
	if id == "" {
		return
	}
	k := kind + ":" + id
	for _, have := range s.affinity {
		if have == k {
			return
		}
	}
	s.affinity = append(s.affinity, k)
}

// state records a reference to upstream-owned context that conversion
// cannot carry. A string or an object with an id names the state.
func (s *scan) state(kind string, raw json.RawMessage) {
	if !featurePresent(raw) {
		return
	}
	s.stateful = true
	id := featureString(raw)
	if id == "" {
		id = featureString(featureMap(raw)["id"])
	}
	s.key(kind, id)
}

// scanRequest follows protocol content paths, not arbitrary JSON keys. A
// tool's schema or result JSON can contain identical words without requiring
// media support. The fixed ordering makes Inspect output reproducible.
func scanRequest(id apix.Protocol, body []byte, stream bool) scan {
	root := featureMap(body)
	s := scan{set: featureSet{}}
	if stream {
		s.set[apix.FeatureStream] = true
	}
	s.tools(root["tools"])
	switch id {
	case ids.ChatCompletions, ids.Messages:
		if id == ids.Messages {
			s.content(root["system"])
			s.state("container", root["container"])
		}
		for _, raw := range featureList(root["messages"]) {
			message := featureMap(raw)
			s.content(message["content"])
			if len(featureList(message["images"])) > 0 {
				s.set[apix.FeatureImageInput] = true
			}
			if featurePresent(message["audio"]) {
				s.set[apix.FeatureAudioInput] = true
			}
			s.carrier(message)
		}
		if id == ids.ChatCompletions {
			s.outputModalities(root["modalities"])
			if featurePresent(root["audio"]) {
				s.set[apix.FeatureAudioOutput] = true
			}
			s.structured(root["response_format"])
		} else {
			s.structured(featureMap(root["output_config"])["format"])
		}
	case ids.Responses:
		s.state("response", root["previous_response_id"])
		s.state("conversation", root["conversation"])
		s.state("prompt", root["prompt"])
		s.content(root["input"])
		s.structured(featureMap(root["text"])["format"])
	case ids.GenerateContent:
		s.state("cached-content", featureAlias(root, "cachedContent", "cached_content"))
		s.geminiContent(featureAlias(root, "systemInstruction", "system_instruction"))
		for _, v := range featureList(root["contents"]) {
			s.geminiContent(v)
		}
		cfg := featureMap(featureAlias(root, "generationConfig", "generation_config"))
		s.outputModalities(featureAlias(cfg, "responseModalities", "response_modalities"))
		if featurePresent(featureAlias(cfg, "imageConfig", "image_config")) {
			s.set[apix.FeatureImageOutput] = true
		}
		if featurePresent(featureAlias(cfg, "speechConfig", "speech_config")) {
			s.set[apix.FeatureAudioOutput] = true
		}
		if featureString(featureAlias(cfg, "responseMimeType", "response_mime_type")) == "application/json" || featurePresent(featureAlias(cfg, "responseJsonSchema", "response_json_schema", "responseSchema", "response_schema")) {
			s.set[apix.FeatureStructuredOutput] = true
		}
	case ids.Interactions:
		s.state("interaction", root["previous_interaction_id"])
		s.content(root["input"])
		s.outputModalities(root["response_modalities"])
		s.structured(root["response_format"])
	}
	return s
}

// features returns the collected features in a fixed order.
func (s *scan) features() []string {
	var out []string
	for _, f := range []string{apix.FeatureStream, apix.FeatureTools, apix.FeatureImageInput, apix.FeatureFileInput, apix.FeatureAudioInput, apix.FeatureVideoInput, apix.FeatureImageOutput, apix.FeatureAudioOutput, apix.FeatureVideoOutput, apix.FeatureStructuredOutput, apix.FeatureWebSearch} {
		if s.set[f] {
			out = append(out, f)
		}
	}
	return out
}

func (s *scan) content(raw json.RawMessage) {
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
	s.files(part)
	typ := featureString(part["type"])
	switch typ {
	case "image", "image_url", "input_image":
		s.set[apix.FeatureImageInput] = true
	case "file", "input_file", "document":
		s.set[apix.FeatureFileInput] = true
	case "input_audio", "audio":
		s.set[apix.FeatureAudioInput] = true
	case "input_video", "video_url", "video":
		s.set[apix.FeatureVideoInput] = true
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
	case "item_reference":
		s.state("item", part["id"])
	case "":
		if featureString(part["role"]) != "" {
			s.content(part["content"])
		}
	}
}

// files records uploaded file IDs: Chat file parts, Responses input_file and
// input_image parts, and Messages file sources.
func (s *scan) files(part featureObject) {
	s.key("file", featureString(part["file_id"]))
	s.key("file", featureString(featureMap(part["file"])["file_id"]))
	if source := featureMap(part["source"]); featureString(source["type"]) == "file" {
		s.key("file", featureString(source["file_id"]))
	}
}

func (s *scan) carrier(o featureObject) {
	google := featureMap(featureMap(o["extra_content"])["google"])
	for _, raw := range featureList(google["parts"]) {
		s.geminiPart(raw)
	}
}

func (s *scan) geminiContent(raw json.RawMessage) {
	for _, part := range featureList(featureMap(raw)["parts"]) {
		s.geminiPart(part)
	}
}

func (s *scan) geminiPart(raw json.RawMessage) {
	part := featureMap(raw)
	for _, key := range []string{"inlineData", "inline_data", "fileData", "file_data"} {
		if featurePresent(part[key]) {
			media := featureMap(part[key])
			s.mime(featureString(featureAlias(media, "mimeType", "mime_type")))
			// Files API URIs belong to the project that uploaded them.
			if uri := featureString(featureAlias(media, "fileUri", "file_uri")); strings.Contains(uri, "/files/") {
				s.key("file", uri)
			}
		}
	}
	// Function response attachments are content; the response/args objects are
	// arbitrary tool data and must never be traversed.
	response := featureMap(featureAlias(part, "functionResponse", "function_response"))
	for _, attachment := range featureList(response["parts"]) {
		s.geminiPart(attachment)
	}
}

func (s *scan) mime(mime string) {
	switch mime = strings.ToLower(strings.TrimSpace(mime)); {
	case strings.HasPrefix(mime, "image/"):
		s.set[apix.FeatureImageInput] = true
	case strings.HasPrefix(mime, "audio/"):
		s.set[apix.FeatureAudioInput] = true
	case strings.HasPrefix(mime, "video/"):
		s.set[apix.FeatureVideoInput] = true
	default:
		s.set[apix.FeatureFileInput] = true
	}
}

func (s *scan) tools(raw json.RawMessage) {
	tools := featureList(raw)
	if len(tools) > 0 {
		s.set[apix.FeatureTools] = true
	}
	for _, raw := range tools {
		tool := featureMap(raw)
		typ := featureString(tool["type"])
		if typ == "namespace" {
			s.tools(featureAlias(tool, "tools", "children"))
		}
		if typ == "web_search" || typ == "web_search_preview" || strings.HasPrefix(typ, "web_search_") || typ == "google_search" {
			s.set[apix.FeatureWebSearch] = true
		}
		if typ == "image_generation" {
			s.set[apix.FeatureImageOutput] = true
		}
		for _, key := range []string{"googleSearch", "google_search", "googleSearchRetrieval", "google_search_retrieval"} {
			if featurePresent(tool[key]) {
				s.set[apix.FeatureWebSearch] = true
			}
		}
	}
}

func (s *scan) structured(raw json.RawMessage) {
	format := featureMap(raw)
	typ := featureString(format["type"])
	if typ == "json_schema" || typ == "json_object" {
		s.set[apix.FeatureStructuredOutput] = true
	}
}

func (s *scan) outputModalities(raw json.RawMessage) {
	for _, v := range featureList(raw) {
		switch strings.ToLower(featureString(v)) {
		case "image":
			s.set[apix.FeatureImageOutput] = true
		case "audio":
			s.set[apix.FeatureAudioOutput] = true
		case "video":
			s.set[apix.FeatureVideoOutput] = true
		}
	}
}
