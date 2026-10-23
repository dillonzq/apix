package codec

import (
	"encoding/json"

	"github.com/dillonzq/apix"
	"github.com/dillonzq/apix/internal/llm/adapter/interactions"
	"github.com/dillonzq/apix/internal/llm/adapter/shared"
	"github.com/dillonzq/apix/internal/llm/ids"
)

// InteractionsCodec implements the native Gemini Interactions protocol. It
// validates requests, responses and streams and rewrites model aliases, but
// converts to no other protocol; stored interactions stay upstream-owned.
type InteractionsCodec struct{}

// NewInteractions returns the Interactions codec.
func NewInteractions() *InteractionsCodec { return &InteractionsCodec{} }

// Spec describes POST /v1beta/interactions with the x-goog-api-key header.
func (*InteractionsCodec) Spec() apix.ProtocolSpec {
	return apix.ProtocolSpec{ID: ids.Interactions, Kind: apix.KindLLM, Method: "POST", Auth: apix.AuthScheme{Header: "x-goog-api-key"}, Stream: apix.FramingSSE,
		Path: func(apix.RequestInfo, string) string { return "/v1beta/interactions" }}
}

// Accepts reports true only for Interactions itself.
func (*InteractionsCodec) Accepts(p apix.Protocol) bool { return p == ids.Interactions }

// Inspect requires exactly one of model or agent; an agent routes as the model.
func (*InteractionsCodec) Inspect(req apix.Payload) (apix.RequestInfo, error) {
	info, err := inspectInteractions(req.Body)
	if err != nil {
		return apix.RequestInfo{}, err
	}
	return info, nil
}

func inspectInteractions(body []byte) (apix.RequestInfo, *apix.Error) {
	info, err := inspect(ids.Interactions, body)
	if err != nil {
		return info, err
	}
	var root map[string]json.RawMessage
	if json.Unmarshal(body, &root) != nil || root == nil {
		return info, apix.Translation("request must be a JSON object")
	}
	var agent string
	if root["agent"] != nil && json.Unmarshal(root["agent"], &agent) != nil {
		return info, apix.Translation("agent must be a string")
	}
	if (info.Model == "") == (agent == "") {
		return info, apix.Translation("Interactions requires exactly one of model or agent")
	}
	if agent != "" {
		info.Model = agent
	}
	return info, nil
}

// EncodeError renders err in the Google error envelope.
func (*InteractionsCodec) EncodeError(err error) apix.Payload {
	return envelope(googleEnvelope).encode(err)
}

// ValidateOptions accepts only a loss policy, which has no effect on a
// native exchange.
func (c *InteractionsCodec) ValidateOptions(opts apix.ExchangeOptions) error {
	if err := c.check(opts); err != nil {
		return err
	}
	return nil
}

func (*InteractionsCodec) check(opts apix.ExchangeOptions) *apix.Error {
	switch {
	case !validLossPolicy(opts.LossPolicy):
		return apix.Unsupported("unknown loss policy: " + string(opts.LossPolicy))
	case opts.Output != "" && opts.Output != ids.Interactions:
		return apix.Unsupported("no response conversion from " + string(ids.Interactions) + " to " + string(opts.Output))
	case opts.Reasoning != nil:
		return apix.Unsupported(string(ids.Interactions) + " does not support reasoning overrides")
	case opts.Ext != nil:
		return apix.Unsupported(string(ids.Interactions) + " defines no exchange options")
	}
	return nil
}

// NewExchange creates a native Interactions exchange.
func (c *InteractionsCodec) NewExchange(client apix.Protocol, opts apix.ExchangeOptions) (apix.Exchange, error) {
	if client != ids.Interactions {
		return nil, apix.Unsupported("no conversion from " + string(client) + " to " + string(ids.Interactions))
	}
	if err := c.check(opts); err != nil {
		return nil, err
	}
	return &interactionsExchange{recorder: &recorder{kind: "interaction"}}, nil
}

type interactionsExchange struct {
	recorder *recorder
}

func (e *interactionsExchange) ConvertRequest(req apix.Payload, upstreamModel string) (apix.Payload, error) {
	out, err := e.convertRequest(req, upstreamModel)
	if err != nil {
		return apix.Payload{}, err
	}
	return out, nil
}

func (e *interactionsExchange) convertRequest(req apix.Payload, upstreamModel string) (apix.Payload, *apix.Error) {
	info, err := inspectInteractions(req.Body)
	if err != nil {
		return apix.Payload{}, err
	}
	info = req.Metadata.Apply(info)
	if upstreamModel == "" {
		upstreamModel = info.Model
	}
	body := req.Body
	var doc map[string]json.RawMessage
	if json.Unmarshal(body, &doc) != nil {
		return apix.Payload{}, apix.Translation("invalid Interactions request")
	}
	if shared.HasContent(doc["agent"]) {
		if upstreamModel != "" && upstreamModel != info.Model {
			return apix.Payload{}, apix.Unsupported("Interactions agent cannot be rewritten as a model")
		}
	} else if body, err = shared.RewriteModelID(upstreamModel, body, interactions.Format); err != nil {
		return apix.Payload{}, err
	}
	if body, err = applyStreamMetadata(body, req.Metadata.Stream); err != nil {
		return apix.Payload{}, err
	}
	return apix.Payload{Body: body, ContentType: "application/json"}, nil
}

func (e *interactionsExchange) ConvertResponse(status int, resp apix.Payload) (apix.Payload, error) {
	body, err := interactions.ConvertResponse(status, resp.Body)
	if err != nil {
		return apix.Payload{}, err
	}
	e.recorder.body(body)
	return apix.Payload{Body: body, ContentType: "application/json"}, nil
}

func (e *interactionsExchange) NewStreamConverter() (apix.StreamConverter, error) {
	return &stream{engine: interactions.NewStreamConverter(), recorder: e.recorder}, nil
}

func (*interactionsExchange) Diagnostics() []apix.Diagnostic { return nil }

// CreatedState implements apix.StateReporter.
func (e *interactionsExchange) CreatedState() []string { return e.recorder.keys() }
