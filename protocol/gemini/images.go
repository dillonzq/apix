package gemini

import (
	"github.com/dillonzq/apix"
	"github.com/dillonzq/apix/internal/image"
)

const (
	// GenerateContentImage is "gemini.generate-content.image": GenerateContent
	// sent to an image model, such as gemini-2.5-flash-image, for image
	// generation and editing. It shares GenerateContent's wire format and URL
	// (the model and mode live in the URL; pass them as apix.Payload.Metadata)
	// but belongs to apix.KindImage, so it converts directly to and from
	// openai.ImagesGenerations, openai.ImagesEdits and Imagen. Converted
	// requests ask for responseModalities ["IMAGE"], and native ones keep
	// the client's, unless GenerateContentImageOptions says otherwise.
	GenerateContentImage = image.GeminiImage
	// Imagen is "gemini.imagen": POST /v1beta/models/{model}:predict. It
	// belongs to apix.KindImage, generates without input images, does not
	// stream, and converts directly to and from openai.ImagesGenerations and
	// GenerateContentImage.
	Imagen = image.Imagen
)

// Modality is a GenerateContent response modality.
type Modality string

const (
	ModalityText  Modality = "TEXT"
	ModalityImage Modality = "IMAGE"
)

// GenerateContentImageOptions configures exchanges whose upstream speaks
// GenerateContentImage. Pass it, or a pointer to it, as
// apix.ExchangeOptions.Ext, usually through route.EndpointOptions.
type GenerateContentImageOptions struct {
	// ResponseModalities sets generationConfig.responseModalities on every
	// request this upstream receives, for models that need text alongside
	// images: []Modality{ModalityText, ModalityImage}. It replaces ["IMAGE"]
	// on converted requests and the client's value on native ones. It must
	// include ModalityImage and list each modality once. Empty sends
	// ["IMAGE"] on converted requests and leaves native ones unchanged. Text
	// the model returns is reported as a response loss when the output
	// protocol cannot carry it.
	ResponseModalities []Modality
}

// GenerateContentImageCodec returns the GenerateContent image codec. It
// accepts GenerateContentImageOptions.
func GenerateContentImageCodec() apix.Codec {
	return image.New(GenerateContentImage, func(ext any) (image.Options, bool) {
		var o GenerateContentImageOptions
		switch v := ext.(type) {
		case GenerateContentImageOptions:
			o = v
		case *GenerateContentImageOptions:
			if v != nil {
				o = *v
			}
		default:
			return image.Options{}, false
		}
		seen := map[Modality]bool{}
		modalities := make([]string, 0, len(o.ResponseModalities))
		for _, m := range o.ResponseModalities {
			if (m != ModalityText && m != ModalityImage) || seen[m] {
				return image.Options{}, false
			}
			seen[m] = true
			modalities = append(modalities, string(m))
		}
		if len(modalities) > 0 && !seen[ModalityImage] {
			return image.Options{}, false
		}
		return image.Options{ResponseModalities: modalities}, true
	})
}

// ImagenCodec returns the Imagen codec. It defines no exchange options.
func ImagenCodec() apix.Codec { return image.New(Imagen, nil) }
