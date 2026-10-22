package gateway_test

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/dillonzq/apix/gateway"
	"github.com/dillonzq/apix/protocol/anthropic"
)

func TestHandler(t *testing.T) {
	chat := newUpstream(t, func(w http.ResponseWriter, r *http.Request) {
		if strings.Contains(r.Header.Get("Accept"), "text/event-stream") {
			respond(200, "text/event-stream", chatStream)(w, r)
			return
		}
		respond(200, "application/json", chatReply)(w, r)
	})
	g := newGateway(t, gateway.Config{Upstreams: []gateway.Upstream{chatUpstream(chat)}})
	srv := httptest.NewServer(g.Handler(anthropic.Messages))
	t.Cleanup(srv.Close)

	post := func(body string) (*http.Response, []byte) {
		t.Helper()
		resp, err := http.Post(srv.URL, "application/json", strings.NewReader(body))
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		b, _ := io.ReadAll(resp.Body)
		return resp, b
	}

	resp, body := post(clientRequest)
	if resp.StatusCode != 200 || messageText(t, body) != "from chat" {
		t.Fatalf("%d %s", resp.StatusCode, body)
	}

	resp, body = post(clientStream)
	if resp.StatusCode != 200 || resp.Header.Get("Content-Type") != "text/event-stream" {
		t.Fatalf("stream %d %v", resp.StatusCode, resp.Header)
	}
	if names := eventNames(decodeEvents(t, body)); names[len(names)-1] != "message_stop" {
		t.Fatalf("stream events %v", names)
	}

	resp, body = post(`{"model":"unknown","max_tokens":1,"messages":[]}`)
	var envelope struct {
		Type  string
		Error struct{ Type, Message string }
	}
	if resp.StatusCode != 404 || json.Unmarshal(body, &envelope) != nil || envelope.Type != "error" || envelope.Error.Type != "not_found_error" {
		t.Fatalf("unknown model %d %s", resp.StatusCode, body)
	}

	resp, body = post(`not json`)
	if resp.StatusCode != 400 || json.Unmarshal(body, &envelope) != nil || envelope.Error.Type != "invalid_request_error" {
		t.Fatalf("malformed %d %s", resp.StatusCode, body)
	}

	get, err := http.Get(srv.URL)
	if err != nil {
		t.Fatal(err)
	}
	get.Body.Close()
	if get.StatusCode != http.StatusMethodNotAllowed || get.Header.Get("Allow") != "POST" {
		t.Fatalf("GET %d", get.StatusCode)
	}
}

func TestHandlerRequestLimit(t *testing.T) {
	chat := newUpstream(t, respond(200, "application/json", chatReply))
	g := newGateway(t, gateway.Config{Upstreams: []gateway.Upstream{chatUpstream(chat)}, MaxRequestBytes: 16})
	srv := httptest.NewServer(g.Handler(anthropic.Messages))
	t.Cleanup(srv.Close)
	resp, err := http.Post(srv.URL, "application/json", strings.NewReader(clientRequest))
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusRequestEntityTooLarge || chat.hits() != 0 {
		t.Fatalf("status %d hits %d", resp.StatusCode, chat.hits())
	}
}
