// Package codec adapts the LLM conversion engine in internal/llm/translate to
// apix.Codec. The vendor packages protocol/openai and protocol/anthropic
// construct these codecs and translate their public option types into
// Options.
package codec

import (
	"bytes"
	"encoding/json"

	"github.com/dillonzq/apix"
	"github.com/dillonzq/apix/internal/llm/translate"
)

// Public protocol IDs. The vendor packages declare the same values; a test
// keeps them in sync.
const (
	ChatCompletions apix.Protocol = "openai.chat-completions"
	Responses       apix.Protocol = "openai.responses"
	Messages        apix.Protocol = "anthropic.messages"
)

// formats maps public protocol IDs to the engine's internal format names.
var formats = map[apix.Protocol]string{
	ChatCompletions: translate.FormatChatCompletions,
	Responses:       translate.FormatResponses,
	Messages:        translate.FormatMessages,
}

// protocolOf maps an internal format name back to its public protocol ID.
func protocolOf(format string) apix.Protocol {
	for p, f := range formats {
		if f == format {
			return p
		}
	}
	return apix.Protocol(format)
}

// Options are the engine options an upstream protocol's package may set.
type Options struct {
	// AllowUnsignedThinking replays cross-protocol reasoning as unsigned
	// Messages thinking blocks (upstream Messages only).
	AllowUnsignedThinking bool
	// NormalizeResponsesTools adapts native Responses requests for upstreams
	// that only accept function tools (client and upstream Responses only).
	NormalizeResponsesTools bool
}

// ExtFunc converts apix.ExchangeOptions.Ext into Options. It reports false for
// option types the protocol does not define. A nil Ext never reaches it.
type ExtFunc func(ext any) (Options, bool)

// Codec implements apix.Codec for one LLM protocol.
type Codec struct {
	id     apix.Protocol
	format string
	ext    ExtFunc
}

// New returns the codec for one of the LLM protocol IDs above. ext may be
// nil when the protocol defines no options.
func New(id apix.Protocol, ext ExtFunc) *Codec {
	format, ok := formats[id]
	if !ok {
		panic("codec: unknown LLM protocol " + string(id))
	}
	return &Codec{id: id, format: format, ext: ext}
}

// Spec returns the protocol description with the vendor's default endpoint.
func (c *Codec) Spec() apix.ProtocolSpec {
	spec := apix.ProtocolSpec{ID: c.id, Kind: apix.KindLLM, Method: "POST", Auth: apix.AuthBearer, Stream: apix.FramingSSE}
	path := ""
	switch c.id {
	case ChatCompletions:
		path = "/v1/chat/completions"
	case Responses:
		path = "/v1/responses"
	case Messages:
		path = "/v1/messages"
		spec.Auth = apix.AuthScheme{Header: "x-api-key"}
		spec.Headers = map[string]string{"anthropic-version": "2023-06-01"}
	}
	spec.Path = func(apix.RequestInfo, string) string { return path }
	return spec
}

// Accepts reports whether the engine has a direct path between p and this
// protocol. All three LLM protocols convert to one another in both directions.
func (c *Codec) Accepts(p apix.Protocol) bool {
	_, ok := formats[p]
	return ok
}

// Inspect reads model, stream, and tool use from a JSON request. All three
// protocols carry these as top-level "model", "stream", and "tools" fields.
func (c *Codec) Inspect(req apix.Payload) (apix.RequestInfo, *apix.Error) {
	return inspect(c.id, req.Body)
}

func inspect(id apix.Protocol, body []byte) (apix.RequestInfo, *apix.Error) {
	var doc struct {
		Model  json.RawMessage `json:"model"`
		Stream json.RawMessage `json:"stream"`
		Tools  json.RawMessage `json:"tools"`
	}
	if err := json.Unmarshal(body, &doc); err != nil {
		return apix.RequestInfo{}, apix.Translation("malformed " + string(id) + " request JSON: " + err.Error())
	}
	info := apix.RequestInfo{Kind: apix.KindLLM, Protocol: id}
	if len(doc.Model) > 0 && json.Unmarshal(doc.Model, &info.Model) != nil {
		return apix.RequestInfo{}, apix.Translation(string(id) + " request model must be a string")
	}
	if len(doc.Stream) > 0 && !bytes.Equal(doc.Stream, []byte("null")) && json.Unmarshal(doc.Stream, &info.Stream) != nil {
		return apix.RequestInfo{}, apix.Translation(string(id) + " request stream must be a boolean")
	}
	if info.Stream {
		info.Features = append(info.Features, apix.FeatureStream)
	}
	var tools []json.RawMessage
	if json.Unmarshal(doc.Tools, &tools) == nil && len(tools) > 0 {
		info.Features = append(info.Features, apix.FeatureTools)
	}
	return info, nil
}

// EncodeError renders err in the vendor's error envelope.
func (c *Codec) EncodeError(err *apix.Error) apix.Payload {
	if err == nil {
		err = apix.UpstreamFallback("unknown error")
	}
	var doc any
	if c.id == Messages {
		doc = map[string]any{"type": "error", "error": map[string]string{"type": anthropicErrorType(err), "message": err.Message}}
	} else {
		doc = map[string]any{"error": map[string]any{"message": err.Message, "type": openAIErrorType(err), "code": string(err.Class)}}
	}
	body, _ := json.Marshal(doc)
	return apix.Payload{Body: body, ContentType: "application/json"}
}

func openAIErrorType(err *apix.Error) string {
	switch err.Class {
	case apix.ClassAuth:
		return "authentication_error"
	case apix.ClassRateLimit:
		return "rate_limit_error"
	case apix.ClassQuota, apix.ClassBilling:
		return "insufficient_quota"
	case apix.ClassUpstream, apix.ClassNetwork:
		return "server_error"
	default:
		return "invalid_request_error"
	}
}

func anthropicErrorType(err *apix.Error) string {
	switch err.Class {
	case apix.ClassAuth:
		return "authentication_error"
	case apix.ClassInvalidModel:
		return "not_found_error"
	case apix.ClassRateLimit, apix.ClassQuota:
		return "rate_limit_error"
	case apix.ClassBilling:
		return "billing_error"
	case apix.ClassUpstream, apix.ClassNetwork:
		return "api_error"
	default:
		return "invalid_request_error"
	}
}

// NewExchange creates the engine state for one execution from client to this
// protocol, converting responses to opts.Output (client when empty).
func (c *Codec) NewExchange(client apix.Protocol, opts apix.ExchangeOptions) (apix.Exchange, *apix.Error) {
	from, ok := formats[client]
	if !ok {
		return nil, apix.Unsupported("no conversion from " + string(client) + " to " + string(c.id))
	}
	to := from
	if opts.Output != "" {
		if to, ok = formats[opts.Output]; !ok {
			return nil, apix.Unsupported("no response conversion from " + string(c.id) + " to " + string(opts.Output))
		}
	}
	var o Options
	if opts.Ext != nil {
		if c.ext == nil {
			return nil, apix.Unsupported(string(c.id) + " defines no exchange options")
		}
		if o, ok = c.ext(opts.Ext); !ok {
			return nil, apix.Unsupported("unsupported " + string(c.id) + " exchange options")
		}
	}
	state := translate.NewRequestState()
	state.Options.LossPolicy = translate.LossPolicy(opts.LossPolicy)
	state.Options.AllowUnsignedThinking = o.AllowUnsignedThinking
	return &exchange{codec: c, clientID: client, client: from, output: to, state: state, normalize: o.NormalizeResponsesTools}, nil
}

type exchange struct {
	codec     *Codec
	clientID  apix.Protocol
	client    string // request format
	output    string // response format
	state     *translate.RequestState
	normalize bool
}

func (e *exchange) ConvertRequest(req apix.Payload, upstreamModel string) (apix.Payload, *apix.Error) {
	if upstreamModel == "" {
		info, err := inspect(e.clientID, req.Body)
		if err != nil {
			return apix.Payload{}, err
		}
		upstreamModel = info.Model
	}
	body := req.Body
	if e.normalize && e.client == translate.FormatResponses && e.codec.format == translate.FormatResponses {
		var err *apix.Error
		if body, err = translate.NormalizeResponsesRequest(body, e.state); err != nil {
			return apix.Payload{}, err
		}
	}
	out, err := translate.ConvertRequest(e.codec.format, upstreamModel, e.client, body, e.state)
	if err != nil {
		return apix.Payload{}, err
	}
	return apix.Payload{Body: out, ContentType: "application/json"}, nil
}

func (e *exchange) ConvertResponse(status int, resp apix.Payload) (apix.Payload, *apix.Error) {
	out, err := translate.ConvertResponse(e.codec.format, e.output, status, resp.Body, e.state)
	if err != nil {
		return apix.Payload{}, err
	}
	return apix.Payload{Body: out, ContentType: "application/json"}, nil
}

func (e *exchange) NewStreamConverter() (apix.StreamConverter, *apix.Error) {
	return translate.NewStreamConverter(e.codec.format, e.output, e.state)
}

func (e *exchange) Diagnostics() []apix.Diagnostic {
	in := e.state.Diagnostics()
	if len(in) == 0 {
		return nil
	}
	out := make([]apix.Diagnostic, len(in))
	for i, d := range in {
		out[i] = apix.Diagnostic{Code: d.Code, Path: d.Path, Message: d.Message, Semantic: d.Semantic, From: protocolOf(d.From), To: protocolOf(d.To)}
	}
	return out
}
