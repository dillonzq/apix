package codec

import (
	"encoding/json"
	"net/url"
	"strings"

	"github.com/dillonzq/apix"
)

// geminiPath builds the GenerateContent URL path, which carries the model and
// the streaming mode.
func geminiPath(info apix.RequestInfo, model string) string {
	action := "generateContent"
	if info.Stream {
		action = "streamGenerateContent?alt=sse"
	}
	return "/v1beta/models/" + url.PathEscape(strings.TrimPrefix(model, "models/")) + ":" + action
}

// parseGeminiPath reads the model and streaming mode from a client URL path.
func parseGeminiPath(path string) (apix.RequestMetadata, error) {
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

func validateGemini(root map[string]json.RawMessage) *apix.Error {
	var contents []json.RawMessage
	if json.Unmarshal(root["contents"], &contents) != nil || len(contents) == 0 {
		return apix.Translation("Gemini request requires contents")
	}
	return nil
}
