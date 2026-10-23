package gemini

import (
	"fmt"
	"strings"
	"unicode/utf8"

	"github.com/dillonzq/apix/internal/llm/adapter/shared"
)

// Gemini segment offsets count UTF-8 bytes within one part. The other
// protocols' annotation offsets count runes in their visible output text.
func (c *conversion) groundingCitations(candidate object) []shared.Citation {
	grounding := obj(candidate["groundingMetadata"])
	chunks := list(grounding["groundingChunks"])
	parts := list(obj(candidate["content"])["parts"])
	var out []shared.Citation
	for si, sv := range list(grounding["groundingSupports"]) {
		support := obj(sv)
		segment := obj(support["segment"])
		pi := int(number(segment["partIndex"]))
		var start, end *int
		text := str(segment["text"])
		if pi >= 0 && pi < len(parts) {
			part := obj(parts[pi])
			raw := str(part["text"])
			a, b := int(number(segment["startIndex"])), int(number(segment["endIndex"]))
			if !boolean(part["thought"]) && a >= 0 && b >= a && b <= len(raw) && utf8.ValidString(raw[:a]) && utf8.ValidString(raw[:b]) {
				offset := 0
				for _, pv := range parts[:pi] {
					p := obj(pv)
					if !boolean(p["thought"]) {
						offset += utf8.RuneCountInString(str(p["text"]))
					}
				}
				sa, sb := offset+utf8.RuneCountInString(raw[:a]), offset+utf8.RuneCountInString(raw[:b])
				start, end = &sa, &sb
				text = raw[a:b]
			}
		}
		if start == nil {
			c.ctx.Report(Format, c.to, fmt.Sprintf("groundingMetadata.groundingSupports[%d].segment", si), false)
		}
		for _, iv := range list(support["groundingChunkIndices"]) {
			i := int(number(iv))
			if i < 0 || i >= len(chunks) {
				c.ctx.Report(Format, c.to, "groundingMetadata.groundingChunkIndices", false)
				continue
			}
			web := obj(obj(chunks[i])["web"])
			if str(web["uri"]) == "" {
				c.ctx.Report(Format, c.to, "groundingMetadata.groundingChunks", false)
				continue
			}
			out = append(out, shared.Citation{URL: str(web["uri"]), Title: str(web["title"]), Text: text, Start: start, End: end})
		}
	}
	return out
}
func (c *conversion) applyCitations(candidate object, content, items []any) []any {
	citations := c.groundingCitations(candidate)
	chatAnnotations := make([]any, 0, len(citations))
	for _, citation := range citations {
		chatAnnotations = append(chatAnnotations, citation.Wire(chat))
	}
	if c.to == messages {
		for _, citation := range citations {
			placed := false
			for _, pv := range content {
				p := obj(pv)
				if str(p["type"]) == "text" && citation.Text != "" && strings.Contains(str(p["text"]), citation.Text) {
					p["citations"] = append(list(p["citations"]), citation.Wire(messages))
					placed = true
					break
				}
			}
			if !placed {
				c.ctx.Report(Format, messages, "groundingMetadata.citation_position", false)
			}
		}
	}
	if c.to == responses {
		for _, citation := range citations {
			offset := 0
			placed := false
			for _, iv := range items {
				item := obj(iv)
				if str(item["type"]) != "message" {
					continue
				}
				for _, pv := range list(item["content"]) {
					p := obj(pv)
					length := utf8.RuneCountInString(str(p["text"]))
					if citation.Start != nil && citation.End != nil && *citation.Start >= offset && *citation.End <= offset+length {
						adjusted := citation
						a, b := *citation.Start-offset, *citation.End-offset
						adjusted.Start, adjusted.End = &a, &b
						p["annotations"] = append(list(p["annotations"]), adjusted.Wire(responses))
						placed = true
					}
					offset += length
				}
			}
			if !placed {
				c.ctx.Report(Format, responses, "groundingMetadata.citation_position", false)
			}
		}
	}
	return nonNil(chatAnnotations)
}
func (c *conversion) responseGrounding(root object, parts []any) object {
	var citations []shared.Citation
	read := func(v any, text string, offset int) {
		if v != nil {
			citations = append(citations, shared.DecodeCitations(encode(v), c.from, Format, "annotations", text, offset, c.ctx)...)
		}
	}
	switch c.from {
	case chat:
		choices := list(root["choices"])
		if len(choices) > 0 {
			m := obj(obj(choices[0])["message"])
			read(m["annotations"], str(m["content"]), 0)
		}
	case messages:
		offset := 0
		for _, pv := range list(root["content"]) {
			p := obj(pv)
			if str(p["type"]) == "text" {
				text := str(p["text"])
				read(p["citations"], text, offset)
				offset += utf8.RuneCountInString(text)
			}
		}
	case responses:
		offset := 0
		for _, iv := range list(root["output"]) {
			item := obj(iv)
			if str(item["type"]) != "message" {
				continue
			}
			for _, pv := range list(item["content"]) {
				p := obj(pv)
				text := str(p["text"])
				read(p["annotations"], text, offset)
				offset += utf8.RuneCountInString(text)
			}
		}
	}
	return c.citationsToGrounding(citations, parts)
}
func (c *conversion) citationsToGrounding(citations []shared.Citation, parts []any) object {
	var chunks, supports []any
	for _, citation := range citations {
		offset := 0
		placed := false
		for pi, pv := range parts {
			p := obj(pv)
			if boolean(p["thought"]) {
				continue
			}
			text := str(p["text"])
			runes := []rune(text)
			length := len(runes)
			if citation.Start != nil && citation.End != nil && *citation.Start >= offset && *citation.End <= offset+length {
				a, b := *citation.Start-offset, *citation.End-offset
				index := len(chunks)
				chunks = append(chunks, object{"web": object{"uri": citation.URL, "title": citation.Title}})
				supports = append(supports, object{"segment": object{"partIndex": pi, "startIndex": len(string(runes[:a])), "endIndex": len(string(runes[:b])), "text": string(runes[a:b])}, "groundingChunkIndices": []any{index}})
				placed = true
				break
			}
			offset += length
		}
		if !placed {
			c.ctx.Report(c.from, Format, "annotations.position", false)
		}
	}
	if len(chunks) == 0 {
		return nil
	}
	return object{"groundingChunks": chunks, "groundingSupports": supports}
}
