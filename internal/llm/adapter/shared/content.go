package shared

import (
	"encoding/json"
	"fmt"
	"strings"

	"github.com/dillonzq/apix/internal/errclass"
)

// ImageSource converts an image URL into the Anthropic image block
// source. Only data URLs whose media suffix ends in ";base64" decode to
// base64 sources; every other URL — including percent-encoded or plain
// data URLs — passes through verbatim as a url source so the encoding
// stays faithful and an upstream that cannot render it errors descriptively.
func ImageSource(url string) (map[string]any, *errclass.Error) {
	if rest, ok := strings.CutPrefix(url, "data:"); ok {
		comma := strings.Index(rest, ",")
		if comma < 0 {
			return nil, errclass.Translation("malformed data URL: missing ',' separator")
		}
		if strings.HasSuffix(rest[:comma], ";base64") {
			media := strings.TrimSuffix(rest[:comma], ";base64")
			if media == "" {
				media = "application/octet-stream"
			}
			return map[string]any{
				"type": "image",
				"source": map[string]any{
					"type": "base64", "media_type": media, "data": rest[comma+1:],
				},
			}, nil
		}
	}
	return map[string]any{
		"type":   "image",
		"source": map[string]any{"type": "url", "url": url},
	}, nil
}

// ClaudeSystemText renders the Anthropic Messages top-level system field
// (JSON string or text-block array) as instruction text. Text
// blocks join with blank lines; non-text blocks are rejected descriptively
// rather than dropped. One kernel serves every adapter translating a
// Claude system field so separator and rejection policy cannot diverge.
func ClaudeSystemText(raw json.RawMessage) (string, *errclass.Error) {
	if !HasContent(raw) {
		return "", nil
	}
	var s string
	if err := json.Unmarshal(raw, &s); err == nil {
		return s, nil
	}
	var blocks []struct {
		Type string `json:"type"`
		Text string `json:"text"`
	}
	if err := json.Unmarshal(raw, &blocks); err != nil {
		return "", errclass.Translation("malformed system field")
	}
	var b strings.Builder
	for _, blk := range blocks {
		if blk.Type != "text" {
			return "", errclass.Translation(fmt.Sprintf("unsupported system block type %q", blk.Type))
		}
		if b.Len() > 0 {
			b.WriteString("\n\n")
		}
		b.WriteString(blk.Text)
	}
	return b.String(), nil
}

// ToolResultText flattens Claude tool_result content (JSON string or text
// block array) into one text string for the target protocol's tool-result
// field. Non-text blocks have no textual representation and are rejected
// descriptively rather than dropped; targetNoun names the target
// wire vocabulary so each serving route keeps its own wording ("tool
// messages carry text only", "function_call_output carries text only").
// One kernel serves both adapters so the flatten and rejection policy
// cannot diverge again.
func ToolResultText(raw json.RawMessage, targetNoun string) (string, *errclass.Error) {
	if !HasContent(raw) {
		return "", nil
	}
	var s string
	if err := json.Unmarshal(raw, &s); err == nil {
		return s, nil
	}
	var blocks []struct {
		Type string `json:"type"`
		Text string `json:"text"`
	}
	if err := json.Unmarshal(raw, &blocks); err != nil {
		return "", errclass.Translation("tool_result content must be a string or an array of blocks")
	}
	var b strings.Builder
	for _, blk := range blocks {
		if blk.Type != "text" {
			return "", errclass.Translation(fmt.Sprintf(
				"unsupported tool_result block type %q; %s", blk.Type, targetNoun))
		}
		b.WriteString(blk.Text)
	}
	return b.String(), nil
}

// RespOutputText flattens Responses function_call_output output content (JSON
// string or content part array) into one string for target protocol tool results.
// It accepts part types "input_text" and "text", and returns a translation error
// naming targetNoun on unsupported block types.
func RespOutputText(raw json.RawMessage, targetNoun string) (string, *errclass.Error) {
	if !HasContent(raw) {
		return "", nil
	}
	var s string
	if err := json.Unmarshal(raw, &s); err == nil {
		return s, nil
	}
	var parts []struct {
		Type string `json:"type"`
		Text string `json:"text"`
	}
	if err := json.Unmarshal(raw, &parts); err != nil {
		return "", errclass.Translation("tool output must be a string or an array of content parts")
	}
	var b strings.Builder
	for _, p := range parts {
		if p.Type != "input_text" && p.Type != "output_text" && p.Type != "text" {
			return "", errclass.Translation(fmt.Sprintf(
				"unsupported tool output part type %q; %s", p.Type, targetNoun))
		}
		b.WriteString(p.Text)
	}
	return b.String(), nil
}

// ClaudeImageURL converts an Anthropic image block source into an image
// URL for OpenAI-style targets, encoding base64 sources as data URLs
// (multimodal preservation). Strict validation shared by every
// call site: url sources need a url, base64 sources need data, and
// unknown source types are named descriptively — never a silently
// corrupt data: URL.
func ClaudeImageURL(srcType, url, mediaType, data string) (string, *errclass.Error) {
	switch srcType {
	case "url":
		if url == "" {
			return "", errclass.Translation("image source missing url")
		}
		return url, nil
	case "base64":
		if data == "" {
			return "", errclass.Translation("base64 image source missing data")
		}
		if mediaType == "" {
			mediaType = "application/octet-stream"
		}
		return "data:" + mediaType + ";base64," + data, nil
	default:
		return "", errclass.Translation(fmt.Sprintf("unsupported image source type %q", srcType))
	}
}

// JoinTexts concatenates the string values stored under "text" across
// content parts/blocks in order, skipping non-string members — one kernel
// for every adapter that flattens typed parts into plain text.
func JoinTexts(parts []map[string]any) string {
	var b strings.Builder
	for _, p := range parts {
		if t, ok := p["text"].(string); ok {
			b.WriteString(t)
		}
	}
	return b.String()
}

// ContentPart is one OpenAI-style content part decoded by
// DecodeStringOrParts: text, an image URL, or file/audio/video media.
type ContentPart struct {
	CacheControl string
	Text         string
	ImageURL     string     // resolved image URL; empty for text parts
	Media        *MediaPart // file, audio, or video input
}

// DecodeStringOrParts is THE shared string-or-parts kernel owning the
// empty-input policy for every target route (route parity): an absent
// or null field, an empty JSON string, and empty-text parts all decode to
// zero parts, so identical inbound bodies drop identically instead of one
// route shipping dead parts upstream while siblings drop the message.
// Unknown part types are ClassUnsupported naming targetLabel; malformed
// shapes are ClassTranslation. One kernel serves responses/contentParts,
// messages/contentParts, and chatcompletions/respContent so the policy
// cannot diverge again.
func DecodeStringOrParts(raw json.RawMessage, targetLabel string) ([]ContentPart, *errclass.Error) {
	if !HasContent(raw) {
		return nil, nil
	}
	var s string
	if err := json.Unmarshal(raw, &s); err == nil {
		if s == "" {
			return nil, nil
		}
		return []ContentPart{{Text: s}}, nil
	}
	var parts []struct {
		CacheControl json.RawMessage `json:"cache_control"`
		Type         string          `json:"type"`
		Text         string          `json:"text"`
		ImageURL     json.RawMessage `json:"image_url"`
	}
	if err := json.Unmarshal(raw, &parts); err != nil {
		return nil, errclass.Translation("malformed message content: " + err.Error())
	}
	var elems []json.RawMessage
	DecodeProbe(raw, &elems)
	var out []ContentPart
	for i, p := range parts {
		if !IsContentPartType(p.Type) {
			return nil, ValidateContentPart(p.Type, targetLabel)
		}
		if p.Type == "file" || p.Type == "input_file" || p.Type == "input_audio" || p.Type == "video_url" || p.Type == "input_video" {
			m, err := decodeMediaPart(elems[i], p.Type)
			if err != nil {
				return nil, err
			}
			out = append(out, ContentPart{Media: m, CacheControl: string(p.CacheControl)})
			continue
		}
		if p.Type == "image_url" || p.Type == "input_image" {
			url, eErr := OpenAIImageURL(p.ImageURL)
			if eErr != nil {
				return nil, eErr
			}
			out = append(out, ContentPart{ImageURL: url, CacheControl: string(p.CacheControl)})
			continue
		}
		if p.Text != "" { // uniform DROP policy: no dead text parts upstream
			out = append(out, ContentPart{Text: p.Text, CacheControl: string(p.CacheControl)})
		}
	}
	return out, nil
}

// ClaudeBlocksFromOpenAI normalizes an OpenAI-style content field (JSON
// string or typed parts) into Anthropic content blocks preserving
// per-part order (multimodal preservation): text parts become
// text blocks, image parts image sources, and files document blocks.
// THE Claude-blocks kernel for
// every route emitting Anthropic blocks from openai-style content: it
// owns validation and the empty-input policy — empties dropped uniformly
// on every route, unknown types rejected descriptively
// naming target. One kernel serves messages/contentParts and
// chatcompletions/ccContentToBlocks so the policy cannot diverge again.
func ClaudeBlocksFromOpenAI(raw json.RawMessage, target string) ([]map[string]any, *errclass.Error) {
	parts, eErr := DecodeStringOrParts(raw, target)
	if eErr != nil {
		return nil, eErr
	}
	var blocks []map[string]any
	for _, p := range parts {
		var block map[string]any
		if p.Media != nil {
			var err *errclass.Error
			block, err = p.Media.Claude()
			if err != nil {
				return nil, err
			}
		} else if p.ImageURL != "" {
			var err *errclass.Error
			block, err = ImageSource(p.ImageURL)
			if err != nil {
				return nil, err
			}
		} else {
			block = map[string]any{"type": "text", "text": p.Text}
		}
		if HasContent(json.RawMessage(p.CacheControl)) {
			block["cache_control"] = json.RawMessage(p.CacheControl)
		}
		blocks = append(blocks, block)
	}
	return blocks, nil
}

// OpenAIImageURL extracts the image URL from an OpenAI-style content part:
// {"url": ...} object form or plain string form. An empty or absent URL is
// a translation failure.
func OpenAIImageURL(raw json.RawMessage) (string, *errclass.Error) {
	var url string
	if len(raw) > 0 {
		var obj struct {
			URL string `json:"url"`
		}
		if json.Unmarshal(raw, &obj) == nil && obj.URL != "" {
			return obj.URL, nil
		}
		DecodeProbe(raw, &url)
	}
	if url == "" {
		return "", errclass.Translation("image content part missing url")
	}
	return url, nil
}
