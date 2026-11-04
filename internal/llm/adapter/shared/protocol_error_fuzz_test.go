package shared

import (
	"encoding/json"
	"reflect"
	"testing"

	"github.com/dillonzq/apix/internal/errclass"
)

// checkProtocolFailureDecoded is CheckProtocolFailure as it read the body
// before the ojson fast path.
func checkProtocolFailureDecoded(body []byte, format string) *errclass.Error {
	var doc map[string]json.RawMessage
	if json.Unmarshal(body, &doc) != nil {
		return nil //nolint:nilerr // an undecodable body carries no protocol error envelope
	}
	if raw := doc["error"]; len(raw) > 0 && string(raw) != "null" && string(raw) != `""` {
		var e ProtocolError
		if json.Unmarshal(raw, &e) != nil {
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

func FuzzCheckProtocolFailure(f *testing.F) {
	for _, s := range []string{`{"status":"failed"}`, `{"error":{"code":"rate_limit_exceeded","message":"slow"}}`, `{"error":""}`, `{"error":"x"}`, `{"error":null,"status":"cancelled"}`, `{"Error":{}}`, `{"status":"failed","status":"ok"}`, `[]`, `null`, `{`} {
		f.Add([]byte(s), true)
	}
	f.Fuzz(func(t *testing.T, body []byte, responses bool) {
		format := FormatChatCompletions
		if responses {
			format = FormatResponses
		}
		if got, want := CheckProtocolFailure(body, format), checkProtocolFailureDecoded(body, format); !reflect.DeepEqual(got, want) {
			t.Fatalf("%q: got %v, want %v", body, got, want)
		}
	})
}
