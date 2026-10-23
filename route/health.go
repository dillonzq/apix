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

// Prober is an optional Health extension. Router.Begin calls it immediately
// before an attempt is sent; returning false skips the attempt. A Health uses
// it to limit how many requests reach a recovering endpoint at once.
type Prober interface {
	Begin(endpointID string, now time.Time) bool
}

// Breaker is the default Health. After Threshold consecutive failures an
// endpoint is skipped for Cooldown; a failure that carries
// apix.Error.RetryAfter skips it for at least that long. Once the pause ends
// the endpoint is half-open: Begin admits one trial request at a time, a
// success closes the breaker, and a further failure reopens it. A trial whose
// outcome is never reported frees its slot after Cooldown.
//
// Only failures that point at the endpoint count: authentication, rate limit,
// quota, billing, upstream server, and network errors. Unknown models,
// translation and unsupported-request errors describe the request, which a
// client controls, and are ignored. The zero value uses a threshold of 3 and
// a 30 second cooldown.
type Breaker struct {
	Threshold int
	Cooldown  time.Duration

	mu     sync.Mutex
	states map[string]*breakerState
}

type breakerState struct {
	failures int
	// openUntil is zero while the breaker is closed. Once set it stays set
	// until a success: before it the endpoint is open, after it half-open.
	openUntil time.Time
	// probeUntil is when the half-open trial in flight expires.
	probeUntil time.Time
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

// Allow reports whether the endpoint is closed, or half-open with no trial in
// flight.
func (b *Breaker) Allow(endpointID string, now time.Time) bool {
	b.mu.Lock()
	defer b.mu.Unlock()
	s := b.states[endpointID]
	if s == nil || s.openUntil.IsZero() {
		return true
	}
	return !now.Before(s.openUntil) && !now.Before(s.probeUntil)
}

// Begin admits an attempt. A half-open endpoint admits one trial at a time.
func (b *Breaker) Begin(endpointID string, now time.Time) bool {
	b.mu.Lock()
	defer b.mu.Unlock()
	s := b.states[endpointID]
	if s == nil || s.openUntil.IsZero() {
		return true
	}
	if now.Before(s.openUntil) || now.Before(s.probeUntil) {
		return false
	}
	s.probeUntil = now.Add(b.cooldown())
	return true
}

// Report records an attempt's outcome.
func (b *Breaker) Report(endpointID string, o Outcome, now time.Time) {
	err := apix.AsError(o.Err)
	b.mu.Lock()
	defer b.mu.Unlock()
	s := b.states[endpointID]
	if err != nil && !countsAgainstEndpoint(err) {
		// An inconclusive trial frees the slot for the next one.
		if s != nil {
			s.probeUntil = time.Time{}
		}
		return
	}
	if err == nil {
		delete(b.states, endpointID)
		return
	}
	if s == nil {
		if b.states == nil {
			b.states = make(map[string]*breakerState)
		}
		s = &breakerState{}
		b.states[endpointID] = s
	}
	s.failures++
	s.probeUntil = time.Time{}
	var pause time.Duration
	if s.failures >= b.threshold() {
		pause = b.cooldown()
	}
	if err.RetryAfter > pause {
		pause = err.RetryAfter
	}
	if until := now.Add(pause); pause > 0 && until.After(s.openUntil) {
		s.openUntil = until
	}
}

func countsAgainstEndpoint(err *apix.Error) bool {
	switch err.Class {
	case apix.ClassAuth, apix.ClassRateLimit, apix.ClassQuota, apix.ClassBilling, apix.ClassUpstream, apix.ClassNetwork:
		return true
	}
	return false
}
