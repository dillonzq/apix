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

// Client-controlled failures never open the breaker: an unknown model behind a
// wildcard mapping is the request's fault, not the endpoint's.
func TestBreakerIgnoresRequestFaults(t *testing.T) {
	b := &route.Breaker{Threshold: 1}
	now := time.Unix(1000, 0)
	for _, err := range []error{apix.FromStatus(404, "no such model"), apix.FromStatus(400, "bad"), apix.Translation("x")} {
		b.Report("a", route.Outcome{Err: err}, now)
		if !b.Allow("a", now) {
			t.Fatalf("%v opened the breaker", err)
		}
	}
	b.Report("a", route.Outcome{Err: apix.FromStatus(500, "down")}, now)
	if b.Allow("a", now) {
		t.Fatal("server error did not open the breaker")
	}
}

func TestBreakerHalfOpenAdmitsOneTrial(t *testing.T) {
	b := &route.Breaker{Threshold: 1, Cooldown: 10 * time.Second}
	now := time.Unix(1000, 0)
	b.Report("a", route.Outcome{Err: apix.FromStatus(503, "down")}, now)
	if b.Allow("a", now) || b.Begin("a", now) {
		t.Fatal("open breaker admitted a request")
	}
	now = now.Add(10 * time.Second)
	if !b.Allow("a", now) || !b.Begin("a", now) {
		t.Fatal("half-open breaker refused the first trial")
	}
	if b.Allow("a", now) || b.Begin("a", now) {
		t.Fatal("half-open breaker admitted a second concurrent trial")
	}
	// An inconclusive outcome frees the slot without closing the breaker.
	b.Report("a", route.Outcome{Err: apix.Translation("bad request")}, now)
	if !b.Begin("a", now) {
		t.Fatal("inconclusive trial kept the slot")
	}
	// An unreported trial frees the slot after the cooldown.
	if b.Begin("a", now.Add(5*time.Second)) || !b.Begin("a", now.Add(10*time.Second)) {
		t.Fatal("trial slot did not expire with the cooldown")
	}
	b.Report("a", route.Outcome{}, now)
	first, second := b.Begin("a", now), b.Begin("a", now)
	if !first || !second {
		t.Fatal("success did not close the breaker")
	}
}

func TestBreakerHonorsRetryAfter(t *testing.T) {
	b := &route.Breaker{Threshold: 3, Cooldown: time.Second}
	now := time.Unix(1000, 0)
	limited := apix.FromStatus(429, "slow down")
	limited.RetryAfter = time.Minute
	b.Report("a", route.Outcome{Err: limited}, now)
	if b.Allow("a", now.Add(59*time.Second)) {
		t.Fatal("Retry-After ignored below the threshold")
	}
	if !b.Allow("a", now.Add(time.Minute)) {
		t.Fatal("endpoint stayed open after Retry-After")
	}
}

func TestRouterBeginGatesTrials(t *testing.T) {
	now := time.Unix(1000, 0)
	r := newRouter(t, []route.Endpoint{
		{ID: "primary", Protocol: anthropic.Messages},
		{ID: "backup", Protocol: openai.ChatCompletions},
	}, route.WithHealth(&route.Breaker{Threshold: 1, Cooldown: 10 * time.Second}), route.WithClock(func() time.Time { return now }))
	r.Report("primary", route.Outcome{Err: apix.FromStatus(503, "down")})
	now = now.Add(10 * time.Second)
	cands, _ := r.Route(messagesRequest("m"))
	if cands[0].Endpoint.ID != "primary" || !r.Begin(cands[0]) {
		t.Fatalf("half-open endpoint not offered: %v", ids(cands))
	}
	if cands, _ := r.Route(messagesRequest("m")); !slices.Equal(ids(cands), []string{"backup"}) {
		t.Fatalf("endpoint with a trial in flight still routed: %v", ids(cands))
	}
	if r.Begin(cands[0]) {
		t.Fatal("second trial admitted")
	}
	// When every endpoint is unhealthy the fail-open candidates always pass.
	r.Report("backup", route.Outcome{Err: apix.FromStatus(503, "down")})
	cands, _ = r.Route(messagesRequest("m"))
	for _, c := range cands {
		if !r.Begin(c) {
			t.Fatalf("fail-open candidate %s refused", c.Endpoint.ID)
		}
	}
}
