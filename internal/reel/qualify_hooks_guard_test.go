package reel

import (
	"go/ast"
	"go/parser"
	"go/token"
	"io/fs"
	"path/filepath"
	"strings"
	"testing"
)

// qualifyAssignments returns the positions in one parsed file that set a
// field named Qualify: a composite-literal key (Config{Qualify: ...}) or an
// assignment to a selector (cfg.Qualify = ...).
func qualifyAssignments(fset *token.FileSet, file *ast.File) []string {
	var found []string
	ast.Inspect(file, func(n ast.Node) bool {
		switch n := n.(type) {
		case *ast.KeyValueExpr:
			if id, ok := n.Key.(*ast.Ident); ok && id.Name == "Qualify" {
				found = append(found, fset.Position(n.Pos()).String())
			}
		case *ast.AssignStmt:
			for _, lhs := range n.Lhs {
				if sel, ok := lhs.(*ast.SelectorExpr); ok && sel.Sel.Name == "Qualify" {
					found = append(found, fset.Position(lhs.Pos()).String())
				}
			}
		}
		return true
	})
	return found
}

// TestNoProductionCodeSetsQualifyHooks proves visualcapture.Config.Qualify
// stays nil in production (V8 decision 3): no non-test Go source of the module,
// whatever its build tags, sets a Qualify field.
func TestNoProductionCodeSetsQualifyHooks(t *testing.T) {
	// The detector itself must find both forms.
	probe := `package p
func f() { c := Config{Qualify: &QualifyHooks{}}; c.Qualify = nil; _ = c.Qualify }`
	fset := token.NewFileSet()
	file, err := parser.ParseFile(fset, "probe.go", probe, 0)
	if err != nil {
		t.Fatal(err)
	}
	if got := qualifyAssignments(fset, file); len(got) != 2 {
		t.Fatalf("detector found %v in the probe, want the literal key and the assignment only", got)
	}

	root, err := filepath.Abs(filepath.Join("..", ".."))
	if err != nil {
		t.Fatal(err)
	}
	var scanned int
	var offenders []string
	for _, top := range []string{"cmd", "internal"} {
		err := filepath.WalkDir(filepath.Join(root, top), func(path string, d fs.DirEntry, err error) error {
			if err != nil {
				return err
			}
			if d.IsDir() || !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") {
				return nil
			}
			file, err := parser.ParseFile(fset, path, nil, parser.SkipObjectResolution)
			if err != nil {
				return err
			}
			scanned++
			offenders = append(offenders, qualifyAssignments(fset, file)...)
			return nil
		})
		if err != nil {
			t.Fatal(err)
		}
	}
	if scanned < 50 {
		t.Fatalf("scanned only %d non-test Go files under %s", scanned, root)
	}
	if len(offenders) > 0 {
		t.Fatalf("non-test code sets a Qualify field (qualification hooks must stay nil in production):\n%s", strings.Join(offenders, "\n"))
	}
}
