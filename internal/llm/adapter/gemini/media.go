package gemini

import (
	"encoding/base64"
	"strings"
	"unicode/utf8"
)

func (c *conversion) toParts(value any, role string) []any {
	if value == nil {
		return nil
	}
	if text, ok := value.(string); ok {
		return []any{object{"text": text}}
	}
	var parts []any
	for _, v := range c.array(value, "content") {
		p := c.object(v, "content[]")
		if p == nil {
			c.fail("content parts must be objects")
			continue
		}
		if signed := carrierParts(p); signed != nil {
			c.validateParts(signed)
			parts = append(parts, signed...)
			continue
		}
		typ := str(p["type"])
		var part object
		switch typ {
		case "text", "input_text", "output_text":
			part = object{"text": c.text(p["text"], "content.text")}
		case "refusal":
			part = object{"text": c.text(p["refusal"], "refusal")}
		case "thinking":
			part = object{"text": c.text(p["thinking"], "thinking"), "thought": true}
			if p["signature"] != nil {
				c.loss("content.thinking.signature", true)
			}
		case "redacted_thinking":
			c.loss("content.redacted_thinking", true)
			continue
		case "tool_use":
			part = object{"functionCall": object{"id": c.text(p["id"], "tool_use.id"), "name": c.text(p["name"], "tool_use.name"), "args": c.args(p["input"])}}
		case "tool_result":
			result := c.toolResult(p["content"])
			if boolean(p["is_error"]) {
				result = object{"error": result}
			}
			part = object{"functionResponse": object{"id": c.text(p["tool_use_id"], "tool_use_id"), "name": str(p["name"]), "response": result}}
		case "image", "document":
			source := c.object(p["source"], "source")
			kind := str(source["type"])
			mime := str(source["media_type"])
			switch kind {
			case "base64":
				part = c.inline(mime, c.text(source["data"], "source.data"))
			case "url":
				part = c.file(mime, c.text(source["url"], "source.url"))
			case "text":
				part = c.inline("text/plain", base64.StdEncoding.EncodeToString([]byte(c.text(source["data"], "source.data"))))
			default:
				c.reject("unsupported Gemini media source")
			}
			for _, k := range []string{"context", "citations"} {
				if p[k] != nil {
					c.loss("content.document."+k, true)
				}
			}
		case "image_url", "input_image":
			u := p["image_url"]
			if o := obj(u); o != nil {
				u = o["url"]
				if o["detail"] != nil {
					c.loss("content.image_url.detail", false)
				}
			}
			part = c.mediaURL("", c.text(u, "image_url"))
		case "file", "input_file":
			f := p
			if typ == "file" {
				f = c.object(p["file"], "file")
			}
			if f["file_id"] != nil {
				c.reject("provider-scoped file_id cannot be sent to Gemini")
				continue
			}
			if data := str(f["file_data"]); data != "" {
				part = c.mediaURL("", data)
				if !strings.HasPrefix(data, "data:") {
					c.reject("Gemini inline files require a data URI with a MIME type")
				}
			} else {
				part = c.file("", c.text(f["file_url"], "file_url"))
			}
		case "input_audio":
			audio := p
			if p["input_audio"] != nil {
				audio = c.object(p["input_audio"], "input_audio")
			}
			mime := audioMIME(str(audio["format"]))
			if mime == "" {
				c.reject("unsupported audio input format")
			}
			part = c.inline(mime, c.text(audio["data"], "audio.data"))
		case "video_url", "input_video":
			u := p["video_url"]
			if o := obj(u); o != nil {
				u = o["url"]
			}
			part = c.mediaURL("video/mp4", c.text(u, "video_url"))
			if p["processing"] != nil {
				c.loss("content.video.processing", true)
			}
		default:
			c.reject("unsupported content part for Gemini")
		}
		if part != nil {
			if sig := signatureFrom(p); sig != "" {
				part["thoughtSignature"] = sig
			}
			if (role == "system" || role == "developer") && part["text"] == nil {
				c.reject("Gemini system instructions require text")
			}
			if p["cache_control"] != nil {
				c.loss("content.cache_control", false)
			}
			parts = append(parts, part)
		}
	}
	return parts
}
func audioMIME(format string) string {
	switch format {
	case "wav":
		return "audio/wav"
	case "mp3":
		return "audio/mpeg"
	case "flac":
		return "audio/flac"
	case "ogg":
		return "audio/ogg"
	case "aac":
		return "audio/aac"
	case "pcm16":
		return "audio/pcm;rate=24000"
	}
	return ""
}
func (c *conversion) inline(mime, data string) object {
	if mime == "" || data == "" {
		c.fail("inline media requires MIME type and data")
	}
	if _, err := base64.StdEncoding.DecodeString(data); err != nil {
		c.fail("invalid inline media base64")
	}
	return object{"inlineData": object{"mimeType": mime, "data": data}}
}
func (c *conversion) file(mime, uri string) object {
	if uri == "" {
		c.fail("file media requires a URI")
	}
	f := object{"fileUri": uri}
	if mime != "" {
		f["mimeType"] = mime
	}
	return object{"fileData": f}
}
func (c *conversion) mediaURL(mime, uri string) object {
	if strings.HasPrefix(uri, "data:") {
		head, data, ok := strings.Cut(strings.TrimPrefix(uri, "data:"), ",")
		if !ok || !strings.HasSuffix(head, ";base64") {
			c.fail("media data URI must use base64")
			return nil
		}
		return c.inline(strings.TrimSuffix(head, ";base64"), data)
	}
	return c.file(mime, uri)
}
func (c *conversion) fromMedia(part object) object {
	inline := c.object(first(part, "inlineData", "inline_data"), "inlineData")
	file := c.object(first(part, "fileData", "file_data"), "fileData")
	if inline == nil && file == nil {
		c.reject("unsupported Gemini part")
		return nil
	}
	source := inline
	if source == nil {
		source = file
	}
	mime := c.text(first(source, "mimeType", "mime_type"), "mimeType")
	uri := c.text(first(source, "fileUri", "file_uri"), "fileUri")
	data := str(source["data"])
	if inline != nil {
		if mime == "" || data == "" {
			c.fail("inlineData requires MIME type and data")
		}
		if _, err := base64.StdEncoding.DecodeString(data); err != nil {
			c.fail("invalid inlineData base64")
		}
		uri = "data:" + mime + ";base64," + data
	} else if uri == "" {
		c.fail("fileData requires fileUri")
	}
	if part["videoMetadata"] != nil {
		c.loss("contents.parts.videoMetadata", true)
	}
	if c.to == messages {
		if strings.HasPrefix(mime, "image/") {
			s := object{"type": "url", "url": uri}
			if inline != nil {
				s = object{"type": "base64", "media_type": mime, "data": data}
			}
			return object{"type": "image", "source": s}
		}
		if mime == "application/pdf" || mime == "text/plain" {
			s := object{"type": "url", "url": uri}
			if inline != nil {
				if mime == "application/pdf" {
					s = object{"type": "base64", "media_type": mime, "data": data}
				} else {
					b, _ := base64.StdEncoding.DecodeString(data)
					if !utf8.Valid(b) {
						c.fail("text document is not UTF-8")
					}
					s = object{"type": "text", "media_type": "text/plain", "data": string(b)}
				}
			}
			return object{"type": "document", "source": s}
		}
		c.reject("Messages cannot represent this Gemini media type")
		return nil
	}
	if strings.HasPrefix(mime, "image/") {
		if c.to == responses {
			return object{"type": "input_image", "image_url": uri}
		}
		return object{"type": "image_url", "image_url": object{"url": uri}}
	}
	if strings.HasPrefix(mime, "audio/") {
		if inline == nil {
			c.reject("OpenAI audio input requires inline data")
			return nil
		}
		format := strings.TrimPrefix(mime, "audio/")
		if format == "mpeg" {
			format = "mp3"
		}
		return object{"type": "input_audio", "input_audio": object{"data": data, "format": format}}
	}
	if strings.HasPrefix(mime, "video/") {
		typ := "video_url"
		if c.to == responses {
			typ = "input_video"
		}
		return object{"type": typ, "video_url": object{"url": uri}}
	}
	if c.to == responses {
		if inline != nil {
			return object{"type": "input_file", "file_data": uri}
		}
		return object{"type": "input_file", "file_url": uri}
	}
	if inline == nil {
		c.reject("Chat Completions cannot represent a Gemini file URI")
		return nil
	}
	return object{"type": "file", "file": object{"file_data": uri}}
}
