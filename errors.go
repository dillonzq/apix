package apix

import (
	"errors"
	"net/http"
	"regexp"
	"strings"
	"time"
)

// Class is a stable error-category identifier.
type Class string

const (
	ClassAuth         Class = "auth_failure"
	ClassInvalidModel Class = "invalid_model"
	ClassUnsupported  Class = "unsupported_protocol_or_parameter"
	ClassRateLimit    Class = "rate_limit"
	ClassQuota        Class = "quota_exhaustion"
	ClassBilling      Class = "billing"
	ClassUpstream     Class = "upstream_server_failure"
	ClassNetwork      Class = "timeout_or_network_failure"
	ClassTranslation  Class = "translation_failure"
)

// Error carries the error class, HTTP status, and retryability of a failure.
// Functions in this module return it as an error; use AsError or errors.As to
// read it. Messages are redacted when constructed by this module.
type Error struct {
	Class      Class
	Message    string
	StatusCode int
	// Retryable reports that another upstream candidate may succeed.
	Retryable bool
	// RetryAfter is the delay an upstream asked for, from its Retry-After
	// header; zero when it gave none.
	RetryAfter time.Duration
	// Err is the underlying cause, when known, for errors.Is checks such as
	// context.Canceled. Unlike Message, its text is not redacted.
	Err error
}

func (e *Error) Error() string {
	if e == nil {
		return "<nil>"
	}
	return string(e.Class) + ": " + e.Message
}

// Unwrap returns the underlying cause.
func (e *Error) Unwrap() error {
	if e == nil {
		return nil
	}
	return e.Err
}

// AsError returns the *Error in err's chain. Other non-nil errors are
// reported as ClassUpstream with a redacted message. A nil error, or a nil
// *Error stored in an error, returns nil.
func AsError(err error) *Error {
	if err == nil {
		return nil
	}
	var e *Error
	if errors.As(err, &e) {
		return e
	}
	return &Error{Class: ClassUpstream, Message: Redact(err.Error()), Err: err}
}

// Translation reports a conversion failure; it is never retryable.
func Translation(message string) *Error {
	return &Error{Class: ClassTranslation, Message: Redact(message)}
}

// Unsupported reports a request or configuration this library cannot serve.
func Unsupported(message string) *Error {
	return &Error{Class: ClassUnsupported, Message: Redact(message)}
}

// FromStatus classifies an upstream HTTP status. A redacted message
// containing "quota" (case-insensitive) upgrades 429, 402 and 403 to
// ClassQuota, a bounded heuristic given no documented upstream quota
// taxonomy. Statuses another upstream may not share (401, 402, 403, 404, 429
// and 5xx) are retryable; other client errors describe the request and are
// not.
func FromStatus(status int, message string) *Error {
	msg := Redact(message)
	quota := strings.Contains(strings.ToLower(msg), "quota")
	quotaClass := func(base Class) Class {
		if quota {
			return ClassQuota
		}
		return base
	}
	switch status {
	case 401:
		return &Error{Class: ClassAuth, Message: msg, StatusCode: status, Retryable: true}
	case 403:
		return &Error{Class: quotaClass(ClassAuth), Message: msg, StatusCode: status, Retryable: true}
	case 402:
		return &Error{Class: quotaClass(ClassBilling), Message: msg, StatusCode: status, Retryable: true}
	case 404:
		// Model catalogs and stored state differ between upstreams.
		return &Error{Class: ClassInvalidModel, Message: msg, StatusCode: status, Retryable: true}
	case 429:
		return &Error{Class: quotaClass(ClassRateLimit), Message: msg, StatusCode: status, Retryable: true}
	}
	if status >= 500 {
		return &Error{Class: ClassUpstream, Message: msg, StatusCode: status, Retryable: true}
	}
	return &Error{Class: ClassUnsupported, Message: msg, StatusCode: status}
}

// FromNetwork classifies a network failure before the first upstream byte;
// it is retryable. Failures after the first byte use PostFirstByteNetwork.
func FromNetwork(err error) *Error {
	msg := ""
	if err != nil {
		msg = Redact(err.Error())
	}
	return &Error{Class: ClassNetwork, Message: msg, Retryable: true, Err: err}
}

// PostFirstByteNetwork classifies a failure after the first upstream byte;
// it is not retryable because output may already have been delivered.
func PostFirstByteNetwork(message string) *Error {
	return &Error{Class: ClassNetwork, Message: Redact(message)}
}

// UpstreamFallback reports an upstream failure without an HTTP status. It is
// retryable.
func UpstreamFallback(message string) *Error {
	return &Error{Class: ClassUpstream, Message: Redact(message), Retryable: true}
}

// HTTPStatus returns the status to report err with: its StatusCode when set,
// otherwise a status derived from its class. A nil error is 200.
func HTTPStatus(err error) int {
	e := AsError(err)
	if e == nil {
		return http.StatusOK
	}
	if e.StatusCode >= 400 {
		return e.StatusCode
	}
	switch e.Class {
	case ClassAuth:
		return http.StatusUnauthorized
	case ClassInvalidModel:
		return http.StatusNotFound
	case ClassRateLimit, ClassQuota:
		return http.StatusTooManyRequests
	case ClassBilling:
		return http.StatusPaymentRequired
	case ClassUpstream:
		return http.StatusBadGateway
	case ClassNetwork:
		return http.StatusGatewayTimeout
	default:
		return http.StatusBadRequest
	}
}

// bearerRe matches a bearer token (case-insensitive scheme, optional
// space/tab separator, token terminated by whitespace, comma, or quote).
// The separator is optional so a bare "bearer" word is redacted too. Redact
// is idempotent, which makes re-redaction at the edges safe.
var bearerRe = regexp.MustCompile(`(?i)bearer[ \t]*[^\s,"]*`)

// apiKeyRe finds x-api-key header echoes: case-insensitive name, a run of
// quote/colon/equals/space separators, then the value. The value class spans
// hex, base62 and the base64 alphabet. A candidate is only replaced when it
// looks like a key (contains a digit), so prose such as "x-api-key
// authentication failed" passes untouched. "[redacted]" starts with '[',
// outside the value class, so it never re-matches.
var apiKeyRe = regexp.MustCompile(`(?i)x-api-key["':= ]*[A-Za-z0-9_\-.+/=]+`)

func redactAPIKey(match string) string {
	value := strings.TrimLeft(match[len("x-api-key"):], "\"':= ")
	hasDigit := false
	for _, r := range value {
		if r >= '0' && r <= '9' {
			hasDigit = true
			break
		}
	}
	if !hasDigit || len(value) < 8 {
		return match
	}
	return match[:len(match)-len(value)] + "[redacted]"
}

// queryKeyRe redacts secrets in URL query strings: transport failures echo
// the full URL, so ?api_key=… or ?token=… must not reach envelopes or logs.
// The value stops at whitespace, '&' and quotes so later parameters survive;
// the parameter name is kept.
var queryKeyRe = regexp.MustCompile(`(?i)([?&](?:api[_-]?key|key|access[_-]?token|token)=)[^\s&"']+`)

// Redact removes bearer tokens, x-api-key echoes, and key query parameters
// from a message so keys never reach logs or clients.
func Redact(message string) string {
	message = bearerRe.ReplaceAllString(message, "Bearer [redacted]")
	message = apiKeyRe.ReplaceAllStringFunc(message, redactAPIKey)
	return queryKeyRe.ReplaceAllString(message, "${1}[redacted]")
}
