package pressure

import "testing"

func TestParseSysctl(t *testing.T) {
	cases := []struct {
		raw  string
		want Level
		ok   bool
	}{
		{"1", Normal, true},
		{"2\n", Warn, true},
		{"4", Critical, true},
		{"kern.memorystatus_vm_pressure_level: 2", Warn, true},
		{"kern.memorystatus_vm_pressure_level: 4\n", Critical, true},
		{"", 0, false},
		{"bogus", 0, false},
		{"3", 0, false}, // macOS never emits 3; reject unmapped values
	}
	for _, c := range cases {
		got, err := ParseSysctl(c.raw)
		if c.ok && (err != nil || got != c.want) {
			t.Errorf("ParseSysctl(%q) = (%v, %v), want (%v, nil)", c.raw, got, err, c.want)
		}
		if !c.ok && err == nil {
			t.Errorf("ParseSysctl(%q) = (%v, nil), want error", c.raw, got)
		}
	}
}

func TestEvaluate(t *testing.T) {
	cases := []struct {
		name       string
		level      Level
		count, max int
		want       bool
	}{
		{"normal under limit", Normal, 3, 5, false},
		{"warn under limit", Warn, 3, 5, true},
		{"critical under limit", Critical, 0, 5, true},
		{"normal at limit", Normal, 5, 5, true},
		{"normal over limit", Normal, 6, 5, true},
		{"normal one under limit", Normal, 4, 5, false},
		{"count trigger disabled (max<=0)", Normal, 99, 0, false},
	}
	for _, c := range cases {
		got := Evaluate(c.level, c.count, c.max)
		if got.Elevated != c.want {
			t.Errorf("%s: Evaluate(%v,%d,%d).Elevated = %v, want %v",
				c.name, c.level, c.count, c.max, got.Elevated, c.want)
		}
		if got.Elevated && got.Reason == "" {
			t.Errorf("%s: elevated verdict must have a Reason", c.name)
		}
		if !got.Elevated && got.Reason != "" {
			t.Errorf("%s: non-elevated verdict must have empty Reason, got %q", c.name, got.Reason)
		}
	}
}

func TestLevelString(t *testing.T) {
	if Normal.String() != "normal" || Warn.String() != "warn" || Critical.String() != "critical" {
		t.Fatal("level names wrong")
	}
	if Level(99).String() != "unknown" {
		t.Fatal("unmapped level should be 'unknown'")
	}
}
