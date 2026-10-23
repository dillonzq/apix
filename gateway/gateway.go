// Package gateway executes requests over HTTP: it routes each request with
// package route, converts it with the registry's codecs, sends it upstream,
// and converts the response back, failing over to the next candidate while
// nothing has been returned to the caller.
//
// Do returns only at the commit point: after an ordinary response converted
// successfully, or after a stream produced its first converted event. Errors
// before that point fail over when they are retryable or happened before the
// request was sent; errors after it surface from Response.Body.Read and never
// retry, because the client may already have received output.
package gateway

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/dillonzq/apix"
	"github.com/dillonzq/apix/route"
	"github.com/dillonzq/apix/thinking"
)

// Upstream is a routable endpoint plus how to reach it.
type Upstream struct {
	route.Endpoint
	// BaseURL is the scheme and host, plus any path prefix, that the
	// protocol's request path is appended to.
	BaseURL string
	// Path replaces the protocol's default request path when set. It is
	// rejected for protocols whose path depends on the model or the
	// streaming mode, such as Gemini GenerateContent; put a prefix in BaseURL
	// instead.
	Path string
	// APIKey is sent with Auth, or the protocol's default scheme.
	APIKey string
	Auth   *apix.AuthScheme
	// Header holds extra headers for every request to this upstream.
	Header http.Header
}

// Config configures a Gateway.
type Config struct {
	Registry  *apix.Registry
	Upstreams []Upstream
	// Client sends upstream requests. The default has no timeout so streams
	// are not cut off; use CommitTimeout and the request context instead.
	Client *http.Client
	// MaxAttempts limits how many candidates one request tries; 0 tries all.
	MaxAttempts int
	// MaxRequestBytes limits request bodies read by Handler; 0 means 32 MiB.
	MaxRequestBytes int64
	// MaxResponseBytes limits ordinary upstream response bodies; 0 means
	// 64 MiB. A larger response fails over like an upstream error.
	MaxResponseBytes int64
	// CommitTimeout limits how long one attempt may take to reach the commit
	// point: a converted ordinary response, or a stream's first event. On
	// expiry the attempt is canceled and the request fails over. Zero means
	// no limit.
	CommitTimeout time.Duration
	// ForwardHeaders lists upstream response headers copied to
	// Response.Header, and so to Handler clients. A trailing "*" matches a
	// prefix, as in "X-Ratelimit-*". Framing headers such as Content-Type and
	// Content-Length are never copied. Response.UpstreamHeader holds all of
	// them.
	ForwardHeaders []string
	// ThinkingSuffix makes Do parse a trailing "model(value)" reasoning
	// suffix: requests route by the model without it, and a recognized value
	// becomes Request.Reasoning. Off by default because model IDs may contain
	// parentheses.
	ThinkingSuffix bool
	// RouterOptions configure the underlying route.Router.
	RouterOptions []route.Option
	// OnAttempt observes every attempt, for logs and metrics.
	OnAttempt func(Attempt)
}

// Attempt describes one try against an upstream.
type Attempt struct {
	Endpoint      string
	Provider      string
	Protocol      apix.Protocol
	UpstreamModel string
	Native        bool
	// Status is the upstream HTTP status; 0 when no response arrived.
	Status  int
	Err     error
	Latency time.Duration
}

// Request is a client request in a registered protocol.
type Request struct {
	Protocol apix.Protocol
	Payload  apix.Payload
	// Output is the protocol of the response; empty means Protocol.
	Output apix.Protocol
	// Reasoning, when set, replaces the request's reasoning control for
	// every upstream; see apix.ExchangeOptions.Reasoning.
	Reasoning *apix.Reasoning
	// Header holds extra headers to send upstream, such as beta flags.
	// Client credentials are never forwarded implicitly.
	Header http.Header
}

// Response is a committed response in the client's protocol.
type Response struct {
	Status int
	// Header holds the headers to send the client: the content type and any
	// Config.ForwardHeaders.
	Header http.Header
	// UpstreamHeader holds every header of the committed upstream response.
	UpstreamHeader http.Header
	// Body yields the converted response. For streams it yields encoded
	// events as they arrive; a failure after the commit point is returned by
	// Read as an *apix.Error. Always close it.
	Body          io.ReadCloser
	Stream        bool
	Endpoint      string
	UpstreamModel string
	// Attempts lists every attempt, the committed one last.
	Attempts []Attempt
	exchange apix.Exchange
}

// Diagnostics returns the conversion losses recorded for the exchange. For
// streams, call it after Body is drained.
func (r *Response) Diagnostics() []apix.Diagnostic {
	if r == nil || r.exchange == nil {
		return nil
	}
	return r.exchange.Diagnostics()
}

// Gateway routes, converts, and executes requests. It is safe for concurrent
// use.
type Gateway struct {
	reg              *apix.Registry
	router           *route.Router
	client           *http.Client
	maxAttempts      int
	maxRequestBytes  int64
	maxResponseBytes int64
	commitTimeout    time.Duration
	forwardHeaders   []string
	thinkingSuffix   bool
	onAttempt        func(Attempt)
	mu               sync.RWMutex // guards routing and upstream configuration together
	upstreams        map[string]Upstream
}

// New validates cfg and creates a Gateway.
func New(cfg Config) (*Gateway, error) {
	if cfg.Registry == nil {
		return nil, errors.New("gateway: registry is nil")
	}
	g := &Gateway{reg: cfg.Registry, client: cfg.Client, maxAttempts: cfg.MaxAttempts, maxRequestBytes: cfg.MaxRequestBytes,
		maxResponseBytes: cfg.MaxResponseBytes, commitTimeout: cfg.CommitTimeout, forwardHeaders: append([]string(nil), cfg.ForwardHeaders...),
		thinkingSuffix: cfg.ThinkingSuffix, onAttempt: cfg.OnAttempt}
	if g.client == nil {
		g.client = &http.Client{}
	}
	if g.maxRequestBytes <= 0 {
		g.maxRequestBytes = 32 << 20
	}
	if g.maxResponseBytes <= 0 {
		g.maxResponseBytes = 64 << 20
	}
	ups, endpoints, err := validate(cfg.Registry, cfg.Upstreams)
	if err != nil {
		return nil, err
	}
	if g.router, err = route.New(cfg.Registry, endpoints, cfg.RouterOptions...); err != nil {
		return nil, err
	}
	g.upstreams = ups
	return g, nil
}

// Update validates and replaces the upstream set atomically. A rejected update
// leaves the current set intact. In-flight requests keep their configuration
// through failover; new requests use the new set. Health state is kept per ID.
func (g *Gateway) Update(upstreams []Upstream) error {
	ups, endpoints, err := validate(g.reg, upstreams)
	if err != nil {
		return err
	}
	g.mu.Lock()
	defer g.mu.Unlock()
	if err := g.router.Update(endpoints); err != nil {
		return err
	}
	g.upstreams = ups
	return nil
}

// Router returns the underlying router, for inspection, manual reports, and
// Router.Remember.
func (g *Gateway) Router() *route.Router { return g.router }

func validate(reg *apix.Registry, upstreams []Upstream) (map[string]Upstream, []route.Endpoint, error) {
	ups := make(map[string]Upstream, len(upstreams))
	endpoints := make([]route.Endpoint, 0, len(upstreams))
	for _, u := range upstreams {
		parsed, err := url.Parse(u.BaseURL)
		if err != nil || parsed.Scheme == "" || parsed.Host == "" {
			return nil, nil, fmt.Errorf("gateway: upstream %s needs an absolute BaseURL", u.ID)
		}
		if spec, ok := reg.Spec(u.Protocol); ok && u.Path != "" && dynamicPath(spec) {
			return nil, nil, fmt.Errorf("gateway: upstream %s: Path cannot replace the %s path, which depends on the model or streaming mode; put a path prefix in BaseURL", u.ID, u.Protocol)
		}
		u.Header = u.Header.Clone()
		if u.Auth != nil {
			auth := *u.Auth
			u.Auth = &auth
		}
		ups[u.ID] = u
		endpoints = append(endpoints, u.Endpoint)
	}
	return ups, endpoints, nil
}

// dynamicPath reports whether a protocol's request path varies with the
// model or the streaming mode, which a fixed Path cannot express.
func dynamicPath(spec apix.ProtocolSpec) bool {
	if spec.Path == nil {
		return false
	}
	base := spec.Path(apix.RequestInfo{}, "a")
	return spec.Path(apix.RequestInfo{}, "b") != base || spec.Path(apix.RequestInfo{Stream: true}, "a") != base
}

// Do routes and executes req, failing over between candidates until one
// commits. It returns the last error when every candidate fails. Errors are
// *apix.Error values; a canceled context is also reported through errors.Is.
func (g *Gateway) Do(ctx context.Context, req Request) (*Response, error) {
	info, err := g.reg.Inspect(req.Protocol, req.Payload)
	if err != nil {
		return nil, err
	}
	if g.thinkingSuffix && req.Reasoning == nil {
		if s := thinking.ParseSuffix(info.Model); s.HasSuffix {
			info.Model = s.Model
			req.Payload.Metadata.Model = s.Model
			req.Reasoning = s.Reasoning()
		}
	}
	g.mu.RLock()
	candidates, err := g.router.Route(info)
	upstreams := g.upstreams
	g.mu.RUnlock()
	if err != nil {
		return nil, err
	}
	var attempts []Attempt
	var last *apix.Error
	for _, c := range candidates {
		if g.maxAttempts > 0 && len(attempts) >= g.maxAttempts {
			break
		}
		up, ok := upstreams[c.Endpoint.ID]
		if !ok {
			continue // the exposed router may have been updated independently
		}
		if ctx.Err() != nil {
			return nil, canceled(ctx)
		}
		a := Attempt{Endpoint: c.Endpoint.ID, Provider: c.Endpoint.Provider, Protocol: c.Endpoint.Protocol, UpstreamModel: c.UpstreamModel, Native: c.Native}
		start := time.Now()
		resp, sent, aerr := g.attempt(ctx, req, info, c, up, &a)
		a.Latency = time.Since(start)
		if aerr != nil {
			a.Err = aerr
		}
		attempts = append(attempts, a)
		if g.onAttempt != nil {
			g.onAttempt(a)
		}
		if aerr != nil && ctx.Err() != nil {
			return nil, canceled(ctx)
		}
		if sent {
			g.router.Report(c.Endpoint.ID, route.Outcome{Err: a.Err, Latency: a.Latency})
		}
		if aerr == nil {
			resp.Attempts = attempts
			return resp, nil
		}
		last = aerr
		if sent && !aerr.Retryable {
			break
		}
	}
	if last == nil {
		last = apix.Unsupported("no upstream available for model " + info.Model)
	}
	return nil, last
}

func canceled(ctx context.Context) *apix.Error {
	cause := context.Cause(ctx)
	return &apix.Error{Class: apix.ClassNetwork, Message: "request canceled: " + apix.Redact(cause.Error()), Err: cause}
}

var errCommitTimeout = errors.New("gateway: commit timeout")

// commitGuard cancels an attempt that has not committed within the timeout.
type commitGuard struct {
	state atomic.Int32 // 0 pending, 1 committed, 2 expired
	timer *time.Timer
}

func newCommitGuard(d time.Duration, cancel context.CancelCauseFunc) *commitGuard {
	if d <= 0 {
		return nil
	}
	g := &commitGuard{}
	g.timer = time.AfterFunc(d, func() {
		if g.state.CompareAndSwap(0, 2) {
			cancel(errCommitTimeout)
		}
	})
	return g
}

// commit stops the timer and reports whether the attempt beat it.
func (g *commitGuard) commit() bool {
	if g == nil {
		return true
	}
	g.timer.Stop()
	return g.state.CompareAndSwap(0, 1)
}

// attempt tries one candidate. sent reports whether the request reached the
// upstream; failures before that, such as a rejected conversion, always fall
// through to the next candidate.
func (g *Gateway) attempt(ctx context.Context, req Request, info apix.RequestInfo, c route.Candidate, up Upstream, a *Attempt) (*Response, bool, *apix.Error) {
	actx, cancel := context.WithCancelCause(ctx)
	committed := false
	defer func() {
		if !committed {
			cancel(nil)
		}
	}()
	resp, sent, err := g.send(actx, cancel, req, info, c, up, a)
	if err != nil {
		if errors.Is(context.Cause(actx), errCommitTimeout) && ctx.Err() == nil {
			err = &apix.Error{Class: apix.ClassNetwork, Message: fmt.Sprintf("upstream %s did not commit within %s", up.ID, g.commitTimeout), Retryable: true, Err: errCommitTimeout}
		}
		return nil, sent, err
	}
	committed = true
	return resp, true, nil
}

func (g *Gateway) send(ctx context.Context, cancel context.CancelCauseFunc, req Request, info apix.RequestInfo, c route.Candidate, up Upstream, a *Attempt) (*Response, bool, *apix.Error) {
	opts := c.Endpoint.Options.Exchange()
	opts.Output = req.Output
	opts.Reasoning = req.Reasoning
	ex, err := g.reg.NewExchange(req.Protocol, c.Endpoint.Protocol, opts)
	if err != nil {
		return nil, false, apix.AsError(err)
	}
	payload, err := ex.ConvertRequest(req.Payload, c.UpstreamModel)
	if err != nil {
		return nil, false, apix.AsError(err)
	}
	// The converted payload's metadata is authoritative for values the
	// upstream protocol keeps outside the body, such as a model in its URL.
	upInfo := payload.Metadata.Apply(info)
	model := c.UpstreamModel
	if payload.Metadata.Model != "" {
		model = payload.Metadata.Model
	}
	hreq, aerr := g.newRequest(ctx, req, upInfo, model, c, up, payload)
	if aerr != nil {
		return nil, false, aerr
	}
	if !g.router.Begin(c) {
		return nil, false, apix.UpstreamFallback("upstream " + up.ID + " is recovering and already has a trial request in flight")
	}
	guard := newCommitGuard(g.commitTimeout, cancel)
	hresp, herr := g.client.Do(hreq)
	if herr != nil {
		return nil, true, apix.FromNetwork(herr)
	}
	a.Status = hresp.StatusCode
	if hresp.StatusCode >= 400 {
		body, _ := io.ReadAll(io.LimitReader(hresp.Body, 64<<10))
		_ = hresp.Body.Close()
		after := retryAfter(hresp.Header, time.Now())
		if _, err := ex.ConvertResponse(hresp.StatusCode, apix.Payload{Body: body, ContentType: hresp.Header.Get("Content-Type")}); err != nil {
			return nil, true, withRetryAfter(apix.AsError(err), after)
		}
		return nil, true, withRetryAfter(apix.FromStatus(hresp.StatusCode, string(body)), after)
	}
	output := req.Output
	if output == "" {
		output = req.Protocol
	}
	outputSpec, _ := g.reg.Spec(output)
	resp := &Response{Status: hresp.StatusCode, Header: http.Header{}, UpstreamHeader: hresp.Header.Clone(), Stream: upInfo.Stream, Endpoint: c.Endpoint.ID, UpstreamModel: c.UpstreamModel, exchange: ex}
	forward(resp.Header, hresp.Header, g.forwardHeaders)
	if !upInfo.Stream {
		body, rerr := io.ReadAll(io.LimitReader(hresp.Body, g.maxResponseBytes+1))
		_ = hresp.Body.Close()
		if rerr != nil {
			return nil, true, apix.FromNetwork(rerr)
		}
		if int64(len(body)) > g.maxResponseBytes {
			return nil, true, apix.UpstreamFallback(fmt.Sprintf("upstream response exceeds %d bytes", g.maxResponseBytes))
		}
		out, err := ex.ConvertResponse(hresp.StatusCode, apix.Payload{Body: body, ContentType: hresp.Header.Get("Content-Type")})
		if err != nil {
			return nil, true, upstreamFault(err)
		}
		if !guard.commit() {
			return nil, true, apix.FromNetwork(errCommitTimeout)
		}
		cancel(nil) // the body is fully read; nothing else uses the context
		g.remember(ex, c.Endpoint.ID)
		resp.Header.Set("Content-Type", contentType(out.ContentType, "application/json"))
		resp.Body = io.NopCloser(bytes.NewReader(out.Body))
		return resp, true, nil
	}
	sc, err := ex.NewStreamConverter()
	if err != nil {
		_ = hresp.Body.Close()
		return nil, true, apix.AsError(err)
	}
	body := &streamBody{upstream: hresp.Body, conv: sc, chunk: make([]byte, 32<<10), cancel: cancel}
	body.onFailure = func(err error) { g.router.Report(c.Endpoint.ID, route.Outcome{Err: err}) }
	body.onDone = func() { g.remember(ex, c.Endpoint.ID) }
	if err := body.prime(); err != nil {
		_ = body.upstream.Close()
		return nil, true, err
	}
	if !guard.commit() {
		_ = body.upstream.Close()
		return nil, true, apix.FromNetwork(errCommitTimeout)
	}
	g.remember(ex, c.Endpoint.ID)
	if outputSpec.Stream == apix.FramingSSE {
		resp.Header.Set("Content-Type", "text/event-stream")
		resp.Header.Set("Cache-Control", "no-cache")
	} else {
		resp.Header.Set("Content-Type", "application/octet-stream")
	}
	resp.Body = body
	return resp, true, nil
}

// remember records state the exchange's upstream created so follow-up
// requests reach the same endpoint.
func (g *Gateway) remember(ex apix.Exchange, endpointID string) {
	if sr, ok := ex.(apix.StateReporter); ok {
		for _, key := range sr.CreatedState() {
			g.router.Remember(key, endpointID)
		}
	}
}

// upstreamFault reclassifies a failure to decode or convert a successful
// upstream response. The request already converted, so the upstream produced
// output this exchange cannot use; another candidate may succeed.
func upstreamFault(err error) *apix.Error {
	e := apix.AsError(err)
	if e.Class == apix.ClassTranslation {
		return apix.UpstreamFallback("unusable upstream response: " + e.Message)
	}
	return e
}

func withRetryAfter(err *apix.Error, d time.Duration) *apix.Error {
	if d <= 0 || err == nil {
		return err
	}
	e := *err
	e.RetryAfter = d
	return &e
}

// retryAfter reads Retry-After (seconds or an HTTP date) and the common
// Retry-After-Ms extension.
func retryAfter(h http.Header, now time.Time) time.Duration {
	if v := h.Get("Retry-After-Ms"); v != "" {
		if ms, err := strconv.ParseFloat(strings.TrimSpace(v), 64); err == nil && ms > 0 {
			return time.Duration(ms * float64(time.Millisecond))
		}
	}
	v := strings.TrimSpace(h.Get("Retry-After"))
	if v == "" {
		return 0
	}
	if s, err := strconv.Atoi(v); err == nil {
		if s > 0 {
			return time.Duration(s) * time.Second
		}
		return 0
	}
	if t, err := http.ParseTime(v); err == nil && t.After(now) {
		return t.Sub(now)
	}
	return 0
}

// framingHeaders describe the upstream body, not the converted one.
var framingHeaders = map[string]bool{"Content-Type": true, "Content-Length": true, "Content-Encoding": true, "Transfer-Encoding": true, "Connection": true, "Keep-Alive": true, "Trailer": true}

// forward copies the listed upstream headers; a trailing "*" matches a
// prefix, case-insensitively.
func forward(dst, src http.Header, names []string) {
	for _, name := range names {
		prefix, wildcard := strings.CutSuffix(name, "*")
		for k, vs := range src {
			match := strings.EqualFold(k, name)
			if wildcard {
				match = len(k) >= len(prefix) && strings.EqualFold(k[:len(prefix)], prefix)
			}
			if match && !framingHeaders[http.CanonicalHeaderKey(k)] {
				dst[http.CanonicalHeaderKey(k)] = append([]string(nil), vs...)
			}
		}
	}
}

func contentType(got, fallback string) string {
	if got == "" {
		return fallback
	}
	return got
}

func (g *Gateway) newRequest(ctx context.Context, req Request, info apix.RequestInfo, model string, c route.Candidate, up Upstream, payload apix.Payload) (*http.Request, *apix.Error) {
	spec, _ := g.reg.Spec(c.Endpoint.Protocol)
	path := up.Path
	if path == "" && spec.Path != nil {
		path = spec.Path(info, model)
	}
	method := spec.Method
	if method == "" {
		method = http.MethodPost
	}
	hreq, err := http.NewRequestWithContext(ctx, method, strings.TrimRight(up.BaseURL, "/")+path, bytes.NewReader(payload.Body))
	if err != nil {
		return nil, apix.Unsupported("invalid upstream request for " + up.ID + ": " + err.Error())
	}
	hreq.Header.Set("Content-Type", contentType(payload.ContentType, "application/json"))
	if info.Stream && spec.Stream == apix.FramingSSE {
		hreq.Header.Set("Accept", "text/event-stream")
	}
	for k, v := range spec.Headers {
		hreq.Header.Set(k, v)
	}
	for _, h := range []http.Header{req.Header, up.Header} {
		for k, vs := range h {
			hreq.Header[http.CanonicalHeaderKey(k)] = append([]string(nil), vs...)
		}
	}
	if up.APIKey != "" {
		auth := spec.Auth
		if up.Auth != nil {
			auth = *up.Auth
		}
		if auth.Header != "" {
			hreq.Header.Set(auth.Header, auth.Prefix+up.APIKey)
		}
	}
	return hreq, nil
}

// streamBody converts the upstream stream lazily as the caller reads.
type streamBody struct {
	upstream  io.ReadCloser
	conv      apix.StreamConverter
	chunk     []byte
	pending   [][]byte
	current   []byte
	done      bool
	committed bool
	err       *apix.Error
	cancel    context.CancelCauseFunc
	onFailure func(error)
	onDone    func()
}

// prime reads until the first converted event: the commit point. Any failure
// before it is returned so the gateway can fail over.
func (b *streamBody) prime() *apix.Error {
	for len(b.pending) == 0 && !b.done && b.err == nil {
		b.fill()
	}
	if b.err != nil && len(b.pending) == 0 {
		return upstreamFault(b.err)
	}
	// A read may deliver valid events together with EOF or a later error.
	// Those events commit the response regardless of network chunk boundaries.
	b.committed = true
	return nil
}

// fill reads one upstream chunk and converts it.
func (b *streamBody) fill() {
	n, rerr := b.upstream.Read(b.chunk)
	if n > 0 {
		events, done, err := b.conv.Feed(b.chunk[:n])
		b.pending = append(b.pending, events...)
		if err != nil {
			b.fail(err)
			return
		}
		if done {
			b.done = true
			return
		}
	}
	switch {
	case rerr == io.EOF:
		b.done = true
		tail, err := b.conv.Finish()
		b.pending = append(b.pending, tail...)
		if err != nil {
			b.fail(err)
		}
	case rerr != nil:
		if b.committed || len(b.pending) > 0 {
			b.fail(apix.PostFirstByteNetwork(rerr.Error()))
		} else {
			b.fail(apix.FromNetwork(rerr))
		}
	}
}

func (b *streamBody) fail(err error) {
	b.err = apix.AsError(err)
}

func (b *streamBody) Read(p []byte) (int, error) {
	for len(b.current) == 0 {
		if len(b.pending) > 0 {
			b.current, b.pending = b.pending[0], b.pending[1:]
			continue
		}
		if b.err != nil {
			// Report after Do has recorded the committed attempt, including
			// errors buffered alongside the first events during prime.
			if b.onFailure != nil {
				b.onFailure(b.err)
				b.onFailure = nil
			}
			return 0, b.err
		}
		if b.done {
			if b.onDone != nil {
				b.onDone()
				b.onDone = nil
			}
			return 0, io.EOF
		}
		b.fill()
	}
	n := copy(p, b.current)
	b.current = b.current[n:]
	return n, nil
}

func (b *streamBody) Close() error {
	err := b.upstream.Close()
	if b.cancel != nil {
		b.cancel(nil)
	}
	return err
}
