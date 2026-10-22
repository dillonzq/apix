package apix

// Codec implements one protocol. As the client protocol it inspects requests
// and encodes errors; as the upstream protocol it creates exchanges from the
// client protocols it can convert directly.
//
// Conversion paths are direct: an upstream codec lists the protocols it
// converts requests from and responses to. Codecs must be safe for concurrent
// use; exchanges are not.
type Codec interface {
	Spec() ProtocolSpec
	// Inspect reads routing information from a request in this protocol.
	Inspect(req Payload) (RequestInfo, *Error)
	// EncodeError renders err as an error response body in this protocol.
	EncodeError(err *Error) Payload
	// Accepts reports whether a direct path exists between p and this
	// protocol: requests in p convert to this protocol, and this protocol's
	// responses convert to p. Accepts(Spec().ID) is always true.
	Accepts(p Protocol) bool
	// NewExchange creates the state for one execution from client to this
	// protocol, with responses converted to opts.Output (the client protocol
	// when empty). Callers normally use Registry.NewExchange, which validates
	// that both are registered, of the same Kind, and accepted.
	NewExchange(client Protocol, opts ExchangeOptions) (Exchange, *Error)
}

// ExchangeOptions configures one exchange.
type ExchangeOptions struct {
	// LossPolicy selects which known request conversion losses are permitted.
	LossPolicy LossPolicy
	// Output is the protocol responses and streams are converted to. Empty
	// means the client protocol. It must share the client's Kind; hosts that
	// choose the response format separately from the request format set it.
	Output Protocol
	// Ext holds options defined by the upstream protocol's package, such as
	// anthropic.MessagesOptions. Codecs reject option types they do not know.
	Ext any
}

// Exchange converts one execution between a client protocol and an upstream
// protocol and holds its protocol state, such as tool identities and
// diagnostics. Responses are converted to the output protocol, which is the
// client protocol unless ExchangeOptions.Output says otherwise. Use one
// Exchange per execution, sequentially.
type Exchange interface {
	// ConvertRequest converts a client request into an upstream request for
	// upstreamModel. An empty upstreamModel keeps the client's model.
	ConvertRequest(req Payload, upstreamModel string) (Payload, *Error)
	// ConvertResponse converts an ordinary upstream response. For an error
	// status (400 and above) it returns the classified upstream error.
	ConvertResponse(status int, resp Payload) (Payload, *Error)
	// NewStreamConverter converts a streamed upstream response.
	NewStreamConverter() (StreamConverter, *Error)
	// Diagnostics returns a copy of the losses recorded so far.
	Diagnostics() []Diagnostic
}

// StreamConverter accepts arbitrary upstream byte chunks and returns encoded
// client events, each ready to write. It is not safe for concurrent use.
// Always call Finish at EOF: a stream without a valid terminal state fails.
// A call may return events together with an error; write the events, then
// stop. After an error, later calls keep returning it.
type StreamConverter interface {
	Feed(chunk []byte) (events [][]byte, done bool, err *Error)
	Finish() (events [][]byte, err *Error)
}
