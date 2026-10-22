package shared

import (
	"encoding/json"
	"fmt"
	"strings"

	"github.com/dillonzq/apix/internal/errclass"
)

type HostedSearch struct {
	ID      string
	Query   string
	Status  string
	Sources []map[string]string
}

func SearchFromResponse(item map[string]json.RawMessage) (HostedSearch, bool, *errclass.Error) {
	var typ string
	DecodeProbe(item["type"], &typ)
	if typ != "web_search_call" {
		return HostedSearch{}, false, nil
	}
	var s HostedSearch
	var ok bool
	if s.ID, ok = StringField(item["id"]); !ok {
		return HostedSearch{}, false, errclass.Translation("invalid web search identity")
	}
	if s.Status, ok = StringField(item["status"]); !ok {
		return HostedSearch{}, false, errclass.Translation("invalid web search status")
	}
	var action struct {
		Type    string              `json:"type"`
		Query   string              `json:"query"`
		Sources []map[string]string `json:"sources"`
	}
	if HasContent(item["action"]) && json.Unmarshal(item["action"], &action) != nil {
		return HostedSearch{}, false, errclass.Translation("invalid web search action")
	}
	if action.Type != "" && action.Type != "search" {
		return HostedSearch{}, false, nil
	}
	s.Query = action.Query
	s.Sources = action.Sources
	return s, true, nil
}
func (s HostedSearch) ResponseItem() map[string]any {
	status := s.Status
	if status == "" {
		status = "completed"
	}
	action := map[string]any{"type": "search", "query": s.Query}
	if len(s.Sources) > 0 {
		action["sources"] = s.Sources
	}
	return map[string]any{"id": s.ID, "type": "web_search_call", "status": status, "action": action}
}
func (s HostedSearch) MessageBlocks() []map[string]any {
	results := make([]map[string]any, 0, len(s.Sources))
	for _, source := range s.Sources {
		result := map[string]any{"type": "web_search_result", "url": source["url"]}
		if source["title"] != "" {
			result["title"] = source["title"]
		}
		results = append(results, result)
	}
	var content any = results
	if s.Status == "failed" {
		content = map[string]any{"type": "web_search_tool_result_error", "error_code": "unavailable"}
	}
	return []map[string]any{{"type": "server_tool_use", "id": s.ID, "name": "web_search", "input": map[string]string{"query": s.Query}}, {"type": "web_search_tool_result", "tool_use_id": s.ID, "content": content}}
}
func ReadSearchResults(s *HostedSearch, raw json.RawMessage, from, to, path string, state *ConversionContext) {
	var list []map[string]json.RawMessage
	if json.Unmarshal(raw, &list) != nil {
		var errorResult map[string]json.RawMessage
		if json.Unmarshal(raw, &errorResult) == nil && len(errorResult["error_code"]) > 0 {
			s.Status = "failed"
			state.Record(DiagnosticFor(from, to, path+".error_code", true))
		}
		return
	}
	s.Status = "completed"
	for i, result := range list {
		var url, title string
		DecodeProbe(result["url"], &url)
		DecodeProbe(result["title"], &title)
		if url != "" {
			s.Sources = append(s.Sources, map[string]string{"type": "url", "url": url, "title": title})
		}
		for _, key := range []string{"encrypted_content", "page_age"} {
			if len(result[key]) > 0 {
				state.Record(DiagnosticFor(from, to, fmt.Sprintf("%s[%d].%s", path, i, key), key == "encrypted_content"))
			}
		}
	}
}
func IsSearchTool(typ string) bool {
	return typ == "web_search" || typ == "web_search_preview" || strings.HasPrefix(typ, "web_search_")
}
func UnsupportedHostedChoice() *errclass.Error {
	return &errclass.Error{Class: errclass.ClassUnsupported, Message: "Chat Completions cannot force a hosted web search tool"}
}
func HostedChoice(raw json.RawMessage, to string) (json.RawMessage, *errclass.Error) {
	var choice struct {
		Type string `json:"type"`
	}
	DecodeProbe(raw, &choice)
	if choice.Type == "web_search" || choice.Type == "web_search_preview" {
		if to == FormatChatCompletions {
			return nil, UnsupportedHostedChoice()
		}
		return json.RawMessage(`{"type":"function","name":"web_search"}`), nil
	}
	return raw, nil
}
func MessageSearchTool(tool ClaudeTool) RespTool {
	mapped := RespTool{Type: "web_search"}
	if len(tool.UserLocation) > 0 {
		var location map[string]json.RawMessage
		if json.Unmarshal(tool.UserLocation, &location) == nil {
			location["type"] = json.RawMessage(`"approximate"`)
			mapped.UserLocation, _ = json.Marshal(location)
		}
	}
	if len(tool.AllowedDomains) > 0 {
		mapped.Filters, _ = json.Marshal(map[string]json.RawMessage{"allowed_domains": tool.AllowedDomains})
	}
	return mapped
}
func ResponseSearchTool(tool RespTool) ClaudeTool {
	mapped := ClaudeTool{Type: "web_search_20250305", Name: "web_search"}
	if len(tool.UserLocation) > 0 {
		var location map[string]json.RawMessage
		if json.Unmarshal(tool.UserLocation, &location) == nil {
			delete(location, "type")
			mapped.UserLocation, _ = json.Marshal(location)
		}
	}
	var filters map[string]json.RawMessage
	DecodeProbe(tool.Filters, &filters)
	mapped.AllowedDomains = filters["allowed_domains"]
	return mapped
}

func (e ResponsesEventEmitter) SearchDone(index int, search HostedSearch) [][]byte {
	var events [][]byte
	if search.Status == "completed" {
		events = append(events, SSEEvent("response.web_search_call.completed", map[string]any{"type": "response.web_search_call.completed", "output_index": index, "item_id": search.ID}))
	}
	return append(events, e.ItemDone(index, search.ResponseItem()))
}

// FinalizeSearch applies the same missing-result rule to ordinary and streamed
// Messages responses. A query alone is never evidence of search success.
func FinalizeSearch(search *HostedSearch, ctx *ConversionContext) {
	if search.Status == "completed" || search.Status == "failed" {
		return
	}
	search.Status = "failed"
	ctx.Report("messages", "responses", "content.web_search_tool_result", true)
}
