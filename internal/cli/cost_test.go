package cli

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"

	"github.com/srjn45/warden/internal/savings"
	"github.com/srjn45/warden/internal/spend"
)

// costStubDaemon serves the /spend and /savings endpoints the cost umbrella
// reads. A nil report for either side makes that endpoint return 403, so the
// disabled-feature degradation can be exercised.
func costStubDaemon(t *testing.T, rep *spend.Report, sum *savings.Summary) string {
	t.Helper()
	return stubDaemon(t, func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.HasPrefix(r.URL.Path, "/api/v1/spend"):
			if rep == nil {
				w.WriteHeader(http.StatusForbidden)
				return
			}
			_ = json.NewEncoder(w).Encode(rep)
		case strings.HasPrefix(r.URL.Path, "/api/v1/savings"):
			if sum == nil {
				w.WriteHeader(http.StatusForbidden)
				return
			}
			_ = json.NewEncoder(w).Encode(sum)
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	})
}

// TestCostSummaryShowsBothAxes renders the no-subcommand `wd cost` view with both
// the spend rollup and the savings ledger under their labeled sections.
func TestCostSummaryShowsBothAxes(t *testing.T) {
	sum := &savings.Summary{
		Events:              3,
		ContextReductionPct: 62.5,
		ContextSavedDollars: 1.23,
		Features:            []savings.FeatureSummary{{Feature: "check", SavedTokens: 5000, RawTokens: 8000, Events: 3}},
	}
	addr := costStubDaemon(t, sampleSpendReport(), sum)

	out, err := runCLI(t, addr, "cost")
	if err != nil {
		t.Fatalf("wd cost: %v\n%s", err, out)
	}
	for _, want := range []string{
		"SPEND — dollars agents billed Claude",
		"$8.00 total",
		"SAVINGS — tokens warden kept out of context",
		"token savings",
		"check",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("wd cost output missing %q:\n%s", want, out)
		}
	}
}

// TestCostSpendSubcommandMatchesAlias proves `wd cost spend` and the top-level
// alias `wd spend` produce identical output (the umbrella is wiring, not a fork).
func TestCostSpendSubcommandMatchesAlias(t *testing.T) {
	addr := costStubDaemon(t, sampleSpendReport(), &savings.Summary{})

	viaCost, err := runCLI(t, addr, "cost", "spend", "--json")
	if err != nil {
		t.Fatalf("wd cost spend: %v\n%s", err, viaCost)
	}
	viaAlias, err := runCLI(t, addr, "spend", "--json")
	if err != nil {
		t.Fatalf("wd spend: %v\n%s", err, viaAlias)
	}
	if viaCost != viaAlias {
		t.Errorf("`wd cost spend` and `wd spend` diverged:\ncost:  %q\nalias: %q", viaCost, viaAlias)
	}
}

// TestCostSavingsSubcommandWiresFlags confirms `wd cost savings` honors the same
// flags as the alias (here --json yields the structured summary, not the table).
func TestCostSavingsSubcommandWiresFlags(t *testing.T) {
	addr := costStubDaemon(t, sampleSpendReport(), &savings.Summary{Events: 1})

	out, err := runCLI(t, addr, "cost", "savings", "--json")
	if err != nil {
		t.Fatalf("wd cost savings --json: %v\n%s", err, out)
	}
	if !strings.Contains(out, "\"events\"") && !strings.Contains(out, "\"Events\"") {
		t.Errorf("expected JSON summary from `wd cost savings --json`:\n%s", out)
	}
}

// TestCostSummaryDisabledDegrades shows each section falls back to its own enable
// hint when the daemon reports the feature off (403), without failing the view.
func TestCostSummaryDisabledDegrades(t *testing.T) {
	addr := costStubDaemon(t, nil, nil)

	out, err := runCLI(t, addr, "cost")
	if err != nil {
		t.Fatalf("wd cost (disabled): %v\n%s", err, out)
	}
	if !strings.Contains(out, "spend tracking is disabled") {
		t.Errorf("expected spend disabled hint:\n%s", out)
	}
	if !strings.Contains(out, "savings ledger is disabled") {
		t.Errorf("expected savings disabled hint:\n%s", out)
	}
}
