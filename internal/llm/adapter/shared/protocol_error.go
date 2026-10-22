package shared

import (
	"encoding/json"
	"github.com/dillonzq/apix/internal/errclass"
	"strconv"
	"strings"
)

// ProtocolError is an upstream error carried inside a successful HTTP body.
type ProtocolError struct {
	Message string          `json:"message"`
	Type    string          `json:"type"`
	Code    json.RawMessage `json:"code"`
}

func ProtocolFailure(e *ProtocolError, fallback string) *errclass.Error {
	if e == nil {
		return errclass.UpstreamFallback(fallback)
	}
	var code string
	if json.Unmarshal(e.Code, &code) != nil {
		code = string(e.Code)
	}
	if status, err := strconv.Atoi(code); err == nil && status >= 400 && status <= 599 {
		return errclass.FromStatus(status, e.Message)
	}
	if strings.Contains(code+" "+e.Type, "rate_limit") {
		return errclass.FromStatus(429, e.Message)
	}
	if strings.Contains(code+" "+e.Type, "authentication") {
		return errclass.FromStatus(401, e.Message)
	}
	message := e.Message
	if message == "" {
		message = fallback
	}
	return errclass.UpstreamFallback(message)
}

func CheckProtocolFailure(body []byte, format string) *errclass.Error {
	var doc map[string]json.RawMessage
	if json.Unmarshal(body, &doc) != nil {
		return nil //nolint:nilerr // an undecodable body carries no protocol error envelope
	}
	if raw := doc["error"]; len(raw) > 0 && string(raw) != "null" && string(raw) != `""` {
		var e ProtocolError
		if json.Unmarshal(raw, &e) != nil {
			// The envelope does not match the known shape. It is still a
			// failure: an unreadable envelope must never read as success.
			// An empty error string is ignored because it carries no detail.
			return errclass.UpstreamFallback("upstream response failed")
		}
		return ProtocolFailure(&e, "upstream response failed")
	}
	if format == FormatResponses {
		var status string
		DecodeProbe(doc["status"], &status)
		if status == "failed" || status == "cancelled" {
			return ProtocolFailure(nil, "Responses response "+status)
		}
	}
	return nil
}
