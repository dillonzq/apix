package image

import (
	"encoding/base64"
	"encoding/json"
	"mime"
	"slices"
	"strconv"
	"strings"

	"github.com/dillonzq/apix"
)

// clientRequest is a decoded request of one protocol family.
type clientRequest interface {
	info() apix.RequestInfo
}

func decodeRequest(id apix.Protocol, req apix.Payload) (clientRequest, *apix.Error) {
	switch protocols[id].family {
	case familyGemini:
		return decodeGemini(req.Body)
	case familyImagen:
		return decodeImagen(req.Body)
	}
	return decodeOpenAI(id, req)
}

type exchange struct {
	client, upstream, output apix.Protocol
	policy                   apix.LossPolicy
	options                  Options
	diagnostics              []apix.Diagnostic

	// Set by ConvertRequest for the response.
	model string // upstream model, reported as Gemini modelVersion
	// upstreamStream reports that the upstream was asked to stream; it only
	// happens when the upstream speaks the output protocol.
	upstreamStream bool
	// emulate renders the converted response as the output's event stream.
	emulate bool
	// candidates is the candidateCount a Gemini client asked for.
	candidates int
	// expected is how many completed images a native OpenAI stream carries.
	expected int
}

func (e *exchange) ConvertRequest(req apix.Payload, upstreamModel string) (apix.Payload, error) {
	out, err := e.convertRequest(req, upstreamModel)
	if err != nil {
		return apix.Payload{}, err
	}
	return out, nil
}

func (e *exchange) convertRequest(req apix.Payload, upstreamModel string) (apix.Payload, *apix.Error) {
	r, err := decodeRequest(e.client, req)
	if err != nil {
		return apix.Payload{}, err
	}
	info := req.Metadata.Apply(r.info())
	model := upstreamModel
	if model == "" {
		model = info.Model
	}
	up, out := protocols[e.upstream], protocols[e.output]
	if up.urlModel() && model == "" {
		return apix.Payload{}, apix.Translation(string(e.upstream) + " requires an upstream model in request metadata or ConvertRequest")
	}
	e.model = model
	e.upstreamStream = info.Stream && e.output == e.upstream && up.stream != apix.FramingNone
	e.emulate = info.Stream && !e.upstreamStream && out.stream != apix.FramingNone
	e.expected = 1
	var payload apix.Payload
	switch src := r.(type) {
	case *openaiRequest:
		if src.N > 1 {
			e.expected = src.N
		}
		switch {
		case e.client == e.upstream:
			payload, err = e.nativeOpenAI(req, src, model)
		case up.family == familyGemini:
			payload, err = e.openaiToGemini(src)
		default:
			payload = e.openaiToImagen(src)
		}
	case *geminiRequest:
		e.candidates = src.CandidateCount
		switch {
		case e.client == e.upstream:
			payload, err = e.nativeGemini(req)
		case up.family == familyOpenAI:
			payload, err = e.geminiToOpenAI(src, model)
		default:
			payload, err = e.geminiToImagen(src)
		}
	case *imagenRequest:
		switch {
		case e.client == e.upstream:
			payload = apix.Payload{Body: req.Body, ContentType: "application/json"}
		case up.family == familyOpenAI:
			payload = e.imagenToOpenAI(src, model)
		default:
			payload = e.imagenToGemini(src)
		}
	}
	if err != nil {
		return apix.Payload{}, err
	}
	if err := e.enforcePolicy(); err != nil {
		return apix.Payload{}, err
	}
	// The streaming mode always travels in metadata: conversion may turn a
	// streamed client request into an ordinary upstream call.
	stream := e.upstreamStream
	payload.Metadata = apix.RequestMetadata{Stream: &stream}
	if up.urlModel() {
		payload.Metadata.Model = model
	}
	return payload, nil
}

// requestLoss records a request field conversion omits or approximates.
func (e *exchange) requestLoss(path string, semantic bool) {
	e.record(apix.Diagnostic{Code: "unrepresentable_field", Path: path, Message: "conversion cannot preserve " + path,
		Semantic: semantic, From: e.client, To: e.upstream})
}

// responseLoss records a response field conversion omits.
func (e *exchange) responseLoss(path string) {
	e.record(apix.Diagnostic{Code: "unrepresentable_field", Path: path, Message: "conversion cannot preserve " + path,
		From: e.upstream, To: e.output})
}

func (e *exchange) record(d apix.Diagnostic) {
	d.Path, d.Message = apix.Redact(d.Path), apix.Redact(d.Message)
	if !slices.Contains(e.diagnostics, d) {
		e.diagnostics = append(e.diagnostics, d)
	}
}

// enforcePolicy rejects the request losses the loss policy forbids.
func (e *exchange) enforcePolicy() *apix.Error {
	for _, d := range e.diagnostics {
		if d.From != e.client || d.To != e.upstream {
			continue
		}
		if e.policy == apix.LossPolicyStrict || (e.policy == apix.LossPolicySafe && d.Semantic) {
			return apix.Unsupported(d.Message)
		}
	}
	return nil
}

func (e *exchange) Diagnostics() []apix.Diagnostic {
	if len(e.diagnostics) == 0 {
		return nil
	}
	return slices.Clone(e.diagnostics)
}

// modalities returns the responseModalities of converted GenerateContent
// requests.
func (e *exchange) modalities() []string {
	if len(e.options.ResponseModalities) > 0 {
		return e.options.ResponseModalities
	}
	return []string{"IMAGE"}
}

func encodeJSON(v any) apix.Payload {
	body, _ := json.Marshal(v)
	return apix.Payload{Body: body, ContentType: "application/json"}
}

// nativeOpenAI rewrites the model and, when the upstream must not stream,
// the stream field.
func (e *exchange) nativeOpenAI(req apix.Payload, r *openaiRequest, model string) (apix.Payload, *apix.Error) {
	if model == r.Model {
		model = ""
	}
	var stream *bool
	if e.upstreamStream != r.Stream {
		stream = &e.upstreamStream
	}
	if model == "" && stream == nil {
		return apix.Payload{Body: req.Body, ContentType: contentTypeOr(req.ContentType)}, nil
	}
	return rewriteOpenAI(req, model, stream)
}

// nativeGemini replaces the client's responseModalities with the
// configured ones, keeping every other field. Without configured modalities
// the body passes unchanged.
func (e *exchange) nativeGemini(req apix.Payload) (apix.Payload, *apix.Error) {
	if len(e.options.ResponseModalities) == 0 {
		return apix.Payload{Body: req.Body, ContentType: "application/json"}, nil
	}
	var root map[string]json.RawMessage
	if json.Unmarshal(req.Body, &root) != nil || root == nil {
		return apix.Payload{}, apix.Translation("request must be a JSON object")
	}
	key := "generationConfig"
	if root[key] == nil && root["generation_config"] != nil {
		key = "generation_config"
	}
	config := map[string]json.RawMessage{}
	if raw := root[key]; raw != nil && string(raw) != "null" {
		if json.Unmarshal(raw, &config) != nil || config == nil {
			return apix.Payload{}, apix.Translation("generationConfig must be a JSON object")
		}
	}
	delete(config, "response_modalities")
	config["responseModalities"], _ = json.Marshal(e.options.ResponseModalities)
	root[key], _ = json.Marshal(config)
	body, _ := json.Marshal(root)
	return apix.Payload{Body: body, ContentType: "application/json"}, nil
}

func contentTypeOr(ct string) string {
	if ct == "" {
		return "application/json"
	}
	return ct
}

// openaiLosses reports the OpenAI request fields a Google upstream cannot
// carry. mapped lists the fields the converter already translated.
func (e *exchange) openaiLosses(r *openaiRequest, mapped ...string) {
	for _, k := range r.keys {
		switch {
		case slices.Contains(mapped, k):
		case k == "mask", k == "response_format" && r.ResponseFormat == "url", k == "background" && r.Background == "transparent":
			e.requestLoss(k, true)
		case k == "quality" && r.Quality != "auto", k == "output_format" && r.OutputFormat != "png", k == "partial_images" && r.PartialImages > 0:
			e.requestLoss(k, false)
		case k == "output_compression", k == "moderation", k == "style", k == "user", k == "safety_identifier", k == "input_fidelity":
			e.requestLoss(k, false)
		case !openaiFields[k]:
			e.requestLoss(k, true)
		}
	}
}

// inlineImages renders input images as Gemini inlineData parts. Stored
// OpenAI files and remote URLs cannot be read without I/O.
func inlineImages(images []media) ([]map[string]any, *apix.Error) {
	parts := make([]map[string]any, 0, len(images))
	for i, m := range images {
		path := "images[" + strconv.Itoa(i) + "]"
		switch {
		case m.FileID != "":
			return nil, apix.Unsupported(path + " refers to an OpenAI file, which another upstream cannot read")
		case m.URI != "":
			return nil, apix.Unsupported(path + " is a remote URL; conversion needs inline image data")
		}
		parts = append(parts, map[string]any{"inlineData": map[string]string{"mimeType": m.MIME, "data": base64.StdEncoding.EncodeToString(m.Data)}})
	}
	return parts, nil
}

func (e *exchange) openaiToGemini(r *openaiRequest) (apix.Payload, *apix.Error) {
	images, err := inlineImages(r.Images)
	if err != nil {
		return apix.Payload{}, err
	}
	parts := append([]map[string]any{{"text": r.Prompt}}, images...)
	config := map[string]any{"responseModalities": e.modalities()}
	if r.N > 1 {
		config["candidateCount"] = r.N
	}
	aspect, imageSize, exact, known := googleSize(r.Size, geminiAspects, "4K")
	if !exact {
		e.requestLoss("size", !known)
	}
	imageConfig := map[string]string{}
	if aspect != "" {
		imageConfig["aspectRatio"] = aspect
	}
	if imageSize != "" {
		imageConfig["imageSize"] = imageSize
	}
	if len(imageConfig) > 0 {
		config["imageConfig"] = imageConfig
	}
	e.openaiLosses(r, "model", "prompt", "n", "size", "stream", "image", "image[]", "images")
	return encodeJSON(map[string]any{"contents": []map[string]any{{"role": "user", "parts": parts}}, "generationConfig": config}), nil
}

func (e *exchange) openaiToImagen(r *openaiRequest) apix.Payload {
	body := imagenBody{Instances: []imagenInstance{{Prompt: r.Prompt}}, Parameters: imagenParams{SampleCount: r.N}}
	aspect, imageSize, exact, known := googleSize(r.Size, imagenAspects, "2K")
	if !exact {
		e.requestLoss("size", !known)
	}
	body.Parameters.AspectRatio, body.Parameters.ImageSize = aspect, imageSize
	e.openaiLosses(r, "model", "prompt", "n", "size", "stream")
	return encodeJSON(body)
}

// geminiPrompt joins the text of user turns and collects their images.
// Other turns and parts are reported; media other than inline images fails.
func (e *exchange) geminiPrompt(r *geminiRequest) (string, []media, *apix.Error) {
	var texts []string
	var images []media
	for i, c := range r.Contents {
		path := "contents[" + strconv.Itoa(i) + "]"
		if c.Role != "" && c.Role != "user" {
			e.requestLoss(path, true)
			continue
		}
		for j, p := range c.Parts {
			ppath := path + ".parts[" + strconv.Itoa(j) + "]"
			switch {
			case p.Media != nil:
				// The media type becomes a multipart header; only a parsed
				// type without parameters may reach it.
				mt, _, err := mime.ParseMediaType(p.Media.MIME)
				switch {
				case err != nil:
					return "", nil, apix.Translation(ppath + " has an invalid mimeType")
				case !strings.HasPrefix(mt, "image/"):
					return "", nil, apix.Unsupported(ppath + " carries " + mt + " input, which " + string(e.upstream) + " cannot read")
				case p.Media.URI != "":
					return "", nil, apix.Unsupported(ppath + " refers to a file URI; conversion needs inline image data")
				}
				images = append(images, media{MIME: mt, Data: p.Media.Data})
			case p.Text != "":
				texts = append(texts, p.Text)
			case p.Other != "":
				e.requestLoss(ppath+"."+p.Other, true)
			}
		}
	}
	if len(texts) == 0 {
		return "", nil, apix.Translation("Gemini request has no text prompt")
	}
	return strings.Join(texts, "\n\n"), images, nil
}

// geminiLosses reports the Gemini request fields another upstream cannot
// carry, beyond contents and the mapped generationConfig fields.
func (e *exchange) geminiLosses(r *geminiRequest, imageSizeMapped bool) {
	for _, k := range r.keys {
		switch k {
		case "contents", "generationConfig":
		case "safetySettings", "labels":
			e.requestLoss(k, false)
		default:
			e.requestLoss(k, true)
		}
	}
	for _, k := range r.configKeys {
		switch {
		case k == "candidateCount", k == "imageConfig":
		case k == "responseModalities":
			if slices.ContainsFunc(r.Modalities, func(m string) bool { return !strings.EqualFold(m, "IMAGE") }) {
				e.requestLoss("generationConfig.responseModalities", false)
			}
		default:
			e.requestLoss("generationConfig."+k, false)
		}
	}
	for _, k := range r.imageConfigKeys {
		switch {
		case k == "aspectRatio":
		case k == "imageSize" && (imageSizeMapped || strings.EqualFold(r.ImageSize, "1K")):
		default:
			e.requestLoss("generationConfig.imageConfig."+k, false)
		}
	}
}

func (e *exchange) geminiToOpenAI(r *geminiRequest, model string) (apix.Payload, *apix.Error) {
	prompt, images, err := e.geminiPrompt(r)
	if err != nil {
		return apix.Payload{}, err
	}
	edit := protocols[e.upstream].edit
	switch {
	case edit && len(images) == 0:
		return apix.Payload{}, apix.Unsupported(string(e.upstream) + " requires an input image")
	case !edit && len(images) > 0:
		return apix.Payload{}, apix.Unsupported(string(e.upstream) + " cannot take input images; send edits to " + string(OpenAIEdits))
	}
	size, exact := openaiSize(r.AspectRatio)
	if !exact {
		e.requestLoss("generationConfig.imageConfig.aspectRatio", size == "")
	}
	e.geminiLosses(r, false)
	fields := []formField{{"prompt", prompt}}
	if model != "" {
		fields = append(fields, formField{"model", model})
	}
	if r.CandidateCount > 1 {
		fields = append(fields, formField{"n", strconv.Itoa(r.CandidateCount)})
	}
	if size != "" {
		fields = append(fields, formField{"size", size})
	}
	if e.upstreamStream {
		fields = append(fields, formField{"stream", "true"})
	}
	return e.encodeOpenAI(fields, images), nil
}

// encodeOpenAI renders a converted request: a multipart form for edits and
// JSON for generations. Numeric and boolean fields become JSON values.
func (e *exchange) encodeOpenAI(fields []formField, images []media) apix.Payload {
	if protocols[e.upstream].edit {
		return encodeEditsForm(fields, images)
	}
	body := map[string]any{}
	for _, f := range fields {
		switch f.name {
		case "n", "output_compression":
			body[f.name], _ = strconv.Atoi(f.value)
		case "stream":
			body[f.name] = f.value == "true"
		default:
			body[f.name] = f.value
		}
	}
	return encodeJSON(body)
}

func (e *exchange) geminiToImagen(r *geminiRequest) (apix.Payload, *apix.Error) {
	prompt, images, err := e.geminiPrompt(r)
	if err != nil {
		return apix.Payload{}, err
	}
	if len(images) > 0 {
		return apix.Payload{}, apix.Unsupported(string(Imagen) + " cannot take input images")
	}
	body := imagenBody{Instances: []imagenInstance{{Prompt: prompt}}}
	if r.CandidateCount > 1 {
		body.Parameters.SampleCount = r.CandidateCount
	}
	if r.AspectRatio != "" {
		rt, ok := ratio(r.AspectRatio, ":")
		if !ok {
			e.requestLoss("generationConfig.imageConfig.aspectRatio", true)
		} else {
			aspect, exact := nearestAspect(rt, imagenAspects)
			if !exact {
				e.requestLoss("generationConfig.imageConfig.aspectRatio", false)
			}
			body.Parameters.AspectRatio = aspect
		}
	}
	switch strings.ToUpper(r.ImageSize) {
	case "", "1K":
	case "2K":
		body.Parameters.ImageSize = "2K"
	default:
		body.Parameters.ImageSize = "2K"
		e.requestLoss("generationConfig.imageConfig.imageSize", false)
	}
	e.geminiLosses(r, true)
	return encodeJSON(body), nil
}

// imagenLosses reports the Imagen request fields another upstream cannot
// carry. mapped lists the parameters and output options already translated.
func (e *exchange) imagenLosses(r *imagenRequest, imageSizeMapped bool, mapped ...string) {
	if r.Instances > 1 {
		e.requestLoss("instances[1]", true)
	}
	for _, k := range r.instanceKeys {
		if k != "prompt" {
			e.requestLoss("instances[0]."+k, true)
		}
	}
	for _, k := range r.keys {
		if k != "instances" && k != "parameters" {
			e.requestLoss(k, true)
		}
	}
	for _, k := range r.paramKeys {
		switch {
		case k == "sampleCount", k == "aspectRatio", k == "outputOptions":
		case k == "imageSize" && (imageSizeMapped || strings.EqualFold(r.ImageSize, "1K")):
		case k == "negativePrompt":
			e.requestLoss("parameters."+k, true)
		default:
			e.requestLoss("parameters."+k, false)
		}
	}
	for _, k := range r.outputKeys {
		if !slices.Contains(mapped, k) {
			e.requestLoss("parameters.outputOptions."+k, false)
		}
	}
}

func (e *exchange) imagenToOpenAI(r *imagenRequest, model string) apix.Payload {
	size, exact := openaiSize(r.AspectRatio)
	if !exact {
		e.requestLoss("parameters.aspectRatio", size == "")
	}
	fields := []formField{{"prompt", r.Prompt}}
	if model != "" {
		fields = append(fields, formField{"model", model})
	}
	if r.SampleCount > 1 {
		fields = append(fields, formField{"n", strconv.Itoa(r.SampleCount)})
	}
	if size != "" {
		fields = append(fields, formField{"size", size})
	}
	var mapped []string
	if format := formatOf(r.OutputMIME); format != "" {
		fields = append(fields, formField{"output_format", format})
		mapped = append(mapped, "mimeType")
	}
	if r.Compression != nil {
		fields = append(fields, formField{"output_compression", strconv.Itoa(*r.Compression)})
		mapped = append(mapped, "compressionQuality")
	}
	e.imagenLosses(r, false, mapped...)
	return e.encodeOpenAI(fields, nil)
}

func (e *exchange) imagenToGemini(r *imagenRequest) apix.Payload {
	config := map[string]any{"responseModalities": e.modalities()}
	if r.SampleCount > 1 {
		config["candidateCount"] = r.SampleCount
	}
	imageConfig := map[string]string{}
	if r.AspectRatio != "" {
		imageConfig["aspectRatio"] = r.AspectRatio
	}
	if r.ImageSize != "" {
		imageConfig["imageSize"] = r.ImageSize
	}
	if len(imageConfig) > 0 {
		config["imageConfig"] = imageConfig
	}
	e.imagenLosses(r, true)
	parts := []map[string]any{{"text": r.Prompt}}
	return encodeJSON(map[string]any{"contents": []map[string]any{{"role": "user", "parts": parts}}, "generationConfig": config})
}
