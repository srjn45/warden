package dbtx

import (
	"go/ast"
	"go/parser"
	"go/token"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestImportFence (audit G13): no package other than the boundary may use the
// engine's cross-collection transaction API.
func TestImportFence(t *testing.T) {
	root, err := filepath.Abs("../..")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(root, "go.mod")); err != nil {
		t.Fatal(err)
	}
	banned := map[string]bool{"CommitXTx": true, "XTxStatus": true, "XTxOp": true, "XTxOpResult": true, "XTxResult": true}
	self := filepath.Join(root, "internal", "dbtx")
	fset := token.NewFileSet()
	var offenders []string
	_ = filepath.WalkDir(root, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return nil
		}
		if d.IsDir() {
			switch d.Name() {
			case ".git", ".worktrees", "node_modules", "vendor", "testdata":
				return fs.SkipDir
			}
			if p == self {
				return fs.SkipDir
			}
			return nil
		}
		if !strings.HasSuffix(p, ".go") {
			return nil
		}
		f, perr := parser.ParseFile(fset, p, nil, 0)
		if perr != nil {
			return nil
		}
		ast.Inspect(f, func(n ast.Node) bool {
			if sel, ok := n.(*ast.SelectorExpr); ok && banned[sel.Sel.Name] {
				offenders = append(offenders, p+": "+sel.Sel.Name)
			}
			return true
		})
		return nil
	})
	if len(offenders) > 0 {
		t.Fatalf("engine XTx API used outside internal/dbtx:\n%s", strings.Join(offenders, "\n"))
	}
}
