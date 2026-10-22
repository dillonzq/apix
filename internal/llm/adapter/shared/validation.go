package shared

import (
	"fmt"

	"github.com/dillonzq/apix/internal/errclass"
)

// UnsupportedFormat reports a source protocol no translator in the calling
// adapter handles, naming the target endpoint.
func UnsupportedFormat(format, target string) *errclass.Error {
	return &errclass.Error{
		Class:   errclass.ClassUnsupported,
		Message: fmt.Sprintf("source protocol %q cannot be translated to %s", format, target),
	}
}

// UnsupportedPartType reports a client content-block/part type the target
// endpoint cannot represent (ClassUnsupported; explicit policy).
func UnsupportedPartType(partType, target string) *errclass.Error {
	return &errclass.Error{
		Class:   errclass.ClassUnsupported,
		Message: fmt.Sprintf("unsupported content block type %q for %s", partType, target),
	}
}

// UnsupportedInputItemType reports an unrecognized Responses input item type
// (ClassUnsupported; explicit policy).
func UnsupportedInputItemType(itemType string) *errclass.Error {
	return &errclass.Error{
		Class:   errclass.ClassUnsupported,
		Message: fmt.Sprintf("unsupported Responses input item type %q", itemType),
	}
}

// IsContentPartType reports whether an OpenAI-style content part type is
// accepted, covering both Chat Completions names (text, image_url) and
// their Responses-flavored aliases (input_text, output_text, input_image)
// so identical client bodies succeed on every route.
func IsContentPartType(partType string) bool {
	switch partType {
	case "text", "input_text", "output_text", "image_url", "input_image", "file", "input_file", "input_audio", "video_url", "input_video":
		return true
	}
	return false
}

// ValidateContentPart rejects an unknown OpenAI-style content part type
// as unsupported_protocol_or_parameter, naming the target endpoint.
// One kernel serves all three adapters so
// the error class cannot diverge again.
func ValidateContentPart(partType, target string) *errclass.Error {
	return &errclass.Error{
		Class:   errclass.ClassUnsupported,
		Message: fmt.Sprintf("unsupported content part type %q for %s", partType, target),
	}
}

// ValidateRole rejects a message role no translator in the calling
// adapter handles, same error class and rationale as ValidateContentPart.
func ValidateRole(role, target string) *errclass.Error {
	return &errclass.Error{
		Class:   errclass.ClassUnsupported,
		Message: fmt.Sprintf("unsupported message role %q for %s", role, target),
	}
}

// SystemImageRejected reports image content inside a system/instruction
// message, which no target protocol can represent. One kernel
// serves every adapter so the wording cannot diverge again.
func SystemImageRejected() *errclass.Error {
	return errclass.Translation("system messages cannot carry image content")
}
