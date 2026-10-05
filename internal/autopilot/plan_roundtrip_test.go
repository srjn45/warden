package autopilot

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/srjn45/warden/internal/planexport"
	"github.com/srjn45/warden/internal/planstore"
)

var trickyStrings = []string{
	"Zero parsing at the hub: the hub is a pure relay",
	"trailing comment # not a comment",
	"- leading dash item",
	`it's "quoted" and 'single'`,
	"line one\nline two: still text\n# not a comment\n- not a list",
	"key: value",
}

func trickyPlan() *planstore.Plan {
	return &planstore.Plan{
		Name:        "rt",
		Goal:        "Goal: ship it # now\nsecond line",
		Constraints: trickyStrings,
		DoneWhen:    trickyStrings,
		Tasks: []planstore.PlanTask{
			{ID: "t1", Prompt: trickyStrings[4]},
			{ID: "t2", Prompt: trickyStrings[3], After: []string{"t1"}},
		},
	}
}

func assertRoundTrip(t *testing.T, want *planstore.Plan, got Plan) {
	t.Helper()
	if got.Goal != want.Goal {
		t.Errorf("goal = %q, want %q", got.Goal, want.Goal)
	}
	if strings.Join(got.Constraints, "\x00") != strings.Join(want.Constraints, "\x00") {
		t.Errorf("constraints = %q, want %q", got.Constraints, want.Constraints)
	}
	if strings.Join(got.DoneWhen, "\x00") != strings.Join(want.DoneWhen, "\x00") {
		t.Errorf("done_when = %q, want %q", got.DoneWhen, want.DoneWhen)
	}
	for i, tk := range want.Tasks {
		if got.Tasks[i].Prompt != tk.Prompt {
			t.Errorf("task %s prompt = %q, want %q", tk.ID, got.Tasks[i].Prompt, tk.Prompt)
		}
	}
}

// stripEnvelope drops the replica comment and envelope keys the plan loader's
// KnownFields(true) rejects, keeping the exporter's bytes for the plan body.
func stripEnvelope(t *testing.T, b []byte) []byte {
	t.Helper()
	drop := map[string]bool{"schema_version": true, "plan_id": true, "revision": true,
		"content_hash": true, "exported_at": true, "lifecycle": true, "execution_summary_ref": true}
	lines := strings.Split(string(b), "\n")
	var out []string
	for _, l := range lines {
		if k, _, ok := strings.Cut(l, ":"); ok && drop[k] && !strings.HasPrefix(l, " ") {
			continue
		}
		out = append(out, l)
	}
	return []byte(strings.Join(out, "\n"))
}

func TestPlanExportYAMLRoundTrip(t *testing.T) {
	want := trickyPlan()
	res, err := planexport.YAMLRenderer{}.Render(want, planexport.Options{ExportedAt: time.Unix(1700000000, 0)})
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(t.TempDir(), "plan.yaml")
	if err := os.WriteFile(path, stripEnvelope(t, res.Bytes), 0o644); err != nil {
		t.Fatal(err)
	}
	got, err := LoadPlan(path)
	if err != nil {
		t.Fatalf("LoadPlan: %v\n%s", err, res.Bytes)
	}
	assertRoundTrip(t, want, got)
	lp, _, err := loadPlanLenient(path)
	if err != nil {
		t.Fatalf("loadPlanLenient: %v", err)
	}
	assertRoundTrip(t, want, lp)
}

func TestLegacyPlanYAMLRoundTrip(t *testing.T) {
	want := trickyPlan()
	doc := planstore.LegacyPlanDocument{Version: 1, Name: want.Name, Goal: want.Goal,
		Constraints: want.Constraints, DoneWhen: want.DoneWhen}
	for _, tk := range want.Tasks {
		doc.Tasks = append(doc.Tasks, planstore.LegacyPlanTask{ID: tk.ID, Prompt: tk.Prompt, After: tk.After})
	}
	b, err := planstore.LegacyMarshalPlanYAML(doc)
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(t.TempDir(), "plan.yaml")
	if err := os.WriteFile(path, b, 0o644); err != nil {
		t.Fatal(err)
	}
	got, err := LoadPlan(path)
	if err != nil {
		t.Fatalf("LoadPlan: %v\n%s", err, b)
	}
	assertRoundTrip(t, want, got)
}

func TestLoadPlanLenientDecodeErrorHint(t *testing.T) {
	path := filepath.Join(t.TempDir(), "plan.yaml")
	body := "version: 1\ngoal: g\nconstraints:\n  - Zero parsing at the hub: pure relay\ntasks:\n  - id: a\n    prompt: p\n"
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	_, _, err := loadPlanLenient(path)
	if err == nil {
		t.Fatal("expected error")
	}
	msg := err.Error()
	if !strings.Contains(msg, "line 4") || !strings.Contains(msg, "quote") {
		t.Errorf("error lacks line/quote hint: %v", msg)
	}
}
