package translate_test

import (
	"reflect"
	"strings"
	"testing"

	"github.com/dillonzq/apix/internal/llm/translate"
)

// Errors must not discard earlier events based on network chunk boundaries.
func TestStreamErrorPreservesCompletedEvents(t *testing.T) {
	fixtures := []struct{ source, first, broken string }{
		{translate.FormatChatCompletions, "data: {\"id\":\"r\",\"model\":\"m\",\"choices\":[{\"delta\":{\"content\":\"hello\"}}]}\n\n", "data: {broken\n\n"},
		{translate.FormatMessages, "event: message_start\ndata: {\"type\":\"message_start\",\"message\":{\"id\":\"r\",\"model\":\"m\"}}\n\n", "event: message_stop\ndata: {broken\n\n"},
		{translate.FormatResponses, "event: response.created\ndata: {\"response\":{\"id\":\"r\",\"model\":\"m\"}}\n\n", "event: response.completed\ndata: {broken\n\n"},
	}
	for _, fixture := range fixtures {
		for _, target := range formats {
			t.Run(fixture.source+"/"+target, func(t *testing.T) {
				run := func(body string, step int) []wireEvent {
					t.Helper()
					sc, err := translate.NewStreamConverter(fixture.source, target, nil)
					if err != nil {
						t.Fatal(err)
					}
					var frames [][]byte
					failed := false
					for i := 0; i < len(body); i += step {
						end := min(i+step, len(body))
						events, done, err := sc.Feed([]byte(body[i:end]))
						frames = append(frames, events...)
						if done {
							t.Fatal("broken stream completed")
						}
						if err != nil {
							failed = true
							break
						}
					}
					if !failed {
						tail, err := sc.Finish()
						frames = append(frames, tail...)
						failed = err != nil
					}
					if !failed {
						t.Fatal("broken stream succeeded")
					}
					if tail, err := sc.Finish(); err == nil || len(tail) != 0 {
						t.Fatal("failed stream recovered")
					}
					if events, done, err := sc.Feed(nil); err == nil || done || len(events) != 0 {
						t.Fatal("failed stream recovered on Feed")
					}
					events := wireEvents(t, frames)
					normalizeEventTimes(events)
					if len(events) == 0 {
						t.Fatal("earlier events were discarded")
					}
					return events
				}
				for _, body := range []string{fixture.first + fixture.broken, fixture.first + strings.TrimRight(fixture.broken, "\n"), strings.TrimRight(fixture.first, "\n")} {
					whole := run(body, len(body))
					chunked := run(body, 1)
					if !reflect.DeepEqual(whole, chunked) {
						t.Fatal("error output depends on chunk boundaries")
					}
				}
			})
		}
	}
}

// A single parsed event can emit valid content before a later tool or item
// fails. Preserve that content as well as events from earlier SSE frames.
func TestStreamErrorInsideEventPreservesOutput(t *testing.T) {
	chatFrame := "data: " + `{"id":"r","model":"m","choices":[{"delta":{"content":"before failure","tool_calls":[{"index":0,"id":"call","function":{"arguments":"{}"}}]},"finish_reason":"tool_calls"}]}` + "\n\n"
	responseStart := "event: response.created\ndata: " + `{"type":"response.created","response":{"id":"r","model":"m"}}` + "\n\n"
	responseFrame := "event: response.completed\ndata: " + `{"type":"response.completed","response":{"id":"r","model":"m","object":"response","status":"completed","output":[{"type":"message","id":"first","role":"assistant","content":[{"type":"output_text","text":"first"}]},{"type":"message","id":"second","role":"assistant","content":[{"type":"output_text","text":"second"}]},{"type":"future_item","action":{"content":"cannot represent"}}]}}` + "\n\n"
	fixtures := []struct {
		source, target, prefix, frame string
		names, texts                  []string
	}{
		{translate.FormatChatCompletions, translate.FormatMessages, "", chatFrame,
			[]string{"message_start", "content_block_start", "content_block_delta"}, []string{"before failure"}},
		{translate.FormatChatCompletions, translate.FormatResponses, "", chatFrame,
			[]string{"response.created", "response.output_item.added", "response.output_text.delta"}, []string{"before failure"}},
		{translate.FormatResponses, translate.FormatMessages, responseStart, responseFrame,
			[]string{"content_block_start", "content_block_delta", "content_block_delta"}, []string{"first", "second"}},
		{translate.FormatResponses, translate.FormatChatCompletions, responseStart, responseFrame,
			[]string{"", ""}, []string{"first", "second"}},
	}
	for _, fixture := range fixtures {
		t.Run(fixture.source+"/"+fixture.target, func(t *testing.T) {
			run := func(step int) []wireEvent {
				t.Helper()
				sc, err := translate.NewStreamConverter(fixture.source, fixture.target, nil)
				if err != nil {
					t.Fatal(err)
				}
				if fixture.prefix != "" {
					events, done, err := sc.Feed([]byte(fixture.prefix))
					if err != nil || done || len(events) == 0 {
						t.Fatalf("invalid fixture prefix: events=%d done=%t err=%v", len(events), done, err)
					}
				}
				var frames [][]byte
				var failure *translate.Error
				for i := 0; i < len(fixture.frame); i += step {
					events, done, err := sc.Feed([]byte(fixture.frame[i:min(i+step, len(fixture.frame))]))
					frames = append(frames, events...)
					if done {
						t.Fatal("invalid tool or output item completed successfully")
					}
					if err != nil {
						failure = err
						break
					}
				}
				if failure == nil || failure.Class != translate.ClassTranslation {
					t.Fatalf("expected translation failure, got %v", failure)
				}
				events := wireEvents(t, frames)
				var names, texts []string
				for _, event := range events {
					names = append(names, event.Name)
					switch fixture.target {
					case translate.FormatMessages:
						if event.Name == "content_block_delta" {
							delta := event.Data["delta"].(map[string]any)
							if delta["type"] != "text_delta" || event.Data["index"] != float64(0) {
								t.Fatalf("unexpected text delta: %v", event.Data)
							}
							texts = append(texts, delta["text"].(string))
						}
					case translate.FormatResponses:
						if event.Name == "response.output_text.delta" {
							texts = append(texts, event.Data["delta"].(string))
						}
					case translate.FormatChatCompletions:
						choice := event.Data["choices"].([]any)[0].(map[string]any)
						if choice["finish_reason"] != nil {
							t.Fatalf("failure emitted a finish reason: %v", choice)
						}
						texts = append(texts, choice["delta"].(map[string]any)["content"].(string))
					}
				}
				if !reflect.DeepEqual(names, fixture.names) || !reflect.DeepEqual(texts, fixture.texts) {
					t.Fatalf("output before failure changed: names=%v texts=%v", names, texts)
				}
				if events, done, err := sc.Feed([]byte("data: [DONE]\n\n")); err != failure || done || len(events) != 0 {
					t.Fatal("failed stream recovered or changed its error")
				}
				if tail, err := sc.Finish(); err != failure || len(tail) != 0 {
					t.Fatal("failed stream emitted completion or changed its error at EOF")
				}
				normalizeEventTimes(events)
				return events
			}
			whole := run(len(fixture.frame))
			chunked := run(1)
			if !reflect.DeepEqual(whole, chunked) {
				t.Fatal("output before failure depends on network chunk boundaries")
			}
		})
	}
}
