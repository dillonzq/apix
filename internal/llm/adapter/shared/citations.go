package shared

import (
	"encoding/json"
	"fmt"
	"strings"
	"unicode/utf8"
)

type Citation struct {
	Origin string
	URL    string
	Title  string
	Text   string
	Start  *int
	End    *int
}

func intField(raw json.RawMessage) *int {
	var n int
	if json.Unmarshal(raw, &n) == nil && n >= 0 {
		return &n
	}
	return nil
}
func ReadCitation(raw json.RawMessage, from, to, path, text string, offset int, state *ConversionContext) (Citation, bool) {
	var doc map[string]json.RawMessage
	if json.Unmarshal(raw, &doc) != nil || doc == nil {
		state.Record(DiagnosticFor(from, to, path, false))
		return Citation{}, false
	}
	for _, key := range []string{"encrypted_index", "document_index", "page_number"} {
		if len(doc[key]) > 0 {
			state.Record(DiagnosticFor(from, to, path+"."+key, false))
		}
	}
	var typ string
	DecodeProbe(doc["type"], &typ)
	c := Citation{}
	if typ == "url_citation" {
		if nested := doc["url_citation"]; len(nested) > 0 {
			DecodeProbe(nested, &doc)
		}
		c.Start = intField(doc["start_index"])
		c.End = intField(doc["end_index"])
	} else if typ != "web_search_result_location" {
		state.Record(DiagnosticFor(from, to, path, false))
		return c, false
	}
	DecodeProbe(doc["url"], &c.URL)
	DecodeProbe(doc["title"], &c.Title)
	DecodeProbe(doc["cited_text"], &c.Text)
	if c.URL == "" {
		state.Record(DiagnosticFor(from, to, path, false))
		return c, false
	}
	if c.Start != nil && c.End != nil && *c.Start <= *c.End && *c.End <= utf8.RuneCountInString(text) {
		c.Text = string([]rune(text)[*c.Start:*c.End])
	}
	if c.Start == nil && c.Text != "" {
		if i := strings.Index(text, c.Text); i >= 0 {
			start := utf8.RuneCountInString(text[:i])
			end := start + utf8.RuneCountInString(c.Text)
			c.Start = &start
			c.End = &end
		}
	}
	if c.Start != nil {
		v := *c.Start + offset
		c.Start = &v
	}
	if c.End != nil {
		v := *c.End + offset
		c.End = &v
	}
	return c, true
}
func (c Citation) Wire(to string) map[string]any {
	if to == FormatMessages {
		out := map[string]any{"type": "web_search_result_location", "url": c.URL}
		if c.Title != "" {
			out["title"] = c.Title
		}
		if c.Text != "" {
			out["cited_text"] = c.Text
		}
		return out
	}
	out := map[string]any{"url": c.URL, "title": c.Title}
	if c.Start != nil {
		out["start_index"] = *c.Start
	}
	if c.End != nil {
		out["end_index"] = *c.End
	}
	if to == FormatChatCompletions {
		return map[string]any{"type": "url_citation", "url_citation": out}
	}
	out["type"] = "url_citation"
	return out
}

func DecodeCitations(raw json.RawMessage, from, to, path, text string, offset int, ctx *ConversionContext) []Citation {
	var list []json.RawMessage
	if json.Unmarshal(raw, &list) != nil {
		if HasContent(raw) {
			// A citations field that is not an array cannot be read per item;
			// record the loss instead of dropping the references silently.
			ctx.Record(DiagnosticFor(from, to, path, false))
		}
		return nil
	}
	var out []Citation
	for i, item := range list {
		if c, ok := ReadCitation(item, from, to, fmt.Sprintf("%s[%d]", path, i), text, offset, ctx); ok {
			out = append(out, c)
		}
	}
	return out
}
func CitationValues(citations []Citation, to string) []map[string]any {
	out := make([]map[string]any, 0, len(citations))
	for _, c := range citations {
		out = append(out, c.Wire(to))
	}
	return out
}
func AddBlockCitations(blocks []map[string]any, citations []Citation) {
	for _, c := range citations {
		base := 0
		chosen := -1
		for i, block := range blocks {
			if block["type"] != "text" {
				continue
			}
			text, _ := block["text"].(string)
			length := utf8.RuneCountInString(text)
			if chosen < 0 {
				chosen = i
			}
			if (c.Text != "" && strings.Contains(text, c.Text)) || (c.Start != nil && *c.Start >= base && *c.Start < base+length) {
				chosen = i
				break
			}
			base += length
		}
		if chosen >= 0 {
			previous, _ := blocks[chosen]["citations"].([]map[string]any)
			blocks[chosen]["citations"] = append(previous, c.Wire(FormatMessages))
		}
	}
}
func (e ResponsesEventEmitter) AnnotationAdded(id string, index, contentIndex, annotationIndex int, c Citation) []byte {
	return SSEEvent("response.output_text.annotation.added", map[string]any{"type": "response.output_text.annotation.added", "item_id": id, "output_index": index, "content_index": contentIndex, "annotation_index": annotationIndex, "annotation": c.Wire(FormatResponses)})
}
