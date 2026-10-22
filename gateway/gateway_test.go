package gateway_test

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/dillonzq/apix"
	"github.com/dillonzq/apix/gateway"
	"github.com/dillonzq/apix/protocol/anthropic"
	"github.com/dillonzq/apix/protocol/openai"
	"github.com/dillonzq/apix/route"
	"github.com/dillonzq/apix/sse"
)

const (
	messagesReply = `{"id":"r","type":"message","role":"assistant","model":"claude-up","content":[{"type":"text","text":"from messages"}],"stop_reason":"end_turn","usage":{"input_tokens":1,"output_tokens":1}}`
	chatReply     = `{"id":"r","object":"chat.completion","model":"gpt-up","choices":[{"index":0,"message":{"role":"assistant","content":"from chat"},"finish_reason":"stop"}],"usage":{"prompt_tokens":1,"completion_tokens":1}}`
	chatStream    = "data: {\"id\":\"r\",\"model\":\"gpt-up\",\"choices\":[{\"delta\":{\"content\":\"hello\"}}]}\n\ndata: {\"choices\":[{\"delta\":{},\"finish_reason\":\"stop\"}]}\n\ndata: [DONE]\n\n"
	messagesStart = "event: message_start\ndata: {\"type\":\"message_start\",\"message\":{\"id\":\"r\",\"model\":\"claude-up\"}}\n\n"
	clientRequest = `{"model":"fast","max_tokens":16,"messages":[{"role":"user","content":"hi"}]}`
	clientStream  = `{"model":"fast","max_tokens":16,"stream":true,"messages":[{"role":"user","content":"hi"}]}`
)

// upstream is a fake provider that records requests and answers with reply.
type upstream struct {
	*httptest.Server
	mu       sync.Mutex
	requests []recorded
	reply    func(w http.ResponseWriter, r *http.Request)
}

type recorded struct {
	path   string
	header http.Header
	body   map[string]any
}

func newUpstream(t *testing.T, reply func(w http.ResponseWriter, r *http.Request)) *upstream {
	u := &upstream{reply: reply}
	u.Server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body map[string]any
		raw, _ := io.ReadAll(r.Body)
		_ = json.Unmarshal(raw, &body)
		u.mu.Lock()
		u.requests = append(u.requests, recorded{path: r.URL.Path, header: r.Header.Clone(), body: body})
		u.mu.Unlock()
		u.reply(w, r)
	}))
	t.Cleanup(u.Close)
	return u
}

func (u *upstream) hits() int {
	u.mu.Lock()
	defer u.mu.Unlock()
	return len(u.requests)
}

func (u *upstream) last() recorded {
	u.mu.Lock()
	defer u.mu.Unlock()
	return u.requests[len(u.requests)-1]
}

func respond(status int, contentType, body string) func(http.ResponseWriter, *http.Request) {
	return func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", contentType)
		w.WriteHeader(status)
		_, _ = io.WriteString(w, body)
	}
}

func registry(t *testing.T) *apix.Registry {
	t.Helper()
	reg, err := apix.NewRegistry(slices.Concat(openai.Codecs(), anthropic.Codecs())...)
	if err != nil {
		t.Fatal(err)
	}
	return reg
}

func messagesUpstream(u *upstream) gateway.Upstream {
	return gateway.Upstream{
		Endpoint: route.Endpoint{ID: "messages", Provider: "anthropic", Protocol: anthropic.Messages, Priority: 5, Models: map[string]string{"fast": "claude-up"}},
		BaseURL:  u.URL, APIKey: "k-msg",
	}
}

func chatUpstream(u *upstream) gateway.Upstream {
	return gateway.Upstream{
		Endpoint: route.Endpoint{ID: "chat", Provider: "openai", Protocol: openai.ChatCompletions, Models: map[string]string{"fast": "gpt-up"}},
		BaseURL:  u.URL + "/", APIKey: "k-chat",
	}
}

func newGateway(t *testing.T, cfg gateway.Config) *gateway.Gateway {
	t.Helper()
	cfg.Registry = registry(t)
	g, err := gateway.New(cfg)
	if err != nil {
		t.Fatal(err)
	}
	return g
}

func do(t *testing.T, g *gateway.Gateway, body string) (*gateway.Response, *apix.Error) {
	t.Helper()
	return g.Do(context.Background(), gateway.Request{Protocol: anthropic.Messages, Payload: apix.Payload{Body: []byte(body)}})
}

func readAll(t *testing.T, resp *gateway.Response) []byte {
	t.Helper()
	defer resp.Body.Close()
	b, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

func messageText(t *testing.T, body []byte) string {
	t.Helper()
	var doc struct {
		Content []struct{ Text string }
	}
	if err := json.Unmarshal(body, &doc); err != nil || len(doc.Content) == 0 {
		t.Fatalf("not a Messages response: %s", body)
	}
	return doc.Content[0].Text
}

func TestNativeEndpointPreferred(t *testing.T) {
	msgs := newUpstream(t, respond(200, "application/json", messagesReply))
	chat := newUpstream(t, respond(200, "application/json", chatReply))
	up := messagesUpstream(msgs)
	up.Header = http.Header{"X-Operator": {"op"}}
	g := newGateway(t, gateway.Config{Upstreams: []gateway.Upstream{chatUpstream(chat), up}})

	resp, err := g.Do(context.Background(), gateway.Request{Protocol: anthropic.Messages, Payload: apix.Payload{Body: []byte(clientRequest)}, Header: http.Header{"Anthropic-Beta": {"b1"}}})
	if err != nil {
		t.Fatal(err)
	}
	if got := messageText(t, readAll(t, resp)); got != "from messages" {
		t.Fatalf("text %q", got)
	}
	if chat.hits() != 0 || msgs.hits() != 1 {
		t.Fatalf("hits chat=%d messages=%d", chat.hits(), msgs.hits())
	}
	req := msgs.last()
	if req.path != "/v1/messages" || req.header.Get("X-Api-Key") != "k-msg" || req.header.Get("Anthropic-Version") == "" || req.header.Get("Authorization") != "" {
		t.Fatalf("request %s %v", req.path, req.header)
	}
	if req.header.Get("Anthropic-Beta") != "b1" || req.header.Get("X-Operator") != "op" {
		t.Fatalf("extra headers %v", req.header)
	}
	if req.body["model"] != "claude-up" {
		t.Fatalf("model %v", req.body["model"])
	}
	if resp.Endpoint != "messages" || len(resp.Attempts) != 1 || !resp.Attempts[0].Native || resp.Attempts[0].Status != 200 {
		t.Fatalf("response %+v", resp)
	}
	if resp.Header.Get("Content-Type") != "application/json" {
		t.Fatalf("content type %q", resp.Header.Get("Content-Type"))
	}
}

func TestFailoverToConvertibleProtocol(t *testing.T) {
	for _, status := range []int{429, 500, 503} {
		t.Run(http.StatusText(status), func(t *testing.T) {
			msgs := newUpstream(t, respond(status, "application/json", `{"type":"error","error":{"type":"overloaded_error","message":"busy"}}`))
			chat := newUpstream(t, respond(200, "application/json", chatReply))
			var seen []gateway.Attempt
			g := newGateway(t, gateway.Config{
				Upstreams: []gateway.Upstream{messagesUpstream(msgs), chatUpstream(chat)},
				OnAttempt: func(a gateway.Attempt) { seen = append(seen, a) },
			})
			resp, err := do(t, g, clientRequest)
			if err != nil {
				t.Fatal(err)
			}
			if got := messageText(t, readAll(t, resp)); got != "from chat" {
				t.Fatalf("text %q", got)
			}
			req := chat.last()
			if req.path != "/v1/chat/completions" || req.header.Get("Authorization") != "Bearer k-chat" || req.body["model"] != "gpt-up" {
				t.Fatalf("chat request %s %v %v", req.path, req.header, req.body)
			}
			if len(resp.Attempts) != 2 || resp.Attempts[0].Status != status || resp.Attempts[0].Err == nil || !resp.Attempts[0].Err.Retryable || resp.Attempts[1].Native {
				t.Fatalf("attempts %+v", resp.Attempts)
			}
			if len(seen) != 2 || seen[1].Endpoint != "chat" {
				t.Fatalf("OnAttempt %+v", seen)
			}
		})
	}
}

func TestNoFailoverOnClientError(t *testing.T) {
	msgs := newUpstream(t, respond(400, "application/json", `{"type":"error","error":{"type":"invalid_request_error","message":"bad field"}}`))
	chat := newUpstream(t, respond(200, "application/json", chatReply))
	g := newGateway(t, gateway.Config{Upstreams: []gateway.Upstream{messagesUpstream(msgs), chatUpstream(chat)}})
	_, err := do(t, g, clientRequest)
	if err == nil || err.Retryable || apix.HTTPStatus(err) != 400 || !strings.Contains(err.Message, "bad field") {
		t.Fatalf("err %v", err)
	}
	if chat.hits() != 0 {
		t.Fatal("failed over on a client error")
	}
}

func TestNetworkFailover(t *testing.T) {
	dead := newUpstream(t, nil)
	dead.Close()
	chat := newUpstream(t, respond(200, "application/json", chatReply))
	g := newGateway(t, gateway.Config{Upstreams: []gateway.Upstream{messagesUpstream(dead), chatUpstream(chat)}})
	resp, err := do(t, g, clientRequest)
	if err != nil {
		t.Fatal(err)
	}
	if resp.Attempts[0].Err.Class != apix.ClassNetwork || resp.Attempts[0].Status != 0 || resp.Endpoint != "chat" {
		t.Fatalf("attempts %+v", resp.Attempts)
	}
}

func TestConversionRejectionTriesNextCandidate(t *testing.T) {
	strict := newUpstream(t, respond(200, "application/json", messagesReply))
	lenient := newUpstream(t, respond(200, "application/json", messagesReply))
	safe := messagesUpstream(strict)
	safe.ID, safe.Priority, safe.Options = "safe", 0, apix.ExchangeOptions{LossPolicy: apix.LossPolicySafe}
	allow := messagesUpstream(lenient)
	allow.ID, allow.Priority = "allow", 1
	g := newGateway(t, gateway.Config{Upstreams: []gateway.Upstream{safe, allow}})
	resp, err := g.Do(context.Background(), gateway.Request{Protocol: openai.ChatCompletions, Payload: apix.Payload{Body: []byte(`{"model":"fast","messages":[{"role":"user","content":"hi"}],"seed":42}`)}})
	if err != nil {
		t.Fatal(err)
	}
	if strict.hits() != 0 || lenient.hits() != 1 || resp.Attempts[0].Status != 0 || resp.Attempts[0].Err.Class != apix.ClassUnsupported {
		t.Fatalf("attempts %+v", resp.Attempts)
	}
	if len(resp.Diagnostics()) == 0 {
		t.Fatal("allowed loss left no diagnostic")
	}
	var doc struct{ Object string }
	if json.Unmarshal(readAll(t, resp), &doc) != nil || doc.Object != "chat.completion" {
		t.Fatal("response not converted to Chat Completions")
	}
}

func decodeEvents(t *testing.T, wire []byte) []sse.Event {
	t.Helper()
	var d sse.Decoder
	return append(d.Feed(wire), d.Finish()...)
}

func eventNames(events []sse.Event) []string {
	var names []string
	for _, e := range events {
		names = append(names, e.Event)
	}
	return names
}

func TestStreamFailoverBeforeFirstEvent(t *testing.T) {
	msgs := newUpstream(t, respond(200, "text/event-stream", "")) // ends without output
	chat := newUpstream(t, respond(200, "text/event-stream", chatStream))
	g := newGateway(t, gateway.Config{Upstreams: []gateway.Upstream{messagesUpstream(msgs), chatUpstream(chat)}})
	resp, err := do(t, g, clientStream)
	if err != nil {
		t.Fatal(err)
	}
	if !resp.Stream || resp.Header.Get("Content-Type") != "text/event-stream" || resp.Endpoint != "chat" {
		t.Fatalf("response %+v", resp)
	}
	if chat.last().header.Get("Accept") != "text/event-stream" {
		t.Fatal("stream request did not ask for SSE")
	}
	events := decodeEvents(t, readAll(t, resp))
	names := eventNames(events)
	if names[0] != "message_start" || names[len(names)-1] != "message_stop" || !bytes.Contains(bytes.Join([][]byte{events[2].Data}, nil), []byte("hello")) {
		t.Fatalf("events %v", names)
	}
	if resp.Attempts[0].Err == nil || !resp.Attempts[0].Err.Retryable {
		t.Fatalf("first attempt %+v", resp.Attempts[0])
	}
}

func TestStreamFailureAfterCommitDoesNotRetry(t *testing.T) {
	msgs := newUpstream(t, func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = io.WriteString(w, messagesStart)
		w.(http.Flusher).Flush()
		// The stream ends without a terminal event.
	})
	chat := newUpstream(t, respond(200, "text/event-stream", chatStream))
	g := newGateway(t, gateway.Config{Upstreams: []gateway.Upstream{messagesUpstream(msgs), chatUpstream(chat)}})
	resp, err := do(t, g, clientStream)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	got, rerr := io.ReadAll(resp.Body)
	var aerr *apix.Error
	if !errors.As(rerr, &aerr) || !strings.Contains(aerr.Message, "terminal") {
		t.Fatalf("read error %v", rerr)
	}
	if names := eventNames(decodeEvents(t, got)); len(names) == 0 || names[0] != "message_start" {
		t.Fatalf("committed events %v", names)
	}
	if chat.hits() != 0 {
		t.Fatal("retried after the commit point")
	}
}

type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

// dataAndErrorReader returns the final bytes together with EOF or a read error.
type dataAndErrorReader struct {
	*strings.Reader
	err error
}

func (r dataAndErrorReader) Read(p []byte) (int, error) {
	n, err := r.Reader.Read(p)
	if r.Len() == 0 {
		err = r.err
	}
	return n, err
}

func TestFirstStreamReadPreservesEventsBeforeError(t *testing.T) {
	for _, tc := range []struct {
		name, wire string
		readErr    error
		wantClass  apix.Class
	}{
		{"EOF", messagesStart, io.EOF, apix.ClassTranslation},
		{"network failure", messagesStart, io.ErrUnexpectedEOF, apix.ClassNetwork},
		{"malformed event", messagesStart + "event: message_stop\ndata: {broken\n\n", io.EOF, apix.ClassTranslation},
	} {
		t.Run(tc.name, func(t *testing.T) {
			hits := 0
			client := &http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
				hits++
				body := io.NopCloser(strings.NewReader(chatStream))
				if r.URL.Host == "primary.example" {
					body = io.NopCloser(dataAndErrorReader{strings.NewReader(tc.wire), tc.readErr})
				}
				return &http.Response{StatusCode: 200, Header: http.Header{"Content-Type": {"text/event-stream"}}, Body: body}, nil
			})}
			g := newGateway(t, gateway.Config{
				Client: client,
				Upstreams: []gateway.Upstream{
					{Endpoint: route.Endpoint{ID: "primary", Protocol: anthropic.Messages}, BaseURL: "https://primary.example"},
					{Endpoint: route.Endpoint{ID: "fallback", Protocol: openai.ChatCompletions}, BaseURL: "https://fallback.example"},
				},
				RouterOptions: []route.Option{route.WithHealth(&route.Breaker{Threshold: 1})},
			})
			resp, err := do(t, g, clientStream)
			if err != nil {
				t.Fatal(err)
			}
			defer resp.Body.Close()
			got, readErr := io.ReadAll(resp.Body)
			var aerr *apix.Error
			if !errors.As(readErr, &aerr) || aerr.Class != tc.wantClass || aerr.Retryable {
				t.Fatalf("stream error = %v; want non-retryable %s", readErr, tc.wantClass)
			}
			if string(got) != messagesStart || hits != 1 || resp.Endpoint != "primary" {
				t.Fatalf("lost output or retried: events %q, hits %d, endpoint %s", got, hits, resp.Endpoint)
			}
			if tc.wantClass == apix.ClassNetwork {
				candidates, err := g.Router().Route(apix.RequestInfo{Protocol: anthropic.Messages, Model: "fast"})
				if err != nil || len(candidates) != 1 || candidates[0].Endpoint.ID != "fallback" {
					t.Fatalf("stream failure did not update health: %v %v", candidates, err)
				}
			}
		})
	}
}

func TestBreakerSkipsFailingEndpoint(t *testing.T) {
	msgs := newUpstream(t, respond(500, "application/json", `{"type":"error","error":{"type":"api_error","message":"down"}}`))
	chat := newUpstream(t, respond(200, "application/json", chatReply))
	g := newGateway(t, gateway.Config{
		Upstreams:     []gateway.Upstream{messagesUpstream(msgs), chatUpstream(chat)},
		RouterOptions: []route.Option{route.WithHealth(&route.Breaker{Threshold: 2, Cooldown: time.Hour})},
	})
	for range 3 {
		resp, err := do(t, g, clientRequest)
		if err != nil {
			t.Fatal(err)
		}
		readAll(t, resp)
	}
	if msgs.hits() != 2 || chat.hits() != 3 {
		t.Fatalf("hits messages=%d chat=%d", msgs.hits(), chat.hits())
	}
}

func TestMaxAttempts(t *testing.T) {
	msgs := newUpstream(t, respond(500, "application/json", `{}`))
	chat := newUpstream(t, respond(200, "application/json", chatReply))
	g := newGateway(t, gateway.Config{Upstreams: []gateway.Upstream{messagesUpstream(msgs), chatUpstream(chat)}, MaxAttempts: 1})
	if _, err := do(t, g, clientRequest); err == nil || err.Class != apix.ClassUpstream {
		t.Fatalf("err %v", err)
	}
	if chat.hits() != 0 {
		t.Fatal("exceeded MaxAttempts")
	}
}

func TestContextCanceled(t *testing.T) {
	release := make(chan struct{})
	msgs := newUpstream(t, func(w http.ResponseWriter, r *http.Request) {
		select {
		case <-r.Context().Done():
		case <-release:
		}
	})
	defer close(release)
	chat := newUpstream(t, respond(200, "application/json", chatReply))
	g := newGateway(t, gateway.Config{Upstreams: []gateway.Upstream{messagesUpstream(msgs), chatUpstream(chat)}})
	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	_, err := g.Do(ctx, gateway.Request{Protocol: anthropic.Messages, Payload: apix.Payload{Body: []byte(clientRequest)}})
	if err == nil || err.Class != apix.ClassNetwork || err.Retryable {
		t.Fatalf("err %v", err)
	}
	if chat.hits() != 0 {
		t.Fatal("failed over after cancellation")
	}
}

// Hosts such as CPA pick the response format separately from the request
// format: here a Chat request is answered in Messages format from a Chat
// upstream.
func TestOutputProtocol(t *testing.T) {
	chat := newUpstream(t, func(w http.ResponseWriter, r *http.Request) {
		if strings.Contains(r.Header.Get("Accept"), "text/event-stream") {
			respond(200, "text/event-stream", chatStream)(w, r)
			return
		}
		respond(200, "application/json", chatReply)(w, r)
	})
	g := newGateway(t, gateway.Config{Upstreams: []gateway.Upstream{chatUpstream(chat)}})
	request := func(body string) gateway.Request {
		return gateway.Request{Protocol: openai.ChatCompletions, Output: anthropic.Messages, Payload: apix.Payload{Body: []byte(body)}}
	}
	resp, err := g.Do(context.Background(), request(`{"model":"fast","messages":[{"role":"user","content":"hi"}]}`))
	if err != nil {
		t.Fatal(err)
	}
	if got := messageText(t, readAll(t, resp)); got != "from chat" || !resp.Attempts[0].Native {
		t.Fatalf("text %q attempts %+v", got, resp.Attempts)
	}
	resp, err = g.Do(context.Background(), request(`{"model":"fast","stream":true,"messages":[{"role":"user","content":"hi"}]}`))
	if err != nil {
		t.Fatal(err)
	}
	if names := eventNames(decodeEvents(t, readAll(t, resp))); names[0] != "message_start" || names[len(names)-1] != "message_stop" {
		t.Fatalf("stream events %v", names)
	}
	bad := request(`{"model":"fast","messages":[]}`)
	bad.Output = "nope"
	if _, err := g.Do(context.Background(), bad); err == nil || err.Class != apix.ClassUnsupported || chat.hits() != 2 {
		t.Fatalf("unregistered output: %v, hits %d", err, chat.hits())
	}
}

func TestUpdate(t *testing.T) {
	msgs := newUpstream(t, respond(200, "application/json", messagesReply))
	chat := newUpstream(t, respond(200, "application/json", chatReply))
	g := newGateway(t, gateway.Config{Upstreams: []gateway.Upstream{messagesUpstream(msgs)}})
	if err := g.Update([]gateway.Upstream{chatUpstream(chat)}); err != nil {
		t.Fatal(err)
	}
	resp, err := do(t, g, clientRequest)
	if err != nil {
		t.Fatal(err)
	}
	if got := messageText(t, readAll(t, resp)); got != "from chat" || msgs.hits() != 0 {
		t.Fatalf("text %q messages hits %d", got, msgs.hits())
	}
	bad := chatUpstream(chat)
	bad.BaseURL = "relative/path"
	if err := g.Update([]gateway.Upstream{bad}); err == nil {
		t.Fatal("accepted a relative BaseURL")
	}
	if _, err := gateway.New(gateway.Config{}); err == nil {
		t.Fatal("accepted a nil registry")
	}
}

func TestRejectedUpdateKeepsUpstreams(t *testing.T) {
	msgs := newUpstream(t, respond(200, "application/json", messagesReply))
	chat := newUpstream(t, respond(200, "application/json", chatReply))
	for _, invalid := range []string{"missing ID", "duplicate ID", "unregistered protocol"} {
		t.Run(invalid, func(t *testing.T) {
			g := newGateway(t, gateway.Config{Upstreams: []gateway.Upstream{messagesUpstream(msgs)}})
			up := chatUpstream(chat)
			next := []gateway.Upstream{up}
			switch invalid {
			case "missing ID":
				next[0].ID = ""
			case "duplicate ID":
				next = append(next, up)
			case "unregistered protocol":
				next[0].Protocol = "unknown"
			}
			if err := g.Update(next); err == nil {
				t.Fatal("accepted invalid upstreams")
			}
			resp, err := do(t, g, clientRequest)
			if err != nil {
				t.Fatalf("rejected update broke the current configuration: %v", err)
			}
			if got := messageText(t, readAll(t, resp)); got != "from messages" {
				t.Fatalf("response %q", got)
			}
		})
	}
	if chat.hits() != 0 {
		t.Fatal("rejected upstream received a request")
	}
}

func TestUpdateDuringFailoverUsesOneConfiguration(t *testing.T) {
	msgs := newUpstream(t, respond(503, "application/json", `{"error":{"message":"down"}}`))
	chat := newUpstream(t, respond(200, "application/json", chatReply))
	replacement := newUpstream(t, respond(200, "application/json", messagesReply))
	next := messagesUpstream(replacement)
	next.ID = "chat" // Reuse an ID while changing its protocol, URL, and credentials.
	var g *gateway.Gateway
	g = newGateway(t, gateway.Config{
		Upstreams: []gateway.Upstream{messagesUpstream(msgs), chatUpstream(chat)},
		OnAttempt: func(a gateway.Attempt) {
			if a.Endpoint == "messages" {
				if err := g.Update([]gateway.Upstream{next}); err != nil {
					t.Fatal(err)
				}
			}
		},
	})
	resp, err := do(t, g, clientRequest)
	if err != nil {
		t.Fatal(err)
	}
	if got := messageText(t, readAll(t, resp)); got != "from chat" || replacement.hits() != 0 {
		t.Fatalf("in-flight request mixed configurations: response %q, replacement hits %d", got, replacement.hits())
	}
	resp, err = do(t, g, clientRequest)
	if err != nil {
		t.Fatal(err)
	}
	if got := messageText(t, readAll(t, resp)); got != "from messages" || replacement.last().path != "/v1/messages" {
		t.Fatalf("next request did not use the new configuration: %q", got)
	}
}

func TestUpstreamAuthIsCopied(t *testing.T) {
	for _, update := range []bool{false, true} {
		t.Run(map[bool]string{false: "New", true: "Update"}[update], func(t *testing.T) {
			msgs := newUpstream(t, respond(200, "application/json", messagesReply))
			auth := &apix.AuthScheme{Header: "X-Custom-Key", Prefix: "Key "}
			up := messagesUpstream(msgs)
			up.Auth = auth
			var g *gateway.Gateway
			if update {
				g = newGateway(t, gateway.Config{})
				if err := g.Update([]gateway.Upstream{up}); err != nil {
					t.Fatal(err)
				}
			} else {
				g = newGateway(t, gateway.Config{Upstreams: []gateway.Upstream{up}})
			}
			auth.Header, auth.Prefix = "X-Changed-Key", "Changed "
			resp, err := do(t, g, clientRequest)
			if err != nil {
				t.Fatal(err)
			}
			readAll(t, resp)
			if got := msgs.last().header.Get("X-Custom-Key"); got != "Key k-msg" {
				t.Fatalf("auth changed through caller-owned config: %q", got)
			}
		})
	}
}
