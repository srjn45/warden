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
		name         string
		level        Level
		count, max   int
		wantElevated bool
		wantAdvisory bool
	}{
		{"normal under limit", Normal, 3, 5, false, false},
		// Warn no longer BLOCKS — it is advisory only. Blocking every spawn at
		// warn (a common, recoverable macOS state) trained operators to --force
		// reflexively; only critical (imminent swap) is worth a hard gate.
		{"warn under limit is advisory not blocking", Warn, 3, 5, false, true},
		{"critical under limit blocks", Critical, 0, 5, true, false},
		{"normal at limit blocks on count", Normal, 5, 5, true, false},
		{"normal over limit blocks on count", Normal, 6, 5, true, false},
		{"normal one under limit", Normal, 4, 5, false, false},
		{"count trigger disabled (max<=0)", Normal, 99, 0, false, false},
		// When warn coincides with the count cap, the count hard-blocks and the
		// advisory is subsumed (a blocking verdict is not also "advisory").
		{"warn at count limit blocks, not advisory", Warn, 5, 5, true, false},
		{"critical over count limit blocks", Critical, 6, 5, true, false},
	}
	for _, c := range cases {
		got := Evaluate(c.level, c.count, c.max)
		if got.Elevated != c.wantElevated {
			t.Errorf("%s: Evaluate(%v,%d,%d).Elevated = %v, want %v",
				c.name, c.level, c.count, c.max, got.Elevated, c.wantElevated)
		}
		if got.Advisory != c.wantAdvisory {
			t.Errorf("%s: Evaluate(%v,%d,%d).Advisory = %v, want %v",
				c.name, c.level, c.count, c.max, got.Advisory, c.wantAdvisory)
		}
		// Any non-normal verdict (blocking or advisory) must explain itself; a
		// fully-normal verdict must stay silent.
		if (got.Elevated || got.Advisory) && got.Reason == "" {
			t.Errorf("%s: elevated/advisory verdict must have a Reason", c.name)
		}
		if !got.Elevated && !got.Advisory && got.Reason != "" {
			t.Errorf("%s: normal verdict must have empty Reason, got %q", c.name, got.Reason)
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
