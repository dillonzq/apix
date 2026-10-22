package messages

import (
	"fmt"
	"reflect"
	"testing"
)

func TestResponsesToolOutputImages(t *testing.T) {
	for _, stream := range []bool{false, true} {
		for _, outputType := range []string{"function_call_output", "custom_tool_call_output"} {
			t.Run(fmt.Sprintf("%s/stream=%t", outputType, stream), func(t *testing.T) {
				body := fmt.Sprintf(`{"stream":%t,"input":[
				{"type":"function_call","call_id":"c1","name":"read","arguments":"{}"},
				{"type":%q,"call_id":"c1","output":[
					{"type":"input_text","text":"before"},
					{"type":"input_image","image_url":"data:image/png;base64,AAAA"},
					{"type":"output_text","text":"after"},
					{"type":"input_image","image_url":{"url":"https://x/i.png"}}]}
			]}`, stream, outputType)
				m, eErr := respReq(t, body)
				if eErr != nil {
					t.Fatalf("unexpected error: %v", eErr)
				}
				if got, _ := m["stream"].(bool); got != stream {
					t.Fatal("stream flag lost")
				}
				msgs := m["messages"].([]any)
				if len(msgs) != 2 {
					t.Fatalf("message count = %d", len(msgs))
				}
				tool := msgs[1].(map[string]any)["content"].([]any)[0].(map[string]any)
				if tool["type"] != "tool_result" || tool["tool_use_id"] != "c1" {
					t.Fatalf("tool result = %v", tool)
				}
				want := []any{
					map[string]any{"type": "text", "text": "before"},
					map[string]any{"type": "image", "source": map[string]any{"type": "base64", "media_type": "image/png", "data": "AAAA"}},
					map[string]any{"type": "text", "text": "after"},
					map[string]any{"type": "image", "source": map[string]any{"type": "url", "url": "https://x/i.png"}},
				}
				if !reflect.DeepEqual(tool["content"], want) {
					t.Fatalf("mixed tool content = %v, want %v", tool["content"], want)
				}
			})
		}
	}
}

func TestResponsesToolOutputInvalidParts(t *testing.T) {
	for _, output := range []string{`42`, `[{"type":"input_image"}]`, `[{"type":"input_audio"}]`} {
		body := fmt.Sprintf(`{"input":[{"type":"function_call_output","call_id":"c1","output":%s}]}`, output)
		if _, eErr := respReq(t, body); eErr == nil {
			t.Fatalf("invalid output accepted: %s", output)
		}
	}
}
