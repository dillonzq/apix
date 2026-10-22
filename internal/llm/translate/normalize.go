package translate

import "github.com/dillonzq/apix/internal/llm/adapter/shared"

// NormalizeResponsesRequest explicitly adapts a native Responses request for
// an upstream that requires function tools. It flattens namespaces, wraps
// custom inputs, adapts local shell calls, merges additional_tools, and records
// identities for response restoration. Hosted tools and unrelated fields are preserved; callers own
// upstream capability checks and model rewriting. State is required and must
// also be passed to ConvertResponse or NewStreamConverter.
// Repeated declarations of the same identity keep the first declaration.
// References must use original names and namespaces, not flattened wire names.
// ConvertRequest's same-protocol passthrough behavior remains unchanged.
func NormalizeResponsesRequest(payload []byte, state *RequestState) ([]byte, *Error) {
	if state == nil {
		return nil, Translation("Responses tool normalization requires request state")
	}
	return shared.NormalizeResponsesRequest(payload, conversionContext(state))
}
