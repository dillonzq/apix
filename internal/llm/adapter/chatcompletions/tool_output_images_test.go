package chatcompletions

import (
	"fmt"
	"reflect"
	"testing"
)

func TestResponsesToolOutputImages(t *testing.T) {
	for _, stream := range []bool{false, true} {
		for _, outputType := range []string{"function_call_output", "custom_tool_call_output"} {
			for _, tail := range []string{"", `,{"type":"message","role":"assistant","content":"done"}`} {
				t.Run(fmt.Sprintf("%s/stream=%t/tail=%t", outputType, stream, tail != ""), func(t *testing.T) {
					body := fmt.Sprintf(`{"stream":%t,"input":[
					{"type":"function_call","call_id":"c1","name":"read","arguments":"{}"},
					{"type":"function_call","call_id":"c2","name":"read","arguments":"{}"},
					{"type":%q,"call_id":"c1","output":[
						{"type":"input_text","text":"before"},
						{"type":"input_image","image_url":"data:image/png;base64,AAAA"},
						{"type":"output_text","text":"after"},
						{"type":"input_image","image_url":"https://x/second.png"}]},
					{"type":"reasoning","summary":[{"type":"summary_text","text":"thinking"}]},
					{"type":"message","role":"user","content":""},
					{"type":"message","role":"assistant","content":null},
					{"type":"message","role":"developer","content":[]},
					{"type":"message","role":"system","content":[{"type":"input_text","text":""}]},
					{"type":"function_call_output","call_id":"c2","output":[
						{"type":"input_image","image_url":{"url":"https://x/i.png"}}]}
					%s]}`, stream, outputType, tail)
					m := mustBuild(t, "responses", body)
					if got, _ := m["stream"].(bool); got != stream {
						t.Fatal("stream flag lost")
					}
					msgs := m["messages"].([]any)
					wantLen := 4
					if tail != "" {
						wantLen++
					}
					if len(msgs) != wantLen {
						t.Fatalf("message count = %d, want %d", len(msgs), wantLen)
					}
					for i, wantText := range []string{"beforeafter", ""} {
						tool := msgs[i+1].(map[string]any)
						if tool["role"] != "tool" || tool["tool_call_id"] != fmt.Sprintf("c%d", i+1) || tool["content"] != wantText {
							t.Fatalf("tool result %d = %v", i, tool)
						}
					}
					user := msgs[3].(map[string]any)
					if user["role"] != "user" {
						t.Fatalf("image message role = %v", user["role"])
					}
					want := []any{
						map[string]any{"type": "text", "text": `Tool output for call_id "c1" (2 images):`},
						map[string]any{"type": "text", "text": "before"},
						map[string]any{"type": "image_url", "image_url": map[string]any{"url": "data:image/png;base64,AAAA"}},
						map[string]any{"type": "text", "text": "after"},
						map[string]any{"type": "image_url", "image_url": map[string]any{"url": "https://x/second.png"}},
						map[string]any{"type": "text", "text": `Tool output for call_id "c2" (1 images):`},
						map[string]any{"type": "image_url", "image_url": map[string]any{"url": "https://x/i.png"}},
					}
					if !reflect.DeepEqual(user["content"], want) {
						t.Fatalf("tool image grouping/order lost: %v", user["content"])
					}
					if tail != "" && msgs[4].(map[string]any)["content"] != "done" {
						t.Fatal("following assistant message lost")
					}
				})
			}
		}
	}
}

func TestResponsesToolOutputInvalidParts(t *testing.T) {
	for _, output := range []string{`42`, `[{"type":"input_image"}]`, `[{"type":"input_audio"}]`} {
		body := fmt.Sprintf(`{"input":[{"type":"function_call_output","call_id":"c1","output":%s}]}`, output)
		if _, eErr := BuildRequest("m", "responses", []byte(body), nil); eErr == nil {
			t.Fatalf("invalid output accepted: %s", output)
		}
	}
}
