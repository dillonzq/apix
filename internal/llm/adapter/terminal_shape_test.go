package adapters_test

import (
	"encoding/json"
	"fmt"
	"strings"
	"testing"

	"github.com/dillonzq/apix/internal/errclass"
	"github.com/dillonzq/apix/internal/llm/adapter/messages"
	"github.com/dillonzq/apix/internal/llm/adapter/responses"
)

func TestMessagesTerminalPayloadShape(t *testing.T) {
	for _, format := range []string{"chat-completions", "messages", "responses"} {
		for _, payload := range []string{`{}`, `{"unrelated":"private-prompt-marker"}`, `{"type":null}`, `{"type":1}`, `{"type":"ping"}`, `{"type":"message_delta"}`} {
			for _, eof := range []bool{false, true} {
				t.Run(fmt.Sprintf("%s/%s/eof=%t", format, payload, eof), func(t *testing.T) {
					converter := messages.NewStreamConverter(format)
					frame := "event: message_stop\ndata: " + payload
					var events [][]byte
					var done bool
					var err *errclass.Error
					if eof {
						_, done, eErr := converter.Feed([]byte(frame))
						if eErr != nil || done {
							t.Fatal("buffered frame failed early")
						}
						events, eErr = converter.Finish()
						err = eErr
					} else {
						ev, d, e := converter.Feed([]byte(frame + "\n\n"))
						events, done, err = ev, d, e
					}
					if err == nil || done || len(events) != 0 {
						t.Fatal("incomplete message_stop reported success")
					}
					if strings.Contains(err.Error(), "private-prompt-marker") {
						t.Fatal("terminal validation leaked payload")
					}
				})
			}
		}
		for _, eof := range []bool{false, true} {
			for _, eventLine := range []string{"event: message_stop\n", ""} {
				t.Run(fmt.Sprintf("valid/%s/eof=%t/event=%q", format, eof, eventLine), func(t *testing.T) {
					converter := messages.NewStreamConverter(format)
					frame := eventLine + `data: {"type":"message_stop","future_field":true}`
					if eof {
						_, _, eErr := converter.Feed([]byte(frame))
						if eErr != nil {
							t.Fatal(eErr)
						}
						if _, eErr := converter.Finish(); eErr != nil {
							t.Fatal(eErr)
						}
					} else {
						_, done, eErr := converter.Feed([]byte(frame + "\n\n"))
						if eErr != nil || !done {
							t.Fatalf("valid terminal: %v %t", eErr, done)
						}
					}
					if events, eErr := converter.Finish(); eErr != nil || len(events) != 0 {
						t.Fatal("EOF duplicated terminal")
					}
				})
			}
		}
	}
}

func TestResponsesTerminalPayloadShape(t *testing.T) {
	for _, event := range []string{"response.completed", "response.incomplete"} {
		status := strings.TrimPrefix(event, "response.")
		valid := map[string]any{"type": event, "response": map[string]any{"id": "resp_terminal", "object": "response", "status": status, "output": []any{}}}
		bodies := []string{`{}`, `{"unrelated":"private-prompt-marker"}`, `{"response":{}}`, `{"response":null}`, `{"response":[]}`}
		mutations := []struct {
			field  string
			values []any
		}{
			{"id", []any{nil, "", " ", 1}},
			{"object", []any{nil, "", "message", 1}},
			{"status", []any{nil, "", "in_progress", "failed", 1}},
			{"output", []any{nil, "", map[string]any{}, 1, []any{nil}, []any{1}}},
		}
		for _, mutation := range mutations {
			for _, value := range mutation.values {
				body, _ := json.Marshal(valid)
				var doc map[string]any
				_ = json.Unmarshal(body, &doc)
				if value == nil {
					delete(doc["response"].(map[string]any), mutation.field)
				} else {
					doc["response"].(map[string]any)[mutation.field] = value
				}
				changed, _ := json.Marshal(doc)
				bodies = append(bodies, string(changed))
			}
		}
		for _, value := range []any{nil, "", "response.created", 1} {
			body, _ := json.Marshal(valid)
			var doc map[string]any
			_ = json.Unmarshal(body, &doc)
			if value == nil {
				delete(doc, "type")
			} else {
				doc["type"] = value
			}
			changed, _ := json.Marshal(doc)
			bodies = append(bodies, string(changed))
		}
		wrongStatus := map[string]string{"completed": "incomplete", "incomplete": "completed"}[status]
		bodies = append(bodies, fmt.Sprintf(`{"type":%q,"response":{"id":"resp_terminal","object":"response","status":%q,"output":[]}}`, event, wrongStatus))
		bodies = append(bodies, fmt.Sprintf(`{"type":%q,"response":{"id":"resp_terminal","object":"response","status":%q,"output":null}}`, event, status))
		for _, format := range []string{"chat-completions", "messages", "responses"} {
			for i, body := range bodies {
				for _, eof := range []bool{false, true} {
					t.Run(fmt.Sprintf("%s/%s/%d/eof=%t", event, format, i, eof), func(t *testing.T) {
						converter := responses.NewStreamConverter(format)
						frame := "event: " + event + "\ndata: " + body
						if eof {
							_, done, eErr := converter.Feed([]byte(frame))
							if eErr != nil || done {
								t.Fatal("buffered frame failed early")
							}
							events, eErr := converter.Finish()
							if eErr == nil || len(events) != 0 {
								t.Fatal("incomplete EOF terminal accepted")
							}
							if strings.Contains(eErr.Message, "private-prompt-marker") {
								t.Fatal("payload leaked")
							}
						} else {
							events, done, eErr := converter.Feed([]byte(frame + "\n\n"))
							if eErr == nil || done || len(events) != 0 {
								t.Fatal("incomplete terminal accepted")
							}
							if strings.Contains(eErr.Message, "private-prompt-marker") {
								t.Fatal("payload leaked")
							}
						}
					})
				}
			}
		}
		for _, format := range []string{"chat-completions", "messages", "responses"} {
			for _, eof := range []bool{false, true} {
				t.Run(fmt.Sprintf("valid/%s/%s/eof=%t", event, format, eof), func(t *testing.T) {
					doc := map[string]any{"type": event, "sequence_number": 1, "response": map[string]any{"id": "resp_terminal", "object": "response", "status": status, "output": []any{}, "future_field": true}}
					body, _ := json.Marshal(doc)
					frame := "event: " + event + "\ndata: " + string(body)
					converter := responses.NewStreamConverter(format)
					var events [][]byte
					if eof {
						_, _, eErr := converter.Feed([]byte(frame))
						if eErr != nil {
							t.Fatal(eErr)
						}
						ev, e := converter.Finish()
						if e != nil {
							t.Fatal(e)
						}
						events = ev
					} else {
						ev, done, eErr := converter.Feed([]byte(frame + "\n\n"))
						if eErr != nil || !done {
							t.Fatalf("valid terminal: %v %t", eErr, done)
						}
						events = ev
					}
					if len(events) == 0 {
						t.Fatal("valid terminal missing")
					}
					if format == "responses" && !strings.Contains(string(events[0]), string(body)) {
						t.Fatal("native terminal payload changed")
					}
					if ev, eErr := converter.Finish(); eErr != nil || len(ev) != 0 {
						t.Fatal("EOF duplicated terminal")
					}
				})
			}
		}
	}
}
