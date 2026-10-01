package planexport

import (
	"path/filepath"
	"strings"

	"github.com/srjn45/warden/internal/planstore"
)

// ExportPath returns the conventional YAML replica path
// plans/{lifecycle}/<slug>.yaml. Prefer ExportPathFormat when the caller
// knows the render Format.
//
// The lifecycle segment mirrors Plan.Status for human layout only — importers
// and runtime must not treat the path component as lifecycle authority
// (docs/specs/2026-09-30-scrivadb-canonical-plans.md D3 / §8.2).
func ExportPath(lifecycle planstore.PlanStatus, name string) string {
	return ExportPathFormat(lifecycle, name, FormatYAML)
}

// ExportPathFormat returns plans/{lifecycle}/<slug>.{yaml|json} for the given
// Format. Unknown formats fall back to .yaml so Default() callers stay safe.
func ExportPathFormat(lifecycle planstore.PlanStatus, name string, format Format) string {
	ext := ".yaml"
	if format == FormatJSON {
		ext = ".json"
	}
	return filepath.ToSlash(filepath.Join("plans", string(lifecycle), slug(name)+ext))
}

func slug(name string) string {
	s := strings.ToLower(strings.ReplaceAll(strings.TrimSpace(name), " ", "-"))
	s = filepath.Base(s)
	if s == "" || s == "." || s == string(filepath.Separator) {
		return "plan"
	}
	return s
}
