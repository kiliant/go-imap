package imapclient

import (
	"go/ast"
	"go/parser"
	"go/token"
	"io/fs"
	"strings"
	"testing"
)

// TestNotifyVocabularyMirrorsRootPackage requires imapclient's NOTIFY constants
// to be one-for-one with the root package's.
//
// The values cannot drift: each constant here is a conversion of the root
// constant, resolved at compile time. Membership can. A later RFC registers a
// NOTIFY event, package imap gains a constant, and this package silently does
// not — with a green build, because nothing refers to the missing one. A client
// author then has no name for an event the server can already send, which is the
// same divergence that made this vocabulary shared in the first place, arriving
// by omission instead of by disagreement.
//
// imapclient is frozen, so it can only ever gain constants; this gate is what
// makes gaining them non-optional.
func TestNotifyVocabularyMirrorsRootPackage(t *testing.T) {
	client := notifyConstantNames(t, ".")
	root := notifyConstantNames(t, "..")

	for name := range root {
		if !client[name] {
			t.Errorf("imap.%s has no counterpart in imapclient; add it, deriving the value "+
				"from the root constant so the two cannot disagree", name)
		}
	}
	for name := range client {
		if !root[name] {
			t.Errorf("imapclient.%s has no counterpart in package imap; the vocabulary is "+
				"defined there, so a client-only NOTIFY name will drift", name)
		}
	}
	if len(root) == 0 {
		t.Fatal("no NOTIFY constants found in package imap; has the naming changed?")
	}
}

// notifyConstantNames collects the exported NOTIFY constants declared in a
// package directory, keyed by name.
//
// Names are compared rather than values because the values are already pinned by
// constant conversion — this gate covers the axis that mechanism leaves open.
// notifyConstantNames collects exported constants whose name begins with
// "Notify".
//
// The prefix is the whole selector, and that is an assumption worth naming
// before it misfires in either direction. A root constant named for NOTIFY but
// not part of the wire vocabulary — a tuning threshold, say — would be demanded
// of imapclient by this gate, and its failure message tells the next agent to
// add a constant to a frozen package, which is permanent. Conversely an event
// constant not spelled Notify* would escape the mirror entirely.
//
// Neither has happened; both are cheap to spot if this test ever fails in a way
// that reads as absurd. Prefer renaming the new constant to widening the gate.
func notifyConstantNames(t *testing.T, directory string) map[string]bool {
	t.Helper()
	fileSet := token.NewFileSet()
	packages, err := parser.ParseDir(fileSet, directory, func(info fs.FileInfo) bool {
		name := info.Name()
		return strings.HasSuffix(name, ".go") && !strings.HasSuffix(name, "_test.go")
	}, 0)
	if err != nil {
		t.Fatal(err)
	}
	names := make(map[string]bool)
	for _, pkg := range packages {
		for _, file := range pkg.Files {
			for _, declaration := range file.Decls {
				general, ok := declaration.(*ast.GenDecl)
				if !ok || general.Tok != token.CONST {
					continue
				}
				for _, spec := range general.Specs {
					value, ok := spec.(*ast.ValueSpec)
					if !ok {
						continue
					}
					for _, name := range value.Names {
						if strings.HasPrefix(name.Name, "Notify") && ast.IsExported(name.Name) {
							names[name.Name] = true
						}
					}
				}
			}
		}
	}
	return names
}
