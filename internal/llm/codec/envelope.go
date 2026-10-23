package codec

import (
	"encoding/json"

	"github.com/dillonzq/apix"
)

// envelope renders errors in one vendor's error response format.
type envelope func(err *apix.Error) any

func (env envelope) encode(err error) apix.Payload {
	e := apix.AsError(err)
	if e == nil {
		e = apix.UpstreamFallback("unknown error")
	}
	body, _ := json.Marshal(env(e))
	return apix.Payload{Body: body, ContentType: "application/json"}
}

func openAIEnvelope(err *apix.Error) any {
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

func anthropicEnvelope(err *apix.Error) any {
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

func googleEnvelope(err *apix.Error) any {
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
