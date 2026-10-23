package reasoning

import (
	"strconv"
	"strings"

	"github.com/dillonzq/apix"
)

// Suffix describes an optional trailing "model(value)" reasoning control.
// Callers explicitly parse the suffix for model routing and apply its control.
type Suffix struct {
	// Model is the requested model name with the suffix removed.
	Model string
	// Raw is the value between the parentheses; empty unless HasSuffix.
	Raw string
	// HasSuffix reports whether the name ended with "(value)".
	HasSuffix bool
	// Effort is the canonical reasoning effort the raw value selects
	// (none, auto, minimal, low, medium, high, xhigh, max), or "" when the
	// value is not a recognized suffix, so "" means "strip only".
	Effort string
	// Budget is the exact numeric budget when Raw was a non-negative
	// integer: budget-shaped targets keep the requested number instead of
	// re-deriving it from the thresholds.
	Budget *int64
}

// ParseSuffix splits a reasoning suffix off a model name: the last "(" of a
// name that ends with ")" opens the suffix. Unrecognized values yield an
// empty Effort while still removing the suffix from Model.
func ParseSuffix(model string) Suffix {
	lastOpen := strings.LastIndex(model, "(")
	if lastOpen < 0 || !strings.HasSuffix(model, ")") {
		return Suffix{Model: model}
	}
	raw := model[lastOpen+1 : len(model)-1]
	suffix := Suffix{Model: model[:lastOpen], Raw: raw, HasSuffix: true}
	suffix.Effort, suffix.Budget = suffixConfig(raw)
	return suffix
}

// suffixConfig interprets a raw suffix value: the special none/auto
// values ("-1" is auto), the discrete levels, and non-negative numeric budgets
// converted through the fixed budget thresholds. Anything else — including
// negative numbers other than -1 — is not a suffix value.
func suffixConfig(raw string) (string, *int64) {
	switch strings.ToLower(raw) {
	case "none":
		return "none", nil
	case "auto", "-1":
		return "auto", nil
	case "minimal", "low", "medium", "high", "xhigh", "max":
		return strings.ToLower(raw), nil
	}
	budget, err := strconv.ParseInt(raw, 10, 64)
	if err != nil || budget < 0 {
		return "", nil
	}
	effort, ok := EffortFromBudget(budget)
	if !ok {
		return "", nil
	}
	return effort, &budget
}

// Reasoning returns the override a recognized suffix selects, for
// apix.ExchangeOptions.Reasoning. It is nil when the suffix is absent or its
// value is not recognized.
func (s Suffix) Reasoning() *apix.Reasoning {
	if s.Effort == "" {
		return nil
	}
	r := &apix.Reasoning{Effort: s.Effort}
	if s.Budget != nil {
		budget := *s.Budget
		r.Budget = &budget
	}
	return r
}
