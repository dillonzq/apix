package route_test

import (
	"math/rand/v2"
	"slices"
	"sync"
	"testing"
	"time"

	"github.com/dillonzq/apix"
	"github.com/dillonzq/apix/protocol/anthropic"
	"github.com/dillonzq/apix/protocol/openai"
	"github.com/dillonzq/apix/route"
)

// imageCodec registers an image protocol so Kind filtering can be tested.
type imageCodec struct{}

func (imageCodec) Spec() apix.ProtocolSpec {
	return apix.ProtocolSpec{ID: "test.images", Kind: apix.KindImage}
}
func (imageCodec) Inspect(apix.Payload) (apix.RequestInfo, *apix.Error) {
	return apix.RequestInfo{}, nil
}
func (imageCodec) EncodeError(*apix.Error) apix.Payload { return apix.Payload{} }
func (imageCodec) Accepts(apix.Protocol) bool           { return true }
func (imageCodec) NewExchange(apix.Protocol, apix.ExchangeOptions) (apix.Exchange, *apix.Error) {
	return nil, nil
}

func registry(t *testing.T) *apix.Registry {
	t.Helper()
	reg, err := apix.NewRegistry(slices.Concat(openai.Codecs(), anthropic.Codecs(), []apix.Codec{imageCodec{}})...)
	if err != nil {
		t.Fatal(err)
	}
	return reg
}

func newRouter(t *testing.T, endpoints []route.Endpoint, opts ...route.Option) *route.Router {
	t.Helper()
	r, err := route.New(registry(t), endpoints, append([]route.Option{route.WithRand(rand.New(rand.NewPCG(1, 2)))}, opts...)...)
	if err != nil {
		t.Fatal(err)
	}
	return r
}

func ids(cands []route.Candidate) []string {
	out := make([]string, len(cands))
	for i, c := range cands {
		out[i] = c.Endpoint.ID
	}
	return out
}

func messagesRequest(model string, features ...string) apix.RequestInfo {
	return apix.RequestInfo{Kind: apix.KindLLM, Protocol: anthropic.Messages, Model: model, Features: features, Stream: slices.Contains(features, apix.FeatureStream)}
}

func TestNativeFirstThenPriority(t *testing.T) {
	r := newRouter(t, []route.Endpoint{
		{ID: "chat-0", Protocol: openai.ChatCompletions, Priority: 0},
		{ID: "responses-1", Protocol: openai.Responses, Priority: 1},
		{ID: "messages-9", Protocol: anthropic.Messages, Priority: 9},
		{ID: "messages-2", Protocol: anthropic.Messages, Priority: 2},
		{ID: "images", Protocol: "test.images"},
	})
	cands, err := r.Route(messagesRequest("m"))
	if err != nil {
		t.Fatal(err)
	}
	if got := ids(cands); !slices.Equal(got, []string{"messages-2", "messages-9", "chat-0", "responses-1"}) {
		t.Fatalf("order %v", got)
	}
	if !cands[0].Native || !cands[1].Native || cands[2].Native || cands[3].Native {
		t.Fatalf("native flags %+v", cands)
	}
}

func TestModelMapping(t *testing.T) {
	r := newRouter(t, []route.Endpoint{
		{ID: "alias", Protocol: anthropic.Messages, Models: map[string]string{"fast": "claude-haiku", "keep": ""}},
		{ID: "wild", Protocol: openai.ChatCompletions, Models: map[string]string{"*": "gpt-default"}},
		{ID: "same", Protocol: openai.Responses, Models: map[string]string{"*": ""}},
		{ID: "any", Protocol: openai.Responses},
		{ID: "other", Protocol: anthropic.Messages, Models: map[string]string{"slow": "claude-opus"}},
	})
	cands, err := r.Route(messagesRequest("fast"))
	if err != nil {
		t.Fatal(err)
	}
	got := map[string]string{}
	for _, c := range cands {
		got[c.Endpoint.ID] = c.UpstreamModel
	}
	want := map[string]string{"alias": "claude-haiku", "wild": "gpt-default", "same": "fast", "any": "fast"}
	if len(got) != len(want) {
		t.Fatalf("candidates %v", got)
	}
	for id, model := range want {
		if got[id] != model {
			t.Fatalf("%s: %q, all %v", id, got[id], got)
		}
	}
	cands, _ = r.Route(messagesRequest("keep"))
	if cands[0].Endpoint.ID != "alias" || cands[0].UpstreamModel != "keep" {
		t.Fatalf("empty mapping should keep the model: %+v", cands[0])
	}
}

func TestFeaturesDisabledAndKind(t *testing.T) {
	r := newRouter(t, []route.Endpoint{
		{ID: "no-stream", Protocol: anthropic.Messages, Features: []string{apix.FeatureTools}},
		{ID: "off", Protocol: anthropic.Messages, Disabled: true},
		{ID: "full", Protocol: openai.ChatCompletions, Features: []string{apix.FeatureStream, apix.FeatureTools}},
		{ID: "any", Protocol: openai.Responses},
		{ID: "images", Protocol: "test.images"},
	})
	cands, err := r.Route(messagesRequest("m", apix.FeatureStream, apix.FeatureTools))
	if err != nil {
		t.Fatal(err)
	}
	if got := ids(cands); len(got) != 2 || !slices.Contains(got, "full") || !slices.Contains(got, "any") {
		t.Fatalf("candidates %v", got)
	}
	cands, _ = r.Route(messagesRequest("m", apix.FeatureTools))
	if cands[0].Endpoint.ID != "no-stream" {
		t.Fatalf("non-streaming request should use the native endpoint: %v", ids(cands))
	}
}

func TestRouteErrors(t *testing.T) {
	r := newRouter(t, []route.Endpoint{
		{ID: "images", Protocol: "test.images"},
		{ID: "mapped", Protocol: anthropic.Messages, Models: map[string]string{"known": "x"}, Features: []string{apix.FeatureTools}},
	})
	if _, err := r.Route(messagesRequest("unknown")); err == nil || err.Class != apix.ClassInvalidModel {
		t.Fatalf("unknown model: %v", err)
	}
	if _, err := r.Route(messagesRequest("known", apix.FeatureStream)); err == nil || err.Class != apix.ClassUnsupported {
		t.Fatalf("unsupported feature: %v", err)
	}
}

func TestWeightedShuffle(t *testing.T) {
	r := newRouter(t, []route.Endpoint{
		{ID: "heavy", Protocol: anthropic.Messages, Weight: 3},
		{ID: "light", Protocol: anthropic.Messages, Weight: 1},
		{ID: "fallback", Protocol: openai.ChatCompletions, Weight: 100},
	})
	first := map[string]int{}
	const n = 4000
	for range n {
		cands, err := r.Route(messagesRequest("m"))
		if err != nil {
			t.Fatal(err)
		}
		if cands[2].Endpoint.ID != "fallback" {
			t.Fatalf("weight crossed tiers: %v", ids(cands))
		}
		first[cands[0].Endpoint.ID]++
	}
	if ratio := float64(first["heavy"]) / n; ratio < 0.7 || ratio > 0.8 {
		t.Fatalf("heavy first %.3f of the time, want about 0.75", ratio)
	}
}

func TestBreaker(t *testing.T) {
	now := time.Unix(1000, 0)
	r := newRouter(t, []route.Endpoint{
		{ID: "primary", Protocol: anthropic.Messages},
		{ID: "backup", Protocol: openai.ChatCompletions},
	}, route.WithHealth(&route.Breaker{Threshold: 2, Cooldown: 10 * time.Second}), route.WithClock(func() time.Time { return now }))
	order := func() []string {
		cands, err := r.Route(messagesRequest("m"))
		if err != nil {
			t.Fatal(err)
		}
		return ids(cands)
	}
	fail := route.Outcome{Err: apix.FromStatus(503, "down")}

	r.Report("primary", route.Outcome{Err: apix.Translation("bad request")}) // not the endpoint's fault
	r.Report("primary", fail)
	if got := order(); !slices.Equal(got, []string{"primary", "backup"}) {
		t.Fatalf("opened below threshold: %v", got)
	}
	r.Report("primary", fail)
	if got := order(); !slices.Equal(got, []string{"backup"}) {
		t.Fatalf("open breaker still routed: %v", got)
	}
	now = now.Add(10 * time.Second)
	if got := order(); !slices.Equal(got, []string{"primary", "backup"}) {
		t.Fatalf("cooldown did not end: %v", got)
	}
	r.Report("primary", fail) // one failure after the cooldown reopens it
	if got := order(); !slices.Equal(got, []string{"backup"}) {
		t.Fatalf("half-open failure did not reopen: %v", got)
	}
	now = now.Add(10 * time.Second)
	r.Report("primary", route.Outcome{})
	r.Report("primary", fail)
	if got := order(); !slices.Equal(got, []string{"primary", "backup"}) {
		t.Fatalf("success did not reset: %v", got)
	}

	r.Report("primary", fail)
	r.Report("backup", fail)
	r.Report("backup", fail)
	if got := order(); !slices.Equal(got, []string{"primary", "backup"}) {
		t.Fatalf("all-open routing should fail open in normal order: %v", got)
	}
}

func TestUpdateValidation(t *testing.T) {
	r := newRouter(t, nil)
	for name, eps := range map[string][]route.Endpoint{
		"no id":     {{Protocol: anthropic.Messages}},
		"duplicate": {{ID: "a", Protocol: anthropic.Messages}, {ID: "a", Protocol: openai.Responses}},
		"protocol":  {{ID: "a", Protocol: "nope"}},
	} {
		if err := r.Update(eps); err == nil {
			t.Fatalf("%s: accepted", name)
		}
	}
	if _, err := route.New(nil, nil); err == nil {
		t.Fatal("nil registry accepted")
	}
	models := map[string]string{"a": "b"}
	if err := r.Update([]route.Endpoint{{ID: "a", Protocol: anthropic.Messages, Models: models}}); err != nil {
		t.Fatal(err)
	}
	models["a"] = "mutated"
	if got := r.Endpoints()[0].Models["a"]; got != "b" {
		t.Fatalf("router shares caller maps: %q", got)
	}
}

func TestConcurrentRouteUpdateReport(t *testing.T) {
	r := newRouter(t, []route.Endpoint{{ID: "a", Protocol: anthropic.Messages}})
	sets := [][]route.Endpoint{
		{{ID: "a", Protocol: anthropic.Messages}},
		{{ID: "b", Protocol: openai.ChatCompletions}, {ID: "a", Protocol: anthropic.Messages, Weight: 2}},
	}
	var wg sync.WaitGroup
	for i := range 8 {
		wg.Go(func() {
			for j := range 200 {
				switch (i + j) % 3 {
				case 0:
					if _, err := r.Route(messagesRequest("m")); err != nil {
						t.Error(err)
					}
				case 1:
					if err := r.Update(sets[j%2]); err != nil {
						t.Error(err)
					}
				default:
					r.Report("a", route.Outcome{Err: apix.FromStatus(500, "x")})
				}
			}
		})
	}
	wg.Wait()
}
