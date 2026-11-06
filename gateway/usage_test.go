package gateway_test

import (
	"bytes"
	"io"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"sync"
	"testing"

	"github.com/dillonzq/apix"
	"github.com/dillonzq/apix/gateway"
	"github.com/dillonzq/apix/protocol/anthropic"
	"github.com/dillonzq/apix/protocol/openai"
)

const (
	usageStart = "event: message_start\ndata: {\"type\":\"message_start\",\"message\":{\"id\":\"r\",\"model\":\"claude-up\",\"usage\":{\"input_tokens\":3,\"cache_read_input_tokens\":2,\"output_tokens\":1}}}\n\n"
	usageRest  = "event: content_block_start\ndata: {\"type\":\"content_block_start\",\"index\":0,\"content_block\":{\"type\":\"text\",\"text\":\"\"}}\n\n" +
		"event: content_block_delta\ndata: {\"type\":\"content_block_delta\",\"index\":0,\"delta\":{\"type\":\"text_delta\",\"text\":\"hi\"}}\n\n" +
		"event: content_block_stop\ndata: {\"type\":\"content_block_stop\",\"index\":0}\n\n" +
		"event: message_delta\ndata: {\"type\":\"message_delta\",\"delta\":{\"stop_reason\":\"end_turn\"},\"usage\":{\"output_tokens\":7}}\n\n" +
		"event: message_stop\ndata: {\"type\":\"message_stop\"}\n\n"
)

// usageLog records OnUsage calls.
type usageLog struct {
	mu    sync.Mutex
	calls []usageCall
}

type usageCall struct {
	endpoint string
	usage    apix.Usage
}

func (l *usageLog) record(a gateway.Attempt, u apix.Usage) {
	l.mu.Lock()
	defer l.mu.Unlock()
	u.Raw = nil
	l.calls = append(l.calls, usageCall{a.Endpoint, u})
}

func (l *usageLog) get() []usageCall {
	l.mu.Lock()
	defer l.mu.Unlock()
	return append([]usageCall(nil), l.calls...)
}

func TestUsageOrdinaryResponse(t *testing.T) {
	msgs := newUpstream(t, respond(500, "application/json", `{"error":{"message":"down"}}`))
	chat := newUpstream(t, respond(200, "application/json", chatReply))
	var log usageLog
	g := newGateway(t, gateway.Config{Upstreams: []gateway.Upstream{messagesUpstream(msgs), chatUpstream(chat)}, OnUsage: log.record})
	resp, err := do(t, g, clientRequest)
	if err != nil {
		t.Fatal(err)
	}
	want := apix.Usage{InputTokens: 1, OutputTokens: 1, TotalTokens: 2}
	// Only the committed attempt reports usage, and before the body is read.
	if calls := log.get(); len(calls) != 1 || calls[0].endpoint != "chat" || !reflect.DeepEqual(calls[0].usage, want) {
		t.Fatalf("calls %+v", calls)
	}
	u, ok := resp.Usage()
	if !ok || string(u.Raw) != `{"prompt_tokens":1,"completion_tokens":1}` {
		t.Fatalf("usage %+v %v", u, ok)
	}
	readAll(t, resp)
	if len(log.get()) != 1 {
		t.Fatal("reported twice")
	}
}

func TestUsageStream(t *testing.T) {
	msgs := newUpstream(t, respond(200, "text/event-stream", usageStart+usageRest))
	var log usageLog
	g := newGateway(t, gateway.Config{Upstreams: []gateway.Upstream{messagesUpstream(msgs)}, OnUsage: log.record})
	resp, err := do(t, g, clientStream)
	if err != nil {
		t.Fatal(err)
	}
	if calls := log.get(); len(calls) != 0 {
		t.Fatalf("reported before the stream ended: %+v", calls)
	}
	readAll(t, resp)
	want := apix.Usage{InputTokens: 5, OutputTokens: 7, TotalTokens: 12, CacheReadTokens: 2}
	if calls := log.get(); len(calls) != 1 || calls[0].endpoint != "messages" || !reflect.DeepEqual(calls[0].usage, want) {
		t.Fatalf("calls %+v", calls)
	}
	if u, ok := resp.Usage(); !ok || u.OutputTokens != 7 {
		t.Fatalf("usage %+v %v", u, ok)
	}
}

func TestUsageStreamClosedEarly(t *testing.T) {
	msgs := newUpstream(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = io.WriteString(w, usageStart)
		w.(http.Flusher).Flush()
		<-r.Context().Done()
	})
	var log usageLog
	g := newGateway(t, gateway.Config{Upstreams: []gateway.Upstream{messagesUpstream(msgs)}, OnUsage: log.record})
	resp, err := do(t, g, clientStream)
	if err != nil {
		t.Fatal(err)
	}
	// Read the committed events, then abort while the next Read waits.
	buf := make([]byte, 1)
	if _, err := resp.Body.Read(buf); err != nil {
		t.Fatal(err)
	}
	done := make(chan struct{})
	go func() {
		defer close(done)
		_, _ = io.Copy(io.Discard, resp.Body)
	}()
	if err := resp.Body.Close(); err != nil {
		t.Fatal(err)
	}
	<-done
	_ = resp.Body.Close()
	want := apix.Usage{InputTokens: 5, OutputTokens: 1, TotalTokens: 6, CacheReadTokens: 2}
	if calls := log.get(); len(calls) != 1 || !reflect.DeepEqual(calls[0].usage, want) {
		t.Fatalf("calls %+v", calls)
	}
}

func TestUsageStreamFailureAfterCommit(t *testing.T) {
	msgs := newUpstream(t, respond(200, "text/event-stream", usageStart)) // no terminal event
	var log usageLog
	g := newGateway(t, gateway.Config{Upstreams: []gateway.Upstream{messagesUpstream(msgs)}, OnUsage: log.record})
	resp, err := do(t, g, clientStream)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := io.ReadAll(resp.Body); err == nil {
		t.Fatal("stream without a terminal event succeeded")
	}
	_ = resp.Body.Close()
	if calls := log.get(); len(calls) != 1 || calls[0].usage.InputTokens != 5 {
		t.Fatalf("calls %+v", calls)
	}
}

func TestUsageNotReported(t *testing.T) {
	msgs := newUpstream(t, respond(200, "application/json", strings.Replace(messagesReply, `,"usage":{"input_tokens":1,"output_tokens":1}`, "", 1)))
	var log usageLog
	g := newGateway(t, gateway.Config{Upstreams: []gateway.Upstream{messagesUpstream(msgs)}, OnUsage: log.record})
	resp, err := do(t, g, clientRequest)
	if err != nil {
		t.Fatal(err)
	}
	readAll(t, resp)
	if _, ok := resp.Usage(); ok || len(log.get()) != 0 {
		t.Fatalf("calls %+v", log.get())
	}
}

func TestUsageHandlerWithChatStreamUsage(t *testing.T) {
	chat := newUpstream(t, respond(200, "text/event-stream", strings.Replace(chatStream, "data: [DONE]",
		"data: {\"choices\":[],\"usage\":{\"prompt_tokens\":4,\"completion_tokens\":2,\"total_tokens\":6}}\n\ndata: [DONE]", 1)))
	up := chatUpstream(chat)
	up.Options.Ext = openai.ChatCompletionsOptions{StreamUsage: true}
	var log usageLog
	g := newGateway(t, gateway.Config{Upstreams: []gateway.Upstream{up}, OnUsage: log.record})
	rec := httptest.NewRecorder()
	g.Handler(anthropic.Messages).ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/v1/messages", bytes.NewReader([]byte(clientStream))))
	if rec.Code != 200 {
		t.Fatalf("status %d: %s", rec.Code, rec.Body)
	}
	if opts, _ := chat.last().body["stream_options"].(map[string]any); opts["include_usage"] != true {
		t.Fatalf("upstream request %+v", chat.last().body)
	}
	want := apix.Usage{InputTokens: 4, OutputTokens: 2, TotalTokens: 6}
	if calls := log.get(); len(calls) != 1 || calls[0].endpoint != "chat" || !reflect.DeepEqual(calls[0].usage, want) {
		t.Fatalf("calls %+v", calls)
	}
}

// The callback may close the body it reports on.
func TestUsageCallbackMayCloseBody(t *testing.T) {
	msgs := newUpstream(t, respond(200, "text/event-stream", usageStart+usageRest))
	var body io.Closer
	called := make(chan struct{})
	g := newGateway(t, gateway.Config{Upstreams: []gateway.Upstream{messagesUpstream(msgs)}, OnUsage: func(gateway.Attempt, apix.Usage) {
		_ = body.Close()
		close(called)
	}})
	resp, err := do(t, g, clientStream)
	if err != nil {
		t.Fatal(err)
	}
	body = resp.Body
	readAll(t, resp)
	<-called
}
