package shared

import (
	"bytes"
	"encoding/json"
	"testing"
)

// The delta event structs must encode exactly as the maps they replaced.
func FuzzDeltaEventsMatchMaps(f *testing.F) {
	for _, s := range []string{"", "hello", "你好🙂", "<a href=\"x\">&</a>", "  ", "\xff\xfe", "line\nquote\"\\"} {
		f.Add(s, "item_1", 3, 1)
	}
	f.Fuzz(func(t *testing.T, delta, id string, index, contentIndex int) {
		if len(delta)+len(id) > 1024 {
			t.Skip()
		}
		for _, c := range []struct {
			got  any
			want map[string]any
		}{
			{contentDeltaEvent{ContentIndex: contentIndex, Delta: delta, ItemID: id, OutputIndex: index, Type: "response.output_text.delta"},
				map[string]any{"type": "response.output_text.delta", "item_id": id, "output_index": index, "content_index": contentIndex, "delta": delta}},
			{itemDeltaEvent{Delta: delta, ItemID: id, OutputIndex: index, Type: "response.function_call_arguments.delta"},
				map[string]any{"type": "response.function_call_arguments.delta", "item_id": id, "output_index": index, "delta": delta}},
			{summaryDeltaEvent{Delta: delta, ItemID: id, OutputIndex: index, Type: "response.reasoning_summary_text.delta"},
				map[string]any{"type": "response.reasoning_summary_text.delta", "item_id": id, "output_index": index, "summary_index": 0, "delta": delta}},
		} {
			got, err := json.Marshal(c.got)
			if err != nil {
				t.Fatal(err)
			}
			want, err := json.Marshal(c.want)
			if err != nil {
				t.Fatal(err)
			}
			if !bytes.Equal(got, want) {
				t.Fatalf("struct encodes as\n%s\nmap as\n%s", got, want)
			}
		}
	})
}
