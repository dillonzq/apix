package gateway_test

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/dillonzq/apix"
	"github.com/dillonzq/apix/gateway"
	"github.com/dillonzq/apix/protocol/anthropic"
	"github.com/dillonzq/apix/protocol/gemini"
	"github.com/dillonzq/apix/protocol/openai"
	"github.com/dillonzq/apix/route"
)

func wildcard(id string, p apix.Protocol, priority int, u *upstream) gateway.Upstream {
	return gateway.Upstream{Endpoint: route.Endpoint{ID: id, Protocol: p, Priority: priority, Models: map[string]string{"*": ""}}, BaseURL: u.URL}
}

// A 404 for a model one upstream lacks fails over and never opens the breaker,
// so clients cannot push healthy endpoints out of rotation with bad names.
func TestUnknownModelFailsOverWithoutTrippingBreaker(t *testing.T) {
	a := newUpstream(t, respond(404, "application/json", `{"error":{"message":"model not found"}}`))
	b := newUpstream(t, respond(200, "application/json", messagesReply))
	g := newGateway(t, gateway.Config{Upstreams: []gateway.Upstream{wildcard("a", anthropic.Messages, 0, a), wildcard("b", anthropic.Messages, 1, b)}})
	for range 5 {
		resp, err := do(t, g, `{"model":"no-such-model","max_tokens":1,"messages":[]}`)
		if err != nil || resp.Endpoint != "b" {
			t.Fatalf("404 did not fail over: %v", err)
		}
		readAll(t, resp)
	}
	cands, err := g.Router().Route(apix.RequestInfo{Protocol: anthropic.Messages, Model: "other"})
	if err != nil || cands[0].Endpoint.ID != "a" {
		t.Fatalf("404s opened the breaker: %v %v", cands, err)
	}
}

func TestMisconfiguredOptionsFailAtConfiguration(t *testing.T) {
	u := newUpstream(t, respond(200, "application/json", chatReply))
	bad := chatUpstream(u)
	bad.Options = route.EndpointOptions{Ext: anthropic.MessagesOptions{}}
	if _, err := gateway.New(gateway.Config{Registry: registry(t), Upstreams: []gateway.Upstream{bad}}); err == nil || !strings.Contains(err.Error(), "chat") {
		t.Fatalf("foreign options accepted: %v", err)
	}
	g := newGateway(t, gateway.Config{Upstreams: []gateway.Upstream{chatUpstream(u)}})
	if err := g.Update([]gateway.Upstream{bad}); err == nil {
		t.Fatal("update accepted foreign options")
	}
}

func TestRetryAfter(t *testing.T) {
	limited := newUpstream(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Retry-After", "120")
		respond(429, "application/json", `{"error":{"message":"slow down"}}`)(w, r)
	})
	g := newGateway(t, gateway.Config{Upstreams: []gateway.Upstream{messagesUpstream(limited)}})
	_, err := do(t, g, clientRequest)
	if err == nil || err.RetryAfter != 2*time.Minute {
		t.Fatalf("Retry-After not carried: %+v", err)
	}
	// The endpoint is open for the advertised time; the lone endpoint is still
	// tried because routing fails open, and Handler relays the delay.
	srv := httptest.NewServer(g.Handler(anthropic.Messages))
	defer srv.Close()
	resp, herr := http.Post(srv.URL, "application/json", strings.NewReader(clientRequest))
	if herr != nil {
		t.Fatal(herr)
	}
	resp.Body.Close()
	if resp.StatusCode != 429 || resp.Header.Get("Retry-After") != "120" {
		t.Fatalf("handler %d Retry-After %q", resp.StatusCode, resp.Header.Get("Retry-After"))
	}
}

func TestCommitTimeout(t *testing.T) {
	slow := newUpstream(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(200)
		w.(http.Flusher).Flush()
		select {
		case <-r.Context().Done():
		case <-time.After(5 * time.Second):
		}
	})
	// The fallback commits at once and then streams past the timeout.
	steady := newUpstream(t, func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		frames := strings.SplitAfter(chatStream, "\n\n")
		_, _ = io.WriteString(w, frames[0])
		w.(http.Flusher).Flush()
		time.Sleep(300 * time.Millisecond)
		_, _ = io.WriteString(w, strings.Join(frames[1:], ""))
	})
	g := newGateway(t, gateway.Config{CommitTimeout: 100 * time.Millisecond, Upstreams: []gateway.Upstream{messagesUpstream(slow), chatUpstream(steady)}})
	resp, err := do(t, g, clientStream)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(readAll(t, resp)), "message_stop") {
		t.Fatal("committed stream was cut off by the commit timeout")
	}
	first := apix.AsError(resp.Attempts[0].Err)
	if resp.Endpoint != "chat" || first.Class != apix.ClassNetwork || !first.Retryable || !strings.Contains(first.Message, "commit") {
		t.Fatalf("attempts %+v", resp.Attempts)
	}
}

func TestMaxResponseBytes(t *testing.T) {
	big := newUpstream(t, respond(200, "application/json", `{"pad":"`+strings.Repeat("x", 4096)+`"}`))
	chat := newUpstream(t, respond(200, "application/json", chatReply))
	g := newGateway(t, gateway.Config{MaxResponseBytes: 1024, Upstreams: []gateway.Upstream{messagesUpstream(big), chatUpstream(chat)}})
	resp, err := do(t, g, clientRequest)
	if err != nil || resp.Endpoint != "chat" || !strings.Contains(apix.AsError(resp.Attempts[0].Err).Message, "exceeds") {
		t.Fatalf("oversized response: %v %+v", err, resp)
	}
	readAll(t, resp)
}

func TestForwardHeaders(t *testing.T) {
	u := newUpstream(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("X-Request-Id", "req-1")
		w.Header().Set("X-Ratelimit-Remaining-Requests", "9")
		w.Header().Set("X-Internal", "secret")
		respond(200, "application/json", messagesReply)(w, r)
	})
	g := newGateway(t, gateway.Config{ForwardHeaders: []string{"x-request-id", "x-ratelimit-*", "Content-Length", "Content-Type"}, Upstreams: []gateway.Upstream{messagesUpstream(u)}})
	resp, err := do(t, g, clientRequest)
	if err != nil {
		t.Fatal(err)
	}
	body := readAll(t, resp)
	h := resp.Header
	if h.Get("X-Request-Id") != "req-1" || h.Get("X-Ratelimit-Remaining-Requests") != "9" || h.Get("X-Internal") != "" || h.Get("Content-Length") != "" {
		t.Fatalf("forwarded %v", h)
	}
	if h.Get("Content-Type") != "application/json" || resp.UpstreamHeader.Get("X-Internal") != "secret" || len(body) == 0 {
		t.Fatalf("headers %v upstream %v", h, resp.UpstreamHeader)
	}
}

func TestPathOverrideNeedsStaticPath(t *testing.T) {
	reg, err := apix.NewRegistry(slices.Concat(openai.Codecs(), gemini.Codecs())...)
	if err != nil {
		t.Fatal(err)
	}
	g := gateway.Upstream{Endpoint: route.Endpoint{ID: "g", Protocol: gemini.GenerateContent}, BaseURL: "https://example.com", Path: "/v1/custom"}
	if _, err := gateway.New(gateway.Config{Registry: reg, Upstreams: []gateway.Upstream{g}}); err == nil {
		t.Fatal("static Path accepted for a model-dependent path")
	}
	c := gateway.Upstream{Endpoint: route.Endpoint{ID: "c", Protocol: openai.ChatCompletions}, BaseURL: "https://example.com", Path: "/openai/deployments/x/chat/completions"}
	if _, err := gateway.New(gateway.Config{Registry: reg, Upstreams: []gateway.Upstream{c}}); err != nil {
		t.Fatal(err)
	}
}

// A follow-up that refers to a stored response reaches the endpoint that
// stored it, even when routing would otherwise prefer another.
func TestStoredResponseAffinity(t *testing.T) {
	const reply = `{"id":"resp_b","object":"response","model":"m","status":"completed","output":[{"type":"message","role":"assistant","content":[{"type":"output_text","text":"hi"}]}],"usage":{"input_tokens":1,"output_tokens":1}}`
	var down atomic.Bool
	down.Store(true)
	a := newUpstream(t, func(w http.ResponseWriter, r *http.Request) {
		if down.Load() {
			respond(503, "application/json", `{"error":{"message":"down"}}`)(w, r)
			return
		}
		respond(404, "application/json", `{"error":{"message":"previous response not found"}}`)(w, r)
	})
	b := newUpstream(t, respond(200, "application/json", reply))
	chat := newUpstream(t, respond(200, "application/json", chatReply))
	g := newGateway(t, gateway.Config{Upstreams: []gateway.Upstream{wildcard("a", openai.Responses, 0, a), wildcard("b", openai.Responses, 1, b), wildcard("chat", openai.ChatCompletions, 0, chat)}})
	send := func(body string) *gateway.Response {
		t.Helper()
		resp, err := g.Do(context.Background(), gateway.Request{Protocol: openai.Responses, Payload: apix.Payload{Body: []byte(body)}})
		if err != nil {
			t.Fatal(err)
		}
		readAll(t, resp)
		return resp
	}
	if resp := send(`{"model":"m","input":"hi"}`); resp.Endpoint != "b" {
		t.Fatalf("first turn served by %s", resp.Endpoint)
	}
	down.Store(false)
	resp := send(`{"model":"m","input":"again","previous_response_id":"resp_b"}`)
	if resp.Endpoint != "b" || len(resp.Attempts) != 1 || chat.hits() != 0 {
		t.Fatalf("follow-up attempts %+v", resp.Attempts)
	}
}

func TestReasoningSuffix(t *testing.T) {
	msgs := newUpstream(t, respond(200, "application/json", messagesReply))
	cfg := gateway.Config{ReasoningSuffix: true, Upstreams: []gateway.Upstream{messagesUpstream(msgs)}}
	g := newGateway(t, cfg)
	resp, err := g.Do(context.Background(), gateway.Request{Protocol: openai.ChatCompletions, Payload: apix.Payload{Body: []byte(`{"model":"fast(high)","messages":[{"role":"user","content":"hi"}],"reasoning_effort":"low"}`)}})
	if err != nil {
		t.Fatal(err)
	}
	readAll(t, resp)
	got := msgs.last().body
	cfgBody, _ := got["output_config"].(map[string]any)
	if got["model"] != "claude-up" || cfgBody["effort"] != "high" {
		t.Fatalf("upstream body %v", got)
	}
	cfg.ReasoningSuffix = false
	plain := newGateway(t, cfg)
	if _, err := plain.Do(context.Background(), gateway.Request{Protocol: openai.ChatCompletions, Payload: apix.Payload{Body: []byte(`{"model":"fast(high)","messages":[]}`)}}); apix.AsError(err).Class != apix.ClassInvalidModel {
		t.Fatalf("suffix parsed while disabled: %v", err)
	}
}

func TestRetryAfterForms(t *testing.T) {
	for _, tc := range []struct {
		name  string
		set   func(http.Header)
		check func(time.Duration) bool
	}{
		{"milliseconds", func(h http.Header) { h.Set("Retry-After-Ms", "1500") }, func(d time.Duration) bool { return d == 1500*time.Millisecond }},
		{"http date", func(h http.Header) {
			h.Set("Retry-After", time.Now().Add(90*time.Second).UTC().Format(http.TimeFormat))
		}, func(d time.Duration) bool { return d > time.Minute && d <= 90*time.Second }},
		{"past date", func(h http.Header) { h.Set("Retry-After", "Mon, 02 Jan 2006 15:04:05 GMT") }, func(d time.Duration) bool { return d == 0 }},
		{"zero", func(h http.Header) { h.Set("Retry-After", "0") }, func(d time.Duration) bool { return d == 0 }},
		{"garbage", func(h http.Header) { h.Set("Retry-After", "soon") }, func(d time.Duration) bool { return d == 0 }},
	} {
		u := newUpstream(t, func(w http.ResponseWriter, r *http.Request) {
			tc.set(w.Header())
			respond(503, "application/json", `{"error":{"message":"busy"}}`)(w, r)
		})
		g := newGateway(t, gateway.Config{Upstreams: []gateway.Upstream{messagesUpstream(u)}})
		if _, err := do(t, g, clientRequest); err == nil || !tc.check(err.RetryAfter) {
			t.Errorf("%s: RetryAfter %v", tc.name, err)
		}
	}
}
