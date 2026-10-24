// Package apix converts API requests, responses, and streams between provider
// protocols and routes them across providers.
//
// The root package is protocol neutral. Protocols are implemented by codecs in
// the protocol/<vendor> packages and combined in an explicit Registry. A
// Registry creates one Exchange per execution: the Exchange converts the client
// request to the upstream protocol and the upstream response to the selected
// output protocol (the client protocol by default). Package route chooses
// upstream endpoints, preferring the client's own protocol, and package gateway
// optionally executes requests over HTTP with failover. The core packages
// perform no network I/O.
package apix

// Kind groups protocols that describe the same kind of operation, such as LLM
// generation or image generation. Conversion only happens within one Kind.
type Kind string

const (
	// KindLLM is text and multimodal generation.
	KindLLM Kind = "llm"
	// KindImage is image generation and editing.
	KindImage Kind = "image"
	// KindAudio is reserved for speech protocols; no built-in codec
	// implements it yet.
	KindAudio Kind = "audio"
)

// Protocol identifies one wire protocol with a vendor-namespaced name, for
// example "openai.chat-completions" or "anthropic.messages".
type Protocol string

// Payload is a protocol message body. Bodies are not assumed to be JSON:
// ContentType distinguishes JSON, multipart forms, and binary media. An empty
// ContentType means the protocol's default.
type Payload struct {
	Body        []byte
	ContentType string
	// Metadata carries request information that may live outside the body,
	// for example the model and operation in a Gemini URL. It is ignored on
	// responses. Metadata takes precedence over body fields when supplied.
	Metadata RequestMetadata
}

// RequestMetadata supplements a request body without coupling codecs to HTTP.
// Empty Model and nil Stream leave the corresponding body values unchanged.
type RequestMetadata struct {
	Model  string
	Stream *bool
}

// Apply overlays routing values while retaining all non-stream requirements.
// It does not mutate the input Features slice and is safe to apply twice.
func (m RequestMetadata) Apply(info RequestInfo) RequestInfo {
	if m.Model != "" {
		info.Model = m.Model
	}
	if m.Stream != nil {
		info.Stream = *m.Stream
		features := make([]string, 0, len(info.Features)+1)
		if info.Stream {
			features = append(features, FeatureStream)
		}
		for _, f := range info.Features {
			if f != FeatureStream {
				features = append(features, f)
			}
		}
		info.Features = features
	}
	return info
}

// Feature names a capability a request needs and an endpoint may support.
const (
	FeatureStream           = "stream"
	FeatureTools            = "tools"
	FeatureImageInput       = "image-input"
	FeatureFileInput        = "file-input"
	FeatureAudioInput       = "audio-input"
	FeatureVideoInput       = "video-input"
	FeatureImageOutput      = "image-output"
	FeatureAudioOutput      = "audio-output"
	FeatureVideoOutput      = "video-output"
	FeatureStructuredOutput = "structured-output"
	FeatureWebSearch        = "web-search"
)

// RequestInfo describes a client request for routing. Codecs fill it from the
// request body with Codec.Inspect.
type RequestInfo struct {
	Kind     Kind
	Protocol Protocol
	Model    string
	Stream   bool
	// Features lists capabilities the request needs, such as FeatureStream.
	Features []string
	// Affinity lists keys of upstream-owned state the request refers to, such
	// as "response:resp_1" or "file:file-1". Routers prefer the endpoint that
	// owns the state; see StateReporter.
	Affinity []string
	// Stateful reports that the request continues upstream-owned context,
	// such as a previous response, that conversion to another protocol would
	// drop. Routers send such requests only to endpoints of the client's
	// protocol.
	Stateful bool
}

// Needs reports whether the request needs feature.
func (i RequestInfo) Needs(feature string) bool {
	for _, f := range i.Features {
		if f == feature {
			return true
		}
	}
	return false
}

// Reasoning overrides an LLM request's reasoning control, for example from a
// "model(high)" suffix parsed with package thinking. Codecs of other Kinds
// reject it.
type Reasoning struct {
	// Effort is none, auto, minimal, low, medium, high, xhigh, or max.
	Effort string
	// Budget is an exact token budget, for upstreams whose control is a
	// budget. Set it with or instead of Effort; -1 means auto and 0 none.
	Budget *int64
}

// AuthScheme describes how an API key is sent: the header name and a prefix
// placed before the key.
type AuthScheme struct {
	Header string
	Prefix string
}

// AuthBearer sends the key as "Authorization: Bearer <key>".
var AuthBearer = AuthScheme{Header: "Authorization", Prefix: "Bearer "}

// Framing describes how a protocol frames streamed responses.
type Framing int

const (
	// FramingNone means the protocol does not stream.
	FramingNone Framing = iota
	// FramingSSE streams Server-Sent Events.
	FramingSSE
	// FramingBinary streams raw bytes, such as audio chunks.
	FramingBinary
)

// ProtocolSpec describes a protocol. The HTTP fields are hints for executors
// such as package gateway; the core never performs I/O.
type ProtocolSpec struct {
	ID   Protocol
	Kind Kind
	// Method is the HTTP method; empty means POST.
	Method string
	// Path returns the request path for an upstream call. It receives the
	// upstream model so protocols that carry the model in the path can use it.
	Path func(info RequestInfo, upstreamModel string) string
	// ParsePath optionally extracts request metadata from an HTTP URL path.
	// Handler calls it before Inspect and replies with a returned *Error in
	// the protocol's envelope. Codecs themselves perform no I/O.
	ParsePath func(path string) (RequestMetadata, error)
	// Auth is the default API key scheme; endpoints may override it.
	Auth AuthScheme
	// Headers are fixed headers every request carries, such as a version.
	Headers map[string]string
	// Stream is how streamed responses are framed.
	Stream Framing
}
