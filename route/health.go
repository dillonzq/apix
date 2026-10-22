package route

import (
	"sync"
	"time"

	"github.com/dillonzq/apix"
)

// Health decides which endpoints may receive traffic. Implementations must be
// safe for concurrent use. Replace the default Breaker to share health across
// processes.
type Health interface {
	Allow(endpointID string, now time.Time) bool
	Report(endpointID string, o Outcome, now time.Time)
}

// Breaker is the default Health: after Threshold consecutive failures an
// endpoint is skipped for Cooldown. Once the cooldown ends it receives traffic
// again, but a single further failure reopens it; a success closes it.
//
// Only failures that point at the endpoint count: authentication, unknown
// model, rate limit, quota, billing, upstream server, and network errors.
// Translation and unsupported-request errors describe the request and are
// ignored. The zero value uses a threshold of 3 and a 30 second cooldown.
type Breaker struct {
	Threshold int
	Cooldown  time.Duration

	mu     sync.Mutex
	states map[string]*breakerState
}

type breakerState struct {
	failures  int
	openUntil time.Time
}

func (b *Breaker) threshold() int {
	if b.Threshold < 1 {
		return 3
	}
	return b.Threshold
}

func (b *Breaker) cooldown() time.Duration {
	if b.Cooldown <= 0 {
		return 30 * time.Second
	}
	return b.Cooldown
}

// Allow reports whether the endpoint is outside its cooldown.
func (b *Breaker) Allow(endpointID string, now time.Time) bool {
	b.mu.Lock()
	defer b.mu.Unlock()
	s := b.states[endpointID]
	return s == nil || !now.Before(s.openUntil)
}

// Report records an attempt's outcome.
func (b *Breaker) Report(endpointID string, o Outcome, now time.Time) {
	if o.Err != nil && !countsAgainstEndpoint(o.Err) {
		return
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	if o.Err == nil {
		delete(b.states, endpointID)
		return
	}
	if b.states == nil {
		b.states = make(map[string]*breakerState)
	}
	s := b.states[endpointID]
	if s == nil {
		s = &breakerState{}
		b.states[endpointID] = s
	}
	s.failures++
	if s.failures >= b.threshold() {
		s.openUntil = now.Add(b.cooldown())
	}
}

func countsAgainstEndpoint(err *apix.Error) bool {
	switch err.Class {
	case apix.ClassAuth, apix.ClassInvalidModel, apix.ClassRateLimit, apix.ClassQuota, apix.ClassBilling, apix.ClassUpstream, apix.ClassNetwork:
		return true
	}
	return false
}
