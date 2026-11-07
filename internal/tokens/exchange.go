package tokens

import (
	"encoding/json"
	"strconv"
	"strings"

	"github.com/dillonzq/apix"
	"github.com/dillonzq/apix/internal/jsonedit"
	"github.com/dillonzq/apix/internal/llm/ids"
	"github.com/dillonzq/apix/internal/llm/translate"
	"github.com/dillonzq/apix/internal/vendorerr"
)

// Fields each upstream count endpoint accepts from a converted generation
// request. Generation-only fields, such as max_tokens, stream and sampling,
// are dropped because count endpoints reject them.
var (
	messagesFields  = []string{"model", "messages", "system", "tools", "tool_choice", "thinking"}
	responsesFields = []string{"model", "input", "instructions", "tools", "tool_choice", "parallel_tool_calls", "reasoning", "text"}
	googleFields    = []string{"contents", "systemInstruction", "system_instruction", "tools", "toolConfig", "tool_config"}
	// googleSchemaFields are the generationConfig members that add a response
	// schema to the input; the other members only steer generation.
	googleSchemaFields = []string{"responseMimeType", "response_mime_type", "responseSchema", "response_schema", "responseJsonSchema", "response_json_schema"}
)

// Google wraps a full generation request in generateContentRequest; the
// proto JSON mapping also accepts the snake_case spelling.
var googleWrappers = []string{"generateContentRequest", "generate_content_request"}

// request is a client count request read as its generation request.
type request struct {
	// body is the generation request: the count body itself for Messages and
	// Responses, and the unwrapped GenerateContent request for Google.
	body []byte
	// model is the model a Google generateContentRequest names, without its
	// "models/" prefix. Other protocols read the model from body.
	model string
	// wrapper is the generateContentRequest key a Google body used, if any.
	wrapper string
}

// decodeRequest reads a client count body as its generation request.
func decodeRequest(id apix.Protocol, body []byte) (request, *apix.Error) {
	if id != CountTokens {
		return request{body: body}, nil
	}
	var root map[string]json.RawMessage
	if err := json.Unmarshal(body, &root); err != nil || root == nil {
		return request{}, apix.Translation("malformed " + string(id) + " request JSON")
	}
	r := request{body: body}
	for _, key := range googleWrappers {
		raw, ok := root[key]
		if !ok || string(raw) == "null" {
			continue
		}
		if r.wrapper != "" {
			return request{}, apix.Translation("Google countTokens request has generateContentRequest twice")
		}
		r.wrapper = key
		var inner map[string]json.RawMessage
		if err := json.Unmarshal(raw, &inner); err != nil || inner == nil {
			return request{}, apix.Translation("Google countTokens " + key + " must be an object")
		}
		if m, ok := inner["model"]; ok {
			if err := json.Unmarshal(m, &r.model); err != nil {
				return request{}, apix.Translation("Google countTokens " + key + ".model must be a string")
			}
			r.model = strings.TrimPrefix(r.model, "models/")
			delete(inner, "model")
		}
		r.body, _ = json.Marshal(inner)
	}
	if r.wrapper != "" {
		if _, ok := root["contents"]; ok {
			return request{}, apix.Translation("Google countTokens request takes contents or generateContentRequest, not both")
		}
	} else if _, ok := root["contents"]; !ok {
		return request{}, apix.Translation("Google countTokens request requires contents or generateContentRequest")
	}
	// GenerateContent requires contents; its codec checks this when it reads
	// the request, which conversion does not repeat.
	var gen struct {
		Contents []json.RawMessage `json:"contents"`
	}
	if json.Unmarshal(r.body, &gen) != nil || len(gen.Contents) == 0 {
		return request{}, apix.Translation("Google request requires contents")
	}
	return r, nil
}

type exchange struct {
	clientID   apix.Protocol
	upstreamID apix.Protocol
	client     protocol
	upstream   protocol
	output     protocol
	state      *translate.RequestState
}

func newExchange(clientID apix.Protocol, client protocol, upstreamID apix.Protocol, upstream, output protocol, policy apix.LossPolicy) *exchange {
	state := translate.NewRequestState()
	state.Options.LossPolicy = translate.LossPolicy(policy)
	return &exchange{clientID: clientID, upstreamID: upstreamID, client: client, upstream: upstream, output: output, state: state}
}

// ConvertRequest converts the client's count request into the upstream's.
// Native requests keep their bytes apart from the model. Converted requests
// carry the content the generation request would, reshaped for the upstream
// count endpoint.
func (e *exchange) ConvertRequest(req apix.Payload, upstreamModel string) (apix.Payload, error) {
	out, model, err := e.convertRequest(req, upstreamModel)
	if err != nil {
		return apix.Payload{}, err
	}
	result := apix.Payload{Body: out, ContentType: "application/json"}
	if e.upstreamID == CountTokens {
		stream := false
		result.Metadata = apix.RequestMetadata{Model: model, Stream: &stream}
	}
	return result, nil
}

func (e *exchange) convertRequest(req apix.Payload, upstreamModel string) ([]byte, string, *apix.Error) {
	r, err := decodeRequest(e.clientID, req.Body)
	if err != nil {
		return nil, "", err
	}
	if upstreamModel == "" {
		upstreamModel = req.Metadata.Model
	}
	if upstreamModel == "" {
		upstreamModel = r.model
	}
	if upstreamModel == "" && e.clientID != CountTokens {
		if upstreamModel, err = bodyModel(r.body); err != nil {
			return nil, "", err
		}
	}
	if e.upstreamID == CountTokens && upstreamModel == "" {
		return nil, "", apix.Translation(string(CountTokens) + " requires an upstream model in request metadata or ConvertRequest")
	}
	if e.clientID == e.upstreamID {
		if e.clientID == CountTokens {
			out, err := googleNative(req.Body, r.wrapper, upstreamModel)
			return out, upstreamModel, err
		}
		out, err := translate.ConvertRequest(e.upstream.format, upstreamModel, e.client.format, r.body, e.state)
		return out, upstreamModel, err
	}
	gen, err := translate.ConvertRequest(e.upstream.format, upstreamModel, e.client.format, r.body, e.state)
	if err != nil {
		return nil, "", err
	}
	out, err := countBody(e.upstream.format, gen, upstreamModel)
	return out, upstreamModel, err
}

// bodyModel reads the model member of a Messages or Responses body.
func bodyModel(body []byte) (string, *apix.Error) {
	if f, ok := jsonedit.Fields(body, "model"); ok {
		if !f[0].Exists() {
			return "", nil
		}
		if s, ok := jsonedit.String(f[0]); ok {
			return s, nil
		}
		return "", apix.Translation("request model must be a string")
	}
	var doc struct {
		Model *string `json:"model"`
	}
	if err := json.Unmarshal(body, &doc); err != nil {
		return "", apix.Translation("malformed request JSON: " + err.Error())
	}
	if doc.Model == nil {
		return "", nil
	}
	return *doc.Model, nil
}

// googleNative keeps a native countTokens body and points the model named
// inside generateContentRequest, if any, at the upstream model.
func googleNative(body []byte, wrapper, model string) ([]byte, *apix.Error) {
	if wrapper == "" {
		return body, nil
	}
	quoted, _ := json.Marshal("models/" + strings.TrimPrefix(model, "models/"))
	if out, ok := jsonedit.Apply(body, jsonedit.Set(quoted, wrapper, "model")); ok {
		return out, nil
	}
	var root map[string]json.RawMessage
	var inner map[string]json.RawMessage
	if json.Unmarshal(body, &root) != nil || json.Unmarshal(root[wrapper], &inner) != nil || inner == nil {
		return nil, apix.Translation("malformed " + string(CountTokens) + " request JSON")
	}
	inner["model"] = quoted
	root[wrapper], _ = json.Marshal(inner)
	out, _ := json.Marshal(root)
	return out, nil
}

// countBody reshapes a converted generation request into the count request
// of the same protocol.
func countBody(format string, gen []byte, model string) ([]byte, *apix.Error) {
	var root map[string]json.RawMessage
	if err := json.Unmarshal(gen, &root); err != nil || root == nil {
		return nil, apix.Translation("converted request must be a JSON object")
	}
	var out map[string]json.RawMessage
	switch format {
	case ids.FormatMessages:
		out = pick(root, messagesFields)
		// output_config.format adds a schema to the input; effort does not.
		if raw, ok := root["output_config"]; ok {
			var cfg map[string]json.RawMessage
			if json.Unmarshal(raw, &cfg) == nil && cfg["format"] != nil {
				out["output_config"], _ = json.Marshal(map[string]json.RawMessage{"format": cfg["format"]})
			}
		}
	case ids.FormatResponses:
		out = pick(root, responsesFields)
	default:
		req := pick(root, googleFields)
		for _, key := range []string{"generationConfig", "generation_config"} {
			var cfg map[string]json.RawMessage
			if raw, ok := root[key]; ok && json.Unmarshal(raw, &cfg) == nil {
				if schema := pick(cfg, googleSchemaFields); len(schema) > 0 {
					req[key], _ = json.Marshal(schema)
				}
			}
		}
		req["model"], _ = json.Marshal("models/" + strings.TrimPrefix(model, "models/"))
		inner, _ := json.Marshal(req)
		out = map[string]json.RawMessage{"generateContentRequest": inner}
	}
	body, _ := json.Marshal(out)
	return body, nil
}

func pick(root map[string]json.RawMessage, keys []string) map[string]json.RawMessage {
	out := make(map[string]json.RawMessage, len(keys))
	for _, k := range keys {
		if v, ok := root[k]; ok {
			out[k] = v
		}
	}
	return out
}

// ConvertResponse converts the upstream count to the output protocol. Native
// replies keep their bytes once the count is checked. Error statuses return
// the classified upstream error.
func (e *exchange) ConvertResponse(status int, resp apix.Payload) (apix.Payload, error) {
	if status >= 400 {
		return apix.Payload{}, vendorerr.UpstreamStatus(status, resp.Body)
	}
	n, err := readCount(e.upstream.format, resp.Body)
	if err != nil {
		return apix.Payload{}, err
	}
	if e.output.format == e.upstream.format {
		return apix.Payload{Body: resp.Body, ContentType: "application/json"}, nil
	}
	count := strconv.FormatInt(n, 10)
	var body string
	switch e.output.format {
	case ids.FormatMessages:
		body = `{"input_tokens":` + count + `}`
	case ids.FormatResponses:
		body = `{"object":"response.input_tokens","input_tokens":` + count + `}`
	default:
		body = `{"totalTokens":` + count + `}`
	}
	return apix.Payload{Body: []byte(body), ContentType: "application/json"}, nil
}

// readCount reads the input token count of an upstream count response:
// input_tokens for Messages and Responses, totalTokens for Google, whose
// proto JSON omits a zero count.
func readCount(format string, body []byte) (int64, *apix.Error) {
	var root map[string]json.RawMessage
	if err := json.Unmarshal(body, &root); err != nil || root == nil {
		return 0, apix.UpstreamFallback("malformed " + string(byFormat(format)) + " response JSON")
	}
	key := "input_tokens"
	if format == ids.FormatGenerateContent {
		key = "totalTokens"
		if _, ok := root[key]; !ok {
			if _, ok := root["total_tokens"]; ok {
				key = "total_tokens"
			} else {
				return 0, nil
			}
		}
	}
	raw, ok := root[key]
	if !ok || string(raw) == "null" {
		return 0, apix.UpstreamFallback(string(byFormat(format)) + " response has no " + key)
	}
	var n int64
	// Google's proto JSON may encode integers as strings.
	if format == ids.FormatGenerateContent && len(raw) > 0 && raw[0] == '"' {
		var s string
		if json.Unmarshal(raw, &s) == nil {
			raw = json.RawMessage(s)
		}
	}
	if err := json.Unmarshal(raw, &n); err != nil || n < 0 {
		return 0, apix.UpstreamFallback(string(byFormat(format)) + " response " + key + " must be a non-negative integer")
	}
	return n, nil
}

// NewStreamConverter reports that counts do not stream.
func (e *exchange) NewStreamConverter() (apix.StreamConverter, error) {
	return nil, apix.Unsupported(string(e.upstreamID) + " does not stream")
}

// Diagnostics returns the losses request conversion recorded, named by the
// count protocols.
func (e *exchange) Diagnostics() []apix.Diagnostic {
	in := e.state.Diagnostics()
	if len(in) == 0 {
		return nil
	}
	out := make([]apix.Diagnostic, len(in))
	for i, d := range in {
		out[i] = apix.Diagnostic{Code: d.Code, Path: d.Path, Message: d.Message, Semantic: d.Semantic, From: byFormat(d.From), To: byFormat(d.To)}
	}
	return out
}
