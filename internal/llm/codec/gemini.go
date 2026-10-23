package codec

import (
	"encoding/json"
	"strings"

	"github.com/dillonzq/apix"
)

func parseGeminiPath(path string) (apix.RequestMetadata, *apix.Error) {
	suffix, ok := strings.CutPrefix(path, "/v1beta/models/")
	if !ok {
		suffix, ok = strings.CutPrefix(path, "/v1/models/")
	}
	model, action, found := strings.Cut(suffix, ":")
	if !ok || !found || model == "" || strings.ContainsAny(model, "/\\?#") || model == ".." {
		return apix.RequestMetadata{}, apix.Unsupported("invalid Gemini model path")
	}
	stream := false
	switch action {
	case "generateContent":
	case "streamGenerateContent":
		stream = true
	default:
		return apix.RequestMetadata{}, apix.Unsupported("unsupported Gemini operation")
	}
	return apix.RequestMetadata{Model: model, Stream: &stream}, nil
}
func inspectRequest(id apix.Protocol, req apix.Payload) (apix.RequestInfo, *apix.Error) {
	info, err := inspect(id, req.Body)
	if err != nil {
		return info, err
	}
	if id == Gemini || id == Interactions {
		var root map[string]json.RawMessage
		if json.Unmarshal(req.Body, &root) != nil || root == nil {
			return info, apix.Translation("request must be a JSON object")
		}
		if id == Gemini {
			var contents []json.RawMessage
			if json.Unmarshal(root["contents"], &contents) != nil || len(contents) == 0 {
				return info, apix.Translation("Gemini request requires contents")
			}
		}
		if id == Interactions {
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
		}
	}
	info = req.Metadata.Apply(info)
	return info, nil
}
func applyStreamMetadata(body []byte, stream *bool) ([]byte, *apix.Error) {
	if stream == nil {
		return body, nil
	}
	var root map[string]json.RawMessage
	if json.Unmarshal(body, &root) != nil || root == nil {
		return nil, apix.Translation("request must be a JSON object")
	}
	root["stream"], _ = json.Marshal(*stream)
	out, _ := json.Marshal(root)
	return out, nil
}
func geminiErrorType(err *apix.Error) string {
	switch err.Class {
	case apix.ClassAuth:
		return "UNAUTHENTICATED"
	case apix.ClassInvalidModel:
		return "NOT_FOUND"
	case apix.ClassRateLimit, apix.ClassQuota:
		return "RESOURCE_EXHAUSTED"
	case apix.ClassBilling:
		return "PERMISSION_DENIED"
	case apix.ClassNetwork:
		return "DEADLINE_EXCEEDED"
	case apix.ClassUpstream:
		return "UNAVAILABLE"
	default:
		return "INVALID_ARGUMENT"
	}
}
