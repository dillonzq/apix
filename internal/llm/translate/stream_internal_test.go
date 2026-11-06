package translate

import (
	"bytes"
	"encoding/json"
	"testing"

	"github.com/dillonzq/apix/internal/llm/adapter/shared"
)

// splitFrame must read what the framer reads wherever it accepts a frame.
func FuzzSplitFrameMatchesFramer(f *testing.F) {
	for _, s := range []string{
		"event: response.output_text.delta\ndata: {\"delta\":\"hi\"}\n\n",
		"event: a\ndata: {}\n\n",
		"event:  spaced \ndata: {}\n\n",
		"event: a\r\ndata: {}\r\n\r\n",
		"event: a\ndata: {\"x\":1}\ndata: {}\n\n",
		"event: a\ndata:{}\n\n",
		"data: {}\n\n",
		"event: a\ndata: [1]\n\n",
		"event:  a\ndata: {}\n\n",
	} {
		f.Add([]byte(s))
	}
	f.Fuzz(func(t *testing.T, frame []byte) {
		name, data, ok := splitFrame(frame)
		if !ok {
			return
		}
		framer := shared.NewSSEFramer(false)
		framer.Push(frame)
		wantName, wantData, _, wantOK := framer.Next()
		if !wantOK || name != wantName || string(data) != wantData {
			t.Fatalf("splitFrame(%q) = %q, %q; framer reads %q, %q, %v", frame, name, data, wantName, wantData, wantOK)
		}
		if _, _, _, more := framer.Next(); more {
			t.Fatalf("splitFrame accepted %q, which holds more than one frame", frame)
		}
	})
}

// drain accepts a Chat Completions payload by the first byte Compact writes;
// that must agree with Compact plus shared.IsJSONObject.
func FuzzCompactObjectCheck(f *testing.F) {
	for _, s := range []string{`{}`, ` {"a":1} `, "\t{\n}\r", `[]`, `"s"`, `1`, `{"a":1}{}`, " {}", "\u0085{}", ``, ` `} {
		f.Add(s)
	}
	f.Fuzz(func(t *testing.T, data string) {
		var buf bytes.Buffer
		buf.WriteString("data: ")
		err := json.Compact(&buf, []byte(data))
		got := err == nil && buf.Bytes()[len("data: ")] == '{'

		var want bytes.Buffer
		wantOK := json.Compact(&want, []byte(data)) == nil && shared.IsJSONObject(data)
		if got != wantOK {
			t.Fatalf("payload %q: first-byte check %v, IsJSONObject %v", data, got, wantOK)
		}
		if got && !bytes.Equal(buf.Bytes()[len("data: "):], want.Bytes()) {
			t.Fatalf("payload %q compacts differently into a prefixed buffer", data)
		}
	})
}
