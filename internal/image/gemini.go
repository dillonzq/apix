package image

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"errors"
	"io"
	"reflect"
	"sort"
	"strconv"
	"strings"

	"github.com/dillonzq/apix"
)

// geminiRequest is a GenerateContent request to an image model. Google's
// JSON accepts camelCase and snake_case field names; both are read.
type geminiRequest struct {
	Contents       []geminiContent
	CandidateCount int
	AspectRatio    string
	ImageSize      string
	Modalities     []string
	CachedContent  string
	// keys, configKeys and imageConfigKeys list the fields present at the
	// top level, in generationConfig and in imageConfig, normalized to
	// camelCase and sorted.
	keys            []string
	configKeys      []string
	imageConfigKeys []string
}

type geminiContent struct {
	Role  string
	Parts []geminiPart
}

// geminiPart is a text or media part; other parts, such as function calls,
// only record their kind for diagnostics.
type geminiPart struct {
	Text    string
	Media   *media
	Thought bool
	Other   string
}

func (r *geminiRequest) info() apix.RequestInfo {
	info := apix.RequestInfo{Kind: apix.KindImage, Protocol: GeminiImage}
	features := map[string]bool{}
	for _, c := range r.Contents {
		for _, p := range c.Parts {
			if p.Media == nil {
				continue
			}
			switch mt := strings.ToLower(p.Media.MIME); {
			case strings.HasPrefix(mt, "image/"):
				features[apix.FeatureImageInput] = true
			case strings.HasPrefix(mt, "audio/"):
				features[apix.FeatureAudioInput] = true
			case strings.HasPrefix(mt, "video/"):
				features[apix.FeatureVideoInput] = true
			default:
				features[apix.FeatureFileInput] = true
			}
			// Files API URIs belong to the project that uploaded them.
			if strings.Contains(p.Media.URI, "/files/") {
				info.Affinity = append(info.Affinity, "file:"+p.Media.URI)
			}
		}
	}
	for _, f := range []string{apix.FeatureImageInput, apix.FeatureFileInput, apix.FeatureAudioInput, apix.FeatureVideoInput} {
		if features[f] {
			info.Features = append(info.Features, f)
		}
	}
	if r.CachedContent != "" {
		info.Affinity = append(info.Affinity, "cached-content:"+r.CachedContent)
		info.Stateful = true
	}
	return info
}

// camel normalizes a snake_case field name.
func camel(key string) string {
	parts := strings.Split(key, "_")
	for i := 1; i < len(parts); i++ {
		if parts[i] != "" {
			parts[i] = strings.ToUpper(parts[i][:1]) + parts[i][1:]
		}
	}
	return strings.Join(parts, "")
}

// fields decodes a JSON object and indexes it by camelCase name, dropping
// null values. It returns the sorted names. When spellings collide, the
// camelCase key wins, then the first snake_case key in sorted order, so the
// result does not depend on map iteration.
func fields(raw json.RawMessage, path string) (map[string]json.RawMessage, []string, *apix.Error) {
	if len(raw) == 0 || string(raw) == "null" {
		return map[string]json.RawMessage{}, nil, nil
	}
	var in map[string]json.RawMessage
	if json.Unmarshal(raw, &in) != nil || in == nil {
		return nil, nil, apix.Translation(path + " must be a JSON object")
	}
	names := make([]string, 0, len(in))
	for k, v := range in {
		if string(v) != "null" {
			names = append(names, k)
		}
	}
	sort.Strings(names)
	out := make(map[string]json.RawMessage, len(names))
	keys := make([]string, 0, len(names))
	for _, k := range names {
		ck := camel(k)
		if _, dup := out[ck]; !dup {
			keys = append(keys, ck)
		} else if k != ck {
			continue
		}
		out[ck] = in[k]
	}
	sort.Strings(keys)
	return out, keys, nil
}

// field decodes one value; a missing value leaves dst unchanged.
func field(m map[string]json.RawMessage, key, path string, dst any) *apix.Error {
	raw, ok := m[key]
	if !ok {
		return nil
	}
	if err := json.Unmarshal(raw, dst); err != nil {
		return apix.Translation(path + " has the wrong type")
	}
	return nil
}

func decodeGemini(body []byte) (*geminiRequest, *apix.Error) {
	var raw json.RawMessage
	if json.Unmarshal(body, &raw) != nil {
		return nil, apix.Translation("malformed Gemini request JSON")
	}
	root, keys, err := fields(raw, "request")
	if err != nil {
		return nil, err
	}
	r := &geminiRequest{keys: keys}
	var contents []json.RawMessage
	if err := field(root, "contents", "contents", &contents); err != nil {
		return nil, err
	}
	if len(contents) == 0 {
		return nil, apix.Translation("Gemini request requires contents")
	}
	for i, c := range contents {
		content, err := decodeGeminiContent(c, "contents["+strconv.Itoa(i)+"]")
		if err != nil {
			return nil, err
		}
		r.Contents = append(r.Contents, content)
	}
	if err := field(root, "cachedContent", "cachedContent", &r.CachedContent); err != nil {
		return nil, err
	}
	config, configKeys, err := fields(root["generationConfig"], "generationConfig")
	if err != nil {
		return nil, err
	}
	r.configKeys = configKeys
	if err := field(config, "candidateCount", "generationConfig.candidateCount", &r.CandidateCount); err != nil {
		return nil, err
	}
	if err := field(config, "responseModalities", "generationConfig.responseModalities", &r.Modalities); err != nil {
		return nil, err
	}
	imageConfig, imageKeys, err := fields(config["imageConfig"], "generationConfig.imageConfig")
	if err != nil {
		return nil, err
	}
	r.imageConfigKeys = imageKeys
	if err := field(imageConfig, "aspectRatio", "generationConfig.imageConfig.aspectRatio", &r.AspectRatio); err != nil {
		return nil, err
	}
	if err := field(imageConfig, "imageSize", "generationConfig.imageConfig.imageSize", &r.ImageSize); err != nil {
		return nil, err
	}
	return r, nil
}

func decodeGeminiContent(raw json.RawMessage, path string) (geminiContent, *apix.Error) {
	m, _, err := fields(raw, path)
	if err != nil {
		return geminiContent{}, err
	}
	var c geminiContent
	if err := field(m, "role", path+".role", &c.Role); err != nil {
		return c, err
	}
	var parts []json.RawMessage
	if err := field(m, "parts", path+".parts", &parts); err != nil {
		return c, err
	}
	for i, p := range parts {
		part, err := decodeGeminiPart(p, path+".parts["+strconv.Itoa(i)+"]")
		if err != nil {
			return c, err
		}
		c.Parts = append(c.Parts, part)
	}
	return c, nil
}

func decodeGeminiPart(raw json.RawMessage, path string) (geminiPart, *apix.Error) {
	m, keys, err := fields(raw, path)
	if err != nil {
		return geminiPart{}, err
	}
	var p geminiPart
	if err := field(m, "thought", path+".thought", &p.Thought); err != nil {
		return p, err
	}
	switch {
	case m["text"] != nil:
		if err := field(m, "text", path+".text", &p.Text); err != nil {
			return p, err
		}
	case m["inlineData"] != nil:
		blob, _, err := fields(m["inlineData"], path+".inlineData")
		if err != nil {
			return p, err
		}
		var mimeType, data string
		if err := field(blob, "mimeType", path+".inlineData.mimeType", &mimeType); err != nil {
			return p, err
		}
		if err := field(blob, "data", path+".inlineData.data", &data); err != nil {
			return p, err
		}
		decoded, derr := base64.StdEncoding.DecodeString(data)
		if derr != nil {
			return p, apix.Translation(path + ".inlineData.data is not valid base64")
		}
		p.Media = &media{MIME: mimeType, Data: decoded}
	case m["fileData"] != nil:
		file, _, err := fields(m["fileData"], path+".fileData")
		if err != nil {
			return p, err
		}
		p.Media = &media{}
		if err := field(file, "mimeType", path+".fileData.mimeType", &p.Media.MIME); err != nil {
			return p, err
		}
		if err := field(file, "fileUri", path+".fileData.fileUri", &p.Media.URI); err != nil {
			return p, err
		}
		if p.Media.URI == "" {
			return p, apix.Translation(path + ".fileData requires fileUri")
		}
	default:
		for _, k := range keys {
			if k != "thought" && k != "thoughtSignature" {
				p.Other = k
				break
			}
		}
	}
	return p, nil
}

// geminiResponse is a GenerateContent response or stream chunk.
type geminiResponse struct {
	Candidates     []geminiCandidate     `json:"candidates,omitempty"`
	PromptFeedback *geminiPromptFeedback `json:"promptFeedback,omitempty"`
	UsageMetadata  *geminiUsage          `json:"usageMetadata,omitempty"`
	ModelVersion   string                `json:"modelVersion,omitempty"`
}

type geminiCandidate struct {
	Content      geminiOutContent `json:"content"`
	FinishReason string           `json:"finishReason,omitempty"`
	Index        int              `json:"index"`
}

type geminiOutContent struct {
	Role  string          `json:"role,omitempty"`
	Parts []geminiOutPart `json:"parts"`
}

type geminiOutPart struct {
	Text       string      `json:"text,omitempty"`
	InlineData *geminiBlob `json:"inlineData,omitempty"`
	Thought    bool        `json:"thought,omitempty"`
}

type geminiBlob struct {
	MIMEType string `json:"mimeType"`
	Data     string `json:"data"`
}

type geminiPromptFeedback struct {
	BlockReason string `json:"blockReason,omitempty"`
}

type geminiUsage struct {
	PromptTokenCount        int64               `json:"promptTokenCount,omitempty"`
	CandidatesTokenCount    int64               `json:"candidatesTokenCount,omitempty"`
	ThoughtsTokenCount      int64               `json:"thoughtsTokenCount,omitempty"`
	TotalTokenCount         int64               `json:"totalTokenCount,omitempty"`
	PromptTokensDetails     []geminiModalityUse `json:"promptTokensDetails,omitempty"`
	CandidatesTokensDetails []geminiModalityUse `json:"candidatesTokensDetails,omitempty"`
}

type geminiModalityUse struct {
	Modality   string `json:"modality"`
	TokenCount int64  `json:"tokenCount"`
}

// decodeGeminiResponse reads a response, accepting snake_case field names by
// normalizing them before typed decoding.
func decodeGeminiResponse(body []byte) (geminiResponse, *apix.Error) {
	var v any
	d := json.NewDecoder(bytes.NewReader(body))
	d.UseNumber()
	if d.Decode(&v) != nil || !errors.Is(d.Decode(new(any)), io.EOF) {
		return geminiResponse{}, apix.Translation("malformed Gemini response JSON")
	}
	if _, ok := v.(map[string]any); !ok {
		return geminiResponse{}, apix.Translation("Gemini response must be a JSON object")
	}
	normalized, _ := json.Marshal(camelKeys(v))
	var r geminiResponse
	if err := json.Unmarshal(normalized, &r); err != nil {
		return geminiResponse{}, apix.Translation("malformed Gemini response: " + err.Error())
	}
	return r, nil
}

// responseFields maps the lowercased JSON names of the response types to
// their exact names.
var responseFields = func() map[string]string {
	out := map[string]string{}
	var walk func(t reflect.Type)
	walk = func(t reflect.Type) {
		for t.Kind() == reflect.Pointer || t.Kind() == reflect.Slice {
			t = t.Elem()
		}
		if t.Kind() != reflect.Struct {
			return
		}
		for i := range t.NumField() {
			name, _, _ := strings.Cut(t.Field(i).Tag.Get("json"), ",")
			if _, seen := out[strings.ToLower(name)]; !seen {
				out[strings.ToLower(name)] = name
				walk(t.Field(i).Type)
			}
		}
	}
	walk(reflect.TypeFor[geminiResponse]())
	return out
}()

// camelKeys renames snake_case object keys recursively. Keys that differ
// from a response field only in case are dropped: Google's JSON is case
// sensitive, but encoding/json would match them.
func camelKeys(v any) any {
	switch t := v.(type) {
	case map[string]any:
		out := make(map[string]any, len(t))
		for k, val := range t {
			k = camel(k)
			if exact, ok := responseFields[strings.ToLower(k)]; ok && exact != k {
				continue
			}
			out[k] = camelKeys(val)
		}
		return out
	case []any:
		for i := range t {
			t[i] = camelKeys(t[i])
		}
	}
	return v
}

// images returns the non-thought image parts of every candidate.
func (r geminiResponse) images() []geminiBlob {
	var out []geminiBlob
	for _, c := range r.Candidates {
		for _, p := range c.Content.Parts {
			if p.InlineData != nil && !p.Thought && strings.HasPrefix(p.InlineData.MIMEType, "image/") {
				out = append(out, *p.InlineData)
			}
		}
	}
	return out
}

// hasText reports whether a candidate returned visible text.
func (r geminiResponse) hasText() bool {
	for _, c := range r.Candidates {
		for _, p := range c.Content.Parts {
			if p.Text != "" && !p.Thought {
				return true
			}
		}
	}
	return false
}

// noImage explains a response without images. Safety and policy stops
// describe the request and are not retryable; other stops may succeed on
// another upstream.
func (r geminiResponse) noImage() *apix.Error {
	reason := ""
	if r.PromptFeedback != nil && r.PromptFeedback.BlockReason != "" {
		reason = "prompt blocked: " + r.PromptFeedback.BlockReason
	} else {
		for _, c := range r.Candidates {
			if c.FinishReason != "" {
				reason = "finish reason " + c.FinishReason
				break
			}
		}
	}
	if reason == "" {
		reason = "no candidates"
	}
	switch {
	case strings.Contains(reason, "SAFETY"), strings.Contains(reason, "PROHIBITED"), strings.Contains(reason, "BLOCKLIST"),
		strings.Contains(reason, "RECITATION"), strings.Contains(reason, "SPII"), strings.HasPrefix(reason, "prompt blocked"):
		return apix.Unsupported("Gemini generated no image: " + reason)
	}
	return apix.UpstreamFallback("Gemini generated no image: " + reason)
}

// renderGeminiStream renders a converted response as a one-chunk stream.
func renderGeminiStream(r geminiResponse) []byte {
	data, _ := json.Marshal(r)
	return append(append([]byte("data: "), data...), '\n', '\n')
}
