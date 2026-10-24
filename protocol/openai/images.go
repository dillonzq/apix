package openai

import (
	"github.com/dillonzq/apix"
	"github.com/dillonzq/apix/internal/image"
)

const (
	// ImagesGenerations is "openai.images.generations": POST
	// /v1/images/generations with a JSON body. It belongs to apix.KindImage
	// and converts directly to and from gemini.GenerateContentImage and
	// gemini.Imagen.
	ImagesGenerations = image.OpenAIGenerations
	// ImagesEdits is "openai.images.edits": POST /v1/images/edits with a
	// multipart form, or a JSON body whose images are data URLs or file IDs.
	// It belongs to apix.KindImage and converts directly to and from
	// gemini.GenerateContentImage. Converted requests are multipart forms.
	ImagesEdits = image.OpenAIEdits
)

// ImagesGenerationsCodec returns the image generations codec. It defines no
// exchange options.
func ImagesGenerationsCodec() apix.Codec { return image.New(ImagesGenerations, nil) }

// ImagesEditsCodec returns the image edits codec. It defines no exchange
// options.
func ImagesEditsCodec() apix.Codec { return image.New(ImagesEdits, nil) }
