package shared

import (
	"encoding/json"

	"github.com/dillonzq/apix/internal/errclass"
)

// RewriteModelID rewrites the top-level model field of a native-format
// request body to the bare upstream ID. The body decodes ONCE one level
// into json.RawMessage values, so every nested value passes through
// byte-exact — notably large integers that a float64 roundtrip would
// mangle (fidelity). When the decoded model field's raw bytes
// already equal the quoted upstream ID (the common bare-ID/prefix client
// case), the original body is returned untouched before any mutation —
// a pure no-op that skips the remarshal. The comparison target mirrors
// the rewrite quoting below, and a decoded RawMessage always holds a
// valid JSON value, so an upstream id that cannot be represented
// (quotes/backslashes/control characters) can never match here and still
// reaches the rewrite's error. Duplicate keys follow encoding/json
// semantics: the LAST occurrence wins. formatLabel names the source
// protocol in the malformed-JSON error.
func RewriteModelID(upstreamModel string, body []byte, formatLabel string) ([]byte, *errclass.Error) {
	var req map[string]json.RawMessage
	if err := json.Unmarshal(body, &req); err != nil {
		return nil, errclass.Translation("malformed " + formatLabel + " request JSON: " + err.Error())
	}
	// A literal JSON null body unmarshals without error into a NIL map;
	// assigning the model field would panic, so reject it as malformed.
	if req == nil {
		return nil, errclass.Translation("malformed request body: JSON null is not a valid request")
	}
	if raw, ok := req["model"]; ok && string(raw) == `"`+upstreamModel+`"` {
		return body, nil
	}
	req["model"] = json.RawMessage(`"` + upstreamModel + `"`)
	b, err := json.Marshal(req)
	if err != nil {
		// A catalog model id containing quotes/backslashes/control
		// characters cannot be represented as a JSON string value.
		return nil, errclass.Translation("model id cannot be represented as JSON")
	}
	return b, nil
}

// HasContent reports whether a raw content/instructions/arguments field is
// present (non-empty and not JSON null); it is the presence test used
// before string-or-parts decoding.
func HasContent(raw json.RawMessage) bool {
	return len(raw) > 0 && string(raw) != "null"
}

// DecodeProbe decodes best-effort JSON into v and discards the error. Callers
// use it only for non-authoritative probes of already parsed JSON. Unmarshal
// can partially modify v before reporting a shape mismatch; this helper offers
// no rollback or validation guarantee. Callers must not rely on partially
// decoded values to rebuild required output. Output-producing decodes must
// call json.Unmarshal and handle the error; errcheck still reports those call
// sites, because this helper is the only sanctioned
// unchecked decode in the package.
func DecodeProbe(data []byte, v any) {
	_ = json.Unmarshal(data, v)
}

// StringField reads a JSON string field: an absent or null value reads as the
// empty string, and any other JSON type reports ok=false. Callers that rewrite
// the field must stop on ok=false, because substituting the empty string would
// drop the upstream bytes.
func StringField(raw json.RawMessage) (value string, ok bool) {
	if !HasContent(raw) {
		return "", true
	}
	if json.Unmarshal(raw, &value) != nil {
		return "", false
	}
	return value, true
}
