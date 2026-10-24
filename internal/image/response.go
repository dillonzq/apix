package image

import (
	"strconv"
	"strings"
	"time"

	"github.com/dillonzq/apix"
	"github.com/dillonzq/apix/internal/vendorerr"
)

// now stamps converted OpenAI responses; tests replace it.
var now = time.Now

func (e *exchange) ConvertResponse(status int, resp apix.Payload) (apix.Payload, error) {
	out, err := e.convertResponse(status, resp)
	if err != nil {
		return apix.Payload{}, err
	}
	return out, nil
}

func (e *exchange) convertResponse(status int, resp apix.Payload) (apix.Payload, *apix.Error) {
	if status >= 400 {
		return apix.Payload{}, vendorerr.UpstreamStatus(status, resp.Body)
	}
	up, out := protocols[e.upstream], protocols[e.output]
	if e.upstream == e.output {
		if err := validateResponse(up.family, resp.Body); err != nil {
			return apix.Payload{}, err
		}
		return apix.Payload{Body: resp.Body, ContentType: "application/json"}, nil
	}
	switch up.family {
	case familyOpenAI:
		r, err := decodeOpenAIResponse(resp.Body)
		if err != nil {
			return apix.Payload{}, err
		}
		if out.family == familyGemini {
			g, err := e.openaiToGeminiResponse(r)
			return e.renderGemini(g), err
		}
		p, err := e.openaiToImagenResponse(r)
		return encodeJSON(p), err
	case familyGemini:
		r, err := decodeGeminiResponse(resp.Body)
		if err != nil {
			return apix.Payload{}, err
		}
		if out.family == familyOpenAI {
			o, err := e.geminiToOpenAIResponse(r)
			return e.renderOpenAI(o), err
		}
		p, err := e.geminiToImagenResponse(r)
		return encodeJSON(p), err
	default:
		r, err := decodeImagenResponse(resp.Body)
		if err != nil {
			return apix.Payload{}, err
		}
		if out.family == familyOpenAI {
			o, err := e.imagenToOpenAIResponse(r)
			return e.renderOpenAI(o), err
		}
		g, err := e.imagenToGeminiResponse(r)
		return e.renderGemini(g), err
	}
}

func validateResponse(f family, body []byte) *apix.Error {
	var err *apix.Error
	switch f {
	case familyOpenAI:
		_, err = decodeOpenAIResponse(body)
	case familyGemini:
		_, err = decodeGeminiResponse(body)
	default:
		_, err = decodeImagenResponse(body)
	}
	return err
}

func (e *exchange) renderOpenAI(r openaiResponse) apix.Payload {
	if e.emulate {
		return apix.Payload{Body: renderOpenAIStream(e.output, r), ContentType: "text/event-stream"}
	}
	return encodeJSON(r)
}

func (e *exchange) renderGemini(r geminiResponse) apix.Payload {
	if e.emulate {
		return apix.Payload{Body: renderGeminiStream(r), ContentType: "text/event-stream"}
	}
	return encodeJSON(r)
}

// openaiImages returns the base64 images of an OpenAI response. URL results
// cannot be fetched without I/O; another upstream may return inline data.
func (e *exchange) openaiImages(r openaiResponse) ([]string, *apix.Error) {
	images := make([]string, 0, len(r.Data))
	for i, d := range r.Data {
		path := "data[" + strconv.Itoa(i) + "]"
		if d.B64JSON == "" {
			return nil, apix.UpstreamFallback(path + " has no b64_json; URL results cannot be converted")
		}
		if d.RevisedPrompt != "" {
			e.responseLoss(path + ".revised_prompt")
		}
		images = append(images, d.B64JSON)
	}
	if len(images) == 0 {
		return nil, apix.UpstreamFallback("OpenAI returned no image")
	}
	return images, nil
}

// geminiCandidates places images in one candidate, or one candidate per image
// when the client asked for several candidates.
func (e *exchange) geminiCandidates(blobs []geminiBlob) []geminiCandidate {
	part := func(b geminiBlob) geminiOutPart {
		return geminiOutPart{InlineData: &geminiBlob{MIMEType: b.MIMEType, Data: b.Data}}
	}
	if e.candidates > 1 {
		out := make([]geminiCandidate, len(blobs))
		for i, b := range blobs {
			out[i] = geminiCandidate{Content: geminiOutContent{Role: "model", Parts: []geminiOutPart{part(b)}}, FinishReason: "STOP", Index: i}
		}
		return out
	}
	c := geminiCandidate{Content: geminiOutContent{Role: "model"}, FinishReason: "STOP"}
	for _, b := range blobs {
		c.Content.Parts = append(c.Content.Parts, part(b))
	}
	return []geminiCandidate{c}
}

func (e *exchange) openaiToGeminiResponse(r openaiResponse) (geminiResponse, *apix.Error) {
	images, err := e.openaiImages(r)
	if err != nil {
		return geminiResponse{}, err
	}
	mimeType := mimeOf(r.OutputFormat)
	blobs := make([]geminiBlob, len(images))
	for i, data := range images {
		blobs[i] = geminiBlob{MIMEType: mimeType, Data: data}
	}
	return geminiResponse{Candidates: e.geminiCandidates(blobs), UsageMetadata: geminiUsageOf(r.Usage), ModelVersion: e.model}, nil
}

func (e *exchange) openaiToImagenResponse(r openaiResponse) (imagenResponse, *apix.Error) {
	images, err := e.openaiImages(r)
	if err != nil {
		return imagenResponse{}, err
	}
	if r.Usage != nil {
		e.responseLoss("usage")
	}
	mimeType := mimeOf(r.OutputFormat)
	out := imagenResponse{Predictions: make([]imagenPrediction, len(images))}
	for i, data := range images {
		out.Predictions[i] = imagenPrediction{BytesBase64Encoded: data, MIMEType: mimeType}
	}
	return out, nil
}

// geminiImages returns the images of a Gemini response and reports dropped
// text; a response without images is an error.
func (e *exchange) geminiImages(r geminiResponse) ([]geminiBlob, *apix.Error) {
	images := r.images()
	if len(images) == 0 {
		return nil, r.noImage()
	}
	if r.hasText() {
		e.responseLoss("candidates.content.parts.text")
	}
	return images, nil
}

func (e *exchange) geminiToOpenAIResponse(r geminiResponse) (openaiResponse, *apix.Error) {
	images, err := e.geminiImages(r)
	if err != nil {
		return openaiResponse{}, err
	}
	out := openaiResponse{Created: now().Unix(), OutputFormat: formatOf(images[0].MIMEType), Usage: openaiUsageOf(r.UsageMetadata)}
	for _, b := range images {
		out.Data = append(out.Data, openaiImage{B64JSON: b.Data})
	}
	return out, nil
}

func (e *exchange) geminiToImagenResponse(r geminiResponse) (imagenResponse, *apix.Error) {
	images, err := e.geminiImages(r)
	if err != nil {
		return imagenResponse{}, err
	}
	if r.UsageMetadata != nil {
		e.responseLoss("usageMetadata")
	}
	out := imagenResponse{Predictions: make([]imagenPrediction, len(images))}
	for i, b := range images {
		out.Predictions[i] = imagenPrediction{BytesBase64Encoded: b.Data, MIMEType: b.MIMEType}
	}
	return out, nil
}

func (e *exchange) imagenToOpenAIResponse(r imagenResponse) (openaiResponse, *apix.Error) {
	images := r.images()
	if len(images) == 0 {
		return openaiResponse{}, r.noImage()
	}
	out := openaiResponse{Created: now().Unix(), OutputFormat: formatOf(images[0].MIMEType)}
	for _, p := range images {
		out.Data = append(out.Data, openaiImage{B64JSON: p.BytesBase64Encoded})
	}
	return out, nil
}

func (e *exchange) imagenToGeminiResponse(r imagenResponse) (geminiResponse, *apix.Error) {
	images := r.images()
	if len(images) == 0 {
		return geminiResponse{}, r.noImage()
	}
	blobs := make([]geminiBlob, len(images))
	for i, p := range images {
		mimeType := p.MIMEType
		if mimeType == "" {
			mimeType = "image/png"
		}
		blobs[i] = geminiBlob{MIMEType: mimeType, Data: p.BytesBase64Encoded}
	}
	return geminiResponse{Candidates: e.geminiCandidates(blobs), ModelVersion: e.model}, nil
}

// geminiUsageOf maps OpenAI image token usage onto usageMetadata.
func geminiUsageOf(u *openaiUsage) *geminiUsage {
	if u == nil {
		return nil
	}
	out := &geminiUsage{PromptTokenCount: u.InputTokens, CandidatesTokenCount: u.OutputTokens, TotalTokenCount: u.TotalTokens}
	if out.TotalTokenCount == 0 {
		out.TotalTokenCount = u.InputTokens + u.OutputTokens
	}
	out.PromptTokensDetails = modalities(u.InputTokensDetails)
	out.CandidatesTokensDetails = modalities(u.OutputTokensDetails)
	return out
}

func modalities(d *openaiTokenDetails) []geminiModalityUse {
	if d == nil {
		return nil
	}
	var out []geminiModalityUse
	if d.TextTokens > 0 {
		out = append(out, geminiModalityUse{Modality: "TEXT", TokenCount: d.TextTokens})
	}
	if d.ImageTokens > 0 {
		out = append(out, geminiModalityUse{Modality: "IMAGE", TokenCount: d.ImageTokens})
	}
	return out
}

// openaiUsageOf maps usageMetadata onto OpenAI image token usage. Thought
// tokens are billed as output.
func openaiUsageOf(u *geminiUsage) *openaiUsage {
	if u == nil {
		return nil
	}
	out := &openaiUsage{InputTokens: u.PromptTokenCount, OutputTokens: u.CandidatesTokenCount + u.ThoughtsTokenCount, TotalTokens: u.TotalTokenCount}
	if out.TotalTokens == 0 {
		out.TotalTokens = out.InputTokens + out.OutputTokens
	}
	out.InputTokensDetails = tokenDetails(u.PromptTokensDetails)
	out.OutputTokensDetails = tokenDetails(u.CandidatesTokensDetails)
	return out
}

func tokenDetails(in []geminiModalityUse) *openaiTokenDetails {
	if len(in) == 0 {
		return nil
	}
	d := &openaiTokenDetails{}
	for _, m := range in {
		switch strings.ToUpper(m.Modality) {
		case "TEXT":
			d.TextTokens += m.TokenCount
		case "IMAGE":
			d.ImageTokens += m.TokenCount
		}
	}
	return d
}
