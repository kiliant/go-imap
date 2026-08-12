package imapserver

import (
	"context"
	"go/ast"
	"go/parser"
	"go/token"
	"io/fs"
	"reflect"
	"slices"
	"strings"
	"testing"
)

func TestExportedSignaturesDoNotLeakInternalPackages(t *testing.T) {
	files := parseServerFiles(t, parser.ParseComments)
	for _, file := range files {
		internalAliases := make(map[string]bool)
		for _, spec := range file.Imports {
			path := strings.Trim(spec.Path.Value, `"`)
			if !strings.Contains(path, "/internal/") {
				continue
			}
			if spec.Name != nil {
				internalAliases[spec.Name.Name] = true
			} else if at := strings.LastIndexByte(path, '/'); at >= 0 {
				internalAliases[path[at+1:]] = true
			}
		}
		for _, declaration := range file.Decls {
			for _, signature := range exportedSignatureNodes(declaration) {
				ast.Inspect(signature, func(node ast.Node) bool {
					selector, ok := node.(*ast.SelectorExpr)
					if !ok {
						return true
					}
					ident, ok := selector.X.(*ast.Ident)
					if ok && internalAliases[ident.Name] {
						t.Errorf("exported signature names internal package %s", ident.Name)
					}
					return true
				})
			}
		}
	}
}

func TestExportedDeclarationsHaveDocs(t *testing.T) {
	files := parseServerFiles(t, parser.ParseComments)
	var missing []string
	for _, file := range files {
		for _, declaration := range file.Decls {
			switch declaration := declaration.(type) {
			case *ast.FuncDecl:
				if exportedFunc(declaration) && declaration.Doc == nil {
					missing = append(missing, declaration.Name.Name)
				}
			case *ast.GenDecl:
				for _, spec := range declaration.Specs {
					typeSpec, ok := spec.(*ast.TypeSpec)
					if !ok || !typeSpec.Name.IsExported() {
						continue
					}
					if typeSpec.Doc == nil && declaration.Doc == nil {
						missing = append(missing, typeSpec.Name.Name)
					}
					structure, ok := typeSpec.Type.(*ast.StructType)
					if !ok {
						continue
					}
					for _, field := range structure.Fields.List {
						for _, name := range field.Names {
							if name.IsExported() && field.Doc == nil && field.Comment == nil {
								missing = append(missing, typeSpec.Name.Name+"."+name.Name)
							}
						}
					}
				}
			}
		}
	}
	slices.Sort(missing)
	if len(missing) > 0 {
		t.Fatalf("exported declarations missing docs: %s", strings.Join(missing, ", "))
	}
}

func TestBlockingServerAPIsTakeContextFirst(t *testing.T) {
	contextType := reflect.TypeFor[context.Context]()
	for _, typeOf := range []reflect.Type{
		reflect.TypeFor[Backend](),
		reflect.TypeFor[Session](),
		reflect.TypeFor[SelectedMailbox](),
		reflect.TypeFor[MoveMailbox](),
		reflect.TypeFor[*ListWriter](),
		reflect.TypeFor[*FetchWriter](),
		reflect.TypeFor[*ExpungeWriter](),
		reflect.TypeFor[*Server](),
	} {
		for i := 0; i < typeOf.NumMethod(); i++ {
			method := typeOf.Method(i)
			if !method.IsExported() {
				continue
			}
			first := 0
			if typeOf.Kind() != reflect.Interface {
				first = 1 // receiver
			}
			if method.Type.NumIn() <= first || method.Type.In(first) != contextType {
				t.Errorf("%s.%s does not take context.Context first", typeOf, method.Name)
			}
		}
	}
}

func parseServerFiles(t *testing.T, mode parser.Mode) map[string]*ast.File {
	t.Helper()
	fset := token.NewFileSet()
	packages, err := parser.ParseDir(fset, ".", func(info fs.FileInfo) bool {
		return strings.HasSuffix(info.Name(), ".go") && !strings.HasSuffix(info.Name(), "_test.go")
	}, mode)
	if err != nil {
		t.Fatal(err)
	}
	return packages["imapserver"].Files
}

func exportedSignatureNodes(declaration ast.Decl) []ast.Node {
	var nodes []ast.Node
	switch declaration := declaration.(type) {
	case *ast.FuncDecl:
		if exportedFunc(declaration) {
			nodes = append(nodes, declaration.Type)
			if declaration.Recv != nil {
				nodes = append(nodes, declaration.Recv)
			}
		}
	case *ast.GenDecl:
		for _, spec := range declaration.Specs {
			switch spec := spec.(type) {
			case *ast.TypeSpec:
				if spec.Name.IsExported() {
					nodes = append(nodes, spec.Type)
				}
			case *ast.ValueSpec:
				for _, name := range spec.Names {
					if name.IsExported() && spec.Type != nil {
						nodes = append(nodes, spec.Type)
						break
					}
				}
			}
		}
	}
	return nodes
}

func exportedFunc(function *ast.FuncDecl) bool {
	if function == nil || !function.Name.IsExported() {
		return false
	}
	if function.Recv == nil || len(function.Recv.List) == 0 {
		return true
	}
	receiver := function.Recv.List[0].Type
	if pointer, ok := receiver.(*ast.StarExpr); ok {
		receiver = pointer.X
	}
	ident, ok := receiver.(*ast.Ident)
	return ok && ident.IsExported()
}
