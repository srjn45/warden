package digest

import (
	"reflect"
	"testing"
)

func TestParseNumstat(t *testing.T) {
	in := "3\t1\ta.go\n0\t5\tb.go\n-\t-\timg.png\nbogus line\n"
	got := ParseNumstat(in)
	want := map[string]LineDelta{
		"a.go":    {Added: 3, Removed: 1},
		"b.go":    {Added: 0, Removed: 5},
		"img.png": {Added: 0, Removed: 0}, // binary file: "-" -> 0, still present
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("ParseNumstat = %v, want %v", got, want)
	}
}

func TestMergeFilesUnion(t *testing.T) {
	// a.go edited+changed; reverted.go edited but reverted (no numstat row);
	// sideeffect.go changed by git only (e.g. a formatter), never an edit target.
	edited := []string{"a.go", "reverted.go"}
	stats := map[string]LineDelta{
		"a.go":          {Added: 3, Removed: 1},
		"sideeffect.go": {Added: 2, Removed: 0},
	}
	got := MergeFiles(edited, stats)
	want := []FileChange{
		{Path: "a.go", Added: 3, Removed: 1, Edited: true},
		{Path: "reverted.go", Added: 0, Removed: 0, Edited: true},
		{Path: "sideeffect.go", Added: 2, Removed: 0, Edited: false},
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("MergeFiles = %+v, want %+v", got, want)
	}
}

func TestMergeFilesNoGit(t *testing.T) {
	got := MergeFiles([]string{"x.go", "y.go"}, nil)
	want := []FileChange{
		{Path: "x.go", Edited: true},
		{Path: "y.go", Edited: true},
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("MergeFiles(no git) = %+v, want %+v", got, want)
	}
}
