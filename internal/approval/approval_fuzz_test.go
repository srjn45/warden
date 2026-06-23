package approval

import "testing"

// FuzzParse exercises the tmux-pane permission-prompt detector against
// arbitrary terminal content. Parse runs over every captured pane on the
// poller's hot path, so the contract under fuzzing is: never panic on any
// byte sequence, and when it claims a confident match (ok=true) the returned
// Approval must be internally consistent — at least two options, and every
// index it reports must fall within those options.
func FuzzParse(f *testing.F) {
	seeds := []string{
		"",
		"Do you want to proceed?\n❯ 1. Yes\n  2. No",
		"Bash(rm -rf node_modules)\nDo you want to proceed?\n  1. Yes\n  2. No, and don't ask again\n  3. Cancel",
		"│ 1. Allow │\n│ 2. Deny │",
		"just some agent prose with a 1. numbered 2. list inline",
		"1. only one option",
		"❯ 1. Yes\n  3. No", // non-sequential numbering
		"────────\n  1. Approve\n  2. Reject",
	}
	for _, s := range seeds {
		f.Add(s)
	}

	f.Fuzz(func(t *testing.T, pane string) {
		a, ok := Parse(pane)

		// Parse is a pure function; a second call must agree with the first.
		a2, ok2 := Parse(pane)
		if ok != ok2 {
			t.Fatalf("Parse not deterministic on ok: %v vs %v", ok, ok2)
		}

		if !ok {
			return
		}
		if len(a.Options) < 2 {
			t.Fatalf("ok match with <2 options: %+v", a)
		}
		if a.SelectedIdx < 0 || a.SelectedIdx > len(a.Options) {
			t.Fatalf("SelectedIdx %d out of range for %d options", a.SelectedIdx, len(a.Options))
		}
		if a.AffirmativeIdx < 0 || a.AffirmativeIdx > len(a.Options) {
			t.Fatalf("AffirmativeIdx %d out of range for %d options", a.AffirmativeIdx, len(a.Options))
		}
		if a2.SelectedIdx != a.SelectedIdx || len(a2.Options) != len(a.Options) {
			t.Fatalf("Parse not deterministic on payload: %+v vs %+v", a, a2)
		}
	})
}
