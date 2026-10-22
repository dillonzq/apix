package adapters_test

import (
	"strings"
	"testing"

	"github.com/dillonzq/apix/internal/errclass"
	"github.com/dillonzq/apix/internal/llm/adapter/chatcompletions"
	"github.com/dillonzq/apix/internal/llm/adapter/messages"
	"github.com/dillonzq/apix/internal/llm/adapter/responses"
)

type eofConverter interface {
	Feed([]byte) ([][]byte, bool, *errclass.Error)
	Finish() ([][]byte, *errclass.Error)
}

func TestStreamEOFRequiresTerminalState(t *testing.T) {
	routes := []struct {
		name                 string
		new                  func(string) eofConverter
		text, tool, terminal string
	}{
		{"chat", func(f string) eofConverter { return chatcompletions.NewStreamConverter(f) },
			`data: {"id":"c1","choices":[{"delta":{"content":"partial"}}]}` + "\n\n",
			`data: {"id":"c1","choices":[{"delta":{"tool_calls":[{"index":0,"id":"t1","function":{"name":"f","arguments":"{\"x\":"}}]}}]}` + "\n\n",
			`data: {"id":"c1","choices":[{"delta":{},"finish_reason":"stop"}]}`},
		{"messages", func(f string) eofConverter { return messages.NewStreamConverter(f) },
			"event: message_start\ndata: {\"type\":\"message_start\",\"message\":{\"id\":\"m1\"}}\n\nevent: content_block_start\ndata: {\"index\":0,\"content_block\":{\"type\":\"text\"}}\n\nevent: content_block_delta\ndata: {\"index\":0,\"delta\":{\"type\":\"text_delta\",\"text\":\"partial\"}}\n\n",
			"event: content_block_start\ndata: {\"index\":0,\"content_block\":{\"type\":\"tool_use\",\"id\":\"t1\",\"name\":\"f\"}}\n\nevent: content_block_delta\ndata: {\"index\":0,\"delta\":{\"type\":\"input_json_delta\",\"partial_json\":\"{\\\"x\\\":\"}}\n\n",
			"event: message_delta\ndata: {\"delta\":{\"stop_reason\":\"end_turn\"}}"},
		{"responses", func(f string) eofConverter { return responses.NewStreamConverter(f) },
			"event: response.created\ndata: {\"response\":{\"id\":\"r1\"}}\n\nevent: response.output_text.delta\ndata: {\"delta\":\"partial\"}\n\n",
			"event: response.output_item.added\ndata: {\"item\":{\"type\":\"function_call\",\"id\":\"t1\",\"call_id\":\"t1\",\"name\":\"f\"}}\n\nevent: response.function_call_arguments.delta\ndata: {\"item_id\":\"t1\",\"delta\":\"{\\\"x\\\":\"}\n\n",
			"event: response.completed\ndata: {\"response\":{\"id\":\"r1\",\"status\":\"completed\",\"output\":[],\"object\":\"response\"},\"type\":\"response.completed\"}"},
	}
	for _, route := range routes {
		for _, format := range []string{"chat-completions", "messages", "responses"} {
			for _, partial := range []struct{ name, body string }{{"text", route.text}, {"tool", route.tool}} {
				t.Run(route.name+"/"+format+"/"+partial.name, func(t *testing.T) {
					converter := route.new(format)
					events, done, eErr := converter.Feed([]byte(partial.body))
					if eErr != nil || done {
						t.Fatalf("partial feed: done=%t err=%v", done, eErr)
					}
					tail, eErr := converter.Finish()
					if eErr == nil {
						t.Fatal("EOF without terminal accepted")
					}
					for _, event := range append(events, tail...) {
						if strings.Contains(string(event), "response.completed") {
							t.Fatal("incomplete stream emitted completion")
						}
					}
				})
			}
			t.Run(route.name+"/"+format+"/terminal-without-separator", func(t *testing.T) {
				converter := route.new(format)
				_, _, eErr := converter.Feed([]byte(route.text + route.terminal))
				if eErr != nil {
					t.Fatal(eErr)
				}
				_, eErr = converter.Finish()
				if eErr != nil {
					t.Fatalf("legal terminal at EOF: %v", eErr)
				}
				again, eErr := converter.Finish()
				if eErr != nil || len(again) != 0 {
					t.Fatal("repeated EOF emitted another terminal")
				}
			})
			// A malformed final SSE data line must not vanish behind a held finish.
			if format != "chat-completions" || route.name != "chat" {
				t.Run(route.name+"/"+format+"/malformed-tail", func(t *testing.T) {
					converter := route.new(format)
					_, _, _ = converter.Feed([]byte(route.text + "data: {truncated"))
					if _, eErr := converter.Finish(); eErr == nil {
						t.Fatal("malformed EOF tail accepted")
					}
				})
			}
		}
	}
}

func TestMessagesFlushDoesNotInventCompletion(t *testing.T) {
	converter := messages.NewStreamConverter("responses")
	_, _, eErr := converter.Feed([]byte("event: message_start\ndata: {\"message\":{\"id\":\"m1\"}}\n\nevent: content_block_delta\ndata: {\"index\":0,\"delta\":{\"type\":\"text_delta\",\"text\":\"partial\"}}\n\n"))
	if eErr != nil {
		t.Fatal(eErr)
	}
	if tail := converter.Flush(); len(tail) != 0 {
		t.Fatal("Flush invented completion without stop_reason")
	}
	if _, eErr := converter.Finish(); eErr == nil {
		t.Fatal("empty Flush suppressed truncated EOF detection")
	}
}

func TestConvertersDoNotDuplicateTerminal(t *testing.T) {
	for _, format := range []string{"chat-completions", "messages", "responses"} {
		converters := []struct {
			converter eofConverter
			body      string
		}{
			{chatcompletions.NewStreamConverter(format), `data: {"id":"c1","choices":[{"delta":{},"finish_reason":"stop"}]}` + "\n\ndata: [DONE]\n\n"},
			{messages.NewStreamConverter(format), "event: message_delta\ndata: {\"delta\":{\"stop_reason\":\"end_turn\"}}\n\nevent: message_stop\ndata: {\"type\":\"message_stop\"}\n\n"},
			{responses.NewStreamConverter(format), "event: response.completed\ndata: {\"response\":{\"status\":\"completed\",\"id\":\"resp_terminal\",\"object\":\"response\",\"output\":[]},\"type\":\"response.completed\"}\n\n"},
		}
		for _, tc := range converters {
			events, done, eErr := tc.converter.Feed([]byte(tc.body))
			if eErr != nil || !done {
				t.Fatalf("terminal: %v %t", eErr, done)
			}
			tail, eErr := tc.converter.Finish()
			if eErr != nil || len(tail) != 0 {
				t.Fatal("EOF duplicated terminal")
			}
			again, done, eErr := tc.converter.Feed([]byte(tc.body))
			if eErr != nil || !done || len(again) != 0 {
				t.Fatal("Feed duplicated terminal")
			}
			_ = events
		}
	}
}

func TestEOFRejectsIncompleteTerminalPayload(t *testing.T) {
	routes := []struct {
		name, terminal, prefix string
		new                    func(string) eofConverter
	}{
		{"messages", "message_stop", "event: message_start\ndata: {\"message\":{\"id\":\"m1\"}}\n\n", func(f string) eofConverter { return messages.NewStreamConverter(f) }},
		{"responses", "response.completed", "event: response.created\ndata: {\"response\":{\"id\":\"r1\"}}\n\n", func(f string) eofConverter { return responses.NewStreamConverter(f) }},
	}
	for _, route := range routes {
		for _, format := range []string{"chat-completions", "messages", "responses"} {
			for _, tail := range []string{"", "\ndata:", "\ndata: {\"type\":", "\ndata: null", "\ndata: []"} {
				t.Run(route.name+"/"+format+"/"+tail, func(t *testing.T) {
					converter := route.new(format)
					_, done, eErr := converter.Feed([]byte(route.prefix + "event: " + route.terminal + tail))
					if eErr != nil || done {
						t.Fatalf("pending terminal: done=%t err=%v", done, eErr)
					}
					events, eErr := converter.Finish()
					if eErr == nil {
						t.Fatal("incomplete terminal at EOF accepted")
					}
					if len(events) != 0 {
						t.Fatal("incomplete terminal emitted completion")
					}
				})
			}
		}
	}
}

func TestNativeChatEOFRejectsMalformedResidualAfterFinish(t *testing.T) {
	converter := chatcompletions.NewStreamConverter("chat-completions")
	_, done, eErr := converter.Feed([]byte(`data: {"choices":[{"delta":{},"finish_reason":"stop"}]}` + "\n\ndata: {\"id\":"))
	if eErr != nil || done {
		t.Fatalf("feed: %v %t", eErr, done)
	}
	if _, eErr := converter.Finish(); eErr == nil {
		t.Fatal("malformed buffered trailer discarded after finish_reason")
	}
}

func TestFramedTerminalRequiresCompletePayload(t *testing.T) {
	for _, format := range []string{"chat-completions", "messages", "responses"} {
		for _, payload := range []string{"", "{", "null", "[]"} {
			converters := []struct {
				converter eofConverter
				event     string
			}{
				{messages.NewStreamConverter(format), "message_stop"},
				{responses.NewStreamConverter(format), "response.completed"},
				{responses.NewStreamConverter(format), "response.incomplete"},
			}
			for _, tc := range converters {
				events, done, eErr := tc.converter.Feed([]byte("event: " + tc.event + "\ndata: " + payload + "\n\n"))
				if eErr == nil || done || len(events) != 0 {
					t.Fatalf("%s/%s accepted malformed terminal", format, tc.event)
				}
			}
		}
	}
}
