package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/srjn45/warden/internal/release"
)

type gitCall []string

func stubRelease(t *testing.T, adv release.Advice, existingTag string) *[]gitCall {
	t.Helper()
	var calls []gitCall
	oa, og := releaseAnalyze, releaseGit
	t.Cleanup(func() { releaseAnalyze, releaseGit = oa, og })
	releaseAnalyze = func(context.Context, string) (release.Advice, error) { return adv, nil }
	releaseGit = func(_ context.Context, _ string, args ...string) (string, error) {
		calls = append(calls, gitCall(args))
		if len(args) >= 3 && args[0] == "tag" && args[1] == "-l" {
			return existingTag, nil
		}
		return "", nil
	}
	return &calls
}

func minorAdvice() release.Advice {
	return release.Advice{
		LatestTag: "v1.2.3", Current: release.Version{Major: 1, Minor: 2, Patch: 3},
		Next: release.Version{Major: 1, Minor: 3}, Bump: release.BumpMinor,
		Changelog: release.Changelog{Features: []string{"add thing"}},
	}
}

func run(t *testing.T, stdin string, yes, push, dry, js bool) (string, error) {
	t.Helper()
	var out bytes.Buffer
	err := runRelease(context.Background(), strings.NewReader(stdin), &out, ".", yes, push, dry, js)
	return out.String(), err
}

func has(calls []gitCall, verb string) bool {
	for _, c := range calls {
		if len(c) > 0 && c[0] == verb {
			return true
		}
	}
	return false
}

func TestReleaseDryRun(t *testing.T) {
	calls := stubRelease(t, minorAdvice(), "")
	out, err := run(t, "y\n", true, true, true, false)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, "v1.3.0") || !strings.Contains(out, "minor") || !strings.Contains(out, "add thing") {
		t.Fatalf("output: %s", out)
	}
	if has(*calls, "tag") || has(*calls, "push") {
		t.Fatalf("dry-run touched git: %v", *calls)
	}
}

func TestReleaseDefaultDoesNotTag(t *testing.T) {
	for _, in := range []string{"", "n\n"} {
		calls := stubRelease(t, minorAdvice(), "")
		if _, err := run(t, in, false, true, false, false); err != nil {
			t.Fatal(err)
		}
		for _, c := range *calls {
			if c[0] == "push" || (c[0] == "tag" && c[1] == "-a") {
				t.Fatalf("stdin %q created tag/push: %v", in, *calls)
			}
		}
	}
}

func TestReleaseYesTagsOnly(t *testing.T) {
	calls := stubRelease(t, minorAdvice(), "")
	if _, err := run(t, "", true, false, false, false); err != nil {
		t.Fatal(err)
	}
	var tagged bool
	for _, c := range *calls {
		if c[0] == "tag" && c[1] == "-a" {
			tagged = true
			if c[2] != "v1.3.0" || !strings.Contains(c[4], "add thing") {
				t.Fatalf("bad tag call %v", c)
			}
		}
	}
	if !tagged || has(*calls, "push") {
		t.Fatalf("calls: %v", *calls)
	}
}

func TestReleaseYesPush(t *testing.T) {
	calls := stubRelease(t, minorAdvice(), "")
	if _, err := run(t, "", true, true, false, false); err != nil {
		t.Fatal(err)
	}
	last := (*calls)[len(*calls)-1]
	if strings.Join(last, " ") != "push origin v1.3.0" {
		t.Fatalf("calls: %v", *calls)
	}
}

func TestReleaseInteractivePushNeedsConfirm(t *testing.T) {
	calls := stubRelease(t, minorAdvice(), "")
	if _, err := run(t, "y\nn\n", false, true, false, false); err != nil {
		t.Fatal(err)
	}
	if has(*calls, "push") || !has(*calls, "tag") {
		t.Fatalf("calls: %v", *calls)
	}
	calls = stubRelease(t, minorAdvice(), "")
	if _, err := run(t, "y\ny\n", false, true, false, false); err != nil {
		t.Fatal(err)
	}
	if !has(*calls, "push") {
		t.Fatalf("calls: %v", *calls)
	}
}

func TestReleaseExistingTagRefused(t *testing.T) {
	calls := stubRelease(t, minorAdvice(), "v1.3.0")
	_, err := run(t, "", true, true, false, false)
	if err == nil || !strings.Contains(err.Error(), "already exists") {
		t.Fatalf("err = %v", err)
	}
	for _, c := range *calls {
		if c[0] == "push" || (c[0] == "tag" && c[1] == "-a") {
			t.Fatalf("created despite existing: %v", *calls)
		}
	}
}

func TestReleaseBumpNone(t *testing.T) {
	adv := minorAdvice()
	adv.Bump = release.BumpNone
	calls := stubRelease(t, adv, "")
	out, err := run(t, "y\n", true, true, false, false)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, "nothing to release") || len(*calls) != 0 {
		t.Fatalf("out=%s calls=%v", out, *calls)
	}
}

func TestReleaseJSON(t *testing.T) {
	stubRelease(t, minorAdvice(), "")
	out, err := run(t, "", true, true, false, true)
	if err != nil {
		t.Fatal(err)
	}
	var got releaseResult
	if err := json.Unmarshal([]byte(out), &got); err != nil {
		t.Fatalf("%v: %s", err, out)
	}
	if got.Next != "v1.3.0" || !got.Tagged || !got.Pushed {
		t.Fatalf("%+v", got)
	}
}
