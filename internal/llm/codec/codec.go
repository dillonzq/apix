// Package codec adapts the LLM conversion engine in internal/llm/translate to
// apix.Codec. The vendor packages protocol/openai, protocol/anthropic and protocol/gemini
// construct these codecs and translate their public option types into
// Options.
package codec

import (
	"bytes"
	"encoding/json"
	"github.com/dillonzq/apix/internal/llm/adapter/interactions"
	"github.com/dillonzq/apix/internal/llm/adapter/shared"
	"net/url"
	"strings"

	"github.com/dillonzq/apix"
	"github.com/dillonzq/apix/internal/llm/translate"
)

// Public protocol IDs. The vendor packages declare the same values; a test
// keeps them in sync.
const (
	ChatCompletions apix.Protocol = "openai.chat-completions"
	Responses       apix.Protocol = "openai.responses"
	Messages        apix.Protocol = "anthropic.messages"
	Gemini          apix.Protocol = "gemini.generate-content"
	Interactions    apix.Protocol = "gemini.interactions"
)

// formats maps public protocol IDs to the engine's internal format names.
var formats = map[apix.Protocol]string{
	ChatCompletions: translate.FormatChatCompletions,
	Responses:       translate.FormatResponses,
	Messages:        translate.FormatMessages,
	Gemini:          translate.FormatGemini,
	Interactions:    interactions.Format,
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
	// MessagesThinkingMode is "manual" for budget thinking (upstream Messages
	// only); empty selects adaptive thinking.
	MessagesThinkingMode string
	// NormalizeResponsesTools adapts native Responses requests for upstreams
	// that only accept function tools (client and upstream Responses only).
	NormalizeResponsesTools        bool
	GeminiThinkingMode             string
	GeminiIncludeThoughts          *bool
	GeminiRequireThoughtSignatures bool
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
	case Gemini:
		spec.Auth = apix.AuthScheme{Header: "x-goog-api-key"}
		spec.Path = func(info apix.RequestInfo, model string) string {
			action := "generateContent"
			if info.Stream {
				action = "streamGenerateContent?alt=sse"
			}
			return "/v1beta/models/" + url.PathEscape(strings.TrimPrefix(model, "models/")) + ":" + action
		}
		spec.ParsePath = parseGeminiPath
		return spec
	case Interactions:
		path = "/v1beta/interactions"
		spec.Auth = apix.AuthScheme{Header: "x-goog-api-key"}
	case Messages:
		path = "/v1/messages"
		spec.Auth = apix.AuthScheme{Header: "x-api-key"}
		spec.Headers = map[string]string{"anthropic-version": "2023-06-01"}
	}
	spec.Path = func(apix.RequestInfo, string) string { return path }
	return spec
}

// Accepts reports whether the engine has a direct path between p and this
// protocol. The four generation protocols convert in both directions; Interactions is native-only.
func (c *Codec) Accepts(p apix.Protocol) bool {
	_, ok := formats[p]
	return ok && (p == c.id || c.id != Interactions && p != Interactions)
}

// Inspect reads routing metadata and required capabilities from known protocol
// fields. Opaque tool arguments, schemas and plain text are never scanned.
func (c *Codec) Inspect(req apix.Payload) (apix.RequestInfo, *apix.Error) {
	return inspectRequest(c.id, req)
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
	info.Features = requiredFeatures(id, body, info.Stream)
	return info, nil
}

// EncodeError renders err in the vendor's error envelope.
func (c *Codec) EncodeError(err *apix.Error) apix.Payload {
	if err == nil {
		err = apix.UpstreamFallback("unknown error")
	}
	var doc any
	if c.id == Gemini || c.id == Interactions {
		doc = map[string]any{"error": map[string]any{"code": apix.HTTPStatus(err), "message": err.Message, "status": geminiErrorType(err)}}
	} else if c.id == Messages {
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
	if !ok || !c.Accepts(client) {
		return nil, apix.Unsupported("no conversion from " + string(client) + " to " + string(c.id))
	}
	to := from
	if opts.Output != "" {
		if to, ok = formats[opts.Output]; !ok || !c.Accepts(opts.Output) {
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
	state.Options.MessagesThinkingMode = o.MessagesThinkingMode
	state.Options.GeminiThinkingMode = o.GeminiThinkingMode
	state.Options.GeminiRequireThoughtSignatures = o.GeminiRequireThoughtSignatures
	if o.GeminiIncludeThoughts != nil {
		value := *o.GeminiIncludeThoughts
		state.Options.GeminiIncludeThoughts = &value
	}
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
	info, err := inspectRequest(e.clientID, req)
	if err != nil {
		return apix.Payload{}, err
	}
	if upstreamModel == "" {
		upstreamModel = info.Model
	}
	if e.codec.id == Gemini && upstreamModel == "" {
		return apix.Payload{}, apix.Translation("Gemini requires an upstream model in request metadata or ConvertRequest")
	}
	body := req.Body
	if e.clientID == Interactions {
		var doc map[string]json.RawMessage
		if json.Unmarshal(body, &doc) != nil {
			return apix.Payload{}, apix.Translation("invalid Interactions request")
		}
		if shared.HasContent(doc["agent"]) {
			if upstreamModel != "" && upstreamModel != info.Model {
				return apix.Payload{}, apix.Unsupported("Interactions agent cannot be rewritten as a model")
			}
		} else {
			body, err = shared.RewriteModelID(upstreamModel, body, interactions.Format)
			if err != nil {
				return apix.Payload{}, err
			}
		}
		body, err = applyStreamMetadata(body, req.Metadata.Stream)
		return apix.Payload{Body: body, ContentType: "application/json"}, err
	}
	if e.clientID != Gemini && req.Metadata.Stream != nil {
		body, err = applyStreamMetadata(body, req.Metadata.Stream)
		if err != nil {
			return apix.Payload{}, err
		}
	}
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
	if e.clientID == Gemini && e.codec.id != Gemini && (info.Stream || req.Metadata.Stream != nil) {
		out, err = applyStreamMetadata(out, &info.Stream)
		if err != nil {
			return apix.Payload{}, err
		}
	}
	result := apix.Payload{Body: out, ContentType: "application/json"}
	if e.codec.id == Gemini {
		result.Metadata = apix.RequestMetadata{Model: upstreamModel, Stream: &info.Stream}
	}
	return result, nil
}

func (e *exchange) ConvertResponse(status int, resp apix.Payload) (apix.Payload, *apix.Error) {
	if e.codec.id == Interactions {
		body, err := interactions.ConvertResponse(status, resp.Body)
		return apix.Payload{Body: body, ContentType: "application/json"}, err
	}
	out, err := translate.ConvertResponse(e.codec.format, e.output, status, resp.Body, e.state)
	if err != nil {
		return apix.Payload{}, err
	}
	return apix.Payload{Body: out, ContentType: "application/json"}, nil
}

func (e *exchange) NewStreamConverter() (apix.StreamConverter, *apix.Error) {
	if e.codec.id == Interactions {
		return interactions.NewStreamConverter(), nil
	}
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
