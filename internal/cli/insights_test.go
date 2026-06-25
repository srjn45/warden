package cli

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/srjn45/warden/internal/client"
	"github.com/srjn45/warden/internal/insights"
)

type fakeInsightsClient struct {
	report *insights.Report
	err    error
	params client.InsightsParams
}

func (f *fakeInsightsClient) Insights(_ context.Context, p client.InsightsParams) (*insights.Report, error) {
	f.params = p
	return f.report, f.err
}

func sampleReport() *insights.Report {
	return &insights.Report{
		Sessions:       3,
		ActiveSessions: 1,
		Durations: []insights.TypeDuration{
			{Type: "development", Count: 2, MedianSec: 600, P90Sec: 1200, MaxSec: 3600, Outliers: []string{"d4"}},
		},
		Parallelizable: []insights.ParallelSuggestion{
			{A: "s1", B: "s2", ALabel: "alpha", BLabel: "beta", Repo: "/r", SavedSec: 600, Reason: "s1 and s2 disjoint"},
			{A: "s3", B: "s4", ALabel: "s3", BLabel: "s4", Repo: "/r", SavedSec: 120, Reason: "s3 and s4 disjoint"},
		},
		ErrorRates: []insights.TypeErrorRate{{Type: "tests", Total: 3, Errored: 1, Rate: 1.0 / 3.0}},
	}
}

func TestRunInsightsNarratesDeterministicWithNilCompleter(t *testing.T) {
	f := &fakeInsightsClient{report: sampleReport()}
	r, narration, err := runInsights(context.Background(), f, nil, client.InsightsParams{}, "")
	if err != nil {
		t.Fatalf("runInsights err: %v", err)
	}
	if r == nil {
		t.Fatal("nil report")
	}
	// Nil completer ⇒ deterministic floor.
	if narration != insights.DeterministicSummary(*r) {
		t.Fatalf("narration should be the deterministic floor, got %q", narration)
	}
}

func TestRunInsightsPropagatesError(t *testing.T) {
	f := &fakeInsightsClient{err: errors.New("daemon down")}
	if _, _, err := runInsights(context.Background(), f, nil, client.InsightsParams{}, ""); err == nil {
		t.Fatal("want error propagated")
	}
}

func TestRunInsightsSessionScoping(t *testing.T) {
	f := &fakeInsightsClient{report: sampleReport()}
	r, _, err := runInsights(context.Background(), f, nil, client.InsightsParams{}, "alpha")
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	if len(r.Parallelizable) != 1 {
		t.Fatalf("session scope should keep only the matching suggestion, got %d", len(r.Parallelizable))
	}
	if r.Parallelizable[0].A != "s1" {
		t.Fatalf("kept wrong suggestion: %+v", r.Parallelizable[0])
	}
}

func TestFilterParallelBySessionEmptyIsNoop(t *testing.T) {
	r := sampleReport()
	got := filterParallelBySession(r, "")
	if len(got) != 2 {
		t.Fatalf("empty filter must keep all, got %d", len(got))
	}
}

func TestFilterParallelBySessionMatchesIDOrLabel(t *testing.T) {
	r := sampleReport()
	// match by id
	if got := filterParallelBySession(r, "s2"); len(got) != 1 || got[0].B != "s2" {
		t.Fatalf("id match failed: %+v", got)
	}
	// match by label
	if got := filterParallelBySession(r, "beta"); len(got) != 1 || got[0].BLabel != "beta" {
		t.Fatalf("label match failed: %+v", got)
	}
	// no match
	if got := filterParallelBySession(r, "nope"); len(got) != 0 {
		t.Fatalf("no-match should be empty, got %+v", got)
	}
}

func TestFormatInsightsSections(t *testing.T) {
	r := sampleReport()
	out := formatInsights(r, "Top suggestion: parallelize alpha and beta.", "")
	for _, want := range []string{
		"Top suggestion: parallelize alpha and beta.",
		"session duration by type:",
		"development",
		"outliers: d4",
		"parallelization opportunities:",
		"alpha + beta",
		"error rate by type:",
		"3 sessions analyzed (1 active)",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("formatInsights missing %q in:\n%s", want, out)
		}
	}
}

func TestFormatInsightsSessionNote(t *testing.T) {
	r := sampleReport()
	out := formatInsights(r, "", "alpha")
	if !strings.Contains(out, "(scoped to session alpha)") {
		t.Errorf("scoped note missing:\n%s", out)
	}
}
