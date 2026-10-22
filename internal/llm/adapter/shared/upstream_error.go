package shared

import (
	"encoding/json"
	"strings"

	"github.com/dillonzq/apix/internal/errclass"
)

// RedactedSnippet bearer-redacts and truncates a payload snippet for error
// messages — never an upstream body echo.
func RedactedSnippet(s string) string {
	s = errclass.Redact(s)
	if len(s) > 256 {
		return s[:256] + "..."
	}
	return s
}

// snippetBound bounds redaction work: only the head of an oversized error
// body can reach the 256-char snippet, so every upstream >=400 site
// truncates to this size before scanning.
const snippetBound = 4096

// extractErrorMessage attempts to extract a clean, human-readable error
// message from upstream JSON error bodies (e.g. OpenAI or Anthropic format).
func extractErrorMessage(body []byte) string {
	if len(body) == 0 {
		return ""
	}
	var probe struct {
		Error   any    `json:"error"`
		Message string `json:"message"`
		Detail  string `json:"detail"`
	}
	if err := json.Unmarshal(body, &probe); err != nil {
		return ""
	}
	switch v := probe.Error.(type) {
	case string:
		if v != "" {
			return strings.TrimSpace(v)
		}
	case map[string]any:
		if m, ok := v["message"].(string); ok && m != "" {
			return strings.TrimSpace(m)
		}
	}
	if probe.Message != "" {
		return strings.TrimSpace(probe.Message)
	}
	if probe.Detail != "" {
		return strings.TrimSpace(probe.Detail)
	}
	return ""
}

// UpstreamStatusError classifies an upstream >=400 response body into one
// kernel used by every adapter: the body is truncated to
// its head, decoded for human-readable error messages if JSON, or fallen
// back to a redacted snippet, then classified via errclass.FromStatus.
// One kernel keeps bounding and redaction from diverging across call sites.
func UpstreamStatusError(status int, body []byte) *errclass.Error {
	if len(body) > snippetBound {
		body = body[:snippetBound]
	}
	msg := extractErrorMessage(body)
	if msg == "" {
		msg = string(body)
	}
	return errclass.FromStatus(status, RedactedSnippet(msg))
}
