package gateway_test

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"testing"

	"github.com/dillonzq/apix"
	"github.com/dillonzq/apix/gateway"
	"github.com/dillonzq/apix/protocol/anthropic"
	"github.com/dillonzq/apix/protocol/gemini"
	"github.com/dillonzq/apix/protocol/openai"
	"github.com/dillonzq/apix/route"
)

func TestGeminiHandlerAndFailover(t *testing.T) {
	reg, e := apix.NewRegistry(slices.Concat(openai.Codecs(), anthropic.Codecs(), gemini.Codecs())...)
	if e != nil {
		t.Fatal(e)
	}
	for _, fail := range []bool{false, true} {
		t.Run(map[bool]string{false: "native", true: "failover"}[fail], func(t *testing.T) {
			body := `{"contents":[{"role":"user","parts":[{"text":"hello"}]}]}`
			gs := newUpstream(t, func(w http.ResponseWriter, r *http.Request) {
				if fail {
					respond(503, "application/json", `{"error":{"code":503,"status":"UNAVAILABLE","message":"retry"}}`)(w, r)
					return
				}
				respond(200, "text/event-stream", "data: {\"responseId\":\"r\",\"candidates\":[{\"content\":{\"parts\":[{\"text\":\"answer\"}]},\"finishReason\":\"STOP\"}]}\n\n")(w, r)
			})
			chat := newUpstream(t, respond(200, "text/event-stream", chatStream))
			gw, e := gateway.New(gateway.Config{Registry: reg, Upstreams: []gateway.Upstream{{Endpoint: route.Endpoint{ID: "gemini", Protocol: gemini.GenerateContent, Priority: 50, Models: map[string]string{"public": "gem-up"}}, BaseURL: gs.URL, APIKey: "gem-key"}, {Endpoint: route.Endpoint{ID: "chat", Protocol: openai.ChatCompletions, Models: map[string]string{"public": "chat-up"}}, BaseURL: chat.URL}}})
			if e != nil {
				t.Fatal(e)
			}
			srv := httptest.NewServer(gw.Handler(gemini.GenerateContent))
			defer srv.Close()
			resp, e := http.Post(srv.URL+"/v1beta/models/public:streamGenerateContent", "application/json", strings.NewReader(body))
			if e != nil {
				t.Fatal(e)
			}
			defer resp.Body.Close()
			result, e := io.ReadAll(resp.Body)
			if e != nil {
				t.Fatal(e)
			}
			if resp.StatusCode != 200 || resp.Header.Get("Content-Type") != "text/event-stream" || !bytes.Contains(result, []byte("finishReason")) {
				t.Fatalf("%d %s", resp.StatusCode, result)
			}
			got := gs.last()
			if got.path != "/v1beta/models/gem-up:streamGenerateContent" || got.header.Get("X-Goog-Api-Key") != "gem-key" {
				t.Fatalf("request %+v", got)
			}
			if got.body["model"] != nil || got.body["stream"] != nil {
				t.Fatalf("transport metadata leaked to body: %v", got.body)
			}
			if fail {
				if chat.last().body["model"] != "chat-up" || chat.last().body["stream"] != true {
					t.Fatal("metadata lost during cross-protocol failover")
				}
			} else if chat.hits() != 0 {
				t.Fatal("native endpoint not preferred")
			}
		})
	}
}
func TestInteractionsHandlerPreservesState(t *testing.T) {
	reg, _ := apix.NewRegistry(gemini.Codecs()...)
	var received map[string]json.RawMessage
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1beta/interactions" || r.Header.Get("X-Goog-Api-Key") != "key" {
			t.Error("wrong route or auth")
		}
		json.NewDecoder(r.Body).Decode(&received)
		w.Header().Set("Content-Type", "application/json")
		io.WriteString(w, `{"id":"i","status":"in_progress","steps":[],"background":true}`)
	}))
	defer srv.Close()
	gw, e := gateway.New(gateway.Config{Registry: reg, Upstreams: []gateway.Upstream{{Endpoint: route.Endpoint{ID: "interactions", Protocol: gemini.Interactions}, BaseURL: srv.URL, APIKey: "key"}}})
	if e != nil {
		t.Fatal(e)
	}
	rr := httptest.NewRecorder()
	gw.Handler(gemini.Interactions).ServeHTTP(rr, httptest.NewRequest("POST", "/v1beta/interactions", strings.NewReader(`{"model":"m","input":"hello","previous_interaction_id":"prior","background":true}`)))
	if rr.Code != 200 || string(received["previous_interaction_id"]) != `"prior"` || string(received["background"]) != "true" {
		t.Fatalf("%d %s %v", rr.Code, rr.Body.String(), received)
	}
}
