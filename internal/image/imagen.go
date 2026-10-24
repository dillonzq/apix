package image

import (
	"encoding/json"
	"strconv"

	"github.com/dillonzq/apix"
)

// imagenRequest is an Imagen :predict request. Only the first instance is
// read; the Gemini API serves one instance per request.
type imagenRequest struct {
	Prompt      string
	Instances   int
	SampleCount int
	AspectRatio string
	ImageSize   string
	OutputMIME  string
	Compression *int
	// instanceKeys, paramKeys and outputKeys list the fields present in the
	// first instance, parameters and parameters.outputOptions, sorted.
	instanceKeys []string
	paramKeys    []string
	outputKeys   []string
	keys         []string
}

func (r *imagenRequest) info() apix.RequestInfo {
	return apix.RequestInfo{Kind: apix.KindImage, Protocol: Imagen}
}

func decodeImagen(body []byte) (*imagenRequest, *apix.Error) {
	var raw json.RawMessage
	if json.Unmarshal(body, &raw) != nil {
		return nil, apix.Translation("malformed Imagen request JSON")
	}
	root, keys, err := fields(raw, "request")
	if err != nil {
		return nil, err
	}
	r := &imagenRequest{keys: keys}
	var instances []json.RawMessage
	if err := field(root, "instances", "instances", &instances); err != nil {
		return nil, err
	}
	if len(instances) == 0 {
		return nil, apix.Translation("Imagen request requires instances")
	}
	r.Instances = len(instances)
	instance, instanceKeys, err := fields(instances[0], "instances[0]")
	if err != nil {
		return nil, err
	}
	r.instanceKeys = instanceKeys
	if err := field(instance, "prompt", "instances[0].prompt", &r.Prompt); err != nil {
		return nil, err
	}
	if r.Prompt == "" {
		return nil, apix.Translation("Imagen request requires instances[0].prompt")
	}
	params, paramKeys, err := fields(root["parameters"], "parameters")
	if err != nil {
		return nil, err
	}
	r.paramKeys = paramKeys
	for key, dst := range map[string]any{"sampleCount": &r.SampleCount, "aspectRatio": &r.AspectRatio, "imageSize": &r.ImageSize} {
		if err := field(params, key, "parameters."+key, dst); err != nil {
			return nil, err
		}
	}
	if r.SampleCount < 0 {
		return nil, apix.Translation("Imagen parameters.sampleCount must not be negative")
	}
	output, outputKeys, err := fields(params["outputOptions"], "parameters.outputOptions")
	if err != nil {
		return nil, err
	}
	r.outputKeys = outputKeys
	if err := field(output, "mimeType", "parameters.outputOptions.mimeType", &r.OutputMIME); err != nil {
		return nil, err
	}
	if err := field(output, "compressionQuality", "parameters.outputOptions.compressionQuality", &r.Compression); err != nil {
		return nil, err
	}
	return r, nil
}

// imagenBody is the request Imagen receives.
type imagenBody struct {
	Instances  []imagenInstance `json:"instances"`
	Parameters imagenParams     `json:"parameters"`
}

type imagenInstance struct {
	Prompt string `json:"prompt"`
}

type imagenParams struct {
	SampleCount int    `json:"sampleCount,omitempty"`
	AspectRatio string `json:"aspectRatio,omitempty"`
	ImageSize   string `json:"imageSize,omitempty"`
}

// imagenResponse is the :predict response.
type imagenResponse struct {
	Predictions []imagenPrediction `json:"predictions"`
}

type imagenPrediction struct {
	BytesBase64Encoded string `json:"bytesBase64Encoded,omitempty"`
	MIMEType           string `json:"mimeType,omitempty"`
	RAIFilteredReason  string `json:"raiFilteredReason,omitempty"`
}

func decodeImagenResponse(body []byte) (imagenResponse, *apix.Error) {
	var root map[string]json.RawMessage
	if json.Unmarshal(body, &root) != nil || root == nil {
		return imagenResponse{}, apix.Translation("Imagen response must be a JSON object")
	}
	var r imagenResponse
	if err := json.Unmarshal(body, &r); err != nil {
		return imagenResponse{}, apix.Translation("malformed Imagen response: " + err.Error())
	}
	return r, nil
}

// images returns the predictions that carry an image.
func (r imagenResponse) images() []imagenPrediction {
	var out []imagenPrediction
	for _, p := range r.Predictions {
		if p.BytesBase64Encoded != "" {
			out = append(out, p)
		}
	}
	return out
}

// noImage explains a response whose predictions were all filtered. Filtering
// describes the request and is not retryable.
func (r imagenResponse) noImage() *apix.Error {
	for _, p := range r.Predictions {
		if p.RAIFilteredReason != "" {
			return apix.Unsupported("Imagen generated no image: filtered by responsible AI policy")
		}
	}
	return apix.Unsupported("Imagen generated no image: " + strconv.Itoa(len(r.Predictions)) + " predictions without image data")
}
