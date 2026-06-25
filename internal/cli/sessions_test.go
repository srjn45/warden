package cli

import (
	"bytes"
	"encoding/json"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/srjn45/warden/internal/lifecycle"
	"github.com/srjn45/warden/internal/store"
)

func TestPrintJSON_EmptySlice(t *testing.T) {
	var buf bytes.Buffer
	if err := printJSON(&buf, []store.Session{}); err != nil {
		t.Fatalf("printJSON returned error: %v", err)
	}
	got := strings.TrimSpace(buf.String())
	if got != "[]" {
		t.Fatalf("empty slice: want %q, got %q", "[]", got)
	}
}

func TestPrintJSON_SessionHasFields(t *testing.T) {
	var buf bytes.Buffer
	s := store.Session{ID: "agent-x1", Status: store.Status("working")}
	if err := printJSON(&buf, s); err != nil {
		t.Fatalf("printJSON returned error: %v", err)
	}
	var round map[string]any
	if err := json.Unmarshal(buf.Bytes(), &round); err != nil {
		t.Fatalf("output is not valid JSON: %v\n%s", err, buf.String())
	}
	if round["id"] != "agent-x1" {
		t.Fatalf("want id=agent-x1, got %v", round["id"])
	}
	if !strings.Contains(buf.String(), "\n  \"id\"") {
		t.Fatalf("expected 2-space indented output, got:\n%s", buf.String())
	}
}

func TestRenderSessions(t *testing.T) {
	var buf bytes.Buffer
	sessions := []*store.Session{
		{ID: "A-1", Name: "alpha", Type: store.Type("development"), Status: store.Status("working"), Subject: "do a thing"},
		{ID: "B-2"}, // sparse session exercises the em-dash fallbacks
	}
	if err := renderSessions(&buf, sessions, false); err != nil {
		t.Fatalf("renderSessions returned error: %v", err)
	}
	out := buf.String()
	for _, want := range []string{"NAME", "ID", "SUBJECT", "alpha", "A-1", "B-2", "do a thing"} {
		if !strings.Contains(out, want) {
			t.Errorf("output missing %q:\n%s", want, out)
		}
	}
	// The unnamed B-2 row falls back to "—" for name and "…" for a pending type.
	if !strings.Contains(out, "—") || !strings.Contains(out, "…") {
		t.Errorf("expected fallback glyphs for sparse row:\n%s", out)
	}
}

func TestContextCell(t *testing.T) {
	cases := []struct {
		tokens int
		state  string
		want   string
	}{
		{0, "", "—"},
		{145000, "ok", "145k"},
		{210000, "warning", "210k"},
		{410000, "critical", "410k"},
	}
	for _, c := range cases {
		if got := contextCell(c.tokens, c.state, false); got != c.want {
			t.Errorf("contextCell(%d,%q)=%q, want %q", c.tokens, c.state, got, c.want)
		}
	}
}

func TestContextCellColor(t *testing.T) {
	// With color on, each state tints the figure and resets it.
	cases := []struct {
		state string
		code  string
	}{
		{"ok", "\033[32m"}, // green default
		{store.ContextWarning, "\033[33m"},
		{store.ContextCritical, "\033[31m"},
	}
	for _, c := range cases {
		got := contextCell(100000, c.state, true)
		if !strings.HasPrefix(got, c.code) || !strings.HasSuffix(got, "\033[0m") {
			t.Errorf("contextCell(state=%q) = %q, want prefix %q and reset", c.state, got, c.code)
		}
	}
	// An unknown gauge stays "—" even with color on.
	if got := contextCell(0, "", true); got != "—" {
		t.Errorf("contextCell(0,\"\",true) = %q, want —", got)
	}
}

func TestStatusCell(t *testing.T) {
	// Without color the raw string passes through unchanged.
	if got := statusCell(store.StatusWorking, false); got != string(store.StatusWorking) {
		t.Errorf("statusCell no-color = %q, want %q", got, store.StatusWorking)
	}
	cases := map[store.Status]string{
		store.StatusDone:        "\033[32m",
		store.StatusWorking:     "\033[34m",
		store.StatusErrored:     "\033[31m",
		store.StatusRateLimited: "\033[33m",
	}
	for st, code := range cases {
		got := statusCell(st, true)
		if !strings.HasPrefix(got, code) || !strings.Contains(got, string(st)) {
			t.Errorf("statusCell(%q) = %q, want prefix %q", st, got, code)
		}
	}
	// An unrecognized status is returned verbatim even with color on.
	if got := statusCell(store.Status("weird"), true); got != "weird" {
		t.Errorf("statusCell(weird,true) = %q, want plain", got)
	}
}

func TestTypeOrPending(t *testing.T) {
	if got := typeOrPending(""); got != "…" {
		t.Errorf("empty type = %q, want …", got)
	}
	if got := typeOrPending(store.Type("development")); got != "development" {
		t.Errorf("type = %q, want development", got)
	}
}

func TestDirName(t *testing.T) {
	if got := dirName(""); got != "—" {
		t.Errorf("empty workdir = %q, want —", got)
	}
	if got := dirName("/home/u/proj/repo"); got != "repo" {
		t.Errorf("dirName = %q, want repo", got)
	}
}

func TestAge(t *testing.T) {
	if got := age(time.Time{}); got != "—" {
		t.Errorf("zero time = %q, want —", got)
	}
	if got := age(time.Now()); got != "<1m" {
		t.Errorf("just now = %q, want <1m", got)
	}
	if got := age(time.Now().Add(-90 * time.Minute)); !strings.Contains(got, "h") {
		t.Errorf("90m ago = %q, want an hours component", got)
	}
}

func TestModelCell(t *testing.T) {
	cases := map[string]string{
		"":                  "sonnet", // empty defaults to sonnet
		"claude-opus-4-8":   "opus",
		"claude-sonnet-4-6": "sonnet",
		"claude-haiku-4-5":  "haiku",
		"claude-fable-5":    "fable",
		"some-custom-model": "some-custom-model", // unknown → full id
	}
	for in, want := range cases {
		if got := modelCell(in); got != want {
			t.Errorf("modelCell(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestModelOrDefault(t *testing.T) {
	if got := modelOrDefault(""); got != lifecycle.DefaultModel {
		t.Errorf("empty = %q, want %q", got, lifecycle.DefaultModel)
	}
	if got := modelOrDefault("claude-opus-4-8"); got != "claude-opus-4-8" {
		t.Errorf("explicit model = %q, want it verbatim", got)
	}
}

func TestFormatDuration(t *testing.T) {
	cases := map[time.Duration]string{
		45 * time.Second: "45s",
		90 * time.Second: "1m 30s",
		(time.Hour + 23*time.Minute + 45*time.Second): "1h 23m 45s",
	}
	for in, want := range cases {
		if got := formatDuration(in); got != want {
			t.Errorf("formatDuration(%s) = %q, want %q", in, got, want)
		}
	}
}

func TestFormatRateLimitInfo(t *testing.T) {
	// A non-rate-limited session yields no detail block.
	if got := formatRateLimitInfo(&store.Session{Status: store.StatusWorking}); got != "" {
		t.Errorf("non-rate-limited = %q, want empty", got)
	}

	limitedAt := time.Date(2026, 6, 25, 10, 0, 0, 0, time.UTC)
	restoreAt := time.Now().Add(30 * time.Minute)
	s := &store.Session{
		Status:              store.StatusRateLimited,
		RateLimitedAt:       &limitedAt,
		RateLimitRestoreAt:  &restoreAt,
		RateLimitRetryCount: 3,
	}
	got := formatRateLimitInfo(s)
	for _, want := range []string{"rate limit:", "limited at:", "resume at:", "in ", "retries:    3"} {
		if !strings.Contains(got, want) {
			t.Errorf("rate-limit detail missing %q:\n%s", want, got)
		}
	}

	// A restore time already in the past renders "resuming...".
	past := time.Now().Add(-1 * time.Minute)
	s.RateLimitRestoreAt = &past
	if got := formatRateLimitInfo(s); !strings.Contains(got, "resuming...") {
		t.Errorf("past restore should say resuming...:\n%s", got)
	}
}

// TestLsCmdJSON drives `ls --json` against a stub daemon and asserts the
// session list round-trips through the JSON output path.
func TestLsCmdJSON(t *testing.T) {
	addr := stubDaemon(t, func(w http.ResponseWriter, _ *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{
			"sessions": []map[string]any{{"id": "code-1", "name": "alpha", "status": "working"}},
		})
	})
	out, err := runCLI(t, addr, "ls", "--json")
	if err != nil {
		t.Fatalf("ls --json: %v", err)
	}
	var got []map[string]any
	if err := json.Unmarshal([]byte(out), &got); err != nil {
		t.Fatalf("ls --json output not valid JSON: %v\n%s", err, out)
	}
	if len(got) != 1 || got[0]["id"] != "code-1" {
		t.Fatalf("unexpected ls payload: %s", out)
	}
}

// TestLsCmdTable renders the human table from a stub daemon's session list.
func TestLsCmdTable(t *testing.T) {
	addr := stubDaemon(t, func(w http.ResponseWriter, _ *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{
			"sessions": []map[string]any{{"id": "code-1", "name": "alpha", "type": "development", "subject": "do x"}},
		})
	})
	out, err := runCLI(t, addr, "ls")
	if err != nil {
		t.Fatalf("ls: %v", err)
	}
	for _, want := range []string{"NAME", "code-1", "alpha", "do x"} {
		if !strings.Contains(out, want) {
			t.Errorf("ls table missing %q:\n%s", want, out)
		}
	}
}

// TestLsCmdDaemonError surfaces a daemon HTTP error to the caller.
func TestLsCmdDaemonError(t *testing.T) {
	addr := stubDaemon(t, func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, "boom", http.StatusInternalServerError)
	})
	if _, err := runCLI(t, addr, "ls"); err == nil {
		t.Fatal("expected an error when the daemon returns 500")
	}
}

func TestStatusCmdRequiresArg(t *testing.T) {
	if _, err := runCLI(t, "", "status"); err == nil {
		t.Fatal("status with no ticket should error")
	}
}

// TestStatusCmd renders the detail view, including the rate-limit block.
func TestStatusCmd(t *testing.T) {
	restore := time.Now().Add(20 * time.Minute)
	addr := stubDaemon(t, func(w http.ResponseWriter, _ *http.Request) {
		_ = json.NewEncoder(w).Encode(&store.Session{
			ID:                 "code-7",
			Name:               "beta",
			Status:             store.StatusRateLimited,
			RateLimitRestoreAt: &restore,
		})
	})
	out, err := runCLI(t, addr, "status", "code-7")
	if err != nil {
		t.Fatalf("status: %v", err)
	}
	for _, want := range []string{"id:", "code-7", "beta", "rate limit:", "events:"} {
		if !strings.Contains(out, want) {
			t.Errorf("status output missing %q:\n%s", want, out)
		}
	}
}
