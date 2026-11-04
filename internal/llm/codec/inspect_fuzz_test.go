package codec

import (
	"bytes"
	"encoding/json"
	"reflect"
	"testing"

	"github.com/dillonzq/apix"
	"github.com/dillonzq/apix/internal/llm/ids"
)

var inspectProtocols = []apix.Protocol{ids.ChatCompletions, ids.Responses, ids.Messages, ids.GenerateContent, ids.Interactions}

// FuzzInspectMatchesDecoded checks the ojson reading of a request against
// the encoding/json reading it replaces: same info, same error.
func FuzzInspectMatchesDecoded(f *testing.F) {
	for _, s := range []string{
		`{"model":"m","stream":true,"messages":[{"role":"user","content":[{"type":"image_url","image_url":{"url":"data:image/png;base64,AA=="}},{"type":"file","file":{"file_id":"f\u00e9"}}]}],"tools":[{"type":"function"}]}`,
		`{"model":"m","input":[{"type":"input_file","file_id":"x"},{"type":"item_reference","id":"i1"}],"previous_response_id":"r1","text":{"format":{"type":"json_schema"}}}`,
		`{"contents":[{"parts":[{"inlineData":{"mimeType":"audio/wav","data":"AA=="}},{"fileData":{"fileUri":"https://g/files/abc"}}]}],"generationConfig":{"responseModalities":["IMAGE"]},"cachedContent":"c"}`,
		`{"model":"m","system":[{"type":"text","text":"s"}],"messages":[{"role":"user","content":[{"type":"document","source":{"type":"file","file_id":"d"}}]}],"container":"k"}`,
		`{"model":1}`, `{"stream":"yes"}`, `{"model":null,"stream":null}`, `null`, `[]`, `{"model":"a","model":"b"}`, `{"Model":"x"}`, `{"contents":[]}`, `{"agent":"a"}`,
		"{\"model\":\"bad\xff\"}",
	} {
		f.Add([]byte(s), uint8(0))
		f.Add([]byte(s), uint8(2))
		f.Add([]byte(s), uint8(3))
	}
	f.Fuzz(func(t *testing.T, body []byte, pick uint8) {
		id := inspectProtocols[int(pick)%len(inspectProtocols)]
		features := pick&8 == 0
		got, gotErr := inspectRequest(id, body, features)
		want, wantErr := inspectDecoded(id, body, features)
		if !reflect.DeepEqual(gotErr, wantErr) {
			t.Fatalf("%s %q: err %v, decoded %v", id, body, gotErr, wantErr)
		}
		if !reflect.DeepEqual(got, want) {
			t.Fatalf("%s %q:\n got %+v\nwant %+v", id, body, got, want)
		}
	})
}

// FuzzFeatureHelpers checks the ojson feature helpers against decoding the
// same checked JSON with encoding/json.
func FuzzFeatureHelpers(f *testing.F) {
	for _, s := range []string{`{"a":1,"a":2,"b":"x"}`, `[1,"a",{}]`, `"café"`, `null`, `{}`, `[]`, `"\ud800"`, "\"\xff\"", `{"type":"image"}`, ` [ 1 , 2 ] `} {
		f.Add([]byte(s))
	}
	f.Fuzz(func(t *testing.T, raw []byte) {
		if !json.Valid(raw) {
			return
		}
		var m featureObject
		_ = json.Unmarshal(raw, &m)
		if got := featureMap(raw); !reflect.DeepEqual(compactMap(got), compactMap(m)) || (got == nil) != (m == nil) {
			t.Fatalf("featureMap(%q) = %q, want %q", raw, got, m)
		}
		var l []json.RawMessage
		_ = json.Unmarshal(raw, &l)
		if got := featureList(raw); !reflect.DeepEqual(compactList(got), compactList(l)) || (got == nil) != (l == nil) {
			t.Fatalf("featureList(%q) = %q, want %q", raw, got, l)
		}
		var s string
		_ = json.Unmarshal(raw, &s)
		if got := featureString(raw); got != s {
			t.Fatalf("featureString(%q) = %q, want %q", raw, got, s)
		}
	})
}

// encoding/json hands RawMessage values over as written; compare them as
// written too, without surrounding space.
func compactMap(m featureObject) map[string]string {
	out := map[string]string{}
	for k, v := range m {
		out[k] = string(bytes.TrimSpace(v))
	}
	return out
}

func compactList(l []json.RawMessage) []string {
	out := []string{}
	for _, v := range l {
		out = append(out, string(bytes.TrimSpace(v)))
	}
	return out
}
