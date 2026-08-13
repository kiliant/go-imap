package imapserver

import (
	"go/ast"
	"go/parser"
	"go/token"
	"io/fs"
	"strings"
	"testing"

	"github.com/kiliant/go-imap"
)

// TestSearchCriteriaContainersAreTraversed requires every container search key in
// package imap to be handled by searchCriteriaChildren.
//
// A "container" is any imap.SearchCriteria implementation with a field of type
// SearchCriteria or []SearchCriteria — a key that holds other keys. The
// framework walks the criteria tree twice, to resolve sequence numbers to UIDs
// and to substitute FILTER, and both walks reach children only through
// searchCriteriaChildren. A container missing from it is not a partial
// traversal; it is a subtree the framework never enters, so a backend receives
// criteria the guarantees on SearchQuery.Criteria say it will never see.
//
// This is not hypothetical. SearchFuzzy (RFC 6203) was a container from the day
// it landed. UID normalisation descended into it and FILTER substitution did
// not, so `SEARCH FUZZY FILTER "x"` handed the backend an unsubstituted
// imap.SearchFilter and skipped the FILTERS capability check on the way. Two
// hand-maintained lists of the same set drifted apart, and no test compared
// them to the type declarations they were supposed to mirror.
//
// The gate reads the declarations instead. RFC N+1 adding a container key fails
// here rather than in a backend's default case.
func TestSearchCriteriaContainersAreTraversed(t *testing.T) {
	fileSet := token.NewFileSet()
	rootPackages, err := parser.ParseDir(fileSet, "..", func(info fs.FileInfo) bool {
		name := info.Name()
		return strings.HasSuffix(name, ".go") && !strings.HasSuffix(name, "_test.go")
	}, 0)
	if err != nil {
		t.Fatal(err)
	}
	root, ok := rootPackages["imap"]
	if !ok {
		t.Fatal("package imap not found in the parent directory")
	}

	// A type is a search key if it declares the unexported marker method, and a
	// container if any of its fields carries criteria.
	markers := make(map[string]bool)
	containers := make(map[string]bool)
	for _, file := range root.Files {
		for _, declaration := range file.Decls {
			if function, ok := declaration.(*ast.FuncDecl); ok {
				if function.Name.Name == "searchCriteria" && function.Recv != nil && len(function.Recv.List) == 1 {
					markers[receiverTypeName(function.Recv.List[0].Type)] = true
				}
				continue
			}
			general, ok := declaration.(*ast.GenDecl)
			if !ok || general.Tok != token.TYPE {
				continue
			}
			for _, spec := range general.Specs {
				typeSpec := spec.(*ast.TypeSpec)
				if carriesCriteria(typeSpec.Type) {
					containers[typeSpec.Name.Name] = true
				}
			}
		}
	}
	if len(markers) == 0 {
		t.Fatal("no search-key types found; the marker method may have been renamed")
	}

	for name := range containers {
		if !markers[name] {
			continue
		}
		zero, ok := zeroSearchCriteria(name)
		if !ok {
			t.Errorf("imap.%s is a container search key with no case in zeroSearchCriteria; "+
				"add one so this gate can exercise it", name)
			continue
		}
		if _, rebuild := searchCriteriaChildren(zero); rebuild == nil {
			t.Errorf("imap.%s holds other search keys but searchCriteriaChildren does not "+
				"descend into it, so neither UID normalisation nor FILTER substitution "+
				"reaches its children", name)
		}
	}
}

// TestSearchCriteriaContainersSubstituteFilters checks the property the previous
// test protects: a FILTER nested inside any container is found.
//
// searchMentionsFilter guards the substitution walk, so a false negative there
// skips substitution entirely rather than merely doing less of it.
func TestSearchCriteriaContainersSubstituteFilters(t *testing.T) {
	filter := imap.SearchFilter("saved")
	for _, testCase := range []struct {
		name     string
		criteria imap.SearchCriteria
	}{
		{"bare", filter},
		{"and", imap.SearchAnd{imap.SearchSeen, filter}},
		{"or-left", imap.SearchOr{Left: filter, Right: imap.SearchSeen}},
		{"or-right", imap.SearchOr{Left: imap.SearchSeen, Right: filter}},
		{"not", imap.SearchNot{Criteria: filter}},
		{"fuzzy", imap.SearchFuzzy{Criteria: filter}},
		{"fuzzy-inside-not", imap.SearchNot{Criteria: imap.SearchFuzzy{Criteria: filter}}},
		{"fuzzy-inside-and", imap.SearchAnd{imap.SearchSeen, imap.SearchFuzzy{Criteria: filter}}},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			if !searchMentionsFilter(testCase.criteria) {
				t.Error("a nested FILTER was not detected, so substitution is skipped and " +
					"the backend receives an imap.SearchFilter it was promised it would not")
			}
		})
	}
}

// zeroSearchCriteria builds an empty value of a named search key.
//
// Written as an explicit switch rather than by reflection so that adding a
// container key requires a deliberate line here; the test above reports a
// missing case as a failure rather than skipping it.
func zeroSearchCriteria(name string) (imap.SearchCriteria, bool) {
	switch name {
	case "SearchAnd":
		return imap.SearchAnd{}, true
	case "SearchOr":
		return imap.SearchOr{}, true
	case "SearchNot":
		return imap.SearchNot{}, true
	case "SearchFuzzy":
		return imap.SearchFuzzy{}, true
	}
	return nil, false
}

func receiverTypeName(expr ast.Expr) string {
	switch receiver := expr.(type) {
	case *ast.Ident:
		return receiver.Name
	case *ast.StarExpr:
		return receiverTypeName(receiver.X)
	}
	return ""
}

// carriesCriteria reports whether a type declaration holds SearchCriteria,
// either as a named struct field or as the element of a slice type.
func carriesCriteria(expr ast.Expr) bool {
	switch declared := expr.(type) {
	case *ast.ArrayType:
		return isCriteriaExpr(declared.Elt)
	case *ast.StructType:
		for _, field := range declared.Fields.List {
			if isCriteriaExpr(field.Type) {
				return true
			}
			if array, ok := field.Type.(*ast.ArrayType); ok && isCriteriaExpr(array.Elt) {
				return true
			}
		}
	}
	return false
}

func isCriteriaExpr(expr ast.Expr) bool {
	identifier, ok := expr.(*ast.Ident)
	return ok && identifier.Name == "SearchCriteria"
}
