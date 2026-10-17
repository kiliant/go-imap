package maildir_test

import (
	"go/parser"
	"go/token"
	"io/fs"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
)

func TestOnlyPublicImports(t *testing.T) {
	err := filepath.WalkDir(".", func(path string, entry fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if entry.IsDir() || !strings.HasSuffix(path, ".go") {
			return nil
		}
		file, err := parser.ParseFile(token.NewFileSet(), path, nil, parser.ImportsOnly)
		if err != nil {
			return err
		}
		for _, spec := range file.Imports {
			imported, err := strconv.Unquote(spec.Path.Value)
			if err != nil {
				return err
			}
			for _, component := range strings.Split(imported, "/") {
				if component == "internal" {
					t.Errorf("%s imports internal package %s", path, imported)
				}
			}
			if imported == "github.com/kiliant/go-imap/imapserver/memory" {
				t.Errorf("%s imports reference backend", path)
			}
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
}
