package imapcodec

import (
	"go/ast"
	"go/parser"
	"go/token"
	"io/fs"
	"reflect"
	"strings"
	"testing"

	"github.com/kiliant/go-imap"
)

// criteriaInterface is the marker every search key implements.
var criteriaInterface = reflect.TypeOf((*imap.SearchCriteria)(nil)).Elem()

// searchCriteriaSamples is one value of every search key in package imap.
//
// Hand-maintained on purpose: TestEverySearchKeyHasASample fails when package
// imap declares a key that is missing here, so adding one is a deliberate line
// rather than a silent gap. This is the only list a new RFC has to touch, and
// forgetting it is a failure rather than a skipped case.
var searchCriteriaSamples = []imap.SearchCriteria{
	imap.SearchAnd{},
	imap.SearchOr{},
	imap.SearchNot{},
	imap.SearchFuzzy{},
	imap.SearchKeyword(""),
	imap.SearchFlagKeyword{},
	imap.SearchString{},
	imap.SearchHeaderField{},
	imap.SearchDate{},
	imap.SearchSize{},
	imap.SearchUID{},
	imap.SearchSeqNum{},
	imap.SearchWithin{},
	imap.SearchModSeq{},
	imap.SearchObjectID{},
	imap.SearchSavedResult{},
	imap.SearchFilter(""),
}

// TestSearchCriteriaContainersAreTraversed requires SearchCriteriaChildren to
// know every container search key in package imap.
//
// A container is any key that holds other keys. Containers matter more than they
// look: every exhaustive walk over a criteria tree stops at one it does not
// recognise, and each such walk has a wrong-but-silent failure mode — the server
// hands a backend criteria it promised it never would, the client sends
// non-ASCII text with no CHARSET declared. Neither raises an error.
//
// The gate works by reflection over a populated value rather than by
// pattern-matching the type declaration. An earlier AST version recognised only
// `Field SearchCriteria` and `[]SearchCriteria`, and missed a field typed as a
// named container, a map, a pointer, an embedded container, and a named slice of
// a named container — five of six shapes, in a test whose entire purpose was to
// be exhaustive. Reflection sees what the value actually holds.
func TestSearchCriteriaContainersAreTraversed(t *testing.T) {
	for _, sample := range searchCriteriaSamples {
		sampleType := reflect.TypeOf(sample)
		t.Run(sampleType.Name(), func(t *testing.T) {
			slots := criteriaSlots(sampleType)
			_, rebuild := SearchCriteriaChildren(sample)
			if slots == 0 {
				if rebuild != nil {
					t.Errorf("imap.%s holds no search keys but SearchCriteriaChildren "+
						"treats it as a container", sampleType.Name())
				}
				return
			}
			if rebuild == nil {
				t.Fatalf("imap.%s holds other search keys but SearchCriteriaChildren does "+
					"not descend into it, so every walk over a tree containing one stops "+
					"there — silently", sampleType.Name())
			}
		})
	}
}

// TestSearchCriteriaChildrenRoundTrip checks that decomposing a container and
// rebuilding it preserves both the children and everything else the container
// carries.
//
// Both walks rebuild unconditionally when they descend, so a rebuild closure
// that forgets a field silently drops it from every tree that contains one — for
// instance a future INTHREAD key whose algorithm is dropped while its criteria
// survive, changing what the command means rather than failing it.
func TestSearchCriteriaChildrenRoundTrip(t *testing.T) {
	for _, sample := range searchCriteriaSamples {
		sampleType := reflect.TypeOf(sample)
		populated, ok := populateCriteria(sample)
		if !ok {
			continue
		}
		children, rebuild := SearchCriteriaChildren(populated)
		if rebuild == nil {
			continue
		}
		t.Run(sampleType.Name(), func(t *testing.T) {
			rebuilt := rebuild(children)
			if !reflect.DeepEqual(rebuilt, populated) {
				t.Errorf("rebuilding imap.%s from its own children changed it:\n got:  %#v\n want: %#v\n"+
					"a field the rebuild closure does not carry is dropped from every tree "+
					"containing this key", sampleType.Name(), rebuilt, populated)
			}
		})
	}
}

// TestEverySearchKeyHasASample keeps searchCriteriaSamples honest by comparing
// it with the type declarations in package imap. Without this, a new key added
// to the root package would simply not be tested by the two gates above, and
// they would stay green while covering less.
func TestEverySearchKeyHasASample(t *testing.T) {
	declared := declaredSearchKeys(t)
	sampled := make(map[string]bool, len(searchCriteriaSamples))
	for _, sample := range searchCriteriaSamples {
		sampled[reflect.TypeOf(sample).Name()] = true
	}
	for name := range declared {
		if !sampled[name] {
			t.Errorf("imap.%s implements SearchCriteria but has no entry in "+
				"searchCriteriaSamples, so the traversal gates do not cover it", name)
		}
	}
	for name := range sampled {
		if !declared[name] {
			t.Errorf("searchCriteriaSamples names %s, which is not a search key in package imap", name)
		}
	}
}

// criteriaSlots counts how many places a type can hold a search key, following
// slices, maps, pointers and embedded fields.
//
// This is the part the AST version got wrong, so it is deliberately structural:
// it asks what the type holds rather than what its declaration looks like.
func criteriaSlots(t reflect.Type) int {
	switch t.Kind() {
	case reflect.Slice, reflect.Array, reflect.Ptr:
		if holdsCriteria(t.Elem()) {
			return 1
		}
	case reflect.Map:
		if holdsCriteria(t.Elem()) {
			return 1
		}
	case reflect.Struct:
		count := 0
		for i := range t.NumField() {
			if holdsCriteria(t.Field(i).Type) {
				count++
			}
		}
		return count
	}
	return 0
}

// holdsCriteria reports whether a field type is, or transitively contains, a
// search key.
func holdsCriteria(t reflect.Type) bool {
	if t.Implements(criteriaInterface) || t == criteriaInterface {
		return true
	}
	switch t.Kind() {
	case reflect.Slice, reflect.Array, reflect.Ptr, reflect.Map:
		return holdsCriteria(t.Elem())
	}
	return false
}

// populateCriteria fills a container's criteria slots with distinguishable
// values so a rebuild that drops one is visible.
func populateCriteria(sample imap.SearchCriteria) (imap.SearchCriteria, bool) {
	switch sample.(type) {
	case imap.SearchAnd:
		return imap.SearchAnd{imap.SearchFilter("first"), imap.SearchFilter("second")}, true
	case imap.SearchOr:
		return imap.SearchOr{Left: imap.SearchFilter("left"), Right: imap.SearchFilter("right")}, true
	case imap.SearchNot:
		return imap.SearchNot{Criteria: imap.SearchFilter("inner")}, true
	case imap.SearchFuzzy:
		return imap.SearchFuzzy{Criteria: imap.SearchFilter("inner")}, true
	}
	return nil, false
}

// declaredSearchKeys finds every type in package imap that declares the
// unexported marker method, whatever its receiver shape.
func declaredSearchKeys(t *testing.T) map[string]bool {
	t.Helper()
	fileSet := token.NewFileSet()
	packages, err := parser.ParseDir(fileSet, "../..", func(info fs.FileInfo) bool {
		name := info.Name()
		return strings.HasSuffix(name, ".go") && !strings.HasSuffix(name, "_test.go")
	}, 0)
	if err != nil {
		t.Fatal(err)
	}
	root, ok := packages["imap"]
	if !ok {
		t.Fatal("package imap not found; the traversal gate is validating the wrong tree")
	}
	names := make(map[string]bool)
	structs := make(map[string][]string)
	for _, file := range root.Files {
		for _, declaration := range file.Decls {
			if function, ok := declaration.(*ast.FuncDecl); ok {
				if function.Name.Name != "searchCriteria" || function.Recv == nil ||
					len(function.Recv.List) != 1 {
					continue
				}
				if name := receiverTypeName(function.Recv.List[0].Type); name != "" {
					names[name] = true
				}
				continue
			}
			general, ok := declaration.(*ast.GenDecl)
			if !ok || general.Tok != token.TYPE {
				continue
			}
			for _, spec := range general.Specs {
				typeSpec := spec.(*ast.TypeSpec)
				structure, ok := typeSpec.Type.(*ast.StructType)
				if !ok {
					continue
				}
				for _, field := range structure.Fields.List {
					// An embedded field has no names; its type is promoted, so
					// embedding a search key makes the outer type one too.
					if len(field.Names) == 0 {
						if embedded := receiverTypeName(field.Type); embedded != "" {
							structs[typeSpec.Name.Name] = append(structs[typeSpec.Name.Name], embedded)
						}
					}
				}
			}
		}
	}
	if len(names) == 0 {
		t.Fatal("no search keys found; the marker method may have been renamed")
	}
	// A type that embeds a search key inherits the marker method and is itself a
	// search key, without declaring anything. That shape is invisible to a scan
	// for method declarations, so it is resolved to a fixpoint here — embedding
	// can chain.
	for changed := true; changed; {
		changed = false
		for name, embeds := range structs {
			if names[name] {
				continue
			}
			for _, embedded := range embeds {
				if names[embedded] {
					names[name] = true
					changed = true
					break
				}
			}
		}
	}
	return names
}

// receiverTypeName unwraps pointer and generic receivers to the declared name.
// The AST version handled only Ident and StarExpr, so a generic receiver
// silently produced no name and dropped the type from the gate entirely.
func receiverTypeName(expr ast.Expr) string {
	switch receiver := expr.(type) {
	case *ast.Ident:
		return receiver.Name
	case *ast.StarExpr:
		return receiverTypeName(receiver.X)
	case *ast.IndexExpr:
		return receiverTypeName(receiver.X)
	case *ast.IndexListExpr:
		return receiverTypeName(receiver.X)
	}
	return ""
}
