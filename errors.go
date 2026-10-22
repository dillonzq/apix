package apix

import (
	"net/http"

	"github.com/dillonzq/apix/internal/errclass"
)

// Error carries the error class, HTTP status, and retryability. Messages are
// redacted when constructed by this package.
type Error = errclass.Error

// Class is a stable error-category identifier.
type Class = errclass.Class

const (
	ClassAuth         = errclass.ClassAuth
	ClassInvalidModel = errclass.ClassInvalidModel
	ClassUnsupported  = errclass.ClassUnsupported
	ClassRateLimit    = errclass.ClassRateLimit
	ClassQuota        = errclass.ClassQuota
	ClassBilling      = errclass.ClassBilling
	ClassUpstream     = errclass.ClassUpstream
	ClassNetwork      = errclass.ClassNetwork
	ClassTranslation  = errclass.ClassTranslation
)

// Translation reports a conversion failure; it is never retryable.
func Translation(message string) *Error { return errclass.Translation(message) }

// Unsupported reports a request or configuration this library cannot serve.
func Unsupported(message string) *Error {
	return &Error{Class: ClassUnsupported, Message: errclass.Redact(message)}
}

// FromStatus classifies an upstream HTTP status.
func FromStatus(status int, message string) *Error { return errclass.FromStatus(status, message) }

// FromNetwork classifies a network failure before the first upstream byte;
// it is retryable.
func FromNetwork(err error) *Error { return errclass.FromNetwork(err) }

// PostFirstByteNetwork classifies a failure after the first upstream byte;
// it is not retryable because output may already have been delivered.
func PostFirstByteNetwork(message string) *Error { return errclass.PostFirstByteNetwork(message) }

// UpstreamFallback reports an upstream failure without an HTTP status.
func UpstreamFallback(message string) *Error { return errclass.UpstreamFallback(message) }

// Redact removes bearer tokens, API keys, and key query parameters.
func Redact(message string) string { return errclass.Redact(message) }

// HTTPStatus returns the status to report err with: its StatusCode when set,
// otherwise a status derived from its class.
func HTTPStatus(err *Error) int {
	if err == nil {
		return http.StatusOK
	}
	if err.StatusCode >= 400 {
		return err.StatusCode
	}
	switch err.Class {
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
