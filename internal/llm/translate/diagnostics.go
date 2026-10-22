package translate

import (
	"github.com/dillonzq/apix/internal/llm/adapter/shared"
)

// LossPolicy selects which known request conversion losses are permitted.
type LossPolicy = shared.LossPolicy

const (
	LossPolicyAllow  = shared.LossPolicyAllow
	LossPolicySafe   = shared.LossPolicySafe
	LossPolicyStrict = shared.LossPolicyStrict
)

// Diagnostic describes a field omitted or approximated by conversion.
type Diagnostic = shared.Diagnostic

// ConversionOptions configures loss handling for one execution.
type ConversionOptions = shared.ConversionOptions

// Diagnostics returns a copy of this execution's recorded losses.
func (s *RequestState) Diagnostics() []Diagnostic {
	if s == nil {
		return nil
	}
	return s.context.Diagnostics()
}
