package cli

import (
	"bytes"
	"encoding/json"
	"strings"
	"testing"

	"github.com/srajanpathak/agentctl/internal/digest"
)

func sampleDigest() *digest.Digest {
	return &digest.Digest{
		Summary: "Refactored the parser and added tests.",
		Branch:  "feature/x",
		Turns:   12,
		Status:  "idle",
		Task:    "Refactor parser",
		Files: []digest.FileChange{
			{Path: "parse.go", Added: 40, Removed: 12, Edited: true},
			{Path: "fmt.go", Added: 2, Removed: 0, Edited: false},
		},
	}
}

func TestFormatDigestHuman(t *testing.T) {
	out := formatDigest(sampleDigest())
	for _, want := range []string{
		"Refactored the parser", "feature/x", "idle", "12",
		"parse.go", "+40", "-12", "fmt.go",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("human output missing %q in:\n%s", want, out)
		}
	}
}

func TestFormatDigestNoFiles(t *testing.T) {
	out := formatDigest(&digest.Digest{Summary: "Nothing changed yet.", Status: "working"})
	if !strings.Contains(out, "no files") {
		t.Errorf("want a 'no files' line, got:\n%s", out)
	}
}

func TestDigestJSONRoundTrips(t *testing.T) {
	var buf bytes.Buffer
	if err := printJSON(&buf, sampleDigest()); err != nil {
		t.Fatal(err)
	}
	var back digest.Digest
	if err := json.Unmarshal(buf.Bytes(), &back); err != nil {
		t.Fatalf("not valid JSON: %v\n%s", err, buf.String())
	}
	if back.Branch != "feature/x" || len(back.Files) != 2 {
		t.Errorf("round-trip lost data: %+v", back)
	}
}
