package codec

import (
	"encoding/json"
	"net/url"
	"strings"

	"github.com/dillonzq/apix"
	"github.com/dillonzq/apix/internal/jsonedit"
)

// googlePath builds the GenerateContent URL path, which carries the model and
// the streaming mode.
func googlePath(info apix.RequestInfo, model string) string {
	action := "generateContent"
	if info.Stream {
		action = "streamGenerateContent?alt=sse"
	}
	return "/v1beta/models/" + url.PathEscape(strings.TrimPrefix(model, "models/")) + ":" + action
}

// parseGooglePath reads the model and streaming mode from a client URL path.
func parseGooglePath(path string) (apix.RequestMetadata, error) {
	suffix, ok := strings.CutPrefix(path, "/v1beta/models/")
	if !ok {
		suffix, ok = strings.CutPrefix(path, "/v1/models/")
	}
	model, action, found := strings.Cut(suffix, ":")
	if !ok || !found || model == "" || strings.ContainsAny(model, "/\\?#") || model == ".." {
		return apix.RequestMetadata{}, apix.Unsupported("invalid Google model path")
	}
	stream := false
	switch action {
	case "generateContent":
	case "streamGenerateContent":
		stream = true
	default:
		return apix.RequestMetadata{}, apix.Unsupported("unsupported Google operation")
	}
	return apix.RequestMetadata{Model: model, Stream: &stream}, nil
}

func validateGoogle(root map[string]json.RawMessage) *apix.Error {
	// root holds checked JSON; contents must be a non-empty array.
	contents := jsonedit.Parse(root["contents"])
	if !contents.IsArray() || !contents.Get("0").Exists() {
		return apix.Translation("Google request requires contents")
	}
	return nil
}
