// Package route chooses upstream endpoints for a request. It performs no I/O:
// callers, or package gateway, send the request to each candidate in order and
// report outcomes back so unhealthy endpoints are skipped.
//
// The default strategy prefers endpoints that speak the client's own protocol
// (no conversion), then falls back to endpoints of the same Kind whose
// protocol the request can be converted to. Within each tier, lower Priority
// wins and equal priorities are shuffled by Weight.
package route

import (
	"fmt"
	"math/rand/v2"
	"slices"
	"sync"
	"sync/atomic"
	"time"

	"github.com/dillonzq/apix"
)

// Endpoint is one upstream API that serves a protocol.
type Endpoint struct {
	// ID uniquely identifies the endpoint; health is tracked per ID.
	ID string
	// Provider is a free-form vendor or account label for logs and metrics.
	Provider string
	// Protocol is the protocol the endpoint speaks.
	Protocol apix.Protocol
	// Models maps client model names or aliases to upstream model names. The
	// key "*" matches any model. An empty value keeps the client's model. An
	// empty map serves every model unchanged.
	Models map[string]string
	// Priority orders endpoints within a tier; lower values are tried first.
	Priority int
	// Weight shuffles endpoints of equal priority; values below 1 count as 1.
	Weight int
	// Features lists the capabilities the endpoint supports, such as
	// apix.FeatureStream. An empty list means no restriction.
	Features []string
	// Disabled removes the endpoint from routing.
	Disabled bool
	// Options are passed to the upstream codec for each exchange.
	Options apix.ExchangeOptions
}

// UpstreamModel returns the upstream model for a client model and whether the
// endpoint serves it.
func (e Endpoint) UpstreamModel(model string) (string, bool) {
	if len(e.Models) == 0 {
		return model, true
	}
	up, ok := e.Models[model]
	if !ok {
		up, ok = e.Models["*"]
	}
	if !ok {
		return "", false
	}
	if up == "" {
		up = model
	}
	return up, true
}

func (e Endpoint) supports(info apix.RequestInfo) bool {
	if len(e.Features) == 0 {
		return true
	}
	for _, f := range info.Features {
		if !slices.Contains(e.Features, f) {
			return false
		}
	}
	return true
}

func (e Endpoint) clone() Endpoint {
	if e.Models != nil {
		models := make(map[string]string, len(e.Models))
		for k, v := range e.Models {
			models[k] = v
		}
		e.Models = models
	}
	e.Features = slices.Clone(e.Features)
	return e
}

// Candidate is an endpoint chosen for a request.
type Candidate struct {
	Endpoint      Endpoint
	UpstreamModel string
	// Native reports that the endpoint speaks the client's protocol, so the
	// request only needs its model rewritten.
	Native bool
}

// Outcome is the result of one attempt against an endpoint. A nil Err is a
// success.
type Outcome struct {
	Err     *apix.Error
	Latency time.Duration
}

// Strategy orders the eligible candidates for a request. intn returns a
// uniform random integer in [0, n) and is safe to call from Order.
type Strategy interface {
	Order(info apix.RequestInfo, candidates []Candidate, intn func(n int) int) []Candidate
}

// Option configures a Router.
type Option func(*Router)

// WithStrategy replaces the default NativeFirst strategy.
func WithStrategy(s Strategy) Option { return func(r *Router) { r.strategy = s } }

// WithHealth replaces the default Breaker.
func WithHealth(h Health) Option { return func(r *Router) { r.health = h } }

// WithRand makes weighted ordering deterministic, for tests.
func WithRand(rnd *rand.Rand) Option { return func(r *Router) { r.rnd = rnd } }

// WithClock replaces time.Now, for tests.
func WithClock(now func() time.Time) Option { return func(r *Router) { r.now = now } }

// Router orders endpoints for requests. It is safe for concurrent use;
// Update replaces the endpoint set atomically.
type Router struct {
	reg       *apix.Registry
	endpoints atomic.Pointer[[]Endpoint]
	strategy  Strategy
	health    Health
	now       func() time.Time
	mu        sync.Mutex // guards rnd
	rnd       *rand.Rand
}

// New creates a router over endpoints whose protocols are registered in reg.
func New(reg *apix.Registry, endpoints []Endpoint, opts ...Option) (*Router, error) {
	if reg == nil {
		return nil, fmt.Errorf("route: registry is nil")
	}
	r := &Router{reg: reg, strategy: NativeFirst{}, now: time.Now}
	for _, opt := range opts {
		opt(r)
	}
	if r.health == nil {
		r.health = &Breaker{}
	}
	if err := r.Update(endpoints); err != nil {
		return nil, err
	}
	return r, nil
}

// Update validates and replaces the endpoint set. Health state is kept per
// endpoint ID.
func (r *Router) Update(endpoints []Endpoint) error {
	seen := make(map[string]bool, len(endpoints))
	next := make([]Endpoint, 0, len(endpoints))
	for i, e := range endpoints {
		switch {
		case e.ID == "":
			return fmt.Errorf("route: endpoint %d has no ID", i)
		case seen[e.ID]:
			return fmt.Errorf("route: endpoint %s is listed twice", e.ID)
		}
		if _, ok := r.reg.Spec(e.Protocol); !ok {
			return fmt.Errorf("route: endpoint %s uses unregistered protocol %q", e.ID, e.Protocol)
		}
		seen[e.ID] = true
		next = append(next, e.clone())
	}
	r.endpoints.Store(&next)
	return nil
}

// Endpoints returns a copy of the current endpoint set.
func (r *Router) Endpoints() []Endpoint {
	current := *r.endpoints.Load()
	out := make([]Endpoint, len(current))
	for i, e := range current {
		out[i] = e.clone()
	}
	return out
}

// Route returns the candidates for a request in the order to try them. It
// fails with ClassInvalidModel when no endpoint serves the model, and with
// ClassUnsupported when endpoints serve it but none can take the request.
//
// Endpoints the health tracker rejects are skipped. When every eligible
// endpoint is unhealthy they are all returned anyway, so an outage of the
// tracker's making never refuses service outright.
func (r *Router) Route(info apix.RequestInfo) ([]Candidate, *apix.Error) {
	now := r.now()
	kind := info.Kind
	if spec, ok := r.reg.Spec(info.Protocol); ok {
		kind = spec.Kind
	}
	var healthy, unhealthy []Candidate
	served := false
	for _, e := range *r.endpoints.Load() {
		if spec, _ := r.reg.Spec(e.Protocol); e.Disabled || spec.Kind != kind {
			continue
		}
		model, ok := e.UpstreamModel(info.Model)
		if !ok {
			continue
		}
		served = true
		if !e.supports(info) || !r.reg.CanConvert(info.Protocol, e.Protocol) {
			continue
		}
		c := Candidate{Endpoint: e.clone(), UpstreamModel: model, Native: e.Protocol == info.Protocol}
		if r.health.Allow(e.ID, now) {
			healthy = append(healthy, c)
		} else {
			unhealthy = append(unhealthy, c)
		}
	}
	if len(healthy) == 0 {
		healthy = unhealthy
	}
	if len(healthy) == 0 {
		if !served {
			return nil, &apix.Error{Class: apix.ClassInvalidModel, Message: apix.Redact("no endpoint serves model " + info.Model)}
		}
		return nil, apix.Unsupported("no endpoint can serve " + string(info.Protocol) + " requests for model " + info.Model)
	}
	return r.strategy.Order(info, healthy, r.intn), nil
}

// Report records the outcome of an attempt against an endpoint.
func (r *Router) Report(endpointID string, o Outcome) {
	r.health.Report(endpointID, o, r.now())
}

func (r *Router) intn(n int) int {
	if r.rnd == nil {
		return rand.IntN(n)
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.rnd.IntN(n)
}

// NativeFirst is the default strategy: endpoints speaking the client's
// protocol come before endpoints that need conversion; within each tier lower
// Priority comes first and equal priorities are shuffled by Weight.
type NativeFirst struct{}

// Order implements Strategy.
func (NativeFirst) Order(_ apix.RequestInfo, candidates []Candidate, intn func(int) int) []Candidate {
	out := slices.Clone(candidates)
	slices.SortStableFunc(out, func(a, b Candidate) int {
		if a.Native != b.Native {
			if a.Native {
				return -1
			}
			return 1
		}
		return a.Endpoint.Priority - b.Endpoint.Priority
	})
	for start := 0; start < len(out); {
		end := start + 1
		for end < len(out) && out[end].Native == out[start].Native && out[end].Endpoint.Priority == out[start].Endpoint.Priority {
			end++
		}
		weightedShuffle(out[start:end], intn)
		start = end
	}
	return out
}

// weightedShuffle orders a group by repeated weighted draws without
// replacement, so an endpoint with twice the weight is twice as likely to be
// tried first.
func weightedShuffle(group []Candidate, intn func(int) int) {
	for i := 0; i < len(group)-1; i++ {
		total := 0
		for _, c := range group[i:] {
			total += weight(c)
		}
		pick := intn(total)
		for j := i; j < len(group); j++ {
			if pick < weight(group[j]) {
				group[i], group[j] = group[j], group[i]
				break
			}
			pick -= weight(group[j])
		}
	}
}

func weight(c Candidate) int {
	if c.Endpoint.Weight < 1 {
		return 1
	}
	return c.Endpoint.Weight
}
