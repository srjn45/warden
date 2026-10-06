package knownprompts

import (
	"context"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"
)

const agyPane = `
Requesting permission for:
   %s

Run this command?
> 1. Yes, run command
  2. Yes, and always allow in this conversation for commands that start with '%s'
  3. Yes, and always allow for commands that start with '%s' (Persist to settings.json)
  4. No, cancel

  ↑/↓ Navigate · tab Amend · ctrl+g edit/expand command
`

func agyScreen(cmd, prefix string) string {
	return sprintf(agyPane, cmd, prefix, prefix)
}

func sprintf(f string, a ...string) string {
	for _, s := range a {
		f = strings.Replace(f, "%s", s, 1)
	}
	return f
}

func agyReading() Reading {
	return Reading{
		Question: "Run this command?",
		Action:   "git rev-parse HEAD origin/main",
		Options: []string{
			"Yes, run command",
			"Yes, and always allow in this conversation for commands that start with 'git rev-parse'",
			"Yes, and always allow for commands that start with 'git rev-parse' (Persist to settings.json)",
			"No, cancel",
		},
		Affirmative: 1,
		Sticky:      []bool{false, true, true, false},
	}
}

func newStore(t *testing.T) *Store {
	t.Helper()
	s, err := New(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = s.Close() })
	return s
}

func TestTemplate(t *testing.T) {
	tests := []struct{ name, in, action, want string }{
		{"single quotes", "start with 'git rev-parse'", "", "start with '{*}'"},
		{"backticks", "run `curl -sI` now", "", "run `{*}` now"},
		{"double quotes", `allow "npm test" again`, "", `allow "{*}" again`},
		{"apostrophe is not a quote", "Yes, and don't ask again", "", "Yes, and don't ask again"},
		{"apostrophe then quote", "Don't ask again for 'make'", "", "Don't ask again for '{*}'"},
		{"action", "Allow git status here?", "git status", "Allow {*} here?"},
		{"action not inside a word", "Always allow ls", "ls", "Always allow ls"},
		{"action repeated", "run foo; run foo", "foo", "run {*}; run {*}"},
		{"action word boundary", "foobar and foo", "foo", "foobar and {*}"},
		{"unclosed quote", "it's 'open", "", "it's 'open"},
		{"nothing variable", "No, cancel", "", "No, cancel"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := Template(tt.in, tt.action)
			if got != tt.want {
				t.Fatalf("Template(%q,%q) = %q, want %q", tt.in, tt.action, got, tt.want)
			}
			if again := Template(got, tt.action); again != got {
				t.Fatalf("not idempotent: %q -> %q", got, again)
			}
		})
	}
}

func TestTemplateRoundTrip(t *testing.T) {
	for _, concrete := range []string{
		"Yes, and always allow for commands that start with 'git rev-parse' (Persist to settings.json)",
		"Run `curl -sI example.com` ?",
	} {
		tmpl := Template(concrete, "")
		re, err := compile(tmpl)
		if err != nil {
			t.Fatal(err)
		}
		if !re.MatchString(concrete) {
			t.Fatalf("template %q does not match its source %q", tmpl, concrete)
		}
		if re.MatchString("something else entirely") {
			t.Fatalf("template %q matches unrelated text", tmpl)
		}
	}
}

func TestLearnAndMatchAgy(t *testing.T) {
	s := newStore(t)
	ctx := context.Background()
	e, created, err := s.Learn(ctx, "antigravity", "1.2.0", agyReading())
	if err != nil || !created {
		t.Fatalf("learn: created=%v err=%v", created, err)
	}
	wantOpts := []string{
		"Yes, run command",
		"Yes, and always allow in this conversation for commands that start with '{*}'",
		"Yes, and always allow for commands that start with '{*}' (Persist to settings.json)",
		"No, cancel",
	}
	if !reflect.DeepEqual(e.Options, wantOpts) {
		t.Fatalf("templates = %q", e.Options)
	}

	// The same menu with a different command matches, and the labels returned
	// are the concrete ones on screen.
	pane := agyScreen("go test ./...", "go test")
	m, ok := s.Match("antigravity", pane)
	if !ok {
		t.Fatal("same shape with another command did not match")
	}
	if m.Options[1] != "Yes, and always allow in this conversation for commands that start with 'go test'" {
		t.Fatalf("labels are not the concrete ones: %q", m.Options)
	}
	if m.Location.Selected != 1 || m.Entry.Affirmative != 1 || !m.Entry.Sticky[1] {
		t.Fatalf("match = %+v", m)
	}
	// And the original command still matches.
	if _, ok := s.Match("antigravity", agyScreen("git rev-parse HEAD", "git rev-parse")); !ok {
		t.Fatal("original did not match")
	}
}

func TestMatchRejections(t *testing.T) {
	s := newStore(t)
	if _, _, err := s.Learn(context.Background(), "antigravity", "", agyReading()); err != nil {
		t.Fatal(err)
	}
	opts := []string{
		"  1. Yes, run command",
		"  2. Yes, and always allow in this conversation for commands that start with 'x'",
		"  3. Yes, and always allow for commands that start with 'x' (Persist to settings.json)",
		"  4. No, cancel",
	}
	pane := func(lines ...string) string { return "Run this command?\n" + strings.Join(lines, "\n") + "\n" }
	tests := []struct {
		name, backend, pane string
	}{
		{"reordered", "antigravity", pane(opts[1], opts[0], opts[2], opts[3])},
		{"missing", "antigravity", pane(opts[0], opts[1], opts[3])},
		{"extra", "antigravity", pane(opts[0], opts[1], "  3. Yes, maybe", opts[2], opts[3])},
		{"other backend", "claude", pane(opts...)},
		{"no menu", "antigravity", "just some output\nmore output\n"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if m, ok := s.Match(tt.backend, tt.pane); ok {
				t.Fatalf("unexpected match: %+v", m)
			}
		})
	}
	if _, ok := s.Match("antigravity", pane(opts...)); !ok {
		t.Fatal("control pane should match")
	}
}

func TestOneRecordPerShape(t *testing.T) {
	s := newStore(t)
	ctx := context.Background()
	a, created, _ := s.Learn(ctx, "antigravity", "", agyReading())
	r2 := agyReading()
	r2.Action = "ls -la"
	r2.Options[1] = "Yes, and always allow in this conversation for commands that start with 'ls'"
	r2.Options[2] = "Yes, and always allow for commands that start with 'ls' (Persist to settings.json)"
	b, created2, err := s.Learn(ctx, "antigravity", "1.3.0", r2)
	if err != nil || !created || created2 || a.ID != b.ID {
		t.Fatalf("same shape must be one record: %v %v %v %q %q", created, created2, err, a.ID, b.ID)
	}
	if b.CLIVersion != "1.3.0" || len(s.List()) != 1 {
		t.Fatalf("entry = %+v, n=%d", b, len(s.List()))
	}
	if _, c, _ := s.Learn(ctx, "claude", "", agyReading()); !c || len(s.List()) != 2 {
		t.Fatal("same shape under another backend must be its own record")
	}
}

func TestHitDeleteAndReload(t *testing.T) {
	dir := t.TempDir()
	s, err := New(dir)
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	e, _, _ := s.Learn(ctx, "antigravity", "", agyReading())
	if err := s.Hit(ctx, e.ID); err != nil {
		t.Fatal(err)
	}
	if err := s.Hit(ctx, "nope"); err != ErrNotFound {
		t.Fatalf("hit missing = %v", err)
	}
	_ = s.Close()

	s, err = New(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	got := s.List()
	if len(got) != 1 || got[0].Hits != 1 {
		t.Fatalf("reload = %+v", got)
	}
	if _, ok := s.Match("antigravity", agyScreen("x y", "x y")); !ok {
		t.Fatal("index not rebuilt at open")
	}
	if err := s.Delete(ctx, e.ID); err != nil {
		t.Fatal(err)
	}
	if _, ok := s.Match("antigravity", agyScreen("x y", "x y")); ok || len(s.List()) != 0 {
		t.Fatal("delete did not clear the index")
	}
}

func TestLearnRejects(t *testing.T) {
	s := newStore(t)
	ctx := context.Background()
	bad := map[string]Reading{
		"one option":      {Options: []string{"Yes"}},
		"bad kind":        {Options: []string{"Yes", "No"}, Kind: "weird"},
		"bad affirm":      {Options: []string{"Yes", "No"}, Affirmative: 3},
		"bad sticky":      {Options: []string{"Yes", "No"}, Sticky: []bool{true}},
		"all-variable":    {Options: []string{"'a b'", "No"}},
		"action as label": {Action: "git status", Options: []string{"git status", "No"}},
	}
	for name, r := range bad {
		if _, _, err := s.Learn(ctx, "antigravity", "", r); err == nil {
			t.Errorf("%s: want error", name)
		}
	}
	if _, _, err := s.Learn(ctx, "", "", agyReading()); err == nil {
		t.Error("empty backend: want error")
	}
}

func TestOnlyShapeIsPersisted(t *testing.T) {
	dir := t.TempDir()
	s, err := New(dir)
	if err != nil {
		t.Fatal(err)
	}
	r := agyReading()
	r.Action = "curl -sI https://secret.example/token"
	if _, _, err := s.Learn(context.Background(), "antigravity", "", r); err != nil {
		t.Fatal(err)
	}
	_ = s.Close()
	var all []byte
	_ = filepath.Walk(dir, func(p string, info os.FileInfo, err error) error {
		if err == nil && !info.IsDir() {
			b, _ := os.ReadFile(p)
			all = append(all, b...)
		}
		return nil
	})
	for _, leak := range []string{"git rev-parse", "secret.example", "curl"} {
		if strings.Contains(string(all), leak) {
			t.Fatalf("concrete text %q was persisted", leak)
		}
	}
	if !strings.Contains(string(all), "Run this command?") {
		t.Fatal("question template should be persisted")
	}
}

func TestMatchRejectsTrailingExtraOption(t *testing.T) {
	s := newStore(t)
	_, _, _ = s.Learn(context.Background(), "antigravity", "", agyReading())
	pane := agyScreen("a b", "a b")
	pane = strings.Replace(pane, "  4. No, cancel\n", "  4. No, cancel\n  5. Something else\n", 1)
	if m, ok := s.Match("antigravity", pane); ok {
		t.Fatalf("unexpected match: %+v", m)
	}
}

func TestStoreEvictsLeastRecentlySeen(t *testing.T) {
	s, err := New(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	clock := time.Unix(1_000_000, 0)
	s.now = func() time.Time { return clock }
	s.SetLimits(2, 0)

	learn := func(label string) string {
		clock = clock.Add(time.Minute)
		e, _, err := s.Learn(context.Background(), "agy", "", Reading{
			Question: "Q?", Options: []string{label + " yes", label + " no"}, Affirmative: 1,
		})
		if err != nil {
			t.Fatal(err)
		}
		return e.ID
	}
	a, b := learn("alpha"), learn("beta")
	clock = clock.Add(time.Minute)
	if err := s.Hit(context.Background(), a); err != nil { // a is now the most recently seen
		t.Fatal(err)
	}
	learn("gamma")
	ids := map[string]bool{}
	for _, e := range s.List() {
		ids[e.ID] = true
	}
	if len(ids) != 2 || !ids[a] || ids[b] {
		t.Fatalf("want a kept and b evicted, got %v", ids)
	}
}

func TestStorePrunesStaleEntries(t *testing.T) {
	dir := t.TempDir()
	s, err := New(dir)
	if err != nil {
		t.Fatal(err)
	}
	clock := time.Unix(1_000_000, 0)
	s.now = func() time.Time { return clock }
	if _, _, err := s.Learn(context.Background(), "agy", "", Reading{
		Question: "Q?", Options: []string{"old yes", "old no"}, Affirmative: 1,
	}); err != nil {
		t.Fatal(err)
	}
	clock = clock.Add(30 * 24 * time.Hour)
	if _, _, err := s.Learn(context.Background(), "agy", "", Reading{
		Question: "Q?", Options: []string{"new yes", "new no"}, Affirmative: 1,
	}); err != nil {
		t.Fatal(err)
	}
	s.SetLimits(0, 20*24*time.Hour)
	if n := s.Prune(); n != 1 || len(s.List()) != 1 || s.List()[0].Options[0] != "new yes" {
		t.Fatalf("prune removed %d, left %v", n, s.List())
	}
	_ = s.Close()
	s2, err := New(dir) // the deletion is durable
	if err != nil {
		t.Fatal(err)
	}
	defer s2.Close()
	if len(s2.List()) != 1 {
		t.Fatalf("reopened with %d entries", len(s2.List()))
	}
}

func TestDeleteAll(t *testing.T) {
	s, err := New(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	ctx := context.Background()
	for _, q := range []string{"First question?", "Second question?"} {
		if _, _, err := s.Learn(ctx, "claude", "", Reading{Question: q, Options: []string{"Yes", "No"}, Affirmative: 1}); err != nil {
			t.Fatal(err)
		}
	}
	n, err := s.DeleteAll(ctx)
	if err != nil || n != 2 || len(s.List()) != 0 {
		t.Fatalf("DeleteAll = %d, %v; left %d", n, err, len(s.List()))
	}
	if n, err := s.DeleteAll(ctx); err != nil || n != 0 {
		t.Fatalf("second DeleteAll = %d, %v", n, err)
	}
}
