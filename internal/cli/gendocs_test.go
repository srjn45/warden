package cli

import (
	"strings"
	"testing"
)

func TestGenerateReference(t *testing.T) {
	doc, err := GenerateReference()
	if err != nil {
		t.Fatalf("GenerateReference: %v", err)
	}
	if !strings.HasPrefix(doc, "---\ntitle: CLI command reference") {
		t.Errorf("generated reference is missing the expected frontmatter, got prefix:\n%.80q", doc)
	}
	// The ASCII banner in the root command's Long must be stripped, not emitted.
	if strings.Contains(doc, banner) {
		t.Error("generated reference still contains the ASCII banner")
	}
	// A representative sample of commands and nested subcommands must appear as
	// their own sections, proving the walk recurses the whole tree.
	for _, section := range []string{
		"\n## warden\n",
		"\n## warden start\n",
		"\n## warden config\n",
		"\n## warden config init\n", // nested subcommand
		"\n## warden version\n",
	} {
		if !strings.Contains(doc, section) {
			t.Errorf("generated reference is missing section header %q", section)
		}
	}
	// The default help flag must be rendered (it is only wired at Execute time,
	// so the generator has to init it explicitly).
	if !strings.Contains(doc, "-h, --help") {
		t.Error("generated reference is missing the -h/--help flag")
	}
}

func TestGenerateReferenceDeterministic(t *testing.T) {
	first, err := GenerateReference()
	if err != nil {
		t.Fatalf("GenerateReference (first): %v", err)
	}
	second, err := GenerateReference()
	if err != nil {
		t.Fatalf("GenerateReference (second): %v", err)
	}
	if first != second {
		t.Error("GenerateReference is not deterministic across calls")
	}
}
