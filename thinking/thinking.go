// Package thinking parses "model(value)" reasoning suffixes and converts
// reasoning controls between wire formats using fixed effort/budget mappings.
// Upstream providers decide which levels and budgets they support.
//
// Hosts that convert through an apix.Exchange pass Suffix.Reasoning as
// apix.ExchangeOptions.Reasoning (gateway.Config.ThinkingSuffix does this
// automatically); the exchange then renders it with the upstream codec's own
// options. ApplySuffix and StripSupersededReasoning serve hosts that build
// upstream bodies themselves.
package thinking

import "github.com/dillonzq/apix/internal/llm/reasoning"

// BudgetFromEffort converts a known level without capability filtering or
// budget clamping. Unknown levels have no numeric equivalent.
func BudgetFromEffort(effort string) (int64, bool) { return reasoning.BudgetFromEffort(effort) }

// EffortFromBudget preserves the zero/off and -1/auto sentinels. Positive
// budgets use the library's fixed thresholds; max is never derived from a budget.
func EffortFromBudget(budget int64) (string, bool) { return reasoning.EffortFromBudget(budget) }
