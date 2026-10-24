// Package vendorerr renders errors in each vendor's error envelope and
// classifies upstream error bodies. Codecs of every Kind share it, because a
// vendor uses one error format across its APIs.
package vendorerr

import (
	"encoding/json"
	"strings"

	"github.com/dillonzq/apix"
)

// Envelope renders errors in one vendor's error response format.
type Envelope func(err *apix.Error) any

// Encode renders err as a JSON error body; a nil error renders as an unknown
// upstream failure.
func (env Envelope) Encode(err error) apix.Payload {
	e := apix.AsError(err)
	if e == nil {
		e = apix.UpstreamFallback("unknown error")
	}
	body, _ := json.Marshal(env(e))
	return apix.Payload{Body: body, ContentType: "application/json"}
}

// OpenAI is the {"error": {"message", "type", "code"}} envelope.
func OpenAI(err *apix.Error) any {
	typ := "invalid_request_error"
	switch err.Class {
	case apix.ClassAuth:
		typ = "authentication_error"
	case apix.ClassRateLimit:
		typ = "rate_limit_error"
	case apix.ClassQuota, apix.ClassBilling:
		typ = "insufficient_quota"
	case apix.ClassUpstream, apix.ClassNetwork:
		typ = "server_error"
	}
	return map[string]any{"error": map[string]any{"message": err.Message, "type": typ, "code": string(err.Class)}}
}

// Anthropic is the {"type": "error", "error": {"type", "message"}} envelope.
func Anthropic(err *apix.Error) any {
	typ := "invalid_request_error"
	switch err.Class {
	case apix.ClassAuth:
		typ = "authentication_error"
	case apix.ClassInvalidModel:
		typ = "not_found_error"
	case apix.ClassRateLimit, apix.ClassQuota:
		typ = "rate_limit_error"
	case apix.ClassBilling:
		typ = "billing_error"
	case apix.ClassUpstream, apix.ClassNetwork:
		typ = "api_error"
	}
	return map[string]any{"type": "error", "error": map[string]string{"type": typ, "message": err.Message}}
}

// Google is the {"error": {"code", "message", "status"}} envelope.
func Google(err *apix.Error) any {
	status := "INVALID_ARGUMENT"
	switch err.Class {
	case apix.ClassAuth:
		status = "UNAUTHENTICATED"
	case apix.ClassInvalidModel:
		status = "NOT_FOUND"
	case apix.ClassRateLimit, apix.ClassQuota:
		status = "RESOURCE_EXHAUSTED"
	case apix.ClassBilling:
		status = "PERMISSION_DENIED"
	case apix.ClassNetwork:
		status = "DEADLINE_EXCEEDED"
	case apix.ClassUpstream:
		status = "UNAVAILABLE"
	}
	return map[string]any{"error": map[string]any{"code": apix.HTTPStatus(err), "message": err.Message, "status": status}}
}

// snippetBound bounds redaction work: only the head of an oversized error
// body can reach the 256-character snippet.
const snippetBound = 4096

// Snippet redacts and truncates a message for error reporting.
func Snippet(s string) string {
	s = apix.Redact(s)
	if len(s) > 256 {
		return s[:256] + "..."
	}
	return s
}

// UpstreamStatus classifies an upstream response of status 400 or above. It
// reads the human-readable message from common JSON error bodies, falls back
// to a redacted snippet of the body, and classifies with apix.FromStatus.
func UpstreamStatus(status int, body []byte) *apix.Error {
	if len(body) > snippetBound {
		body = body[:snippetBound]
	}
	msg := message(body)
	if msg == "" {
		msg = string(body)
	}
	return apix.FromStatus(status, Snippet(msg))
}

// message extracts the message of an OpenAI, Anthropic or Google error body.
func message(body []byte) string {
	if len(body) == 0 {
		return ""
	}
	var probe struct {
		Error   any    `json:"error"`
		Message string `json:"message"`
		Detail  string `json:"detail"`
	}
	if json.Unmarshal(body, &probe) != nil {
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
	return strings.TrimSpace(probe.Detail)
}
