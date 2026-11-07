package counttokens_test

import (
	"encoding/json"
	"testing"

	"github.com/dillonzq/apix"
)

// FuzzCountTokensRequest checks that hostile client bodies never panic, that
// every failure is classified, and that converted bodies are JSON objects.
func FuzzCountTokensRequest(f *testing.F) {
	for i, p := range all {
		for j := range all {
			f.Add(requests[p].Body, uint8(i), uint8(j))
		}
	}
	f.Add([]byte(`{"generateContentRequest":{"model":"models/x","contents":[{"parts":[{"text":"a"}]}]},"contents":null}`), uint8(2), uint8(0))
	f.Add([]byte(`{"model":"m","input":[{"type":"function_call_output","call_id":"c","output":"o"}]}`), uint8(1), uint8(2))
	reg := registry(f)
	f.Fuzz(func(t *testing.T, body []byte, ci, ui uint8) {
		if len(body) > 1<<16 {
			t.Skip()
		}
		client, up := all[int(ci)%len(all)], all[int(ui)%len(all)]
		req := apix.Payload{Body: body, Metadata: apix.RequestMetadata{Model: "m"}}
		if _, err := reg.Inspect(client, req); err != nil && apix.AsError(err).Class == "" {
			t.Fatalf("unclassified Inspect error %v", err)
		}
		ex, err := reg.NewExchange(client, up, apix.ExchangeOptions{})
		if err != nil {
			t.Fatal(err)
		}
		out, err := ex.ConvertRequest(req, "up")
		if err != nil {
			if apix.AsError(err).Class == "" {
				t.Fatalf("unclassified error %v", err)
			}
			return
		}
		if client != up {
			var m map[string]json.RawMessage
			if json.Unmarshal(out.Body, &m) != nil || m == nil {
				t.Fatalf("converted body is not a JSON object: %s", out.Body)
			}
		}
	})
}

// FuzzCountTokensResponse checks that hostile upstream replies never panic,
// fail as upstream errors, and convert to JSON.
func FuzzCountTokensResponse(f *testing.F) {
	for i := range all {
		f.Add([]byte(`{"input_tokens":3}`), uint8(i), uint8(0))
		f.Add([]byte(`{"totalTokens":"9"}`), uint8(i), uint8(2))
	}
	reg := registry(f)
	f.Fuzz(func(t *testing.T, body []byte, ci, ui uint8) {
		if len(body) > 1<<16 {
			t.Skip()
		}
		client, up := all[int(ci)%len(all)], all[int(ui)%len(all)]
		ex, err := reg.NewExchange(client, up, apix.ExchangeOptions{})
		if err != nil {
			t.Fatal(err)
		}
		out, err := ex.ConvertResponse(200, apix.Payload{Body: body})
		if err != nil {
			if apix.AsError(err).Class != apix.ClassUpstream {
				t.Fatalf("reply error is not an upstream fault: %v", err)
			}
			return
		}
		if !json.Valid(out.Body) {
			t.Fatalf("converted reply is not JSON: %s", out.Body)
		}
	})
}
