// Package image implements the apix.KindImage protocols: OpenAI image
// generations and edits, Gemini GenerateContent with image models, and Gemini
// Imagen. The vendor packages protocol/openai and protocol/gemini construct
// these codecs.
//
// Conversion paths are direct: each request converter reads one source wire
// type and writes one target wire type, and each response converter does the
// same in reverse. Cross-protocol streams are emulated: the upstream is called
// without streaming and the converted response is rendered as the output
// protocol's complete event stream. Streams pass through, validated, only when
// the upstream already speaks the output protocol.
package image

import (
	"net/url"
	"slices"
	"strings"

	"github.com/dillonzq/apix"
	"github.com/dillonzq/apix/internal/vendorerr"
)

// Public protocol IDs.
const (
	OpenAIGenerations apix.Protocol = "openai.images.generations"
	OpenAIEdits       apix.Protocol = "openai.images.edits"
	GeminiImage       apix.Protocol = "gemini.generate-content.image"
	Imagen            apix.Protocol = "gemini.imagen"
)

// family groups protocols that share wire types.
type family int

const (
	familyOpenAI family = iota // generations and edits share request fields and the response
	familyGemini               // GenerateContent with an image model
	familyImagen               // Imagen :predict
)

// protocol describes one image protocol at the codec boundary.
type protocol struct {
	family   family
	edit     bool // OpenAI edits: input images are required
	path     string
	auth     apix.AuthScheme
	envelope vendorerr.Envelope
	stream   apix.Framing
	// accepts lists the other protocols with a direct path to and from this one.
	accepts []apix.Protocol
}

var protocols = map[apix.Protocol]protocol{
	OpenAIGenerations: {family: familyOpenAI, path: "/v1/images/generations", auth: apix.AuthBearer, envelope: vendorerr.OpenAI,
		stream: apix.FramingSSE, accepts: []apix.Protocol{GeminiImage, Imagen}},
	OpenAIEdits: {family: familyOpenAI, edit: true, path: "/v1/images/edits", auth: apix.AuthBearer, envelope: vendorerr.OpenAI,
		stream: apix.FramingSSE, accepts: []apix.Protocol{GeminiImage}},
	GeminiImage: {family: familyGemini, auth: apix.AuthScheme{Header: "x-goog-api-key"}, envelope: vendorerr.Google,
		stream: apix.FramingSSE, accepts: []apix.Protocol{OpenAIGenerations, OpenAIEdits, Imagen}},
	Imagen: {family: familyImagen, auth: apix.AuthScheme{Header: "x-goog-api-key"}, envelope: vendorerr.Google,
		stream: apix.FramingNone, accepts: []apix.Protocol{OpenAIGenerations, GeminiImage}},
}

// urlModel reports whether the protocol carries the model in its URL.
func (p protocol) urlModel() bool { return p.family != familyOpenAI }

// Options are the exchange options an image protocol's package may set.
type Options struct {
	// ResponseModalities sets the responseModalities of every request to a
	// GenerateContentImage upstream: it replaces ["IMAGE"] on converted
	// requests and the client's value on native ones.
	ResponseModalities []string
}

// ExtFunc converts apix.ExchangeOptions.Ext into Options. It reports false
// for option types or values the protocol does not define. A nil Ext never
// reaches it.
type ExtFunc func(ext any) (Options, bool)

// Codec implements apix.Codec for one image protocol.
type Codec struct {
	id    apix.Protocol
	proto protocol
	ext   ExtFunc
}

// New returns the codec for an image protocol ID. ext may be nil when the
// protocol defines no options.
func New(id apix.Protocol, ext ExtFunc) *Codec {
	p, ok := protocols[id]
	if !ok {
		panic("image: unknown protocol " + string(id))
	}
	return &Codec{id: id, proto: p, ext: ext}
}

// Spec returns the protocol description with the vendor's default endpoint.
func (c *Codec) Spec() apix.ProtocolSpec {
	spec := apix.ProtocolSpec{ID: c.id, Kind: apix.KindImage, Method: "POST", Auth: c.proto.auth, Stream: c.proto.stream}
	switch c.proto.family {
	case familyGemini:
		spec.Path = func(info apix.RequestInfo, model string) string {
			if info.Stream {
				return modelPath(model, "streamGenerateContent?alt=sse")
			}
			return modelPath(model, "generateContent")
		}
		spec.ParsePath = func(path string) (apix.RequestMetadata, error) {
			return parseModelPath(path, map[string]bool{"generateContent": false, "streamGenerateContent": true})
		}
	case familyImagen:
		spec.Path = func(_ apix.RequestInfo, model string) string { return modelPath(model, "predict") }
		spec.ParsePath = func(path string) (apix.RequestMetadata, error) {
			return parseModelPath(path, map[string]bool{"predict": false})
		}
	default:
		path := c.proto.path
		spec.Path = func(apix.RequestInfo, string) string { return path }
	}
	return spec
}

func modelPath(model, action string) string {
	return "/v1beta/models/" + url.PathEscape(strings.TrimPrefix(model, "models/")) + ":" + action
}

// parseModelPath reads the model and streaming mode from a Google URL path;
// actions maps each accepted action to its streaming mode.
func parseModelPath(path string, actions map[string]bool) (apix.RequestMetadata, error) {
	suffix, ok := strings.CutPrefix(path, "/v1beta/models/")
	if !ok {
		suffix, ok = strings.CutPrefix(path, "/v1/models/")
	}
	model, action, found := strings.Cut(suffix, ":")
	if !ok || !found || model == "" || strings.ContainsAny(model, "/\\?#") || model == ".." {
		return apix.RequestMetadata{}, apix.Unsupported("invalid Gemini model path")
	}
	stream, ok := actions[action]
	if !ok {
		return apix.RequestMetadata{}, apix.Unsupported("unsupported Gemini operation")
	}
	return apix.RequestMetadata{Model: model, Stream: &stream}, nil
}

// Accepts reports whether a direct path exists between p and this protocol.
func (c *Codec) Accepts(p apix.Protocol) bool {
	return p == c.id || slices.Contains(c.proto.accepts, p)
}

// Inspect reads the model, streaming mode, input features and file
// references from the request.
func (c *Codec) Inspect(req apix.Payload) (apix.RequestInfo, error) {
	r, err := decodeRequest(c.id, req)
	if err != nil {
		return apix.RequestInfo{}, err
	}
	return r.info(), nil
}

// EncodeError renders err in the vendor's error envelope.
func (c *Codec) EncodeError(err error) apix.Payload { return c.proto.envelope.Encode(err) }

// ValidateOptions checks options the way NewExchange does.
func (c *Codec) ValidateOptions(opts apix.ExchangeOptions) error {
	if _, err := c.options(opts); err != nil {
		return err
	}
	return nil
}

// options validates the protocol-neutral options and converts Ext.
func (c *Codec) options(opts apix.ExchangeOptions) (Options, *apix.Error) {
	switch {
	case !validLossPolicy(opts.LossPolicy):
		return Options{}, apix.Unsupported("unknown loss policy: " + string(opts.LossPolicy))
	case opts.Output != "" && !c.Accepts(opts.Output):
		return Options{}, apix.Unsupported("no response conversion from " + string(c.id) + " to " + string(opts.Output))
	case opts.Reasoning != nil:
		return Options{}, apix.Unsupported(string(c.id) + " does not support reasoning overrides")
	case opts.Ext == nil:
		return Options{}, nil
	case c.ext == nil:
		return Options{}, apix.Unsupported(string(c.id) + " defines no exchange options")
	}
	o, ok := c.ext(opts.Ext)
	if !ok {
		return Options{}, apix.Unsupported("unsupported " + string(c.id) + " exchange options")
	}
	o.ResponseModalities = slices.Clone(o.ResponseModalities)
	return o, nil
}

func validLossPolicy(p apix.LossPolicy) bool {
	switch p {
	case "", apix.LossPolicyAllow, apix.LossPolicySafe, apix.LossPolicyStrict:
		return true
	}
	return false
}

// NewExchange creates the state for one execution from client to this
// protocol, converting responses to opts.Output (client when empty).
func (c *Codec) NewExchange(client apix.Protocol, opts apix.ExchangeOptions) (apix.Exchange, error) {
	if !c.Accepts(client) {
		return nil, apix.Unsupported("no conversion from " + string(client) + " to " + string(c.id))
	}
	o, err := c.options(opts)
	if err != nil {
		return nil, err
	}
	output := client
	if opts.Output != "" {
		output = opts.Output
	}
	return &exchange{client: client, upstream: c.id, output: output, policy: opts.LossPolicy, options: o}, nil
}
