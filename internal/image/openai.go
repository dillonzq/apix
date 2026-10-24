package image

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"errors"
	"io"
	"mime"
	"mime/multipart"
	"net/http"
	"net/textproto"
	"sort"
	"strconv"
	"strings"

	"github.com/dillonzq/apix"
)

// media is one input image: inline bytes, a URI or a stored file.
type media struct {
	MIME   string
	Data   []byte
	URI    string // remote URL or Gemini fileData URI
	FileID string // OpenAI file ID
}

// openaiRequest is an OpenAI generations or edits request, from JSON or a
// multipart form.
type openaiRequest struct {
	id                apix.Protocol
	Model             string
	Prompt            string
	N                 int // 0 when absent
	Size              string
	Quality           string
	Background        string
	OutputFormat      string
	OutputCompression *int
	ResponseFormat    string
	PartialImages     int
	Stream            bool
	Images            []media
	Mask              *media
	// keys lists every field present with a non-null value, sorted.
	keys []string
}

// openaiFields are the request fields the converters read or report by name.
var openaiFields = map[string]bool{
	"model": true, "prompt": true, "n": true, "size": true, "quality": true, "background": true, "output_format": true,
	"output_compression": true, "response_format": true, "partial_images": true, "stream": true, "moderation": true,
	"style": true, "user": true, "safety_identifier": true, "input_fidelity": true, "image": true, "image[]": true,
	"images": true, "mask": true,
}

func (r *openaiRequest) info() apix.RequestInfo {
	info := apix.RequestInfo{Kind: apix.KindImage, Protocol: r.id, Model: r.Model, Stream: r.Stream}
	if r.Stream {
		info.Features = append(info.Features, apix.FeatureStream)
	}
	if len(r.Images) > 0 {
		info.Features = append(info.Features, apix.FeatureImageInput)
	}
	for _, m := range append(append([]media(nil), r.Images...), maskList(r.Mask)...) {
		if m.FileID != "" {
			info.Affinity = append(info.Affinity, "file:"+m.FileID)
		}
	}
	return info
}

func maskList(m *media) []media {
	if m == nil {
		return nil
	}
	return []media{*m}
}

func (r *openaiRequest) has(key string) bool {
	i := sort.SearchStrings(r.keys, key)
	return i < len(r.keys) && r.keys[i] == key
}

// isMultipart reports whether contentType is a multipart form.
func isMultipart(contentType string) bool {
	mt, _, err := mime.ParseMediaType(contentType)
	return err == nil && mt == "multipart/form-data"
}

func decodeOpenAI(id apix.Protocol, req apix.Payload) (*openaiRequest, *apix.Error) {
	var r *openaiRequest
	var err *apix.Error
	if isMultipart(req.ContentType) {
		r, err = decodeOpenAIForm(id, req)
	} else {
		r, err = decodeOpenAIJSON(id, req.Body)
	}
	if err != nil {
		return nil, err
	}
	switch {
	case r.Prompt == "":
		return nil, apix.Translation(string(id) + " request requires a prompt")
	case protocols[id].edit && len(r.Images) == 0:
		return nil, apix.Translation(string(id) + " request requires an image")
	case !protocols[id].edit && (len(r.Images) > 0 || r.Mask != nil):
		return nil, apix.Translation(string(id) + " request cannot carry input images")
	case r.N < 0 || r.PartialImages < 0:
		return nil, apix.Translation(string(id) + " request n and partial_images must not be negative")
	}
	return r, nil
}

func decodeOpenAIJSON(id apix.Protocol, body []byte) (*openaiRequest, *apix.Error) {
	var root map[string]json.RawMessage
	if json.Unmarshal(body, &root) != nil || root == nil {
		return nil, apix.Translation(string(id) + " request must be a JSON object")
	}
	var doc struct {
		Model             string          `json:"model"`
		Prompt            string          `json:"prompt"`
		N                 *int            `json:"n"`
		Size              string          `json:"size"`
		Quality           string          `json:"quality"`
		Background        string          `json:"background"`
		OutputFormat      string          `json:"output_format"`
		OutputCompression *int            `json:"output_compression"`
		ResponseFormat    string          `json:"response_format"`
		PartialImages     *int            `json:"partial_images"`
		Stream            *bool           `json:"stream"`
		Images            []openaiRef     `json:"images"`
		Mask              *openaiRef      `json:"mask"`
		Image             json.RawMessage `json:"image"`
	}
	if err := json.Unmarshal(body, &doc); err != nil {
		return nil, apix.Translation("malformed " + string(id) + " request JSON: " + err.Error())
	}
	r := &openaiRequest{id: id, Model: doc.Model, Prompt: doc.Prompt, Size: doc.Size, Quality: doc.Quality, Background: doc.Background,
		OutputFormat: doc.OutputFormat, OutputCompression: doc.OutputCompression, ResponseFormat: doc.ResponseFormat}
	if doc.N != nil {
		r.N = *doc.N
	}
	if doc.PartialImages != nil {
		r.PartialImages = *doc.PartialImages
	}
	r.Stream = doc.Stream != nil && *doc.Stream
	for k, v := range root {
		if string(v) != "null" {
			r.keys = append(r.keys, k)
		}
	}
	sort.Strings(r.keys)
	if r.has("image") {
		return nil, apix.Translation(string(id) + " JSON requests carry input images in images; image is a multipart field")
	}
	for i, ref := range doc.Images {
		m, err := ref.media("images[" + strconv.Itoa(i) + "]")
		if err != nil {
			return nil, err
		}
		r.Images = append(r.Images, m)
	}
	if doc.Mask != nil {
		m, err := doc.Mask.media("mask")
		if err != nil {
			return nil, err
		}
		r.Mask = &m
	}
	return r, nil
}

// openaiRef is an image reference in a JSON edits request.
type openaiRef struct {
	ImageURL string `json:"image_url"`
	FileID   string `json:"file_id"`
}

func (ref openaiRef) media(path string) (media, *apix.Error) {
	switch {
	case (ref.ImageURL == "") == (ref.FileID == ""):
		return media{}, apix.Translation(path + " requires exactly one of image_url or file_id")
	case ref.FileID != "":
		return media{FileID: ref.FileID}, nil
	}
	if !strings.HasPrefix(ref.ImageURL, "data:") {
		return media{URI: ref.ImageURL}, nil
	}
	mimeType, data, err := parseDataURL(ref.ImageURL)
	if err != nil {
		return media{}, apix.Translation(path + ": " + err.Message)
	}
	return media{MIME: mimeType, Data: data}, nil
}

// parseDataURL decodes a base64 data URL.
func parseDataURL(s string) (string, []byte, *apix.Error) {
	header, payload, ok := strings.Cut(strings.TrimPrefix(s, "data:"), ",")
	mimeType, encoding, _ := strings.Cut(header, ";")
	if !ok || encoding != "base64" {
		return "", nil, apix.Translation("image data URL must be base64 encoded")
	}
	data, err := base64.StdEncoding.DecodeString(payload)
	if err != nil {
		return "", nil, apix.Translation("image data URL is not valid base64")
	}
	return mimeType, data, nil
}

func decodeOpenAIForm(id apix.Protocol, req apix.Payload) (*openaiRequest, *apix.Error) {
	_, params, err := mime.ParseMediaType(req.ContentType)
	if err != nil || params["boundary"] == "" {
		return nil, apix.Translation(string(id) + " multipart request has no boundary")
	}
	r := &openaiRequest{id: id}
	seen := map[string]bool{}
	mr := multipart.NewReader(bytes.NewReader(req.Body), params["boundary"])
	for {
		part, err := mr.NextPart()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return nil, apix.Translation("malformed " + string(id) + " multipart request: " + err.Error())
		}
		name := part.FormName()
		data, err := io.ReadAll(part)
		if err != nil {
			return nil, apix.Translation("malformed " + string(id) + " multipart request: " + err.Error())
		}
		seen[name] = true
		if aerr := r.setFormField(name, part, data); aerr != nil {
			return nil, aerr
		}
	}
	for k := range seen {
		r.keys = append(r.keys, k)
	}
	sort.Strings(r.keys)
	return r, nil
}

func (r *openaiRequest) setFormField(name string, part *multipart.Part, data []byte) *apix.Error {
	text := string(data)
	integer := func(dst *int) *apix.Error {
		n, err := strconv.Atoi(strings.TrimSpace(text))
		if err != nil {
			return apix.Translation(string(r.id) + " request " + name + " must be an integer")
		}
		*dst = n
		return nil
	}
	switch name {
	case "image", "image[]":
		r.Images = append(r.Images, formMedia(part, data))
	case "mask":
		m := formMedia(part, data)
		r.Mask = &m
	case "model":
		r.Model = text
	case "prompt":
		r.Prompt = text
	case "size":
		r.Size = text
	case "quality":
		r.Quality = text
	case "background":
		r.Background = text
	case "output_format":
		r.OutputFormat = text
	case "response_format":
		r.ResponseFormat = text
	case "n":
		return integer(&r.N)
	case "partial_images":
		return integer(&r.PartialImages)
	case "output_compression":
		var n int
		if err := integer(&n); err != nil {
			return err
		}
		r.OutputCompression = &n
	case "stream":
		b, err := strconv.ParseBool(strings.TrimSpace(text))
		if err != nil {
			return apix.Translation(string(r.id) + " request stream must be a boolean")
		}
		r.Stream = b
	}
	return nil
}

func formMedia(part *multipart.Part, data []byte) media {
	mimeType, _, _ := mime.ParseMediaType(part.Header.Get("Content-Type"))
	if mimeType == "" || mimeType == "application/octet-stream" {
		if ext := strings.ToLower(part.FileName()); ext != "" {
			if i := strings.LastIndexByte(ext, '.'); i >= 0 {
				mimeType, _, _ = mime.ParseMediaType(mime.TypeByExtension(ext[i:]))
			}
		}
	}
	if mimeType == "" || mimeType == "application/octet-stream" {
		mimeType = http.DetectContentType(data)
	}
	return media{MIME: mimeType, Data: data}
}

// rewriteOpenAI sets the model and the stream field of a native request,
// keeping every other field and part unchanged. An empty model keeps the
// client's.
func rewriteOpenAI(req apix.Payload, model string, stream *bool) (apix.Payload, *apix.Error) {
	set := map[string]string{}
	if model != "" {
		set["model"] = model
	}
	if stream != nil {
		set["stream"] = strconv.FormatBool(*stream)
	}
	if isMultipart(req.ContentType) {
		return rewriteForm(req, set)
	}
	var root map[string]json.RawMessage
	if json.Unmarshal(req.Body, &root) != nil || root == nil {
		return apix.Payload{}, apix.Translation("request must be a JSON object")
	}
	if model != "" {
		root["model"], _ = json.Marshal(model)
	}
	if stream != nil {
		if *stream || root["stream"] != nil {
			root["stream"], _ = json.Marshal(*stream)
		}
		if !*stream {
			delete(root, "partial_images")
		}
	}
	body, _ := json.Marshal(root)
	return apix.Payload{Body: body, ContentType: "application/json"}, nil
}

// rewriteForm copies a multipart form, replacing the values of the fields in
// set and appending those it lacks.
func rewriteForm(req apix.Payload, set map[string]string) (apix.Payload, *apix.Error) {
	_, params, _ := mime.ParseMediaType(req.ContentType)
	mr := multipart.NewReader(bytes.NewReader(req.Body), params["boundary"])
	var out bytes.Buffer
	w := multipart.NewWriter(&out)
	done := map[string]bool{}
	for {
		part, err := mr.NextPart()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return apix.Payload{}, apix.Translation("malformed multipart request: " + err.Error())
		}
		name := part.FormName()
		if value, ok := set[name]; ok {
			if !done[name] {
				_ = w.WriteField(name, value)
				done[name] = true
			}
			continue
		}
		if name == "partial_images" && set["stream"] == "false" {
			continue
		}
		header := textproto.MIMEHeader{}
		for k, v := range part.Header {
			header[k] = append([]string(nil), v...)
		}
		pw, _ := w.CreatePart(header)
		if _, err := io.Copy(pw, part); err != nil {
			return apix.Payload{}, apix.Translation("malformed multipart request: " + err.Error())
		}
	}
	for _, name := range []string{"model", "stream"} {
		if value, ok := set[name]; ok && !done[name] && (name != "stream" || value == "true") {
			_ = w.WriteField(name, value)
		}
	}
	_ = w.Close()
	return apix.Payload{Body: out.Bytes(), ContentType: w.FormDataContentType()}, nil
}

// formField is one text field of an encoded edits form.
type formField struct{ name, value string }

// encodeEditsForm renders an edits request as a multipart form. One image
// uses the image field; several use image[].
func encodeEditsForm(fields []formField, images []media) apix.Payload {
	var out bytes.Buffer
	w := multipart.NewWriter(&out)
	name := "image"
	if len(images) > 1 {
		name = "image[]"
	}
	for i, m := range images {
		header := textproto.MIMEHeader{}
		header.Set("Content-Disposition", `form-data; name="`+name+`"; filename="image-`+strconv.Itoa(i+1)+extension(m.MIME)+`"`)
		header.Set("Content-Type", m.MIME)
		pw, _ := w.CreatePart(header)
		_, _ = pw.Write(m.Data)
	}
	for _, f := range fields {
		_ = w.WriteField(f.name, f.value)
	}
	_ = w.Close()
	return apix.Payload{Body: out.Bytes(), ContentType: w.FormDataContentType()}
}

// openaiResponse is the generations and edits response.
type openaiResponse struct {
	Created      int64         `json:"created"`
	Data         []openaiImage `json:"data"`
	Background   string        `json:"background,omitempty"`
	OutputFormat string        `json:"output_format,omitempty"`
	Quality      string        `json:"quality,omitempty"`
	Size         string        `json:"size,omitempty"`
	Usage        *openaiUsage  `json:"usage,omitempty"`
}

type openaiImage struct {
	B64JSON       string `json:"b64_json,omitempty"`
	URL           string `json:"url,omitempty"`
	RevisedPrompt string `json:"revised_prompt,omitempty"`
}

type openaiUsage struct {
	InputTokens        int64               `json:"input_tokens"`
	OutputTokens       int64               `json:"output_tokens"`
	TotalTokens        int64               `json:"total_tokens"`
	InputTokensDetails *openaiTokenDetails `json:"input_tokens_details,omitempty"`
	// OutputTokensDetails is reported by newer GPT image models.
	OutputTokensDetails *openaiTokenDetails `json:"output_tokens_details,omitempty"`
}

type openaiTokenDetails struct {
	ImageTokens int64 `json:"image_tokens"`
	TextTokens  int64 `json:"text_tokens"`
}

func decodeOpenAIResponse(body []byte) (openaiResponse, *apix.Error) {
	var root map[string]json.RawMessage
	if json.Unmarshal(body, &root) != nil || root == nil {
		return openaiResponse{}, apix.Translation("OpenAI images response must be a JSON object")
	}
	var r openaiResponse
	if err := json.Unmarshal(body, &r); err != nil {
		return openaiResponse{}, apix.Translation("malformed OpenAI images response: " + err.Error())
	}
	if root["data"] == nil {
		return openaiResponse{}, apix.Translation("OpenAI images response has no data")
	}
	return r, nil
}

// openaiStreamEvent is a completed or partial image event.
type openaiStreamEvent struct {
	Type         string       `json:"type"`
	B64JSON      string       `json:"b64_json"`
	CreatedAt    int64        `json:"created_at,omitempty"`
	Background   string       `json:"background,omitempty"`
	OutputFormat string       `json:"output_format,omitempty"`
	Quality      string       `json:"quality,omitempty"`
	Size         string       `json:"size,omitempty"`
	Usage        *openaiUsage `json:"usage,omitempty"`
}

// eventPrefix is the stream event namespace of an OpenAI protocol.
func eventPrefix(id apix.Protocol) string {
	if protocols[id].edit {
		return "image_edit"
	}
	return "image_generation"
}

// renderOpenAIStream renders a converted response as the events a stream
// with no partial images carries: one completed event per image. Usage
// belongs to the last event.
func renderOpenAIStream(id apix.Protocol, r openaiResponse) []byte {
	var out bytes.Buffer
	typ := eventPrefix(id) + ".completed"
	for i, img := range r.Data {
		ev := openaiStreamEvent{Type: typ, B64JSON: img.B64JSON, CreatedAt: r.Created, Background: r.Background,
			OutputFormat: r.OutputFormat, Quality: r.Quality, Size: r.Size}
		if i == len(r.Data)-1 {
			ev.Usage = r.Usage
		}
		data, _ := json.Marshal(ev)
		out.WriteString("event: " + typ + "\ndata: ")
		out.Write(data)
		out.WriteString("\n\n")
	}
	return out.Bytes()
}
