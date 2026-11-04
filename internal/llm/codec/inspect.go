package codec

import (
	"bytes"
	"encoding/json"

	"github.com/MoeForge/moego/ojson"

	"github.com/dillonzq/apix"
	"github.com/dillonzq/apix/internal/jsonedit"
)

// inspect reads the routing fields every LLM protocol shares, the protocol's
// own requirements, and the scan of known content paths. It does not apply
// Payload.Metadata; callers overlay it.
func inspect(id apix.Protocol, body []byte) (apix.RequestInfo, *apix.Error) {
	return inspectRequest(id, body, true)
}

// routing is inspect without the content scan: the model, the stream flag
// and the protocol's requirements, which is all request conversion needs.
func routing(id apix.Protocol, body []byte) (apix.RequestInfo, *apix.Error) {
	return inspectRequest(id, body, false)
}

func inspectRequest(id apix.Protocol, body []byte, features bool) (apix.RequestInfo, *apix.Error) {
	root, ok := jsonedit.Object(body)
	var fields []ojson.Result
	if ok {
		fields, ok = jsonedit.Members(root, "model", "stream")
	}
	if !ok {
		// Malformed, non-object and ambiguous bodies keep the encoding/json
		// reading and its error messages.
		return inspectDecoded(id, body, features)
	}
	info := apix.RequestInfo{Kind: apix.KindLLM, Protocol: id}
	if model := fields[0]; model.Exists() && model.Type != ojson.Null {
		if info.Model, ok = jsonedit.String(model); !ok {
			return apix.RequestInfo{}, apix.Translation(string(id) + " request model must be a string")
		}
	}
	if stream := fields[1]; stream.Exists() && stream.Type != ojson.Null {
		if !stream.IsBool() {
			return apix.RequestInfo{}, apix.Translation(string(id) + " request stream must be a boolean")
		}
		info.Stream = stream.Type == ojson.True
	}
	p, hasProtocol := protocols[id]
	if !features && (!hasProtocol || p.validate == nil) {
		return info, nil
	}
	members := featureMap(body)
	if hasProtocol && p.validate != nil {
		if err := p.validate(members); err != nil {
			return apix.RequestInfo{}, err
		}
	}
	if features {
		s := scanRequest(id, members, info.Stream)
		info.Features, info.Affinity, info.Stateful = s.features(), s.affinity, s.stateful
	}
	return info, nil
}

// inspectDecoded is inspectRequest through encoding/json.
func inspectDecoded(id apix.Protocol, body []byte, features bool) (apix.RequestInfo, *apix.Error) {
	var doc struct {
		Model  json.RawMessage `json:"model"`
		Stream json.RawMessage `json:"stream"`
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
	var root map[string]json.RawMessage
	if json.Unmarshal(body, &root) != nil {
		root = nil
	}
	if p, ok := protocols[id]; ok && p.validate != nil {
		if root == nil {
			return apix.RequestInfo{}, apix.Translation("request must be a JSON object")
		}
		if err := p.validate(root); err != nil {
			return apix.RequestInfo{}, err
		}
	}
	if features {
		s := scanRequest(id, root, info.Stream)
		info.Features, info.Affinity, info.Stateful = s.features(), s.affinity, s.stateful
	}
	return info, nil
}
