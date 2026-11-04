package shared

import (
	"encoding/json"

	"github.com/dillonzq/apix/internal/errclass"
	"github.com/dillonzq/apix/internal/jsonedit"
)

// RewriteModelID rewrites the top-level model field of a native-format
// request body to the bare upstream ID. Only the model value changes: every
// other byte, including member order and large integers that a float64
// roundtrip would mangle, passes through as sent. When the model field's raw
// bytes already equal the quoted upstream ID (the common bare-ID/prefix client
// case), the original body is returned untouched. An upstream id that cannot
// be represented as a JSON string (quotes, control characters, a stray
// backslash) never matches and is rejected. Duplicate keys follow
// encoding/json semantics: the LAST occurrence wins, and the rewrite then
// collapses them. formatLabel names the source protocol in the malformed-JSON
// error.
func RewriteModelID(upstreamModel string, body []byte, formatLabel string) ([]byte, *errclass.Error) {
	quoted := `"` + upstreamModel + `"`
	if f, ok := jsonedit.Fields(body, "model"); ok {
		if f[0].Raw == quoted {
			return body, nil
		}
		if !json.Valid([]byte(quoted)) {
			return nil, errclass.Translation("model id cannot be represented as JSON")
		}
		if out, ok := jsonedit.Apply(body, jsonedit.SetString(quoted, "model")); ok {
			return out, nil
		}
	}
	return rewriteModelIDDecoded(quoted, body, formatLabel)
}

// rewriteModelIDDecoded is RewriteModelID through encoding/json, for bodies
// the order-preserving edit refuses: malformed JSON, a non-object root, and
// ambiguous model keys.
func rewriteModelIDDecoded(quoted string, body []byte, formatLabel string) ([]byte, *errclass.Error) {
	var req map[string]json.RawMessage
	if err := json.Unmarshal(body, &req); err != nil {
		return nil, errclass.Translation("malformed " + formatLabel + " request JSON: " + err.Error())
	}
	// A literal JSON null body unmarshals without error into a NIL map;
	// assigning the model field would panic, so reject it as malformed.
	if req == nil {
		return nil, errclass.Translation("malformed request body: JSON null is not a valid request")
	}
	if raw, ok := req["model"]; ok && string(raw) == quoted {
		return body, nil
	}
	req["model"] = json.RawMessage(quoted)
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
