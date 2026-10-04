package cli

import (
	"bytes"
	"context"
	"strings"
	"testing"
	"time"

	"github.com/srjn45/warden/internal/fastbrain"
)

type fakeGH struct {
	auth bool
	args []string
}

func (f *fakeGH) Authenticated(context.Context) bool { return f.auth }
func (f *fakeGH) Run(_ context.Context, a ...string) (string, error) {
	f.args = a
	return "https://github.com/srjn45/warden/issues/999", nil
}

func stageFixture(t *testing.T, gh *fakeGH) string {
	t.Helper()
	dir := t.TempDir()
	d := &fastbrain.IssueDraft{
		ID: "ag1", CreatedAt: time.Now(), Title: "crash(planstore): nil pointer dereference",
		Environment: "Warden v9.10.1 (linux/amd64)", Status: "staged",
		Stack: "panic: boom key=sk-abcdefghijklmnopqrstuvwx token=ghp_abcdefghijklmnopqrstuvwxyz0123456789 at /home/alice/x.go:1",
	}
	if _, err := fastbrain.StageCrashDraft(dir, d); err != nil {
		t.Fatal(err)
	}
	bugReportDir = func() (string, error) { return dir, nil }
	bugReportGH = func() fastbrain.GHRunner { return gh }
	t.Cleanup(func() {
		bugReportDir = fastbrain.DefaultCrashDir
		bugReportGH = fastbrain.DefaultGH
	})
	return dir
}

func runBR(t *testing.T, id, input string) (string, error) {
	var out bytes.Buffer
	err := runBugReport(context.Background(), strings.NewReader(input), &out, id, false)
	return out.String(), err
}

func TestBugReportPreviewAndDecline(t *testing.T) {
	gh := &fakeGH{auth: true}
	stageFixture(t, gh)
	for _, in := range []string{"n\n", "\n", ""} {
		out, err := runBR(t, "ag1", in)
		if err != nil {
			t.Fatal(err)
		}
		if gh.args != nil {
			t.Fatal("gh invoked without approval")
		}
		for _, want := range []string{"nil pointer dereference", "Warden v9.10.1", "[y/N]", "Nothing was submitted"} {
			if !strings.Contains(out, want) {
				t.Errorf("missing %q in %q", want, out)
			}
		}
		for _, secret := range []string{"sk-abcdefgh", "ghp_abcdefgh", "/home/alice"} {
			if strings.Contains(out, secret) {
				t.Errorf("secret %q leaked", secret)
			}
		}
	}
}

func TestBugReportSubmitViaGH(t *testing.T) {
	gh := &fakeGH{auth: true}
	stageFixture(t, gh)
	out, err := runBR(t, "ag1", "y\n")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, "issues/999") || len(gh.args) < 4 || gh.args[0] != "issue" {
		t.Fatalf("out=%q args=%v", out, gh.args)
	}
}

func TestBugReportSubmitWithoutGH(t *testing.T) {
	gh := &fakeGH{}
	stageFixture(t, gh)
	out, err := runBR(t, "ag1", "y\n")
	if err != nil {
		t.Fatal(err)
	}
	if gh.args != nil || !strings.Contains(out, "https://github.com/srjn45/warden/issues/new?") {
		t.Fatalf("out=%q", out)
	}
}

func TestBugReportMissingIDAndList(t *testing.T) {
	stageFixture(t, &fakeGH{})
	if _, err := runBR(t, "nope", "y\n"); err == nil {
		t.Fatal("want error")
	}
	out, err := runBR(t, "", "")
	if err != nil || !strings.Contains(out, "ag1") {
		t.Fatalf("list: %q %v", out, err)
	}
}
