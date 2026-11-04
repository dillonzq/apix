package jsonedit

import (
	"bytes"
	"encoding/json"
	"reflect"
	"testing"
)

func TestApplyKeepsOrderAndBytes(t *testing.T) {
	body := []byte(`{"stream":true, "model":"a","n":1.50,"html":"<b>","nested":{"z":1,"a":2}}`)
	out, ok := Apply(body, SetString(`"b"`, "model"), SetString(`"high"`, "reasoning", "effort"), Delete("n"), SetString(`3`, "nested", "m"))
	if !ok {
		t.Fatal("fast path refused a plain object")
	}
	want := `{"stream":true, "model":"b","html":"<b>","nested":{"z":1,"a":2,"m":3},"reasoning":{"effort":"high"}}`
	if string(out) != want {
		t.Fatalf("got  %s\nwant %s", out, want)
	}
	if !bytes.Equal(body, []byte(`{"stream":true, "model":"a","n":1.50,"html":"<b>","nested":{"z":1,"a":2}}`)) {
		t.Fatal("input modified")
	}
}

func TestApplyPrune(t *testing.T) {
	for _, c := range []struct{ in, want string }{
		{`{"a":1,"r":{"effort":"x"}}`, `{"a":1}`},
		{`{"a":1,"r":{"effort":"x","summary":"auto"}}`, `{"a":1,"r":{"summary":"auto"}}`},
		{`{"a":1,"r":{}}`, `{"a":1,"r":{}}`}, // nothing removed, so nothing pruned
		{`{"a":1}`, `{"a":1}`},
	} {
		out, ok := Apply([]byte(c.in), DeletePrune("r", "effort"))
		if !ok || string(out) != c.want {
			t.Fatalf("%s: got %s, %v; want %s", c.in, out, ok, c.want)
		}
	}
}

func TestApplyRefusesAmbiguousDocuments(t *testing.T) {
	for _, body := range []string{
		`{"model":"a","model":"b"}`,     // duplicate: encoding/json keeps the last
		`{"Model":"a","model":"b"}`,     // case-insensitive match
		`{"MODEL":"a"}`,                 // a struct decode would read it as model
		`{"r":"auto"}`,                  // parent is not an object
		`{"r":null}`,                    // parent is null
		`{"r":{"effort":1,"effort":2}}`, // nested duplicate
		`[1]`, `null`, `"s"`, `{bad`, `{"a":1} x`, `{"a":"\x"}`,
	} {
		if out, ok := Apply([]byte(body), SetString(`"x"`, "model"), SetString(`"y"`, "r", "effort")); ok {
			t.Fatalf("%s: fast path accepted, got %s", body, out)
		}
	}
	if _, ok := Apply([]byte(`{"Other":1,"other":2,"model":"a"}`), SetString(`"x"`, "model")); !ok {
		t.Fatal("duplicates off the path must not block the edit")
	}
}

func TestApplyEscapedKeyAndSpecialCharacters(t *testing.T) {
	out, ok := Apply([]byte(`{"mod\u0065l":"a"}`), SetString(`"b"`, "model"))
	if !ok {
		t.Fatal("refused")
	}
	var got map[string]any
	if json.Unmarshal(out, &got) != nil || got["model"] != "b" || len(got) != 1 {
		t.Fatalf("escaped key: %s", out)
	}
	out, ok = Apply([]byte(`{"a.b":{"c*":1}}`), SetString(`2`, "a.b", "c*"))
	if !ok || string(out) != `{"a.b":{"c*":2}}` {
		t.Fatalf("path characters: %s %v", out, ok)
	}
}

func TestFields(t *testing.T) {
	got, ok := Fields([]byte(`{"stream":false,"model":"m","x":[1]}`), "model", "stream", "missing")
	if !ok || got[0].String() != "m" || got[1].Raw != "false" || got[2].Exists() {
		t.Fatalf("fields = %v %v", got, ok)
	}
	for _, body := range []string{`{"model":"a","model":"b"}`, `{"Model":"a"}`, `[]`, `{`} {
		if _, ok := Fields([]byte(body), "model"); ok {
			t.Fatalf("%s: fast path accepted", body)
		}
	}
}

// FuzzApply checks the fast path against the encoding/json reading: whenever
// Apply accepts a document, its result must decode to what decoding the input,
// editing the maps and re-encoding would give.
func FuzzApply(f *testing.F) {
	for _, s := range []string{
		`{"model":"a","stream":true}`,
		`{"model":"a","r":{"effort":"low","summary":"auto"},"x":[1,{"model":2}]}`,
		`{"r":{"effort":"low"}}`, `{}`, `{"r":{}}`, `{"model":1e400}`, `{"a":"\u00e9","b":12345678901234567890}`,
	} {
		f.Add([]byte(s), uint8(0))
		f.Add([]byte(s), uint8(7))
	}
	f.Fuzz(func(t *testing.T, body []byte, which uint8) {
		ops := fuzzOps[int(which)%len(fuzzOps)]
		out, ok := Apply(body, ops...)
		if !ok {
			return
		}
		want, wantOK := reference(body, ops)
		if !wantOK {
			t.Fatalf("fast path accepted %q, encoding/json rejects it", body)
		}
		var got any
		dec := json.NewDecoder(bytes.NewReader(out))
		dec.UseNumber()
		if err := dec.Decode(&got); err != nil {
			t.Fatalf("output %q of %q is not JSON: %v", out, body, err)
		}
		if !reflect.DeepEqual(got, want) {
			t.Fatalf("body %q ops %d:\n got %#v\nwant %#v", body, which, got, want)
		}
	})
}

var fuzzOps = [][]Op{
	{SetString(`"up"`, "model")},
	{SetString(`true`, "stream")},
	{SetString(`"high"`, "reasoning", "effort")},
	{DeletePrune("reasoning", "effort")},
	{Delete("thinking"), SetString(`"adaptive"`, "thinking", "type")},
	{DeletePrune("r", "effort"), SetString(`"x"`, "r", "effort")},
	{Delete("model"), Delete("stream")},
	{SetString(`{"a":1}`, "g", "t"), Delete("g", "t", "a"), SetString(`2`, "g", "t", "b")},
}

// reference applies ops the way the encoding/json code paths do.
func reference(body []byte, ops []Op) (any, bool) {
	var root map[string]any
	dec := json.NewDecoder(bytes.NewReader(body))
	dec.UseNumber()
	if dec.Decode(&root) != nil || root == nil || dec.More() {
		return nil, false
	}
	for _, op := range ops {
		parents := []map[string]any{root}
		obj := root
		for _, k := range op.path[:len(op.path)-1] {
			next, ok := obj[k].(map[string]any)
			if !ok {
				if op.del {
					obj = nil
					break
				}
				next = map[string]any{}
				obj[k] = next
			}
			obj = next
			parents = append(parents, obj)
		}
		last := op.path[len(op.path)-1]
		switch {
		case !op.del:
			var v any
			d := json.NewDecoder(bytes.NewReader([]byte(op.value)))
			d.UseNumber()
			_ = d.Decode(&v)
			obj[last] = v
		case obj != nil:
			if _, ok := obj[last]; !ok {
				continue
			}
			delete(obj, last)
			if op.prune && len(op.path) > 1 && len(obj) == 0 {
				delete(parents[len(parents)-2], op.path[len(op.path)-2])
			}
		}
	}
	return any(root), true
}
