// Package thinking converts reasoning controls between wire formats using
// fixed effort/budget mappings and optional model-name suffixes. Upstream
// providers decide which levels and budgets they support.
package thinking

import "strings"

var levelBudgetTable = map[string]int64{
	"none":    0,
	"auto":    -1,
	"minimal": 512,
	"low":     1024,
	"medium":  8192,
	"high":    24576,
	"xhigh":   32768,
	"max":     128000,
}

// BudgetFromEffort converts a known level without capability filtering or
// budget clamping. Unknown levels have no numeric equivalent.
func BudgetFromEffort(effort string) (int64, bool) {
	budget, ok := levelBudgetTable[strings.ToLower(strings.TrimSpace(effort))]
	return budget, ok
}

// EffortFromBudget preserves the zero/off and -1/auto sentinels. Positive
// budgets use the library's fixed thresholds; max is never derived from a budget.
func EffortFromBudget(budget int64) (string, bool) {
	switch {
	case budget < -1:
		return "", false
	case budget == -1:
		return "auto", true
	case budget == 0:
		return "none", true
	case budget <= 512:
		return "minimal", true
	case budget <= 1024:
		return "low", true
	case budget <= 8192:
		return "medium", true
	case budget <= 24576:
		return "high", true
	default:
		return "xhigh", true
	}
}
