package memory_test

import (
	"go/parser"
	"go/token"
	"io/fs"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
)

func TestMemoryUsesPublicMessageHelpers(t *testing.T) {
	err := filepath.WalkDir(".", func(path string, entry fs.DirEntry, err error) error {
		if err != nil || entry.IsDir() || !strings.HasSuffix(path, ".go") {
			return err
		}
		file, err := parser.ParseFile(token.NewFileSet(), path, nil, parser.ImportsOnly)
		if err != nil {
			return err
		}
		for _, spec := range file.Imports {
			importPath, err := strconv.Unquote(spec.Path.Value)
			if err != nil {
				return err
			}
			if importPath == "github.com/kiliant/go-imap/internal/imapmessage" {
				t.Errorf("%s bypasses the public message helper", path)
			}
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
}
