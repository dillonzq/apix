// Package codec adapts the LLM conversion engine in internal/llm/translate to
// apix.Codec. The vendor packages protocol/openai, protocol/anthropic and
// protocol/gemini construct these codecs and translate their public option
// types into Options.
//
// The four generation protocols share one Codec type; their differences at
// the codec boundary are data in the protocols table. Interactions has its
// own codec in interactions.go because it never converts.
package codec

import (
	"encoding/json"

	"github.com/dillonzq/apix"
	"github.com/dillonzq/apix/internal/jsonedit"
	"github.com/dillonzq/apix/internal/llm/ids"
	"github.com/dillonzq/apix/internal/llm/reasoning"
	"github.com/dillonzq/apix/internal/llm/translate"
	"github.com/dillonzq/apix/internal/usage"
	"github.com/dillonzq/apix/internal/vendorerr"
)

// protocol describes how one generation protocol differs at the codec
// boundary.
type protocol struct {
	format  string
	path    string
	auth    apix.AuthScheme
	headers map[string]string
	// urlModel marks protocols that carry the model and streaming mode in the
	// request URL rather than the body (GenerateContent).
	urlModel bool
	envelope vendorerr.Envelope
	// validate checks protocol requirements Inspect enforces beyond JSON shape.
	validate func(root map[string]json.RawMessage) *apix.Error
	// usage names the fields upstream responses report usage in.
	usage usage.Dialect
}

var protocols = map[apix.Protocol]protocol{
	ids.ChatCompletions: {format: ids.FormatChatCompletions, path: "/v1/chat/completions", auth: apix.AuthBearer, envelope: vendorerr.OpenAI,
		usage: usage.ChatCompletions},
	ids.Responses: {format: ids.FormatResponses, path: "/v1/responses", auth: apix.AuthBearer, envelope: vendorerr.OpenAI,
		usage: usage.Responses},
	ids.Messages: {format: ids.FormatMessages, path: "/v1/messages", auth: apix.AuthScheme{Header: "x-api-key"},
		headers: map[string]string{"anthropic-version": "2023-06-01"}, envelope: vendorerr.Anthropic, usage: usage.Messages},
	ids.GenerateContent: {format: ids.FormatGemini, auth: apix.AuthScheme{Header: "x-goog-api-key"}, urlModel: true,
		envelope: vendorerr.Google, validate: validateGemini, usage: usage.Gemini},
}

// Options are the engine options an upstream protocol's package may set.
type Options struct {
	// AllowUnsignedThinking replays cross-protocol reasoning as unsigned
	// Messages thinking blocks (upstream Messages only).
	AllowUnsignedThinking bool
	// MessagesThinkingMode is "manual" for budget thinking (upstream Messages
	// only); empty selects adaptive thinking.
	MessagesThinkingMode string
	// NormalizeResponsesTools adapts native Responses requests for upstreams
	// that only accept function tools (client and upstream Responses only).
	NormalizeResponsesTools bool
	// GeminiThinkingMode is "budget" or "level" (upstream GenerateContent
	// only); empty preserves the client's kind of control.
	GeminiThinkingMode string
	// GeminiIncludeThoughts forces thought visibility (upstream
	// GenerateContent only).
	GeminiIncludeThoughts *bool
	// GeminiRequireThoughtSignatures rejects unsigned historical function
	// calls (upstream GenerateContent only).
	GeminiRequireThoughtSignatures bool
	// ChatStreamUsage asks streamed requests for a final usage chunk with
	// stream_options.include_usage (upstream Chat Completions only).
	ChatStreamUsage bool
}

// ExtFunc converts apix.ExchangeOptions.Ext into Options. It reports false for
// option types or values the protocol does not define. A nil Ext never
// reaches it.
type ExtFunc func(ext any) (Options, bool)

// Codec implements apix.Codec for one generation protocol.
type Codec struct {
	id    apix.Protocol
	proto protocol
	ext   ExtFunc
}

// New returns the codec for a generation protocol ID. ext may be nil when
// the protocol defines no options.
func New(id apix.Protocol, ext ExtFunc) *Codec {
	p, ok := protocols[id]
	if !ok {
		panic("codec: unknown generation protocol " + string(id))
	}
	return &Codec{id: id, proto: p, ext: ext}
}

// Spec returns the protocol description with the vendor's default endpoint.
func (c *Codec) Spec() apix.ProtocolSpec {
	spec := apix.ProtocolSpec{ID: c.id, Kind: apix.KindLLM, Method: "POST", Auth: c.proto.auth, Stream: apix.FramingSSE}
	if len(c.proto.headers) > 0 {
		spec.Headers = make(map[string]string, len(c.proto.headers))
		for k, v := range c.proto.headers {
			spec.Headers[k] = v
		}
	}
	if c.proto.urlModel {
		spec.Path = geminiPath
		spec.ParsePath = parseGeminiPath
		return spec
	}
	path := c.proto.path
	spec.Path = func(apix.RequestInfo, string) string { return path }
	return spec
}

// Accepts reports whether the engine has a direct path between p and this
// protocol. The generation protocols convert in both directions.
func (c *Codec) Accepts(p apix.Protocol) bool {
	_, ok := protocols[p]
	return ok
}

// Inspect reads routing metadata, required capabilities and state references
// from known protocol fields. Opaque tool arguments, schemas and plain text
// are never scanned.
func (c *Codec) Inspect(req apix.Payload) (apix.RequestInfo, error) {
	info, err := inspect(c.id, req.Body)
	if err != nil {
		return apix.RequestInfo{}, err
	}
	return info, nil
}

// EncodeError renders err in the vendor's error envelope.
func (c *Codec) EncodeError(err error) apix.Payload {
	return c.proto.envelope.Encode(err)
}

// ValidateOptions checks options the way NewExchange does, for routers that
// validate endpoints when they are configured.
func (c *Codec) ValidateOptions(opts apix.ExchangeOptions) error {
	if _, err := c.options(opts); err != nil {
		return err
	}
	if opts.Output != "" && !c.Accepts(opts.Output) {
		return apix.Unsupported("no response conversion from " + string(c.id) + " to " + string(opts.Output))
	}
	return nil
}

// options validates the protocol-neutral options and converts Ext.
func (c *Codec) options(opts apix.ExchangeOptions) (Options, *apix.Error) {
	if !validLossPolicy(opts.LossPolicy) {
		return Options{}, apix.Unsupported("unknown loss policy: " + string(opts.LossPolicy))
	}
	if opts.Reasoning != nil {
		if _, err := reasoning.Validate(*opts.Reasoning); err != nil {
			return Options{}, err
		}
	}
	if opts.Ext == nil {
		return Options{}, nil
	}
	if c.ext == nil {
		return Options{}, apix.Unsupported(string(c.id) + " defines no exchange options")
	}
	o, ok := c.ext(opts.Ext)
	if !ok {
		return Options{}, apix.Unsupported("unsupported " + string(c.id) + " exchange options")
	}
	return o, nil
}

func validLossPolicy(p apix.LossPolicy) bool {
	switch p {
	case "", apix.LossPolicyAllow, apix.LossPolicySafe, apix.LossPolicyStrict:
		return true
	}
	return false
}

// NewExchange creates the engine state for one execution from client to this
// protocol, converting responses to opts.Output (client when empty).
func (c *Codec) NewExchange(client apix.Protocol, opts apix.ExchangeOptions) (apix.Exchange, error) {
	from, ok := protocols[client]
	if !ok {
		return nil, apix.Unsupported("no conversion from " + string(client) + " to " + string(c.id))
	}
	output := client
	if opts.Output != "" {
		output = opts.Output
	}
	to, ok := protocols[output]
	if !ok {
		return nil, apix.Unsupported("no response conversion from " + string(c.id) + " to " + string(output))
	}
	o, err := c.options(opts)
	if err != nil {
		return nil, err
	}
	state := translate.NewRequestState()
	state.Options.LossPolicy = translate.LossPolicy(opts.LossPolicy)
	state.Options.MaxStreamFrameBytes = opts.MaxStreamFrameBytes
	state.Options.AllowUnsignedThinking = o.AllowUnsignedThinking
	state.Options.MessagesThinkingMode = o.MessagesThinkingMode
	state.Options.GeminiThinkingMode = o.GeminiThinkingMode
	state.Options.GeminiRequireThoughtSignatures = o.GeminiRequireThoughtSignatures
	if o.GeminiIncludeThoughts != nil {
		value := *o.GeminiIncludeThoughts
		state.Options.GeminiIncludeThoughts = &value
	}
	ex := &exchange{codec: c, clientID: client, client: from, output: to, state: state, normalize: o.NormalizeResponsesTools,
		streamUsage: o.ChatStreamUsage, usage: usage.New(c.proto.usage, opts.MaxStreamFrameBytes)}
	if opts.Reasoning != nil {
		r := *opts.Reasoning
		if r.Budget != nil {
			budget := *r.Budget
			r.Budget = &budget
		}
		ex.reasoning = &r
	}
	// Only a native Responses exchange returns a response ID the client can
	// pass back as previous_response_id.
	if c.id == ids.Responses && output == ids.Responses {
		ex.recorder = &recorder{kind: "response"}
	}
	return ex, nil
}

type exchange struct {
	codec     *Codec
	clientID  apix.Protocol
	client    protocol // request protocol
	output    protocol // response protocol
	state     *translate.RequestState
	normalize bool
	reasoning *apix.Reasoning
	recorder  *recorder
	// streamUsage requests a usage chunk from a streaming Chat Completions
	// upstream.
	streamUsage bool
	usage       *usage.Tracker
}

func (e *exchange) ConvertRequest(req apix.Payload, upstreamModel string) (apix.Payload, error) {
	out, err := e.convertRequest(req, upstreamModel)
	if err != nil {
		return apix.Payload{}, err
	}
	return out, nil
}

// convertRequest converts the client request using metadata overrides and
// exchange options. An empty upstreamModel uses the effective request model.
// URL-based upstreams receive model and stream metadata; streamed requests
// request usage when configured. Inspection, conversion, and option rendering
// errors are returned to the caller.
func (e *exchange) convertRequest(req apix.Payload, upstreamModel string) (apix.Payload, *apix.Error) {
	info, err := routing(e.clientID, req.Body)
	if err != nil {
		return apix.Payload{}, err
	}
	info = req.Metadata.Apply(info)
	if upstreamModel == "" {
		upstreamModel = info.Model
	}
	upstream := e.codec.proto
	if upstream.urlModel && upstreamModel == "" {
		return apix.Payload{}, apix.Translation(string(e.codec.id) + " requires an upstream model in request metadata or ConvertRequest")
	}
	body := req.Body
	if !e.client.urlModel && req.Metadata.Stream != nil {
		if body, err = applyStreamMetadata(body, req.Metadata.Stream); err != nil {
			return apix.Payload{}, err
		}
	}
	if e.normalize && e.client.format == ids.FormatResponses && upstream.format == ids.FormatResponses {
		if body, err = translate.NormalizeResponsesRequest(body, e.state); err != nil {
			return apix.Payload{}, err
		}
	}
	if e.reasoning != nil {
		body = translate.StripReasoning(upstream.format, e.client.format, body)
	}
	out, err := translate.ConvertRequest(upstream.format, upstreamModel, e.client.format, body, e.state)
	if err != nil {
		return apix.Payload{}, err
	}
	// A URL-borne streaming mode becomes a body field for body-borne targets.
	if e.client.urlModel && !upstream.urlModel && (info.Stream || req.Metadata.Stream != nil) {
		if out, err = applyStreamMetadata(out, &info.Stream); err != nil {
			return apix.Payload{}, err
		}
	}
	if e.reasoning != nil {
		if out, err = translate.ApplyReasoning(upstream.format, e.client.format, out, *e.reasoning, e.state); err != nil {
			return apix.Payload{}, err
		}
	}
	if e.streamUsage && info.Stream {
		if out, err = includeUsage(out); err != nil {
			return apix.Payload{}, err
		}
	}
	result := apix.Payload{Body: out, ContentType: "application/json"}
	if upstream.urlModel {
		stream := info.Stream
		result.Metadata = apix.RequestMetadata{Model: upstreamModel, Stream: &stream}
	}
	return result, nil
}

// ConvertResponse converts the upstream response to the output protocol. On
// success it records created state from the output and usage from the upstream
// body. Upstream status and conversion errors propagate without recording
// state or usage from the failed response.
func (e *exchange) ConvertResponse(status int, resp apix.Payload) (apix.Payload, error) {
	out, err := translate.ConvertResponse(e.codec.proto.format, e.output.format, status, resp.Body, e.state)
	if err != nil {
		return apix.Payload{}, err
	}
	e.recorder.body(out)
	e.usage.Body(resp.Body)
	return apix.Payload{Body: out, ContentType: "application/json"}, nil
}

// NewStreamConverter creates a converter to the output protocol that records
// created state and upstream usage on this exchange. It propagates errors
// from constructing the protocol converter.
func (e *exchange) NewStreamConverter() (apix.StreamConverter, error) {
	sc, err := translate.NewStreamConverter(e.codec.proto.format, e.output.format, e.state)
	if err != nil {
		return nil, err
	}
	return &stream{engine: sc, recorder: e.recorder, usage: e.usage}, nil
}

func (e *exchange) Diagnostics() []apix.Diagnostic {
	in := e.state.Diagnostics()
	if len(in) == 0 {
		return nil
	}
	out := make([]apix.Diagnostic, len(in))
	for i, d := range in {
		out[i] = apix.Diagnostic{Code: d.Code, Path: d.Path, Message: d.Message, Semantic: d.Semantic, From: ids.Protocol(d.From), To: ids.Protocol(d.To)}
	}
	return out
}

// CreatedState implements apix.StateReporter.
func (e *exchange) CreatedState() []string { return e.recorder.keys() }

// Usage implements apix.UsageReporter.
func (e *exchange) Usage() (apix.Usage, bool) { return e.usage.Usage() }

// includeUsage sets stream_options.include_usage on a streamed Chat
// Completions request, keeping the client's other stream options.
func includeUsage(body []byte) ([]byte, *apix.Error) {
	if out, ok := jsonedit.Apply(body, jsonedit.SetString("true", "stream_options", "include_usage")); ok {
		return out, nil
	}
	var root map[string]json.RawMessage
	if json.Unmarshal(body, &root) != nil || root == nil {
		return nil, apix.Translation("request must be a JSON object")
	}
	var opts map[string]json.RawMessage
	if raw, ok := root["stream_options"]; ok && json.Unmarshal(raw, &opts) != nil {
		return nil, apix.Translation("stream_options must be an object")
	}
	if opts == nil {
		opts = map[string]json.RawMessage{}
	}
	opts["include_usage"] = json.RawMessage("true")
	root["stream_options"], _ = json.Marshal(opts)
	out, _ := json.Marshal(root)
	return out, nil
}

// applyStreamMetadata sets the body's stream field to a URL-borne value.
func applyStreamMetadata(body []byte, stream *bool) ([]byte, *apix.Error) {
	if stream == nil {
		return body, nil
	}
	value := "false"
	if *stream {
		value = "true"
	}
	if out, ok := jsonedit.Apply(body, jsonedit.SetString(value, "stream")); ok {
		return out, nil
	}
	var root map[string]json.RawMessage
	if json.Unmarshal(body, &root) != nil || root == nil {
		return nil, apix.Translation("request must be a JSON object")
	}
	root["stream"], _ = json.Marshal(*stream)
	out, _ := json.Marshal(root)
	return out, nil
}
