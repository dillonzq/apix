package reasoning

import (
	"bytes"
	"encoding/json"
	"reflect"
	"testing"

	"github.com/dillonzq/apix"
	"github.com/dillonzq/apix/internal/llm/ids"
)

var (
	fuzzTargets = []string{ids.FormatChatCompletions, ids.FormatMessages, ids.FormatResponses, ids.FormatGemini, "other"}
	fuzzEfforts = []string{"auto", "none", "minimal", "low", "medium", "high", "xhigh", "max"}
	fuzzModes   = []string{"", "budget", "level"}
)

// FuzzEditsMatchDecoded checks that the order-preserving Apply and Strip
// produce the same request as the encoding/json paths they replace, decoded.
func FuzzEditsMatchDecoded(f *testing.F) {
	for _, s := range []string{
		`{}`, `{"model":"m","reasoning_effort":"low"}`,
		`{"thinking":{"type":"enabled","budget_tokens":1,"display":"omitted"},"output_config":{"effort":"low","other":1}}`,
		`{"reasoning":{"effort":"low","summary":"auto"}}`, `{"reasoning":{"effort":"low"}}`,
		`{"generationConfig":{"temperature":1,"thinkingConfig":{"thinkingBudget":5,"includeThoughts":true}}}`,
		`{"generationConfig":{"thinkingConfig":{"thinking_level":"low"}}}`, `{"thinking":"x"}`, `{"reasoning":null}`,
	} {
		f.Add([]byte(s), uint8(0), uint8(0), int64(-2))
		f.Add([]byte(s), uint8(1), uint8(3), int64(3000))
	}
	f.Fuzz(func(t *testing.T, body []byte, pick, effort uint8, budget int64) {
		target := fuzzTargets[int(pick)%len(fuzzTargets)]
		r := apix.Reasoning{Effort: fuzzEfforts[int(effort)%len(fuzzEfforts)]}
		if budget >= -1 {
			r.Budget = &budget
			if effort%2 == 0 {
				r.Effort = ""
			}
		}
		opts := Options{MessagesManual: pick&8 != 0, GeminiMode: fuzzModes[int(pick>>4)%len(fuzzModes)]}
		r, err := Validate(r)
		if err != nil {
			return
		}
		if out, approx, ok := applyEdits(target, body, r, opts); ok {
			want, wantApprox, wErr := applyDecoded(target, body, r, opts)
			if wErr != nil {
				t.Fatalf("edits accepted %q, decoding fails: %v", body, wErr)
			}
			sameJSON(t, "Apply "+target, body, out, want)
			if !reflect.DeepEqual(approx, wantApprox) {
				t.Fatalf("approximated %v, want %v", approx, wantApprox)
			}
		}
		for _, source := range fuzzTargets[:4] {
			out := Strip(target, source, body)
			dropThinking := source == ids.FormatMessages && (target == ids.FormatChatCompletions || target == ids.FormatResponses || target == ids.FormatGemini)
			dropEffort := (target == ids.FormatMessages || target == ids.FormatGemini) && source == ids.FormatChatCompletions
			dropNested := (target == ids.FormatMessages || target == ids.FormatGemini) && source == ids.FormatResponses
			dropGemini := source == ids.FormatGemini && (target == ids.FormatChatCompletions || target == ids.FormatResponses || target == ids.FormatMessages)
			if !dropThinking && !dropEffort && !dropNested && !dropGemini {
				continue
			}
			want := stripDecoded(body, dropThinking, dropEffort, dropGemini)
			if bytes.Equal(want, body) {
				// The decoded path leaves malformed bodies alone.
				if !json.Valid(body) {
					if !bytes.Equal(out, body) {
						t.Fatalf("Strip changed malformed %q to %q", body, out)
					}
					continue
				}
			}
			sameJSON(t, "Strip "+source+"->"+target, body, out, want)
		}
	})
}

func sameJSON(t *testing.T, what string, body, got, want []byte) {
	t.Helper()
	var g, w any
	if err := decode(got, &g); err != nil {
		t.Fatalf("%s of %q: output %q: %v", what, body, got, err)
	}
	if err := decode(want, &w); err != nil {
		t.Fatalf("%s of %q: reference %q: %v", what, body, want, err)
	}
	if !reflect.DeepEqual(g, w) {
		t.Fatalf("%s of %q:\n got %s\nwant %s", what, body, got, want)
	}
}

func decode(b []byte, v any) error {
	d := json.NewDecoder(bytes.NewReader(b))
	d.UseNumber()
	return d.Decode(v)
}
