package sqlite

import (
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

const criteriaPBImport = "github.com/brokenbots/criteria/sdk/pb/criteria/v1"

func TestPackageDoesNotImportCriteriaWireTypes(t *testing.T) {
	root := "." // this package's directory
	err := filepath.Walk(root, func(path string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		if info.IsDir() || !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") {
			return nil
		}
		fset := token.NewFileSet()
		f, err := parser.ParseFile(fset, path, nil, parser.ImportsOnly)
		if err != nil {
			return err
		}
		for _, imp := range f.Imports {
			path := strings.Trim(imp.Path.Value, `"`)
			if path == criteriaPBImport {
				t.Errorf("%s imports forbidden Criteria protobuf package %q", fset.Position(imp.Pos()).Filename, criteriaPBImport)
			}
		}
		return nil
	})
	if err != nil {
		t.Fatalf("walking package source: %v", err)
	}
}
