// Package tokens implements the apix.KindTokens protocols: Anthropic
// Messages count_tokens, OpenAI Responses input_tokens and Gemini
// countTokens. The vendor packages protocol/anthropic, protocol/openai and
// protocol/gemini construct these codecs.
//
// A count request carries the same content as a generation request, so
// conversion reuses the LLM engine: the client's count body is read as its
// generation request, converted by internal/llm/translate, and reshaped into
// the upstream's count body. The count itself comes from the upstream, in its
// own tokenizer, and only the envelope of the reply is converted. Counts do
// not stream.
package tokens

import (
	"net/url"
	"strings"

	"github.com/dillonzq/apix"
	llmcodec "github.com/dillonzq/apix/internal/llm/codec"
	"github.com/dillonzq/apix/internal/llm/ids"
	"github.com/dillonzq/apix/internal/llm/reasoning"
	"github.com/dillonzq/apix/internal/vendorerr"
)

// Public protocol IDs.
const (
	MessagesCountTokens  apix.Protocol = "anthropic.messages.count-tokens"
	ResponsesInputTokens apix.Protocol = "openai.responses.input-tokens"
	CountTokens          apix.Protocol = "gemini.count-tokens"
)

// protocol describes one count protocol at the codec boundary.
type protocol struct {
	// generation is the LLM protocol whose request body this protocol's
	// request carries.
	generation apix.Protocol
	format     string
	path       string // empty for Gemini, whose path carries the model
	auth       apix.AuthScheme
	headers    map[string]string
	envelope   vendorerr.Envelope
}

var protocols = map[apix.Protocol]protocol{
	MessagesCountTokens: {generation: ids.Messages, format: ids.FormatMessages, path: "/v1/messages/count_tokens",
		auth: apix.AuthScheme{Header: "x-api-key"}, headers: map[string]string{"anthropic-version": "2023-06-01"}, envelope: vendorerr.Anthropic},
	ResponsesInputTokens: {generation: ids.Responses, format: ids.FormatResponses, path: "/v1/responses/input_tokens",
		auth: apix.AuthBearer, envelope: vendorerr.OpenAI},
	CountTokens: {generation: ids.GenerateContent, format: ids.FormatGemini,
		auth: apix.AuthScheme{Header: "x-goog-api-key"}, envelope: vendorerr.Google},
}

// byFormat maps an engine format name back to its count protocol.
func byFormat(format string) apix.Protocol {
	for id, p := range protocols {
		if p.format == format {
			return id
		}
	}
	return apix.Protocol(format)
}

// Codec implements apix.Codec for one count protocol.
type Codec struct {
	id    apix.Protocol
	proto protocol
	// inspector reads the generation request this protocol's body carries.
	inspector apix.Codec
}

// New returns the codec for a count protocol ID.
func New(id apix.Protocol) *Codec {
	p, ok := protocols[id]
	if !ok {
		panic("tokens: unknown protocol " + string(id))
	}
	return &Codec{id: id, proto: p, inspector: llmcodec.New(p.generation, nil)}
}

// Spec returns the protocol description with the vendor's default endpoint.
func (c *Codec) Spec() apix.ProtocolSpec {
	spec := apix.ProtocolSpec{ID: c.id, Kind: apix.KindTokens, Method: "POST", Auth: c.proto.auth, Stream: apix.FramingNone}
	if len(c.proto.headers) > 0 {
		spec.Headers = make(map[string]string, len(c.proto.headers))
		for k, v := range c.proto.headers {
			spec.Headers[k] = v
		}
	}
	if c.proto.path == "" {
		spec.Path = func(_ apix.RequestInfo, model string) string {
			return "/v1beta/models/" + url.PathEscape(strings.TrimPrefix(model, "models/")) + ":countTokens"
		}
		spec.ParsePath = parseGeminiPath
		return spec
	}
	path := c.proto.path
	spec.Path = func(apix.RequestInfo, string) string { return path }
	return spec
}

// parseGeminiPath reads the model from a countTokens URL path.
func parseGeminiPath(path string) (apix.RequestMetadata, error) {
	suffix, ok := strings.CutPrefix(path, "/v1beta/models/")
	if !ok {
		suffix, ok = strings.CutPrefix(path, "/v1/models/")
	}
	model, action, found := strings.Cut(suffix, ":")
	if !ok || !found || model == "" || strings.ContainsAny(model, "/\\?#") || model == ".." {
		return apix.RequestMetadata{}, apix.Unsupported("invalid Gemini model path")
	}
	if action != "countTokens" {
		return apix.RequestMetadata{}, apix.Unsupported("unsupported Gemini operation")
	}
	stream := false
	return apix.RequestMetadata{Model: model, Stream: &stream}, nil
}

// Accepts reports whether a direct path exists between p and this protocol.
// The count protocols convert in both directions.
func (c *Codec) Accepts(p apix.Protocol) bool {
	_, ok := protocols[p]
	return ok
}

// Inspect reads the model, input features and state references the way the
// generation codec does. Counts never stream.
func (c *Codec) Inspect(req apix.Payload) (apix.RequestInfo, error) {
	r, err := decodeRequest(c.id, req.Body)
	if err != nil {
		return apix.RequestInfo{}, err
	}
	info, ierr := c.inspector.Inspect(apix.Payload{Body: r.body})
	if ierr != nil {
		return apix.RequestInfo{}, ierr
	}
	info.Kind, info.Protocol, info.Stream = apix.KindTokens, c.id, false
	if r.model != "" {
		info.Model = r.model
	}
	features := info.Features[:0:0]
	for _, f := range info.Features {
		if f != apix.FeatureStream {
			features = append(features, f)
		}
	}
	info.Features = features
	return info, nil
}

// EncodeError renders err in the vendor's error envelope.
func (c *Codec) EncodeError(err error) apix.Payload { return c.proto.envelope.Encode(err) }

// ValidateOptions checks options the way NewExchange does.
func (c *Codec) ValidateOptions(opts apix.ExchangeOptions) error {
	if err := c.options(opts); err != nil {
		return err
	}
	return nil
}

// options validates the protocol-neutral options. A reasoning override is
// validated and then ignored, because it does not change the input: hosts
// that parse model(high) suffixes can count those requests too.
func (c *Codec) options(opts apix.ExchangeOptions) *apix.Error {
	switch opts.LossPolicy {
	case "", apix.LossPolicyAllow, apix.LossPolicySafe, apix.LossPolicyStrict:
	default:
		return apix.Unsupported("unknown loss policy: " + string(opts.LossPolicy))
	}
	if opts.Output != "" && !c.Accepts(opts.Output) {
		return apix.Unsupported("no response conversion from " + string(c.id) + " to " + string(opts.Output))
	}
	if opts.Reasoning != nil {
		if _, err := reasoning.Validate(*opts.Reasoning); err != nil {
			return err
		}
	}
	if opts.Ext != nil {
		return apix.Unsupported(string(c.id) + " defines no exchange options")
	}
	return nil
}

// NewExchange creates the state for one execution from client to this
// protocol, converting responses to opts.Output (client when empty).
func (c *Codec) NewExchange(client apix.Protocol, opts apix.ExchangeOptions) (apix.Exchange, error) {
	from, ok := protocols[client]
	if !ok {
		return nil, apix.Unsupported("no conversion from " + string(client) + " to " + string(c.id))
	}
	if err := c.options(opts); err != nil {
		return nil, err
	}
	output := client
	if opts.Output != "" {
		output = opts.Output
	}
	return newExchange(client, from, c.id, c.proto, protocols[output], opts.LossPolicy), nil
}
