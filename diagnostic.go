package apix

// LossPolicy selects which known request conversion losses are permitted.
// Responses report losses but are never rejected because of the policy.
type LossPolicy string

const (
	// LossPolicyAllow permits known losses and records diagnostics. The
	// zero value behaves the same.
	LossPolicyAllow LossPolicy = "allow"
	// LossPolicySafe rejects losses that change request semantics.
	LossPolicySafe LossPolicy = "safe"
	// LossPolicyStrict rejects every reported request loss.
	LossPolicyStrict LossPolicy = "strict"
)

func (p LossPolicy) valid() bool {
	switch p {
	case "", LossPolicyAllow, LossPolicySafe, LossPolicyStrict:
		return true
	}
	return false
}

// Diagnostic describes a field omitted or approximated by conversion.
// Messages name fields and never include request values or upstream content.
type Diagnostic struct {
	Code     string
	Path     string
	Message  string
	Semantic bool
	From     Protocol
	To       Protocol
}
