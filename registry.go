package apix

import (
	"encoding/json"
	"fmt"
	"maps"
	"sort"
)

// Registry holds the codecs a program uses, keyed by protocol. It is
// immutable after NewRegistry and safe for concurrent use.
type Registry struct {
	codecs map[Protocol]Codec
	specs  map[Protocol]ProtocolSpec
}

// NewRegistry registers codecs. Every codec needs a protocol ID and Kind, and
// a protocol may only be registered once.
func NewRegistry(codecs ...Codec) (*Registry, error) {
	r := &Registry{codecs: make(map[Protocol]Codec, len(codecs)), specs: make(map[Protocol]ProtocolSpec, len(codecs))}
	for i, c := range codecs {
		if c == nil {
			return nil, fmt.Errorf("apix: codec %d is nil", i)
		}
		spec := c.Spec()
		switch {
		case spec.ID == "":
			return nil, fmt.Errorf("apix: codec %d has no protocol ID", i)
		case spec.Kind == "":
			return nil, fmt.Errorf("apix: protocol %s has no kind", spec.ID)
		case r.codecs[spec.ID] != nil:
			return nil, fmt.Errorf("apix: protocol %s is registered twice", spec.ID)
		}
		r.codecs[spec.ID] = c
		spec.Headers = maps.Clone(spec.Headers)
		r.specs[spec.ID] = spec
	}
	return r, nil
}

// Protocols returns the registered protocols in sorted order.
func (r *Registry) Protocols() []Protocol {
	out := make([]Protocol, 0, len(r.codecs))
	for p := range r.codecs {
		out = append(out, p)
	}
	sort.Slice(out, func(i, j int) bool { return out[i] < out[j] })
	return out
}

// Codec returns the codec registered for p.
func (r *Registry) Codec(p Protocol) (Codec, bool) {
	c, ok := r.codecs[p]
	return c, ok
}

// Spec returns the spec registered for p, with an independent copy of Headers.
func (r *Registry) Spec(p Protocol) (ProtocolSpec, bool) {
	s, ok := r.specs[p]
	s.Headers = maps.Clone(s.Headers)
	return s, ok
}

// Kind returns the Kind registered for p.
func (r *Registry) Kind(p Protocol) (Kind, bool) {
	s, ok := r.specs[p]
	return s.Kind, ok
}

func (r *Registry) lookup(p Protocol) (Codec, error) {
	c, ok := r.codecs[p]
	if !ok {
		return nil, Unsupported("unregistered protocol: " + string(p))
	}
	return c, nil
}

// Inspect reads routing information from a client request in protocol p and
// overlays Payload.Metadata. The returned Protocol and Kind always match the
// registered spec.
func (r *Registry) Inspect(p Protocol, req Payload) (RequestInfo, error) {
	c, err := r.lookup(p)
	if err != nil {
		return RequestInfo{}, err
	}
	info, err := c.Inspect(req)
	if err != nil {
		return RequestInfo{}, err
	}
	info.Protocol, info.Kind = p, r.specs[p].Kind
	return req.Metadata.Apply(info), nil
}

// CanConvert reports whether a client request in protocol client can be sent
// to an upstream in protocol upstream: both are registered, share a Kind, and
// the upstream codec accepts the client protocol.
func (r *Registry) CanConvert(client, upstream Protocol) bool {
	cs, ok := r.specs[client]
	if !ok {
		return false
	}
	us, ok := r.specs[upstream]
	if !ok || cs.Kind != us.Kind {
		return false
	}
	return client == upstream || r.codecs[upstream].Accepts(client)
}

// ValidateOptions checks options for exchanges whose upstream speaks
// protocol upstream: the loss policy, and whatever the codec checks when it
// implements OptionsValidator. Routers call it when endpoints are configured.
func (r *Registry) ValidateOptions(upstream Protocol, opts ExchangeOptions) error {
	c, err := r.lookup(upstream)
	if err != nil {
		return err
	}
	if !opts.LossPolicy.valid() {
		return Unsupported("unknown loss policy: " + string(opts.LossPolicy))
	}
	if opts.Output != "" {
		if _, err := r.lookup(opts.Output); err != nil {
			return err
		}
		if !r.CanConvert(opts.Output, upstream) {
			return Unsupported("no response conversion from " + string(upstream) + " to " + string(opts.Output))
		}
	}
	if v, ok := c.(OptionsValidator); ok {
		return v.ValidateOptions(opts)
	}
	return nil
}

// NewExchange creates the state for one execution from client to upstream.
// Responses convert to opts.Output, which defaults to client; the codec then
// always sees Output set.
func (r *Registry) NewExchange(client, upstream Protocol, opts ExchangeOptions) (Exchange, error) {
	if _, err := r.lookup(client); err != nil {
		return nil, err
	}
	c, err := r.lookup(upstream)
	if err != nil {
		return nil, err
	}
	if !r.CanConvert(client, upstream) {
		return nil, Unsupported("no conversion from " + string(client) + " to " + string(upstream))
	}
	if !opts.LossPolicy.valid() {
		return nil, Unsupported("unknown loss policy: " + string(opts.LossPolicy))
	}
	if opts.Output == "" {
		opts.Output = client
	}
	if opts.Output != client {
		if _, err := r.lookup(opts.Output); err != nil {
			return nil, err
		}
		if !r.CanConvert(opts.Output, upstream) {
			return nil, Unsupported("no response conversion from " + string(upstream) + " to " + string(opts.Output))
		}
	}
	return c.NewExchange(client, opts)
}

// EncodeError renders err as an error response in protocol p. Unregistered
// protocols get a minimal JSON body.
func (r *Registry) EncodeError(p Protocol, err error) Payload {
	if c, ok := r.codecs[p]; ok {
		return c.EncodeError(err)
	}
	msg := ""
	if e := AsError(err); e != nil {
		msg = e.Message
	}
	body, _ := json.Marshal(map[string]map[string]string{"error": {"message": msg}})
	return Payload{Body: body, ContentType: "application/json"}
}
