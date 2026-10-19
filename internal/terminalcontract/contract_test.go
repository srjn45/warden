package terminalcontract

import (
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"
)

const specPath = "docs/specs/2026-10-19-plain-terminal-inventory-and-contract.md"

// repoRoot walks up to go.mod so tests run from any package directory.
func repoRoot(t *testing.T) string {
	t.Helper()
	dir, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	for {
		if _, err := os.Stat(filepath.Join(dir, "go.mod")); err == nil {
			return dir
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			t.Fatal("go.mod not found")
		}
		dir = parent
	}
}

func read(t *testing.T, rel string) string {
	t.Helper()
	b, err := os.ReadFile(filepath.Join(repoRoot(t), rel))
	if err != nil {
		t.Fatalf("read %s: %v", rel, err)
	}
	return string(b)
}

func checkAnchor(t *testing.T, what string, a Anchor) {
	t.Helper()
	if !strings.Contains(read(t, a.File), a.Contains) {
		t.Errorf("%s: anchor %q not found in %s — the code moved: update the matrix AND the spec", what, a.Contains, a.File)
	}
}

func TestPathsAnchoredAndUnique(t *testing.T) {
	seen := map[string]bool{}
	for _, p := range Paths {
		if seen[p.ID] {
			t.Errorf("duplicate path id %s", p.ID)
		}
		seen[p.ID] = true
		if p.Surface == "" || p.Op == "" || p.Runtime == "" || p.Preserves == "" {
			t.Errorf("%s: incomplete row", p.ID)
		}
		checkAnchor(t, p.ID, p.Anchor)
	}
}

// Every operation the task names must be covered by at least one row per
// surface that can perform it.
func TestMatrixCoversRequiredSurfaces(t *testing.T) {
	need := map[string][]Op{
		"cli": {OpCreate}, "api": {OpCreate, OpRestore, OpAttach, OpList, OpStop},
		"mcp": {OpCreate}, "tui": {OpCreate, OpAttach}, "web": {OpCreate},
		"daemon": {OpRestore, OpAuto, OpStop}, "tree": {OpList},
	}
	have := map[string]map[Op]bool{}
	for _, p := range Paths {
		if have[p.Surface] == nil {
			have[p.Surface] = map[Op]bool{}
		}
		have[p.Surface][p.Op] = true
	}
	for s, ops := range need {
		for _, op := range ops {
			if !have[s][op] {
				t.Errorf("matrix has no %s/%s row", s, op)
			}
		}
	}
}

func TestAttributesAnchored(t *testing.T) {
	for _, a := range Attributes {
		checkAnchor(t, "attribute "+a.Name, a.Where)
	}
	for _, n := range []string{"id", "name", "project", "cwd", "shell", "environment", "persistence"} {
		found := false
		for _, a := range Attributes {
			found = found || a.Name == n
		}
		if !found {
			t.Errorf("missing attribute %q", n)
		}
	}
}

func TestPlatformsMatchBuildTags(t *testing.T) {
	tags := read(t, "internal/ptyruntime/termios.go")
	if !strings.Contains(tags, "//go:build linux || darwin") {
		t.Fatalf("ptyruntime build tags changed; update Platforms")
	}
	for _, p := range Platforms {
		if want := strings.Contains(tags, p.GOOS); want != p.Supported {
			t.Errorf("%s: Supported=%v but build tag says %v", p.GOOS, p.Supported, want)
		}
	}
}

// Open gaps carry live evidence; every release-critical gap names fixtures that
// exist in the matrix, and every fixture names a known owner.
func TestGapsAndFixturesConsistent(t *testing.T) {
	fx := map[string]Fixture{}
	for _, f := range Fixtures {
		if _, dup := fx[f.ID]; dup {
			t.Errorf("duplicate fixture %s", f.ID)
		}
		fx[f.ID] = f
		switch f.Owner {
		case "t1", "t2", "t3", "t4", "t5", "plan01":
		default:
			t.Errorf("%s: unknown owner %q", f.ID, f.Owner)
		}
		if f.Test.File != "" {
			checkAnchor(t, f.ID, f.Test)
		}
		if !strings.HasPrefix(f.ID, "F-") || f.What == "" || f.Gate == "" || len(f.Kinds) == 0 {
			t.Errorf("%s: incomplete fixture", f.ID)
		}
	}
	covered := map[string]bool{}
	for _, g := range Gaps {
		if g.Owner == "plan01" || g.Owner == "" {
			t.Errorf("%s: plan 01 is frozen; gap must be owned by a plan 02 task", g.ID)
		}
		if !g.Closed {
			checkAnchor(t, g.ID, g.Evidence)
		}
		if len(g.Fixtures) == 0 {
			t.Errorf("%s: no proving fixture", g.ID)
		}
		for _, id := range g.Fixtures {
			f, ok := fx[id]
			if !ok {
				t.Errorf("%s: unknown fixture %s", g.ID, id)
				continue
			}
			covered[id] = true
			if g.Closed && f.Test.File == "" {
				t.Errorf("%s closed but fixture %s has no test anchor", g.ID, id)
			}
		}
	}
	// Fixtures that prove nothing in the register must still be deliberate.
	for id, f := range fx {
		if !covered[id] && f.Class != "parity" && f.Class != "resource" && f.Class != "privacy" &&
			f.Class != "rollout" && id != "F-FENCE-04" && id != "F-LIFE-05" {
			t.Errorf("fixture %s is not referenced by any gap", id)
		}
	}
	// The three gaps the plan names must be recorded as release-critical.
	for _, id := range []string{"G1", "G4"} {
		for _, g := range Gaps {
			if g.ID == id && g.Severity != ReleaseCritical {
				t.Errorf("%s must be release-critical", id)
			}
		}
	}
}

func TestBudgetsSane(t *testing.T) {
	b := Default
	zero := map[string]int{
		"ReplayOrderViolations": b.ReplayOrderViolations, "PromptFalseApprovals": b.PromptFalseApprovals,
		"ShellClassifierEvents": b.ShellClassifierEvents, "DuplicateProcesses": b.DuplicateProcesses,
		"DuplicateInputBytes": b.DuplicateInputBytes, "StaleControllerWrites": b.StaleControllerWrites,
		"SecretLeaksInDurable": b.SecretLeaksInDurable,
	}
	for n, v := range zero {
		if v != 0 {
			t.Errorf("%s is a zero-tolerance budget, got %d", n, v)
		}
	}
	if b.SurvivalLossPct != 0 || b.ParityByteExactPct != 100 || b.ActionableErrorPct != 100 {
		t.Error("survival/parity/actionable-error budgets must be absolute")
	}
	if b.StateAccuracyPct <= 90 || b.StateAccuracyPct > 100 {
		t.Errorf("StateAccuracyPct %v out of range", b.StateAccuracyPct)
	}
	pos := []time.Duration{b.KeystrokeEchoP99, b.AttachToFirstByteP99, b.ResizeToSIGWINCHP99,
		b.AdoptAfterRestoreP99, b.LeaseTakeoverP99, b.SlowViewerPTYStall, b.RecoveryBound}
	for i, d := range pos {
		if d <= 0 {
			t.Errorf("duration budget #%d not positive", i)
		}
	}
	if b.KeystrokeEchoP99 > b.AttachToFirstByteP99 || b.AdoptAfterRestoreP99 > b.RecoveryBound {
		t.Error("latency budgets are out of order")
	}
	if b.PerSessionRSSKiB <= 0 || b.PerSessionFDs <= 0 || b.JournalMaxBytes <= 0 || b.SoakSessions <= 0 ||
		b.MinSessionsPerStage <= 0 || b.MinSoakHoursPerStage <= 0 {
		t.Error("resource/rollout budgets must be positive")
	}
	if b.MaxStageErrorRatePct <= 0 || b.MaxStageErrorRatePct >= b.MaxStageFallbackPct {
		t.Error("error-rate trigger must be positive and stricter than the fallback trigger")
	}
}

// Every Budgets field a fixture asserts must exist.
func TestFixtureBudgetsExist(t *testing.T) {
	rt := reflect.TypeOf(Budgets{})
	for _, f := range Fixtures {
		if f.Budget == "" {
			continue
		}
		for _, name := range strings.Split(f.Budget, ",") {
			if _, ok := rt.FieldByName(name); !ok {
				t.Errorf("%s asserts unknown budget %q", f.ID, name)
			}
		}
	}
}

func TestStagesShadowIsAgentOnlyAndNonAuthoritative(t *testing.T) {
	for _, s := range Stages {
		if s.ID == "S1" {
			if s.Authoritative || len(s.Kinds) != 1 || s.Kinds[0] != "agent" {
				t.Errorf("shadow must be observe-only and agent-only (no dual ownership of a PTY master): %+v", s)
			}
		}
	}
}

// The spec must mention every ID and budget so prose and data cannot drift.
func TestSpecMentionsEverything(t *testing.T) {
	spec := read(t, specPath)
	var ids []string
	for _, p := range Paths {
		ids = append(ids, p.ID)
	}
	for _, g := range Gaps {
		ids = append(ids, g.ID)
	}
	for _, f := range Fixtures {
		ids = append(ids, f.ID)
	}
	for _, s := range Stages {
		ids = append(ids, s.ID)
	}
	for _, id := range ids {
		if !strings.Contains(spec, "`"+id+"`") {
			t.Errorf("spec does not mention `%s`", id)
		}
	}
	rt := reflect.TypeOf(Budgets{})
	for i := 0; i < rt.NumField(); i++ {
		if n := rt.Field(i).Name; !strings.Contains(spec, "`"+n+"`") {
			t.Errorf("spec does not mention budget `%s`", n)
		}
	}
	if len(SecretDetectionLimits) < 3 {
		t.Error("secret-detection limits must be documented")
	}
}
