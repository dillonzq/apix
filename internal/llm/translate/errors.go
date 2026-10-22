package translate

import (
	"github.com/dillonzq/apix/internal/errclass"
)

// Error carries the error class, HTTP status, and retryability.
type Error = errclass.Error

// Class identifies an error category used by the conversion engine.
type Class = errclass.Class

const (
	ClassUnsupported = errclass.ClassUnsupported
	ClassQuota       = errclass.ClassQuota
	ClassUpstream    = errclass.ClassUpstream
	ClassTranslation = errclass.ClassTranslation
)

func Translation(message string) *Error { return errclass.Translation(message) }
