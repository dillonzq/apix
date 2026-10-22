package translate_test

import (
	"encoding/json"
	"github.com/dillonzq/apix/internal/llm/translate"
	"reflect"
	"strings"
	"testing"
)

func normalizeEventTimes(events []wireEvent) {
	var scrub func(any)
	scrub = func(value any) {
		switch v := value.(type) {
		case map[string]any:
			delete(v, "created")
			delete(v, "created_at")
			for _, child := range v {
				scrub(child)
			}
		case []any:
			for _, child := range v {
				scrub(child)
			}
		}
	}
	for _, event := range events {
		scrub(event.Data)
	}
}
func FuzzStreamChunkBoundaries(f *testing.F) {
	for _, text := range []string{"", "hello", "你好🙂", "line\nquote\""} {
		for pair := byte(0); pair < 9; pair++ {
			f.Add(text, byte(1), pair)
			f.Add(text, byte(7), pair+9)
			f.Add(text, byte(64), pair+18)
		}
	}
	f.Fuzz(func(t *testing.T, text string, size, pair byte) {
		if len(text) > 1024 {
			t.Skip()
		}
		payload, _ := json.Marshal(text)
		source := formats[int(pair)%3]
		target := formats[(int(pair)/3)%3]
		bodies := map[string]string{
			translate.FormatChatCompletions: "data: {\"id\":\"r\",\"model\":\"m\",\"choices\":[{\"delta\":{\"content\":" + string(payload) + "}}]}\n\ndata: {\"choices\":[{\"delta\":{},\"finish_reason\":\"stop\"}]}\n\ndata: [DONE]\n\n",
			translate.FormatMessages:        "event: message_start\ndata: {\"type\":\"message_start\",\"message\":{\"id\":\"r\",\"model\":\"m\"}}\n\nevent: content_block_start\ndata: {\"index\":0,\"content_block\":{\"type\":\"text\",\"text\":\"\"}}\n\nevent: content_block_delta\ndata: {\"index\":0,\"delta\":{\"type\":\"text_delta\",\"text\":" + string(payload) + "}}\n\nevent: content_block_stop\ndata: {\"index\":0}\n\nevent: message_delta\ndata: {\"delta\":{\"stop_reason\":\"end_turn\"}}\n\nevent: message_stop\ndata: {\"type\":\"message_stop\"}\n\n",
			translate.FormatResponses:       "event: response.created\ndata: {\"response\":{\"id\":\"r\",\"model\":\"m\"}}\n\nevent: response.output_text.delta\ndata: {\"item_id\":\"m1\",\"content_index\":0,\"delta\":" + string(payload) + "}\n\nevent: response.completed\ndata: {\"type\":\"response.completed\",\"response\":{\"id\":\"r\",\"model\":\"m\",\"object\":\"response\",\"status\":\"completed\",\"output\":[]}}\n\n",
		}
		ending := []string{"\n", "\r\n", "\r"}[(int(pair)/9)%3]
		for format, body := range bodies {
			bodies[format] = strings.ReplaceAll(body, "\n", ending)
		}
		run := func(step int) []wireEvent {
			sc, err := translate.NewStreamConverter(source, target, nil)
			if err != nil {
				t.Fatal(err)
			}
			body := bodies[source]
			var frames [][]byte
			for i := 0; i < len(body); i += step {
				end := i + step
				if end > len(body) {
					end = len(body)
				}
				events, _, err := sc.Feed([]byte(body[i:end]))
				if err != nil {
					t.Fatal(err)
				}
				frames = append(frames, events...)
			}
			tail, err := sc.Finish()
			if err != nil {
				t.Fatal(err)
			}
			frames = append(frames, tail...)
			events := wireEvents(t, frames)
			normalizeEventTimes(events)
			return events
		}
		whole := run(len(bodies[source]))
		chunked := run(int(size) + 1)
		if !reflect.DeepEqual(whole, chunked) {
			t.Fatalf("chunk-dependent output: %s -> %s", source, target)
		}
	})
}
func FuzzSSEFailureIsTerminal(f *testing.F) {
	f.Add("data: {broken\n\n", byte(0))
	f.Add("event: response.failed\ndata: {\"response\":{\"error\":{\"message\":\"failed\"}}}\n\n", byte(2))
	f.Fuzz(func(t *testing.T, body string, pair byte) {
		if len(body) > 4096 {
			t.Skip()
		}
		source := formats[int(pair)%3]
		target := formats[(int(pair)/3)%3]
		sc, err := translate.NewStreamConverter(source, target, nil)
		if err != nil {
			t.Fatal(err)
		}
		_, _, err = sc.Feed([]byte(body))
		if err == nil {
			_, err = sc.Finish()
		}
		if err != nil {
			events, _, again := sc.Feed([]byte("data: [DONE]\n\n"))
			if again == nil || len(events) > 0 {
				t.Fatal("stream recovered after error")
			}
			tail, again := sc.Finish()
			if again == nil || len(tail) > 0 {
				t.Fatal("failed stream emitted a completion")
			}
		}
	})
}
func TestSSELineEndings(t *testing.T) {
	original := "data: {\"id\":\"r\",\"choices\":[{\"delta\":{\"content\":\"hello\"}}]}\n\ndata: [DONE]\n\n"
	for _, ending := range []string{"\n", "\r\n", "\r"} {
		for _, target := range formats {
			streamFrames(t, translate.FormatChatCompletions, target, strings.ReplaceAll(original, "\n", ending), nil)
		}
	}
}
