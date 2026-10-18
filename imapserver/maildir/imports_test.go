package maildir_test

import (
	"go/parser"
	"go/token"
	"io/fs"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
)

func TestOnlyPublicImports(t *testing.T) {
	output, err := exec.Command("go", "list", "std").Output()
	if err != nil {
		t.Fatal(err)
	}
	standard := make(map[string]bool)
	for _, name := range strings.Fields(string(output)) {
		if !strings.Contains("/"+name+"/", "/internal/") && !strings.HasPrefix(name, "vendor/") {
			standard[name] = true
		}
	}
	err = filepath.WalkDir(".", func(path string, entry fs.DirEntry, err error) error {
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
			allowed := imported == "github.com/kiliant/go-imap" || imported == "github.com/kiliant/go-imap/imapserver" || imported == "github.com/kiliant/go-imap/imapserver/message"
			if strings.HasSuffix(path, "_test.go") {
				allowed = allowed || imported == "github.com/kiliant/go-imap/imapclient" || imported == "github.com/kiliant/go-imap/imapserver/backendtest" || imported == "github.com/kiliant/go-imap/imapserver/maildir"
			}
			allowed = allowed || standard[imported]
			if !allowed {
				t.Errorf("%s imports %s outside its public-package allowlist", path, imported)
			}
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
}
