package codec

import (
	"bytes"
	"encoding/json"

	"github.com/dillonzq/apix"
)

// inspect reads the routing fields every LLM protocol shares, the protocol's
// own requirements, and the scan of known content paths. It does not apply
// Payload.Metadata; callers overlay it.
func inspect(id apix.Protocol, body []byte) (apix.RequestInfo, *apix.Error) {
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
	if p, ok := protocols[id]; ok && p.validate != nil {
		var root map[string]json.RawMessage
		if json.Unmarshal(body, &root) != nil || root == nil {
			return apix.RequestInfo{}, apix.Translation("request must be a JSON object")
		}
		if err := p.validate(root); err != nil {
			return apix.RequestInfo{}, err
		}
	}
	s := scanRequest(id, body, info.Stream)
	info.Features, info.Affinity, info.Stateful = s.features(), s.affinity, s.stateful
	return info, nil
}
