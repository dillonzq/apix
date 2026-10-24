package image_test

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"errors"
	"io"
	"mime"
	"mime/multipart"
	"net/textproto"
	"slices"
	"strings"
	"testing"

	"github.com/dillonzq/apix"
	"github.com/dillonzq/apix/protocol/gemini"
	"github.com/dillonzq/apix/protocol/openai"
)

var png = []byte("\x89PNG\r\n\x1a\nfake")

var b64 = base64.StdEncoding.EncodeToString(png)

func registry(t *testing.T) *apix.Registry {
	t.Helper()
	reg, err := apix.NewRegistry(slices.Concat(openai.Codecs(), gemini.Codecs())...)
	if err != nil {
		t.Fatal(err)
	}
	return reg
}

func exchange(t *testing.T, client, upstream apix.Protocol, opts apix.ExchangeOptions) apix.Exchange {
	t.Helper()
	ex, err := registry(t).NewExchange(client, upstream, opts)
	if err != nil {
		t.Fatal(err)
	}
	return ex
}

// form builds a multipart edits request; files are field name/content type pairs.
func form(t *testing.T, fields map[string]string, files ...[2]string) apix.Payload {
	t.Helper()
	var buf bytes.Buffer
	w := multipart.NewWriter(&buf)
	for _, f := range files {
		h := textproto.MIMEHeader{}
		h.Set("Content-Disposition", `form-data; name="`+f[0]+`"; filename="a.png"`)
		h.Set("Content-Type", f[1])
		pw, _ := w.CreatePart(h)
		pw.Write(png)
	}
	keys := make([]string, 0, len(fields))
	for k := range fields {
		keys = append(keys, k)
	}
	slices.Sort(keys)
	for _, k := range keys {
		w.WriteField(k, fields[k])
	}
	w.Close()
	return apix.Payload{Body: buf.Bytes(), ContentType: w.FormDataContentType()}
}

// readForm returns the text fields and file parts of a multipart payload.
func readForm(t *testing.T, p apix.Payload) (map[string]string, map[string][][]byte) {
	t.Helper()
	_, params, err := mime.ParseMediaType(p.ContentType)
	if err != nil {
		t.Fatalf("content type %q: %v", p.ContentType, err)
	}
	fields, files := map[string]string{}, map[string][][]byte{}
	r := multipart.NewReader(bytes.NewReader(p.Body), params["boundary"])
	for {
		part, err := r.NextPart()
		if errors.Is(err, io.EOF) {
			return fields, files
		}
		if err != nil {
			t.Fatal(err)
		}
		data, _ := io.ReadAll(part)
		if part.FileName() != "" {
			files[part.FormName()] = append(files[part.FormName()], data)
		} else {
			fields[part.FormName()] = string(data)
		}
	}
}

func jsonEqual(t *testing.T, got []byte, want string) {
	t.Helper()
	var g, w any
	if err := json.Unmarshal(got, &g); err != nil {
		t.Fatalf("got invalid JSON %s", got)
	}
	if err := json.Unmarshal([]byte(want), &w); err != nil {
		t.Fatalf("want invalid JSON %s", want)
	}
	gb, _ := json.Marshal(g)
	wb, _ := json.Marshal(w)
	if !bytes.Equal(gb, wb) {
		t.Fatalf("\n got %s\nwant %s", gb, wb)
	}
}

func class(err error) apix.Class {
	if e := apix.AsError(err); e != nil {
		return e.Class
	}
	return ""
}

func TestSpecs(t *testing.T) {
	reg := registry(t)
	for p, want := range map[apix.Protocol]string{
		openai.ImagesGenerations:    "/v1/images/generations",
		openai.ImagesEdits:          "/v1/images/edits",
		gemini.GenerateContentImage: "/v1beta/models/m:generateContent",
		gemini.Imagen:               "/v1beta/models/m:predict",
	} {
		spec, ok := reg.Spec(p)
		if !ok || spec.Kind != apix.KindImage || spec.Method != "POST" || spec.Path(apix.RequestInfo{}, "m") != want {
			t.Fatalf("%s: %+v", p, spec)
		}
	}
	if spec, _ := reg.Spec(gemini.GenerateContentImage); spec.Path(apix.RequestInfo{Stream: true}, "models/m") != "/v1beta/models/m:streamGenerateContent?alt=sse" || spec.Auth.Header != "x-goog-api-key" {
		t.Fatalf("stream path: %+v", spec)
	}
	if spec, _ := reg.Spec(gemini.Imagen); spec.Stream != apix.FramingNone {
		t.Fatal("Imagen must not stream")
	}
	spec, _ := reg.Spec(gemini.Imagen)
	md, err := spec.ParsePath("/v1beta/models/imagen-4.0-generate-001:predict")
	if err != nil || md.Model != "imagen-4.0-generate-001" || md.Stream == nil || *md.Stream {
		t.Fatalf("metadata %+v, err %v", md, err)
	}
	if _, err := spec.ParsePath("/v1beta/models/m:generateContent"); class(err) != apix.ClassUnsupported {
		t.Fatalf("Imagen accepted generateContent: %v", err)
	}
	spec, _ = reg.Spec(gemini.GenerateContentImage)
	if md, err := spec.ParsePath("/v1beta/models/m:streamGenerateContent"); err != nil || !*md.Stream {
		t.Fatalf("metadata %+v, err %v", md, err)
	}
}

func TestConversionPaths(t *testing.T) {
	reg := registry(t)
	for _, tc := range []struct {
		client, upstream apix.Protocol
		want             bool
	}{
		{openai.ImagesGenerations, gemini.GenerateContentImage, true},
		{openai.ImagesGenerations, gemini.Imagen, true},
		{openai.ImagesEdits, gemini.GenerateContentImage, true},
		{openai.ImagesEdits, gemini.Imagen, false},
		{openai.ImagesEdits, openai.ImagesGenerations, false},
		{openai.ImagesGenerations, openai.ImagesEdits, false},
		{gemini.GenerateContentImage, openai.ImagesEdits, true},
		{gemini.Imagen, openai.ImagesGenerations, true},
		{gemini.Imagen, gemini.GenerateContentImage, true},
		// Kinds never mix.
		{openai.ImagesGenerations, openai.ChatCompletions, false},
		{gemini.GenerateContent, gemini.GenerateContentImage, false},
	} {
		if got := reg.CanConvert(tc.client, tc.upstream); got != tc.want {
			t.Errorf("CanConvert(%s, %s) = %v", tc.client, tc.upstream, got)
		}
	}
	if err := reg.ValidateOptions(gemini.Imagen, apix.ExchangeOptions{Ext: struct{}{}}); class(err) != apix.ClassUnsupported {
		t.Fatalf("Ext accepted: %v", err)
	}
	if err := reg.ValidateOptions(gemini.Imagen, apix.ExchangeOptions{Reasoning: &apix.Reasoning{Effort: "high"}}); class(err) != apix.ClassUnsupported {
		t.Fatalf("reasoning accepted: %v", err)
	}
	if err := reg.ValidateOptions(openai.ImagesEdits, apix.ExchangeOptions{Output: gemini.Imagen}); class(err) != apix.ClassUnsupported {
		t.Fatalf("output accepted: %v", err)
	}
}

func TestInspect(t *testing.T) {
	reg := registry(t)
	info, err := reg.Inspect(openai.ImagesGenerations, apix.Payload{Body: []byte(`{"model":"gpt-image-1","prompt":"cat","stream":true}`)})
	if err != nil || info.Kind != apix.KindImage || info.Model != "gpt-image-1" || !info.Stream || !slices.Equal(info.Features, []string{apix.FeatureStream}) {
		t.Fatalf("%+v %v", info, err)
	}
	info, err = reg.Inspect(openai.ImagesEdits, form(t, map[string]string{"model": "gpt-image-1", "prompt": "hat"}, [2]string{"image[]", "image/png"}))
	if err != nil || info.Model != "gpt-image-1" || !info.Needs(apix.FeatureImageInput) {
		t.Fatalf("%+v %v", info, err)
	}
	info, err = reg.Inspect(openai.ImagesEdits, apix.Payload{Body: []byte(`{"model":"gpt-image-1","prompt":"hat","images":[{"file_id":"file-1"},{"image_url":"data:image/png;base64,` + b64 + `"}]}`)})
	if err != nil || !slices.Equal(info.Affinity, []string{"file:file-1"}) {
		t.Fatalf("%+v %v", info, err)
	}
	info, err = reg.Inspect(gemini.GenerateContentImage, apix.Payload{
		Body:     []byte(`{"cached_content":"cachedContents/c1","contents":[{"parts":[{"text":"hat"},{"inline_data":{"mime_type":"image/png","data":"` + b64 + `"}}]}]}`),
		Metadata: apix.RequestMetadata{Model: "gemini-2.5-flash-image"},
	})
	if err != nil || info.Model != "gemini-2.5-flash-image" || !info.Needs(apix.FeatureImageInput) || !info.Stateful || info.Affinity[0] != "cached-content:cachedContents/c1" {
		t.Fatalf("%+v %v", info, err)
	}
	for _, tc := range []struct {
		p    apix.Protocol
		body apix.Payload
	}{
		{openai.ImagesGenerations, apix.Payload{Body: []byte(`{"model":"m"}`)}},
		{openai.ImagesGenerations, apix.Payload{Body: []byte(`{"prompt":1}`)}},
		{openai.ImagesGenerations, apix.Payload{Body: []byte(`{"prompt":"p","images":[{"file_id":"f"}]}`)}},
		{openai.ImagesEdits, form(t, map[string]string{"prompt": "hat"})},
		{openai.ImagesEdits, apix.Payload{Body: []byte(`{"prompt":"hat","images":[{"file_id":"f","image_url":"u"}]}`)}},
		{gemini.GenerateContentImage, apix.Payload{Body: []byte(`{"contents":[]}`)}},
		{gemini.Imagen, apix.Payload{Body: []byte(`{"instances":[{}]}`)}},
	} {
		if _, err := reg.Inspect(tc.p, tc.body); class(err) != apix.ClassTranslation {
			t.Errorf("%s %s: %v", tc.p, tc.body.Body, err)
		}
	}
}

func TestGenerationsToGemini(t *testing.T) {
	ex := exchange(t, openai.ImagesGenerations, gemini.GenerateContentImage, apix.ExchangeOptions{})
	out, err := ex.ConvertRequest(apix.Payload{Body: []byte(`{"model":"gpt-image-1","prompt":"a cat","n":2,"size":"1536x1024","quality":"high","output_format":"png"}`)}, "gemini-2.5-flash-image")
	if err != nil {
		t.Fatal(err)
	}
	jsonEqual(t, out.Body, `{"contents":[{"role":"user","parts":[{"text":"a cat"}]}],"generationConfig":{"responseModalities":["IMAGE"],"candidateCount":2,"imageConfig":{"aspectRatio":"3:2"}}}`)
	if out.Metadata.Model != "gemini-2.5-flash-image" || out.Metadata.Stream == nil || *out.Metadata.Stream {
		t.Fatalf("metadata %+v", out.Metadata)
	}
	if d := ex.Diagnostics(); len(d) != 1 || d[0].Path != "quality" || d[0].Semantic || d[0].From != openai.ImagesGenerations {
		t.Fatalf("diagnostics %+v", d)
	}
	resp, err := ex.ConvertResponse(200, apix.Payload{Body: []byte(`{"candidates":[
		{"index":0,"content":{"role":"model","parts":[{"inlineData":{"mimeType":"image/png","data":"draft"},"thought":true},{"text":"Here you go"},{"inlineData":{"mimeType":"image/png","data":"` + b64 + `"}}]},"finishReason":"STOP"},
		{"index":1,"content":{"parts":[{"inline_data":{"mime_type":"image/png","data":"second"}}]},"finishReason":"STOP"}],
		"usageMetadata":{"promptTokenCount":10,"candidatesTokenCount":1290,"thoughtsTokenCount":5,"totalTokenCount":1305,"promptTokensDetails":[{"modality":"TEXT","tokenCount":10}],"candidatesTokensDetails":[{"modality":"IMAGE","tokenCount":1290}]}}`)})
	if err != nil {
		t.Fatal(err)
	}
	var got map[string]any
	json.Unmarshal(resp.Body, &got)
	if got["created"] == nil {
		t.Fatalf("no created: %s", resp.Body)
	}
	delete(got, "created")
	body, _ := json.Marshal(got)
	jsonEqual(t, body, `{"data":[{"b64_json":"`+b64+`"},{"b64_json":"second"}],"output_format":"png","usage":{"input_tokens":10,"output_tokens":1295,"total_tokens":1305,"input_tokens_details":{"image_tokens":0,"text_tokens":10},"output_tokens_details":{"image_tokens":1290,"text_tokens":0}}}`)
	if d := ex.Diagnostics(); len(d) != 2 || d[1].Path != "candidates.content.parts.text" || d[1].From != gemini.GenerateContentImage {
		t.Fatalf("diagnostics %+v", d)
	}
}

func TestEmulatedStream(t *testing.T) {
	ex := exchange(t, openai.ImagesGenerations, gemini.Imagen, apix.ExchangeOptions{})
	out, err := ex.ConvertRequest(apix.Payload{Body: []byte(`{"prompt":"a cat","stream":true,"partial_images":2,"size":"1024x1792"}`)}, "imagen-4.0-generate-001")
	if err != nil {
		t.Fatal(err)
	}
	jsonEqual(t, out.Body, `{"instances":[{"prompt":"a cat"}],"parameters":{"aspectRatio":"9:16"}}`)
	if *out.Metadata.Stream {
		t.Fatal("Imagen asked to stream")
	}
	if _, err := ex.NewStreamConverter(); class(err) != apix.ClassUnsupported {
		t.Fatalf("stream converter: %v", err)
	}
	resp, err := ex.ConvertResponse(200, apix.Payload{Body: []byte(`{"predictions":[{"bytesBase64Encoded":"` + b64 + `","mimeType":"image/jpeg"}]}`)})
	if err != nil {
		t.Fatal(err)
	}
	if resp.ContentType != "text/event-stream" || !strings.HasPrefix(string(resp.Body), "event: image_generation.completed\ndata: {") || !strings.HasSuffix(string(resp.Body), "}\n\n") {
		t.Fatalf("%s %s", resp.ContentType, resp.Body)
	}
	data := strings.TrimSuffix(strings.SplitN(string(resp.Body), "data: ", 2)[1], "\n\n")
	var ev map[string]any
	if json.Unmarshal([]byte(data), &ev) != nil || ev["type"] != "image_generation.completed" || ev["b64_json"] != b64 || ev["output_format"] != "jpeg" {
		t.Fatalf("event %s", data)
	}
	if d := ex.Diagnostics(); len(d) != 2 || d[0].Path != "partial_images" && d[1].Path != "partial_images" {
		t.Fatalf("diagnostics %+v", d)
	}
}

func TestEditsToGemini(t *testing.T) {
	ex := exchange(t, openai.ImagesEdits, gemini.GenerateContentImage, apix.ExchangeOptions{})
	req := form(t, map[string]string{"model": "gpt-image-1", "prompt": "add a hat", "size": "1024x1024", "input_fidelity": "high"}, [2]string{"image[]", "image/png"}, [2]string{"image[]", "application/octet-stream"})
	out, err := ex.ConvertRequest(req, "gemini-2.5-flash-image")
	if err != nil {
		t.Fatal(err)
	}
	jsonEqual(t, out.Body, `{"contents":[{"role":"user","parts":[{"text":"add a hat"},{"inlineData":{"mimeType":"image/png","data":"`+b64+`"}},{"inlineData":{"mimeType":"image/png","data":"`+b64+`"}}]}],"generationConfig":{"responseModalities":["IMAGE"],"imageConfig":{"aspectRatio":"1:1"}}}`)

	// A mask cannot be expressed: Safe rejects it, Allow records it.
	masked := form(t, map[string]string{"prompt": "add a hat"}, [2]string{"image", "image/png"}, [2]string{"mask", "image/png"})
	safe := exchange(t, openai.ImagesEdits, gemini.GenerateContentImage, apix.ExchangeOptions{LossPolicy: apix.LossPolicySafe})
	if _, err := safe.ConvertRequest(masked, "m"); class(err) != apix.ClassUnsupported || !strings.Contains(err.Error(), "mask") {
		t.Fatalf("safe: %v", err)
	}
	allow := exchange(t, openai.ImagesEdits, gemini.GenerateContentImage, apix.ExchangeOptions{})
	if _, err := allow.ConvertRequest(masked, "m"); err != nil || allow.Diagnostics()[0].Path != "mask" || !allow.Diagnostics()[0].Semantic {
		t.Fatalf("allow: %v %+v", err, allow.Diagnostics())
	}
	strict := exchange(t, openai.ImagesEdits, gemini.GenerateContentImage, apix.ExchangeOptions{LossPolicy: apix.LossPolicyStrict})
	if _, err := strict.ConvertRequest(req, "m"); class(err) != apix.ClassUnsupported {
		t.Fatalf("strict: %v", err)
	}

	// Stored files and remote URLs need I/O that conversion never performs.
	for _, body := range []string{`{"prompt":"p","images":[{"file_id":"file-1"}]}`, `{"prompt":"p","images":[{"image_url":"https://example.com/a.png"}]}`} {
		if _, err := allow.ConvertRequest(apix.Payload{Body: []byte(body)}, "m"); class(err) != apix.ClassUnsupported {
			t.Fatalf("%s: %v", body, err)
		}
	}
	// JSON edits with data URLs convert like forms.
	out, err = allow.ConvertRequest(apix.Payload{Body: []byte(`{"prompt":"p","images":[{"image_url":"data:image/webp;base64,` + b64 + `"}]}`)}, "m")
	if err != nil || !strings.Contains(string(out.Body), `"mimeType":"image/webp"`) {
		t.Fatalf("%s %v", out.Body, err)
	}
}

func TestGeminiToOpenAI(t *testing.T) {
	image := `{"inlineData":{"mimeType":"image/png","data":"` + b64 + `"}}`
	edit := `{"systemInstruction":{"parts":[{"text":"be nice"}]},"contents":[{"role":"user","parts":[{"text":"add a hat"},` + image + `]}],"generationConfig":{"candidateCount":2,"temperature":1,"responseModalities":["TEXT","IMAGE"],"imageConfig":{"aspectRatio":"16:9"}}}`
	ex := exchange(t, gemini.GenerateContentImage, openai.ImagesEdits, apix.ExchangeOptions{})
	out, err := ex.ConvertRequest(apix.Payload{Body: []byte(edit), Metadata: apix.RequestMetadata{Model: "nano"}}, "gpt-image-1")
	if err != nil {
		t.Fatal(err)
	}
	fields, files := readForm(t, out)
	if fields["model"] != "gpt-image-1" || fields["prompt"] != "add a hat" || fields["n"] != "2" || fields["size"] != "1536x1024" || len(files["image"]) != 1 || !bytes.Equal(files["image"][0], png) {
		t.Fatalf("%v %v", fields, files)
	}
	var paths []string
	for _, d := range ex.Diagnostics() {
		paths = append(paths, d.Path)
	}
	slices.Sort(paths)
	if !slices.Equal(paths, []string{"generationConfig.imageConfig.aspectRatio", "generationConfig.responseModalities", "generationConfig.temperature", "systemInstruction"}) {
		t.Fatalf("diagnostics %v", paths)
	}

	// Generations take no input images; edits need one.
	gen := exchange(t, gemini.GenerateContentImage, openai.ImagesGenerations, apix.ExchangeOptions{})
	if _, err := gen.ConvertRequest(apix.Payload{Body: []byte(edit)}, "gpt-image-1"); class(err) != apix.ClassUnsupported {
		t.Fatalf("generations with image: %v", err)
	}
	text := `{"contents":[{"parts":[{"text":"a cat"}]}],"generationConfig":{"imageConfig":{"aspectRatio":"2:3"}}}`
	if _, err := exchange(t, gemini.GenerateContentImage, openai.ImagesEdits, apix.ExchangeOptions{}).ConvertRequest(apix.Payload{Body: []byte(text)}, "gpt-image-1"); class(err) != apix.ClassUnsupported {
		t.Fatalf("edits without image: %v", err)
	}
	// A streamed request converted back to Gemini calls OpenAI without streaming.
	stream := true
	out, err = exchange(t, gemini.GenerateContentImage, openai.ImagesEdits, apix.ExchangeOptions{}).ConvertRequest(apix.Payload{Body: []byte(edit), Metadata: apix.RequestMetadata{Stream: &stream}}, "gpt-image-1")
	if fields, _ := readForm(t, out); err != nil || fields["stream"] != "" || out.Metadata.Stream == nil || *out.Metadata.Stream {
		t.Fatalf("%v %v %+v", err, fields, out.Metadata)
	}
	// A streamed request whose output is the upstream's protocol streams natively.
	native := exchange(t, gemini.GenerateContentImage, openai.ImagesGenerations, apix.ExchangeOptions{Output: openai.ImagesGenerations})
	out, err = native.ConvertRequest(apix.Payload{Body: []byte(text), Metadata: apix.RequestMetadata{Stream: &stream}}, "gpt-image-1")
	if err != nil {
		t.Fatal(err)
	}
	jsonEqual(t, out.Body, `{"model":"gpt-image-1","prompt":"a cat","size":"1024x1536","stream":true}`)
	if _, err := native.NewStreamConverter(); err != nil {
		t.Fatal(err)
	}

	resp, err := ex.ConvertResponse(200, apix.Payload{Body: []byte(`{"created":1,"data":[{"b64_json":"a","revised_prompt":"r"},{"b64_json":"b"}],"output_format":"webp","usage":{"input_tokens":5,"output_tokens":7,"total_tokens":12,"input_tokens_details":{"text_tokens":3,"image_tokens":2}}}`)})
	if err != nil {
		t.Fatal(err)
	}
	jsonEqual(t, resp.Body, `{"candidates":[
		{"content":{"role":"model","parts":[{"inlineData":{"mimeType":"image/webp","data":"a"}}]},"finishReason":"STOP","index":0},
		{"content":{"role":"model","parts":[{"inlineData":{"mimeType":"image/webp","data":"b"}}]},"finishReason":"STOP","index":1}],
		"usageMetadata":{"promptTokenCount":5,"candidatesTokenCount":7,"totalTokenCount":12,"promptTokensDetails":[{"modality":"TEXT","tokenCount":3},{"modality":"IMAGE","tokenCount":2}]},
		"modelVersion":"gpt-image-1"}`)
	_, err = ex.ConvertResponse(200, apix.Payload{Body: []byte(`{"created":1,"data":[{"url":"https://example.com/a.png"}]}`)})
	if aerr := apix.AsError(err); aerr == nil || aerr.Class != apix.ClassUpstream || !aerr.Retryable {
		t.Fatalf("url result must fail over: %v", err)
	}
}

// TestDuplicateSpellings checks that a field sent in both camelCase and
// snake_case reads the camelCase value every time, whatever the map order.
func TestDuplicateSpellings(t *testing.T) {
	body := `{"contents":[{"parts":[{"text":"a fox"}]}],"generation_config":{"candidate_count":3},` +
		`"generationConfig":{"candidate_count":4,"candidateCount":2}}`
	for range 50 {
		ex := exchange(t, gemini.GenerateContentImage, openai.ImagesGenerations, apix.ExchangeOptions{})
		out, err := ex.ConvertRequest(apix.Payload{Body: []byte(body)}, "gpt-image-1")
		if err != nil {
			t.Fatal(err)
		}
		jsonEqual(t, out.Body, `{"model":"gpt-image-1","prompt":"a fox","n":2}`)
	}
}

func TestImagen(t *testing.T) {
	req := apix.Payload{Body: []byte(`{"instances":[{"prompt":"a fox"}],"parameters":{"sampleCount":3,"aspectRatio":"4:3","imageSize":"2K","personGeneration":"allow_adult","outputOptions":{"mimeType":"image/jpeg","compressionQuality":80}}}`)}
	ex := exchange(t, gemini.Imagen, openai.ImagesGenerations, apix.ExchangeOptions{})
	out, err := ex.ConvertRequest(req, "gpt-image-1")
	if err != nil {
		t.Fatal(err)
	}
	jsonEqual(t, out.Body, `{"model":"gpt-image-1","prompt":"a fox","n":3,"size":"1536x1024","output_format":"jpeg","output_compression":80}`)
	var paths []string
	for _, d := range ex.Diagnostics() {
		paths = append(paths, d.Path)
	}
	slices.Sort(paths)
	if !slices.Equal(paths, []string{"parameters.aspectRatio", "parameters.imageSize", "parameters.personGeneration"}) {
		t.Fatalf("diagnostics %v", paths)
	}
	resp, err := ex.ConvertResponse(200, apix.Payload{Body: []byte(`{"created":1,"data":[{"b64_json":"a"}],"output_format":"jpeg","usage":{"input_tokens":1,"output_tokens":2,"total_tokens":3}}`)})
	if err != nil {
		t.Fatal(err)
	}
	jsonEqual(t, resp.Body, `{"predictions":[{"bytesBase64Encoded":"a","mimeType":"image/jpeg"}]}`)

	ex = exchange(t, gemini.Imagen, gemini.GenerateContentImage, apix.ExchangeOptions{})
	out, err = ex.ConvertRequest(apix.Payload{Body: req.Body, Metadata: apix.RequestMetadata{Model: "imagen-4"}}, "gemini-3-pro-image")
	if err != nil {
		t.Fatal(err)
	}
	jsonEqual(t, out.Body, `{"contents":[{"role":"user","parts":[{"text":"a fox"}]}],"generationConfig":{"responseModalities":["IMAGE"],"candidateCount":3,"imageConfig":{"aspectRatio":"4:3","imageSize":"2K"}}}`)
	if out.Metadata.Model != "gemini-3-pro-image" {
		t.Fatalf("metadata %+v", out.Metadata)
	}
	resp, err = ex.ConvertResponse(200, apix.Payload{Body: []byte(`{"candidates":[{"content":{"parts":[{"inlineData":{"mimeType":"image/png","data":"a"}}]},"finishReason":"STOP"}]}`)})
	if err != nil {
		t.Fatal(err)
	}
	jsonEqual(t, resp.Body, `{"predictions":[{"bytesBase64Encoded":"a","mimeType":"image/png"}]}`)

	ex = exchange(t, gemini.GenerateContentImage, gemini.Imagen, apix.ExchangeOptions{})
	out, err = ex.ConvertRequest(apix.Payload{Body: []byte(`{"contents":[{"parts":[{"text":"a fox"}]}],"generationConfig":{"imageConfig":{"aspectRatio":"21:9","imageSize":"4K"}}}`)}, "imagen-4")
	if err != nil {
		t.Fatal(err)
	}
	jsonEqual(t, out.Body, `{"instances":[{"prompt":"a fox"}],"parameters":{"aspectRatio":"16:9","imageSize":"2K"}}`)
	if len(ex.Diagnostics()) != 2 {
		t.Fatalf("diagnostics %+v", ex.Diagnostics())
	}
}

func TestNoImage(t *testing.T) {
	ex := exchange(t, openai.ImagesGenerations, gemini.GenerateContentImage, apix.ExchangeOptions{})
	if _, err := ex.ConvertRequest(apix.Payload{Body: []byte(`{"prompt":"p"}`)}, "m"); err != nil {
		t.Fatal(err)
	}
	for body, want := range map[string]bool{
		`{"candidates":[{"content":{"parts":[{"text":"no"}]},"finishReason":"IMAGE_SAFETY"}]}`: false,
		`{"promptFeedback":{"blockReason":"PROHIBITED_CONTENT"}}`:                              false,
		`{"candidates":[{"content":{"parts":[{"text":"sure"}]},"finishReason":"STOP"}]}`:       true,
		`{"candidates":[{"content":{"parts":[]},"finishReason":"NO_IMAGE"}]}`:                  true,
	} {
		_, err := ex.ConvertResponse(200, apix.Payload{Body: []byte(body)})
		if e := apix.AsError(err); e == nil || e.Retryable != want {
			t.Errorf("%s: %+v", body, e)
		}
	}
	ex = exchange(t, openai.ImagesGenerations, gemini.Imagen, apix.ExchangeOptions{})
	if _, err := ex.ConvertResponse(200, apix.Payload{Body: []byte(`{}`)}); class(err) != apix.ClassUnsupported {
		t.Fatalf("filtered Imagen: %v", err)
	}
	if _, err := ex.ConvertResponse(429, apix.Payload{Body: []byte(`{"error":{"code":429,"message":"slow down","status":"RESOURCE_EXHAUSTED"}}`)}); class(err) != apix.ClassRateLimit || !strings.Contains(err.Error(), "slow down") {
		t.Fatalf("status: %v", err)
	}
	if _, err := ex.ConvertResponse(200, apix.Payload{Body: []byte(`[]`)}); class(err) != apix.ClassTranslation {
		t.Fatalf("malformed: %v", err)
	}
}

func TestNative(t *testing.T) {
	ex := exchange(t, openai.ImagesEdits, openai.ImagesEdits, apix.ExchangeOptions{})
	req := form(t, map[string]string{"model": "alias", "prompt": "add a hat", "stream": "true"}, [2]string{"image", "image/png"})
	out, err := ex.ConvertRequest(req, "gpt-image-1")
	if err != nil {
		t.Fatal(err)
	}
	fields, files := readForm(t, out)
	if fields["model"] != "gpt-image-1" || fields["prompt"] != "add a hat" || fields["stream"] != "true" || !bytes.Equal(files["image"][0], png) {
		t.Fatalf("%v %v", fields, files)
	}
	if out.Metadata.Model != "" || out.Metadata.Stream == nil || !*out.Metadata.Stream {
		t.Fatalf("metadata %+v", out.Metadata)
	}
	unchanged, err := ex.ConvertRequest(req, "alias")
	if err != nil || !bytes.Equal(unchanged.Body, req.Body) || unchanged.ContentType != req.ContentType {
		t.Fatalf("unchanged request rewritten: %v", err)
	}
	body := []byte(`{"created":1,"data":[{"b64_json":"a"}],"extra":true}`)
	if resp, err := ex.ConvertResponse(200, apix.Payload{Body: body}); err != nil || !bytes.Equal(resp.Body, body) {
		t.Fatalf("%s %v", resp.Body, err)
	}

	gen := exchange(t, openai.ImagesGenerations, openai.ImagesGenerations, apix.ExchangeOptions{})
	out, err = gen.ConvertRequest(apix.Payload{Body: []byte(`{"model":"alias","prompt":"p","quality":"high"}`)}, "gpt-image-1")
	if err != nil {
		t.Fatal(err)
	}
	jsonEqual(t, out.Body, `{"model":"gpt-image-1","prompt":"p","quality":"high"}`)
	if len(gen.Diagnostics()) != 0 {
		t.Fatalf("native diagnostics %+v", gen.Diagnostics())
	}

	g := exchange(t, gemini.GenerateContentImage, gemini.GenerateContentImage, apix.ExchangeOptions{})
	stream := true
	greq := apix.Payload{Body: []byte(`{"contents":[{"parts":[{"text":"p"}]}]}`), Metadata: apix.RequestMetadata{Model: "alias", Stream: &stream}}
	out, err = g.ConvertRequest(greq, "gemini-2.5-flash-image")
	if err != nil || !bytes.Equal(out.Body, greq.Body) || out.Metadata.Model != "gemini-2.5-flash-image" || !*out.Metadata.Stream {
		t.Fatalf("%+v %v", out, err)
	}
}

func feedAll(sc apix.StreamConverter, wire string, bytewise bool) ([]byte, bool, error) {
	var out []byte
	chunks := []string{wire}
	if bytewise {
		chunks = strings.Split(wire, "")
	}
	for _, c := range chunks {
		events, done, err := sc.Feed([]byte(c))
		out = append(out, bytes.Join(events, nil)...)
		if err != nil || done {
			return out, done, err
		}
	}
	events, err := sc.Finish()
	return append(out, bytes.Join(events, nil)...), false, err
}

func TestNativeStreams(t *testing.T) {
	partial := `{"type":"image_generation.partial_image","b64_json":"p","partial_image_index":0}`
	completed := `{"type":"image_generation.completed","b64_json":"a","usage":{"input_tokens":1,"output_tokens":2,"total_tokens":3}}`
	wire := "event: image_generation.partial_image\ndata: " + partial + "\n\n: keepalive\n\nevent: image_generation.completed\ndata: " + completed + "\n\n"
	for _, bytewise := range []bool{false, true} {
		ex := exchange(t, openai.ImagesGenerations, openai.ImagesGenerations, apix.ExchangeOptions{})
		if _, err := ex.ConvertRequest(apix.Payload{Body: []byte(`{"prompt":"p","stream":true,"partial_images":1}`)}, ""); err != nil {
			t.Fatal(err)
		}
		sc, err := ex.NewStreamConverter()
		if err != nil {
			t.Fatal(err)
		}
		out, done, err := feedAll(sc, wire, bytewise)
		if err != nil || !done || !strings.Contains(string(out), partial) || !strings.HasSuffix(string(out), completed+"\n\n") {
			t.Fatalf("bytewise=%v: %q %v %v", bytewise, out, done, err)
		}
	}

	// Two expected images: the stream is not done after the first.
	ex := exchange(t, openai.ImagesGenerations, openai.ImagesGenerations, apix.ExchangeOptions{})
	ex.ConvertRequest(apix.Payload{Body: []byte(`{"prompt":"p","stream":true,"n":2}`)}, "")
	sc, _ := ex.NewStreamConverter()
	if _, done, err := sc.Feed([]byte("data: " + completed + "\n\n")); done || err != nil {
		t.Fatalf("done after one of two: %v %v", done, err)
	}
	if _, err := sc.Finish(); err != nil {
		t.Fatalf("EOF after one completed image: %v", err)
	}

	for name, wire := range map[string]string{
		"no completed image": "data: " + partial + "\n\n",
		"error event":        "event: error\ndata: {\"type\":\"error\",\"error\":{\"message\":\"moderation_blocked\"}}\n\n",
		"malformed":          "data: {\n\n",
		"completed no image": "data: {\"type\":\"image_generation.completed\"}\n\n",
		// Found by FuzzImageStreamChunkBoundaries: encoding/json matches keys
		// case-insensitively, but clients read b64_json exactly.
		"case variant": "event: image_generation.completed\ndata: {\"B64_json\":\"0\"}\n\n",
	} {
		ex := exchange(t, openai.ImagesGenerations, openai.ImagesGenerations, apix.ExchangeOptions{})
		ex.ConvertRequest(apix.Payload{Body: []byte(`{"prompt":"p","stream":true}`)}, "")
		sc, _ := ex.NewStreamConverter()
		if _, _, err := feedAll(sc, wire, false); err == nil {
			t.Errorf("%s: no error", name)
		}
		if _, _, err := sc.Feed([]byte("data: " + completed + "\n\n")); err == nil {
			t.Errorf("%s: error not sticky", name)
		}
	}

	g := exchange(t, gemini.GenerateContentImage, gemini.GenerateContentImage, apix.ExchangeOptions{})
	stream := true
	if _, err := g.ConvertRequest(apix.Payload{Body: []byte(`{"contents":[{"parts":[{"text":"p"}]}]}`), Metadata: apix.RequestMetadata{Stream: &stream}}, "m"); err != nil {
		t.Fatal(err)
	}
	gwire := "data: {\"candidates\":[{\"content\":{\"parts\":[{\"text\":\"ok\"}]}}]}\n\ndata: {\"candidates\":[{\"content\":{\"parts\":[{\"inlineData\":{\"mimeType\":\"image/png\",\"data\":\"a\"}}]},\"finishReason\":\"STOP\"}]}"
	for _, bytewise := range []bool{false, true} {
		sc, err := g.NewStreamConverter()
		if err != nil {
			t.Fatal(err)
		}
		if out, _, err := feedAll(sc, gwire, bytewise); err != nil || !strings.Contains(string(out), `"finishReason":"STOP"`) {
			t.Fatalf("gemini bytewise=%v: %q %v", bytewise, out, err)
		}
	}
	sc, _ = g.NewStreamConverter()
	if _, _, err := feedAll(sc, "data: {\"candidates\":[{\"content\":{\"parts\":[{\"text\":\"ok\"}]}}]}\n\n", false); err == nil {
		t.Fatal("gemini stream without finish reason succeeded")
	}
	// Found by FuzzImageStreamChunkBoundaries: trailing bytes after a JSON
	// value are not a valid chunk.
	sc, _ = g.NewStreamConverter()
	if _, _, err := feedAll(sc, "data: {\"candidates\":[{\"finishReason\":\"STOP\"}]}0\n\n", false); class(err) != apix.ClassTranslation {
		t.Fatalf("trailing bytes accepted: %v", err)
	}
	// Found by FuzzImageStreamChunkBoundaries: a case variant of
	// finishReason is not a finish reason.
	sc, _ = g.NewStreamConverter()
	if _, _, err := feedAll(sc, "data: {\"candidates\":[{\"finishReAson\":\"STOP\"}]}\n\n", false); class(err) != apix.ClassTranslation {
		t.Fatalf("case variant accepted: %v", err)
	}
}

func TestResponseModalities(t *testing.T) {
	reg := registry(t)
	both := gemini.GenerateContentImageOptions{ResponseModalities: []gemini.Modality{gemini.ModalityText, gemini.ModalityImage}}
	for _, ext := range []any{both, &both, gemini.GenerateContentImageOptions{}, (*gemini.GenerateContentImageOptions)(nil)} {
		if err := reg.ValidateOptions(gemini.GenerateContentImage, apix.ExchangeOptions{Ext: ext}); err != nil {
			t.Errorf("%#v: %v", ext, err)
		}
	}
	for _, ext := range []any{
		gemini.GenerateContentImageOptions{ResponseModalities: []gemini.Modality{gemini.ModalityText}},
		gemini.GenerateContentImageOptions{ResponseModalities: []gemini.Modality{gemini.ModalityImage, gemini.ModalityImage}},
		gemini.GenerateContentImageOptions{ResponseModalities: []gemini.Modality{"AUDIO", gemini.ModalityImage}},
		gemini.GenerateContentOptions{},
	} {
		if err := reg.ValidateOptions(gemini.GenerateContentImage, apix.ExchangeOptions{Ext: ext}); class(err) != apix.ClassUnsupported {
			t.Errorf("%#v accepted: %v", ext, err)
		}
	}
	if err := reg.ValidateOptions(gemini.Imagen, apix.ExchangeOptions{Ext: both}); class(err) != apix.ClassUnsupported {
		t.Fatalf("Imagen accepted GenerateContentImageOptions: %v", err)
	}

	for client, body := range map[apix.Protocol]string{
		openai.ImagesGenerations: `{"prompt":"a cat"}`,
		gemini.Imagen:            `{"instances":[{"prompt":"a cat"}]}`,
	} {
		ex := exchange(t, client, gemini.GenerateContentImage, apix.ExchangeOptions{Ext: &both})
		out, err := ex.ConvertRequest(apix.Payload{Body: []byte(body)}, "m")
		if err != nil {
			t.Fatal(err)
		}
		jsonEqual(t, out.Body, `{"contents":[{"role":"user","parts":[{"text":"a cat"}]}],"generationConfig":{"responseModalities":["TEXT","IMAGE"]}}`)
	}
	// The caller's options cannot change an exchange after it is created.
	opts := gemini.GenerateContentImageOptions{ResponseModalities: []gemini.Modality{gemini.ModalityImage, gemini.ModalityText}}
	ex := exchange(t, openai.ImagesGenerations, gemini.GenerateContentImage, apix.ExchangeOptions{Ext: opts})
	opts.ResponseModalities[0] = gemini.ModalityText
	if out, _ := ex.ConvertRequest(apix.Payload{Body: []byte(`{"prompt":"a cat"}`)}, "m"); !strings.Contains(string(out.Body), `"responseModalities":["IMAGE","TEXT"]`) {
		t.Fatalf("options aliased: %s", out.Body)
	}

	// Native requests take the configured modalities and keep their other
	// fields, whichever case the client used.
	stream := true
	for req, want := range map[string]string{
		`{"contents":[{"parts":[{"text":"p"}]}],"generationConfig":{"responseModalities":["IMAGE"],"imageConfig":{"aspectRatio":"16:9"}}}`: `{"contents":[{"parts":[{"text":"p"}]}],"generationConfig":{"responseModalities":["TEXT","IMAGE"],"imageConfig":{"aspectRatio":"16:9"}}}`,
		`{"contents":[{"parts":[{"text":"p"}]}],"generation_config":{"response_modalities":["IMAGE"],"temperature":1}}`:                    `{"contents":[{"parts":[{"text":"p"}]}],"generation_config":{"responseModalities":["TEXT","IMAGE"],"temperature":1}}`,
		`{"contents":[{"parts":[{"text":"p"}]}],"generationConfig":null}`:                                                                  `{"contents":[{"parts":[{"text":"p"}]}],"generationConfig":{"responseModalities":["TEXT","IMAGE"]}}`,
		`{"contents":[{"parts":[{"text":"p"}]}]}`:                                                                                          `{"contents":[{"parts":[{"text":"p"}]}],"generationConfig":{"responseModalities":["TEXT","IMAGE"]}}`,
	} {
		native := exchange(t, gemini.GenerateContentImage, gemini.GenerateContentImage, apix.ExchangeOptions{Ext: both, LossPolicy: apix.LossPolicyStrict})
		out, err := native.ConvertRequest(apix.Payload{Body: []byte(req), Metadata: apix.RequestMetadata{Stream: &stream}}, "m")
		if err != nil {
			t.Fatal(err)
		}
		jsonEqual(t, out.Body, want)
		if out.Metadata.Model != "m" || !*out.Metadata.Stream || len(native.Diagnostics()) != 0 {
			t.Fatalf("%+v %+v", out.Metadata, native.Diagnostics())
		}
	}
	// Without configured modalities native requests pass unchanged.
	plain := exchange(t, gemini.GenerateContentImage, gemini.GenerateContentImage, apix.ExchangeOptions{})
	req := `{"contents":[{"parts":[{"text":"p"}]}],"generation_config":{"response_modalities":["IMAGE"]}}`
	if out, err := plain.ConvertRequest(apix.Payload{Body: []byte(req)}, "m"); err != nil || string(out.Body) != req {
		t.Fatalf("%s %v", out.Body, err)
	}
	bad := exchange(t, gemini.GenerateContentImage, gemini.GenerateContentImage, apix.ExchangeOptions{Ext: both})
	if _, err := bad.ConvertRequest(apix.Payload{Body: []byte(`{"contents":[{"parts":[{"text":"p"}]}],"generationConfig":[]}`)}, "m"); class(err) != apix.ClassTranslation {
		t.Fatalf("array generationConfig: %v", err)
	}
}
