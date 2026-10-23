package reasoning

import "testing"

func TestParseSuffix(t *testing.T) {
	budget := func(v int64) *int64 { return &v }
	for _, tc := range []struct {
		model     string
		wantModel string
		raw       string
		has       bool
		effort    string
		budget    *int64
	}{
		{"glm-5.2", "glm-5.2", "", false, "", nil},
		{"opencode-go/glm-5.2(high)", "opencode-go/glm-5.2", "high", true, "high", nil},
		{"glm-5.2(HIGH)", "glm-5.2", "HIGH", true, "high", nil},
		{"glm-5.2(16384)", "glm-5.2", "16384", true, "high", budget(16384)},
		{"glm-5.2(08192)", "glm-5.2", "08192", true, "medium", budget(8192)},
		{"glm-5.2(1)", "glm-5.2", "1", true, "minimal", budget(1)},
		{"glm-5.2(0)", "glm-5.2", "0", true, "none", budget(0)},
		{"glm-5.2(-1)", "glm-5.2", "-1", true, "auto", nil},
		{"glm-5.2(none)", "glm-5.2", "none", true, "none", nil},
		{"glm-5.2(auto)", "glm-5.2", "auto", true, "auto", nil},
		{"glm-5.2(max)", "glm-5.2", "max", true, "max", nil},
		{"glm-5.2(xhigh)", "glm-5.2", "xhigh", true, "xhigh", nil},
		{"glm-5.2(24577)", "glm-5.2", "24577", true, "xhigh", budget(24577)},
		{"glm-5.2(turbo)", "glm-5.2", "turbo", true, "", nil},
		{"glm-5.2(-2)", "glm-5.2", "-2", true, "", nil},
		{"glm-5.2()", "glm-5.2", "", true, "", nil},
		{"glm-5.2( high )", "glm-5.2", " high ", true, "", nil},
		{"glm-5.2(9223372036854775808)", "glm-5.2", "9223372036854775808", true, "", nil},
		// A suffix is stripped only when the name ends with ")".
		{"glm-5.2(high", "glm-5.2(high", "", false, "", nil},
		{"glm-5.2)", "glm-5.2)", "", false, "", nil},
		// The last pair wins, using the last opening parenthesis.
		{"minimax(m3)(low)", "minimax(m3)", "low", true, "low", nil},
	} {
		got := ParseSuffix(tc.model)
		if got.Model != tc.wantModel || got.Raw != tc.raw || got.HasSuffix != tc.has || got.Effort != tc.effort {
			t.Errorf("ParseSuffix(%q) = %+v, want model=%q raw=%q has=%t effort=%q",
				tc.model, got, tc.wantModel, tc.raw, tc.has, tc.effort)
		}
		switch {
		case tc.budget == nil && got.Budget != nil:
			t.Errorf("ParseSuffix(%q).Budget = %d, want nil", tc.model, *got.Budget)
		case tc.budget != nil && got.Budget == nil:
			t.Errorf("ParseSuffix(%q).Budget = nil, want %d", tc.model, *tc.budget)
		case tc.budget != nil && *got.Budget != *tc.budget:
			t.Errorf("ParseSuffix(%q).Budget = %d, want %d", tc.model, *got.Budget, *tc.budget)
		}
	}
}
