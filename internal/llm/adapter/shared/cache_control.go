package shared

import "encoding/json"

// ToolResultCacheControl returns the only child hint that can migrate to the
// parent tool_result without changing its source boundary. Empty text parts
// are dropped by content decoding, so their hints cannot migrate either.
func ToolResultCacheControl(raw json.RawMessage) json.RawMessage {
	var parts []struct {
		Type         string          `json:"type"`
		Text         string          `json:"text"`
		CacheControl json.RawMessage `json:"cache_control"`
	}
	if json.Unmarshal(raw, &parts) != nil || len(parts) == 0 {
		return nil
	}
	last := parts[len(parts)-1]
	if (last.Type == "text" || last.Type == "input_text" || last.Type == "output_text") && last.Text == "" {
		return nil
	}
	return last.CacheControl
}
