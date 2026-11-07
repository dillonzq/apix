package gateway_test

import (
	"context"
	"errors"
	"io"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/dillonzq/apix"
	"github.com/dillonzq/apix/gateway"
	"github.com/dillonzq/apix/protocol/anthropic"
	"github.com/dillonzq/apix/route"
)

type observedHealth struct {
	route.Breaker
	mu       sync.Mutex
	outcomes []route.Outcome
}

func (health *observedHealth) Report(endpoint string, outcome route.Outcome, now time.Time) {
	health.mu.Lock()
	health.outcomes = append(health.outcomes, outcome)
	health.mu.Unlock()
	health.Breaker.Report(endpoint, outcome, now)
}

func (health *observedHealth) count() int {
	health.mu.Lock()
	defer health.mu.Unlock()
	return len(health.outcomes)
}

func healthGateway(test *testing.T, health *observedHealth, body func(context.Context) io.ReadCloser) *gateway.Gateway {
	test.Helper()
	return newGateway(test, gateway.Config{
		Upstreams:     []gateway.Upstream{{Endpoint: route.Endpoint{ID: "primary", Protocol: anthropic.Messages}, BaseURL: "https://primary.example"}},
		RouterOptions: []route.Option{route.WithHealth(health)},
		Client: &http.Client{Transport: roundTripFunc(func(request *http.Request) (*http.Response, error) {
			return &http.Response{StatusCode: 200, Header: http.Header{"Content-Type": {"text/event-stream"}}, Body: body(request.Context())}, nil
		})},
	})
}

func TestConsecutiveCommittedStreamFailuresTripBreaker(test *testing.T) {
	health := &observedHealth{Breaker: route.Breaker{Threshold: 3}}
	instance := healthGateway(test, health, func(context.Context) io.ReadCloser {
		return io.NopCloser(dataAndErrorReader{strings.NewReader(messagesStart), io.ErrUnexpectedEOF})
	})
	for attempt := range 3 {
		response, err := do(test, instance, clientStream)
		if err != nil {
			test.Fatal(err)
		}
		if health.count() != attempt {
			test.Fatal("commit reported a health outcome")
		}
		if _, err := io.ReadAll(response.Body); !errors.Is(err, io.ErrUnexpectedEOF) || apix.AsError(err).Retryable {
			test.Fatalf("stream failure lost its cause or became retryable: %v", err)
		}
		_, _ = response.Body.Read(make([]byte, 1))
		_ = response.Body.Close()
		if health.count() != attempt+1 {
			test.Fatal("stream outcome was not reported exactly once")
		}
	}
	if health.Allow("primary", time.Now()) {
		test.Fatal("consecutive committed stream failures did not open the breaker")
	}
}

type cancelBody struct {
	reader  *strings.Reader
	ctx     context.Context
	started chan struct{}
	eof     bool
}

func (body *cancelBody) Read(buffer []byte) (int, error) {
	if body.reader.Len() > 0 {
		return body.reader.Read(buffer)
	}
	if body.started != nil {
		close(body.started)
		body.started = nil
	}
	<-body.ctx.Done()
	if body.eof {
		return 0, io.EOF
	}
	return 0, body.ctx.Err()
}

func TestCanceledStreamEOFPreservesCause(test *testing.T) {
	health := &observedHealth{Breaker: route.Breaker{Threshold: 1}}
	instance := healthGateway(test, health, func(ctx context.Context) io.ReadCloser {
		return &cancelBody{reader: strings.NewReader(messagesStart), ctx: ctx, eof: true}
	})
	ctx, cancel := context.WithCancelCause(context.Background())
	defer cancel(nil)
	cause := errors.New("caller stopped before upstream EOF")
	response, err := instance.Do(ctx, gateway.Request{Protocol: anthropic.Messages, Payload: apix.Payload{Body: []byte(clientStream)}})
	if err != nil {
		test.Fatal(err)
	}
	defer response.Body.Close()
	cancel(cause)
	_, err = io.ReadAll(response.Body)
	if !errors.Is(err, context.Canceled) || !errors.Is(err, cause) {
		test.Fatalf("EOF hid caller cancellation: %v", err)
	}
	if !health.Allow("primary", time.Now()) || health.count() != 1 {
		test.Fatal("canceled EOF penalized the endpoint or reported twice")
	}
}

func (body *cancelBody) Close() error { return nil }

func TestCommittedStreamCancellationDoesNotPenalizeEndpoint(test *testing.T) {
	for _, cause := range []error{context.Canceled, context.DeadlineExceeded, errors.New("caller stopped")} {
		test.Run(cause.Error(), func(test *testing.T) {
			health := &observedHealth{Breaker: route.Breaker{Threshold: 1}}
			instance := healthGateway(test, health, func(ctx context.Context) io.ReadCloser {
				return &cancelBody{reader: strings.NewReader(messagesStart), ctx: ctx}
			})
			ctx, cancel := context.WithCancelCause(context.Background())
			defer cancel(nil)
			response, err := instance.Do(ctx, gateway.Request{Protocol: anthropic.Messages, Payload: apix.Payload{Body: []byte(clientStream)}})
			if err != nil {
				test.Fatal(err)
			}
			cancel(cause)
			_, readErr := io.ReadAll(response.Body)
			_ = response.Body.Close()
			if !errors.Is(readErr, context.Canceled) || !errors.Is(readErr, cause) || apix.AsError(readErr).Retryable {
				test.Fatalf("cancellation lost its cause: %v", readErr)
			}
			if !health.Allow("primary", time.Now()) || health.count() != 1 {
				test.Fatal("client cancellation penalized the endpoint or reported twice")
			}
		})
	}
}

func TestStreamCloseInterruptsReadWithoutPenalizingEndpoint(test *testing.T) {
	health := &observedHealth{Breaker: route.Breaker{Threshold: 1}}
	started := make(chan struct{})
	instance := healthGateway(test, health, func(ctx context.Context) io.ReadCloser {
		return &cancelBody{reader: strings.NewReader(messagesStart), ctx: ctx, started: started}
	})
	response, err := do(test, instance, clientStream)
	if err != nil {
		test.Fatal(err)
	}
	test.Cleanup(func() { _ = response.Body.Close() })
	finished := make(chan error, 1)
	go func() {
		_, readErr := io.ReadAll(response.Body)
		finished <- readErr
	}()
	<-started
	if err := response.Body.Close(); err != nil {
		test.Fatal(err)
	}
	if err := <-finished; !errors.Is(err, context.Canceled) {
		test.Fatalf("concurrent read: %v", err)
	}
	if !health.Allow("primary", time.Now()) || health.count() != 1 {
		test.Fatal("Close penalized the endpoint or reported twice")
	}
}

func TestStreamHealthSuccessAndEarlyClose(test *testing.T) {
	for _, complete := range []bool{false, true} {
		test.Run(map[bool]string{false: "early close", true: "completed"}[complete], func(test *testing.T) {
			health := &observedHealth{Breaker: route.Breaker{Threshold: 2}}
			health.Breaker.Report("primary", route.Outcome{Err: apix.FromNetwork(io.ErrUnexpectedEOF)}, time.Now())
			instance := healthGateway(test, health, func(ctx context.Context) io.ReadCloser {
				if complete {
					return io.NopCloser(strings.NewReader(messagesStart + "event: message_stop\ndata: {\"type\":\"message_stop\"}\n\n"))
				}
				return &cancelBody{reader: strings.NewReader(messagesStart), ctx: ctx}
			})
			response, err := do(test, instance, clientStream)
			if err != nil {
				test.Fatal(err)
			}
			if complete {
				readAll(test, response)
			} else {
				_ = response.Body.Close()
			}
			health.Breaker.Report("primary", route.Outcome{Err: apix.FromNetwork(io.ErrUnexpectedEOF)}, time.Now())
			if health.Allow("primary", time.Now()) != complete || health.count() != 1 {
				test.Fatal("only a completed stream should clear prior failures")
			}
		})
	}
}

func TestHalfOpenStreamCloseReleasesProbe(test *testing.T) {
	health := &observedHealth{Breaker: route.Breaker{Threshold: 1, Cooldown: time.Minute}}
	health.Breaker.Report("primary", route.Outcome{Err: apix.FromNetwork(io.ErrUnexpectedEOF)}, time.Now().Add(-2*time.Minute))
	instance := healthGateway(test, health, func(ctx context.Context) io.ReadCloser {
		return &cancelBody{reader: strings.NewReader(messagesStart), ctx: ctx}
	})
	response, err := do(test, instance, clientStream)
	if err != nil {
		test.Fatal(err)
	}
	if health.Allow("primary", time.Now()) {
		test.Fatal("commit prematurely released a half-open probe")
	}
	_ = response.Body.Close()
	if !health.Allow("primary", time.Now()) || health.count() != 1 {
		test.Fatal("early close did not release the half-open probe")
	}
}

func TestStreamCloseReportsAlreadyObservedFailure(test *testing.T) {
	health := &observedHealth{Breaker: route.Breaker{Threshold: 1}}
	instance := healthGateway(test, health, func(context.Context) io.ReadCloser {
		return io.NopCloser(dataAndErrorReader{strings.NewReader(messagesStart), io.ErrUnexpectedEOF})
	})
	response, err := do(test, instance, clientStream)
	if err != nil {
		test.Fatal(err)
	}
	_ = response.Body.Close()
	if health.Allow("primary", time.Now()) || health.count() != 1 {
		test.Fatal("Close hid a failure already observed alongside committed events")
	}
}
