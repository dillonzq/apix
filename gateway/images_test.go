package gateway_test

import (
	"bytes"
	"encoding/json"
	"io"
	"mime"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"sync"
	"testing"

	"github.com/dillonzq/apix"
	"github.com/dillonzq/apix/gateway"
	"github.com/dillonzq/apix/protocol/gemini"
	"github.com/dillonzq/apix/protocol/openai"
	"github.com/dillonzq/apix/route"
)

const geminiImageReply = `{"candidates":[{"content":{"role":"model","parts":[{"inlineData":{"mimeType":"image/png","data":"aW1n"}}]},"finishReason":"STOP"}],"usageMetadata":{"promptTokenCount":3,"candidatesTokenCount":1290,"totalTokenCount":1293}}`

// rawUpstream records raw request bodies, which may be multipart forms.
type rawUpstream struct {
	*httptest.Server
	mu   sync.Mutex
	reqs []*http.Request
	body [][]byte
}

func newRawUpstream(t *testing.T, status int, contentType, reply string) *rawUpstream {
	u := &rawUpstream{}
	u.Server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		raw, _ := io.ReadAll(r.Body)
		u.mu.Lock()
		u.reqs, u.body = append(u.reqs, r.Clone(r.Context())), append(u.body, raw)
		u.mu.Unlock()
		w.Header().Set("Content-Type", contentType)
		w.WriteHeader(status)
		io.WriteString(w, reply)
	}))
	t.Cleanup(u.Close)
	return u
}

func (u *rawUpstream) last(t *testing.T) (*http.Request, []byte) {
	t.Helper()
	u.mu.Lock()
	defer u.mu.Unlock()
	if len(u.reqs) == 0 {
		t.Fatal("upstream not called")
	}
	return u.reqs[len(u.reqs)-1], u.body[len(u.body)-1]
}

func (u *rawUpstream) hits() int {
	u.mu.Lock()
	defer u.mu.Unlock()
	return len(u.reqs)
}

func imageRegistry(t *testing.T) *apix.Registry {
	t.Helper()
	reg, err := apix.NewRegistry(slices.Concat(openai.Codecs(), gemini.Codecs())...)
	if err != nil {
		t.Fatal(err)
	}
	return reg
}

// Image requests never reach LLM endpoints, fail over from a native endpoint
// to a convertible one, and reach Gemini at the model's URL.
func TestImagesGenerationsFailover(t *testing.T) {
	native := newRawUpstream(t, 503, "application/json", `{"error":{"message":"overloaded","type":"server_error"}}`)
	gem := newRawUpstream(t, 200, "application/json", geminiImageReply)
	chat := newRawUpstream(t, 200, "application/json", chatReply)
	gw, err := gateway.New(gateway.Config{Registry: imageRegistry(t), Upstreams: []gateway.Upstream{
		{Endpoint: route.Endpoint{ID: "chat", Protocol: openai.ChatCompletions}, BaseURL: chat.URL},
		{Endpoint: route.Endpoint{ID: "gem", Protocol: gemini.GenerateContentImage, Models: map[string]string{"image": "gemini-2.5-flash-image"},
			Options: route.EndpointOptions{Ext: gemini.GenerateContentImageOptions{ResponseModalities: []gemini.Modality{gemini.ModalityText, gemini.ModalityImage}}}}, BaseURL: gem.URL, APIKey: "gem-key"},
		{Endpoint: route.Endpoint{ID: "openai", Protocol: openai.ImagesGenerations, Priority: 9, Models: map[string]string{"image": "gpt-image-1"}}, BaseURL: native.URL, APIKey: "oa-key"},
	}})
	if err != nil {
		t.Fatal(err)
	}
	rr := httptest.NewRecorder()
	gw.Handler(openai.ImagesGenerations).ServeHTTP(rr, httptest.NewRequest("POST", "/v1/images/generations", strings.NewReader(`{"model":"image","prompt":"a cat","size":"1024x1024"}`)))
	if rr.Code != 200 || rr.Header().Get("Content-Type") != "application/json" {
		t.Fatalf("%d %s", rr.Code, rr.Body)
	}
	var resp struct {
		Data []struct {
			B64JSON string `json:"b64_json"`
		} `json:"data"`
		Usage struct {
			TotalTokens int `json:"total_tokens"`
		} `json:"usage"`
	}
	if json.Unmarshal(rr.Body.Bytes(), &resp) != nil || len(resp.Data) != 1 || resp.Data[0].B64JSON != "aW1n" || resp.Usage.TotalTokens != 1293 {
		t.Fatalf("response %s", rr.Body)
	}
	if r, _ := native.last(t); r.URL.Path != "/v1/images/generations" || r.Header.Get("Authorization") != "Bearer oa-key" {
		t.Fatalf("native request %s %v", r.URL.Path, r.Header)
	}
	r, body := gem.last(t)
	if r.URL.Path != "/v1beta/models/gemini-2.5-flash-image:generateContent" || r.Header.Get("X-Goog-Api-Key") != "gem-key" || !bytes.Contains(body, []byte(`"aspectRatio":"1:1"`)) || !bytes.Contains(body, []byte(`"responseModalities":["TEXT","IMAGE"]`)) {
		t.Fatalf("gemini request %s %v %s", r.URL.Path, r.Header, body)
	}
	if chat.hits() != 0 {
		t.Fatal("an image request reached an LLM endpoint")
	}
}

// Endpoint options are validated when the gateway is configured.
func TestImagesInvalidEndpointOptions(t *testing.T) {
	_, err := gateway.New(gateway.Config{Registry: imageRegistry(t), Upstreams: []gateway.Upstream{
		{Endpoint: route.Endpoint{ID: "gem", Protocol: gemini.GenerateContentImage,
			Options: route.EndpointOptions{Ext: gemini.GenerateContentImageOptions{ResponseModalities: []gemini.Modality{gemini.ModalityText}}}}, BaseURL: "https://example.com"},
	}})
	if err == nil {
		t.Fatal("text-only modalities accepted")
	}
}

// A streamed multipart edit to Gemini is sent without streaming and returned
// as a complete event stream.
func TestImagesEditsEmulatedStream(t *testing.T) {
	gem := newRawUpstream(t, 200, "application/json", geminiImageReply)
	gw, err := gateway.New(gateway.Config{Registry: imageRegistry(t), Upstreams: []gateway.Upstream{
		{Endpoint: route.Endpoint{ID: "gem", Protocol: gemini.GenerateContentImage, Models: map[string]string{"*": "gemini-2.5-flash-image"}}, BaseURL: gem.URL},
	}})
	if err != nil {
		t.Fatal(err)
	}
	var buf bytes.Buffer
	w := multipart.NewWriter(&buf)
	fw, _ := w.CreateFormFile("image", "a.png")
	fw.Write([]byte("\x89PNG\r\n\x1a\n"))
	w.WriteField("prompt", "add a hat")
	w.WriteField("model", "gpt-image-1")
	w.WriteField("stream", "true")
	w.Close()
	req := httptest.NewRequest("POST", "/v1/images/edits", &buf)
	req.Header.Set("Content-Type", w.FormDataContentType())
	rr := httptest.NewRecorder()
	gw.Handler(openai.ImagesEdits).ServeHTTP(rr, req)
	if rr.Code != 200 || rr.Header().Get("Content-Type") != "text/event-stream" || !strings.HasPrefix(rr.Body.String(), "event: image_edit.completed\n") {
		t.Fatalf("%d %s %s", rr.Code, rr.Header(), rr.Body)
	}
	r, body := gem.last(t)
	if r.URL.Path != "/v1beta/models/gemini-2.5-flash-image:generateContent" || !bytes.Contains(body, []byte(`"inlineData":{"data":"iVBORw0KGgo=","mimeType":"image/png"}`)) {
		t.Fatalf("gemini request %s %s", r.URL.Path, body)
	}
}

// A Gemini image edit reaches OpenAI edits as a multipart form, and the
// response returns in the Gemini format.
func TestGeminiImageToOpenAIEdits(t *testing.T) {
	edits := newRawUpstream(t, 200, "application/json", `{"created":1,"data":[{"b64_json":"aW1n"}],"output_format":"png"}`)
	gw, err := gateway.New(gateway.Config{Registry: imageRegistry(t), Upstreams: []gateway.Upstream{
		{Endpoint: route.Endpoint{ID: "edits", Protocol: openai.ImagesEdits, Models: map[string]string{"nano": "gpt-image-1"}}, BaseURL: edits.URL, APIKey: "oa-key"},
	}})
	if err != nil {
		t.Fatal(err)
	}
	body := `{"contents":[{"role":"user","parts":[{"text":"add a hat"},{"inlineData":{"mimeType":"image/png","data":"aW1n"}}]}]}`
	rr := httptest.NewRecorder()
	gw.Handler(gemini.GenerateContentImage).ServeHTTP(rr, httptest.NewRequest("POST", "/v1beta/models/nano:generateContent", strings.NewReader(body)))
	if rr.Code != 200 || !strings.Contains(rr.Body.String(), `"inlineData":{"mimeType":"image/png","data":"aW1n"}`) {
		t.Fatalf("%d %s", rr.Code, rr.Body)
	}
	r, raw := edits.last(t)
	_, params, _ := mime.ParseMediaType(r.Header.Get("Content-Type"))
	if r.URL.Path != "/v1/images/edits" || r.Header.Get("Authorization") != "Bearer oa-key" || params["boundary"] == "" {
		t.Fatalf("edits request %s %v", r.URL.Path, r.Header)
	}
	form, err := multipart.NewReader(bytes.NewReader(raw), params["boundary"]).ReadForm(1 << 20)
	if err != nil || form.Value["model"][0] != "gpt-image-1" || form.Value["prompt"][0] != "add a hat" || len(form.File["image"]) != 1 {
		t.Fatalf("form %v %v", form, err)
	}

	// A streamed request calls OpenAI without streaming and returns one chunk.
	rr = httptest.NewRecorder()
	gw.Handler(gemini.GenerateContentImage).ServeHTTP(rr, httptest.NewRequest("POST", "/v1beta/models/nano:streamGenerateContent", strings.NewReader(body)))
	if rr.Code != 200 || rr.Header().Get("Content-Type") != "text/event-stream" || !strings.HasPrefix(rr.Body.String(), "data: {") || !strings.HasSuffix(rr.Body.String(), "}\n\n") {
		t.Fatalf("%d %s %s", rr.Code, rr.Header(), rr.Body)
	}
	if _, raw := edits.last(t); bytes.Contains(raw, []byte(`name="stream"`)) {
		t.Fatal("OpenAI asked to stream for a Gemini client")
	}

	// A text-only request cannot become an edit; the conversion error is
	// returned in the Gemini envelope without calling the upstream.
	rr = httptest.NewRecorder()
	gw.Handler(gemini.GenerateContentImage).ServeHTTP(rr, httptest.NewRequest("POST", "/v1beta/models/nano:generateContent", strings.NewReader(`{"contents":[{"parts":[{"text":"a cat"}]}]}`)))
	if rr.Code != 400 || !strings.Contains(rr.Body.String(), `"status":"INVALID_ARGUMENT"`) || edits.hits() != 2 {
		t.Fatalf("%d %s", rr.Code, rr.Body)
	}
}

// Endpoint modalities also replace those of native Gemini image requests.
func TestGeminiImageNativeModalities(t *testing.T) {
	gem := newRawUpstream(t, 200, "application/json", geminiImageReply)
	gw, err := gateway.New(gateway.Config{Registry: imageRegistry(t), Upstreams: []gateway.Upstream{
		{Endpoint: route.Endpoint{ID: "gem", Protocol: gemini.GenerateContentImage, Models: map[string]string{"nano": "gemini-3-pro-image"},
			Options: route.EndpointOptions{Ext: gemini.GenerateContentImageOptions{ResponseModalities: []gemini.Modality{gemini.ModalityText, gemini.ModalityImage}}}}, BaseURL: gem.URL},
	}})
	if err != nil {
		t.Fatal(err)
	}
	body := `{"contents":[{"parts":[{"text":"a cat"}]}],"generationConfig":{"responseModalities":["IMAGE"],"imageConfig":{"imageSize":"2K"}}}`
	rr := httptest.NewRecorder()
	gw.Handler(gemini.GenerateContentImage).ServeHTTP(rr, httptest.NewRequest("POST", "/v1beta/models/nano:generateContent", strings.NewReader(body)))
	if rr.Code != 200 || rr.Body.String() != geminiImageReply {
		t.Fatalf("%d %s", rr.Code, rr.Body)
	}
	r, sent := gem.last(t)
	if r.URL.Path != "/v1beta/models/gemini-3-pro-image:generateContent" || !bytes.Contains(sent, []byte(`"responseModalities":["TEXT","IMAGE"]`)) || !bytes.Contains(sent, []byte(`"imageSize":"2K"`)) {
		t.Fatalf("%s %s", r.URL.Path, sent)
	}
}
