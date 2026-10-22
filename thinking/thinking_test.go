package thinking

import "testing"

func TestEffortFromBudget(t *testing.T) {
	for _, tc := range []struct {
		budget int64
		effort string
		ok     bool
	}{
		{-2, "", false}, {-1, "auto", true}, {0, "none", true},
		{1, "minimal", true}, {512, "minimal", true}, {513, "low", true},
		{1024, "low", true}, {1025, "medium", true}, {8192, "medium", true},
		{8193, "high", true}, {24576, "high", true}, {24577, "xhigh", true},
		{128000, "xhigh", true},
	} {
		effort, ok := EffortFromBudget(tc.budget)
		if effort != tc.effort || ok != tc.ok {
			t.Errorf("EffortFromBudget(%d) = (%q, %t), want (%q, %t)", tc.budget, effort, ok, tc.effort, tc.ok)
		}
	}
}

func TestBudgetFromEffort(t *testing.T) {
	for _, tc := range []struct {
		effort string
		budget int64
		ok     bool
	}{
		{"none", 0, true}, {" AUTO ", -1, true}, {"minimal", 512, true},
		{"low", 1024, true}, {"medium", 8192, true}, {"high", 24576, true},
		{" XHigh ", 32768, true}, {"max", 128000, true},
		{"", 0, false}, {"turbo", 0, false},
	} {
		budget, ok := BudgetFromEffort(tc.effort)
		if budget != tc.budget || ok != tc.ok {
			t.Errorf("BudgetFromEffort(%q) = (%d, %t), want (%d, %t)", tc.effort, budget, ok, tc.budget, tc.ok)
		}
	}
}
