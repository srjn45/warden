package lifecycle

import "testing"

func TestParseDoneSignal(t *testing.T) {
	cases := []struct {
		name       string
		pane       string
		wantOK     bool
		wantStatus string
		wantSummry string
	}{
		{
			name:       "plain sentinel",
			pane:       `<<WARDEN_DONE>>{"status":"success","summary":"added the done-signal"}`,
			wantOK:     true,
			wantStatus: "success",
			wantSummry: "added the done-signal",
		},
		{
			name:       "sentinel amid other output",
			pane:       "$ wd job done --summary x\nbuilding...\n<<WARDEN_DONE>>{\"status\":\"failure\",\"summary\":\"tests red\"}\n$ ",
			wantOK:     true,
			wantStatus: "failure",
			wantSummry: "tests red",
		},
		{
			name:       "last sentinel wins",
			pane:       "<<WARDEN_DONE>>{\"status\":\"blocked\",\"summary\":\"first\"}\n<<WARDEN_DONE>>{\"status\":\"success\",\"summary\":\"second\"}",
			wantOK:     true,
			wantStatus: "success",
			wantSummry: "second",
		},
		{
			name:       "missing status defaults to success",
			pane:       `<<WARDEN_DONE>>{"summary":"no status field"}`,
			wantOK:     true,
			wantStatus: "success",
			wantSummry: "no status field",
		},
		{
			name:       "trailing pane border trimmed",
			pane:       `│ <<WARDEN_DONE>>{"status":"success","summary":"boxed"}   │`,
			wantOK:     true,
			wantStatus: "success",
			wantSummry: "boxed",
		},
		{
			name:   "marker with no json ignored",
			pane:   "here is how you finish: <<WARDEN_DONE>>",
			wantOK: false,
		},
		{
			name:   "marker with malformed json ignored",
			pane:   `<<WARDEN_DONE>>{status: not json}`,
			wantOK: false,
		},
		{
			name:   "no marker",
			pane:   "just some normal agent output\nnothing to see here",
			wantOK: false,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			sig, ok := ParseDoneSignal(tc.pane)
			if ok != tc.wantOK {
				t.Fatalf("ok=%v want %v (sig=%+v)", ok, tc.wantOK, sig)
			}
			if !tc.wantOK {
				return
			}
			if sig.Status != tc.wantStatus {
				t.Errorf("status=%q want %q", sig.Status, tc.wantStatus)
			}
			if sig.Summary != tc.wantSummry {
				t.Errorf("summary=%q want %q", sig.Summary, tc.wantSummry)
			}
		})
	}
}

func TestNormalizeDoneStatus(t *testing.T) {
	cases := map[string]string{
		"":            "success",
		"success":     "success",
		"SUCCESS":     "success",
		" done ":      "success", // unrecognized ⇒ success
		"failure":     "failure",
		"failed":      "failure",
		"error":       "failure",
		"blocked":     "blocked",
		"needs-input": "blocked",
		"stuck":       "blocked",
	}
	for in, want := range cases {
		if got := NormalizeDoneStatus(in); got != want {
			t.Errorf("NormalizeDoneStatus(%q)=%q want %q", in, got, want)
		}
	}
}
