package spend

import "testing"

func TestEvaluateBudget(t *testing.T) {
	cases := []struct {
		name                               string
		daily, weekly, capDaily, capWeekly float64
		wantOver                           bool
	}{
		{"under both", 5, 20, 25, 100, false},
		{"at daily cap", 25, 20, 25, 100, true},
		{"over daily cap", 30, 20, 25, 100, true},
		{"at weekly cap", 5, 100, 25, 100, true},
		{"daily axis off", 999, 20, 0, 100, false},
		{"weekly axis off", 5, 999, 25, 0, false},
		{"both axes off", 999, 999, 0, 0, false},
		{"both over", 30, 120, 25, 100, true},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			v := EvaluateBudget(c.daily, c.weekly, c.capDaily, c.capWeekly)
			if v.Over != c.wantOver {
				t.Errorf("Over = %v, want %v (%+v)", v.Over, c.wantOver, v)
			}
			if c.wantOver && v.Reason == "" {
				t.Errorf("expected a non-empty Reason when over: %+v", v)
			}
			if !c.wantOver && v.Reason != "" {
				t.Errorf("expected empty Reason when under: %q", v.Reason)
			}
		})
	}
}
