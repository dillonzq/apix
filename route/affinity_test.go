package route_test

import (
	"slices"
	"testing"
	"time"

	"github.com/dillonzq/apix"
	"github.com/dillonzq/apix/protocol/anthropic"
	"github.com/dillonzq/apix/protocol/openai"
	"github.com/dillonzq/apix/route"
)

func responsesRequest(affinity []string, stateful bool) apix.RequestInfo {
	return apix.RequestInfo{Kind: apix.KindLLM, Protocol: openai.Responses, Model: "m", Affinity: affinity, Stateful: stateful}
}

func TestAffinityPinsOwner(t *testing.T) {
	now := time.Unix(1000, 0)
	r := newRouter(t, []route.Endpoint{
		{ID: "a", Protocol: openai.Responses, Priority: 0},
		{ID: "b", Protocol: openai.Responses, Priority: 1},
		{ID: "chat", Protocol: openai.ChatCompletions},
		{ID: "messages", Protocol: anthropic.Messages},
	}, route.WithHealth(&route.Breaker{Threshold: 1, Cooldown: time.Minute}), route.WithClock(func() time.Time { return now }))
	r.Remember("response:resp_1", "b")

	// Stateful requests stay on the client's protocol, owner first.
	cands, err := r.Route(responsesRequest([]string{"response:resp_1"}, true))
	if err != nil || !slices.Equal(ids(cands), []string{"b", "a"}) {
		t.Fatalf("stateful order %v %v", ids(cands), err)
	}
	// Affinity alone, such as a file, reorders without excluding conversions.
	r.Remember("file:f1", "b")
	cands, _ = r.Route(responsesRequest([]string{"file:unknown", "file:f1"}, false))
	if got := ids(cands); got[0] != "b" || len(got) != 4 {
		t.Fatalf("file affinity order %v", got)
	}
	// The owner of stateful context is still tried while unhealthy, because
	// no other endpoint can serve it.
	r.Report("b", route.Outcome{Err: apix.FromStatus(503, "down")})
	cands, _ = r.Route(responsesRequest([]string{"response:resp_1"}, true))
	if !slices.Equal(ids(cands), []string{"b", "a"}) || !r.Begin(cands[0]) {
		t.Fatalf("unhealthy owner skipped: %v", ids(cands))
	}
	cands, _ = r.Route(responsesRequest([]string{"file:f1"}, false))
	if slices.Contains(ids(cands), "b") {
		t.Fatalf("unhealthy owner of a file routed: %v", ids(cands))
	}
}

func TestStatefulNeedsNativeEndpoint(t *testing.T) {
	r := newRouter(t, []route.Endpoint{{ID: "chat", Protocol: openai.ChatCompletions}})
	_, err := r.Route(responsesRequest([]string{"response:resp_1"}, true))
	if err == nil || apix.AsError(err).Class != apix.ClassUnsupported {
		t.Fatalf("stateful request converted: %v", err)
	}
	if _, err := r.Route(responsesRequest(nil, false)); err != nil {
		t.Fatal(err)
	}
}

func TestAffinityDisabled(t *testing.T) {
	r := newRouter(t, []route.Endpoint{
		{ID: "a", Protocol: openai.Responses, Priority: 0},
		{ID: "b", Protocol: openai.Responses, Priority: 1},
	}, route.WithAffinity(nil))
	r.Remember("response:resp_1", "b")
	cands, _ := r.Route(responsesRequest([]string{"response:resp_1"}, true))
	if !slices.Equal(ids(cands), []string{"a", "b"}) {
		t.Fatalf("disabled affinity reordered: %v", ids(cands))
	}
}

func TestAffinityCacheEvictsLeastRecentlyUsed(t *testing.T) {
	c := &route.AffinityCache{Capacity: 2}
	c.Remember("k1", "a")
	c.Remember("k2", "b")
	if _, ok := c.Lookup("k1"); !ok {
		t.Fatal("k1 missing")
	}
	c.Remember("k3", "c") // evicts k2, the least recently used
	if _, ok := c.Lookup("k2"); ok {
		t.Fatal("k2 not evicted")
	}
	c.Remember("k1", "z")
	if id, ok := c.Lookup("k1"); !ok || id != "z" {
		t.Fatalf("k1 = %q %v", id, ok)
	}
	if id, ok := c.Lookup("k3"); !ok || id != "c" {
		t.Fatalf("k3 = %q %v", id, ok)
	}
}
