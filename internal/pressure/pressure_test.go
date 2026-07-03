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

func TestParsePSI(t *testing.T) {
	cases := []struct {
		name    string
		raw     string
		want    Level
		wantErr bool
	}{
		{"idle", "some avg10=0.00 avg60=0.00 avg300=0.00 total=0\nfull avg10=0.00 avg60=0.00 avg300=0.00 total=0\n", Normal, false},
		{"light load below thresholds", "some avg10=12.40 avg60=8.00 avg300=2.00 total=100\nfull avg10=1.20 avg60=0.50 avg300=0.10 total=10\n", Normal, false},
		{"warn by some", "some avg10=25.00 avg60=10.00 avg300=3.00 total=100\nfull avg10=0.00 avg60=0.00 avg300=0.00 total=0\n", Warn, false},
		{"warn by full", "some avg10=10.00 avg60=4.00 avg300=1.00 total=100\nfull avg10=5.00 avg60=2.00 avg300=0.50 total=10\n", Warn, false},
		{"critical by full", "some avg10=40.00 avg60=30.00 avg300=10.00 total=100\nfull avg10=20.00 avg60=15.00 avg300=5.00 total=10\n", Critical, false},
		{"critical by some", "some avg10=61.00 avg60=50.00 avg300=20.00 total=100\nfull avg10=10.00 avg60=8.00 avg300=3.00 total=10\n", Critical, false},
		{"some-only file still parses", "some avg10=70.00 avg60=50.00 avg300=20.00 total=100\n", Critical, false},
		{"empty", "", 0, true},
		{"garbage", "cpu usage 42%\n", 0, true},
		{"bad avg10 value", "some avg10=notanumber avg60=0.00 avg300=0.00 total=0\n", 0, true},
	}
	for _, c := range cases {
		got, err := ParsePSI(c.raw)
		if c.wantErr {
			if err == nil {
				t.Errorf("%s: ParsePSI(%q) = %v, want error", c.name, c.raw, got)
			}
			continue
		}
		if err != nil || got != c.want {
			t.Errorf("%s: ParsePSI(%q) = (%v,%v), want (%v,nil)", c.name, c.raw, got, err, c.want)
		}
	}
}
