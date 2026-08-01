package imap_test

import (
	"fmt"
	"go/ast"
	"go/importer"
	"go/parser"
	"go/token"
	"go/types"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/kiliant/go-imap/imapclient"
)

var apiPackages = []struct {
	importPath string
	dir        string
}{
	{"github.com/kiliant/go-imap", "."},
	{"github.com/kiliant/go-imap/imapclient", "imapclient"},
}

func TestAPISurfaceNoInternalLeak(t *testing.T) {
	fset := token.NewFileSet()
	imp := &localImporter{
		gc:    importer.ForCompiler(fset, "gc", nil),
		local: make(map[string]*types.Package),
	}

	// Type-check module packages in dependency order so imapclient can be
	// checked from source without relying on a partial reflect seed list.
	for _, pkg := range []struct {
		path string
		dir  string
	}{
		{"github.com/kiliant/go-imap/internal/unicodenorm", "internal/unicodenorm"},
		{"github.com/kiliant/go-imap/internal/imapwire", "internal/imapwire"},
		{"github.com/kiliant/go-imap/internal/imapsasl", "internal/imapsasl"},
		{"github.com/kiliant/go-imap/internal/saslprep", "internal/saslprep"},
		{"github.com/kiliant/go-imap", "."},
		{"github.com/kiliant/go-imap/imapclient", "imapclient"},
	} {
		typeCheckDir(t, fset, pkg.path, pkg.dir, imp)
	}

	seen := make(map[types.Type]struct{})
	for _, path := range []string{"github.com/kiliant/go-imap", "github.com/kiliant/go-imap/imapclient"} {
		t.Run(path, func(t *testing.T) {
			walkExportedObjects(t, path, imp.local[path], seen)
		})
	}
}

type localImporter struct {
	gc    types.Importer
	local map[string]*types.Package
}

func (i *localImporter) Import(path string) (*types.Package, error) {
	if pkg, ok := i.local[path]; ok {
		return pkg, nil
	}
	return i.gc.Import(path)
}

func typeCheckDir(t *testing.T, fset *token.FileSet, importPath, dir string, imp *localImporter) *types.Package {
	t.Helper()
	if imp == nil {
		imp = &localImporter{local: make(map[string]*types.Package)}
	}
	if pkg, ok := imp.local[importPath]; ok {
		return pkg
	}
	files, err := parsePackageGoFiles(fset, dir)
	if err != nil {
		t.Fatalf("parse %s: %v", importPath, err)
	}
	cfg := types.Config{Importer: imp}
	typPkg, err := cfg.Check(importPath, fset, files, nil)
	if err != nil {
		t.Fatalf("type-check %s: %v", importPath, err)
	}
	imp.local[importPath] = typPkg
	return typPkg
}

func parsePackageGoFiles(fset *token.FileSet, dir string) ([]*ast.File, error) {
	pkgs, err := parser.ParseDir(fset, dir, func(fi os.FileInfo) bool {
		n := fi.Name()
		return !fi.IsDir() && strings.HasSuffix(n, ".go") && !strings.HasSuffix(n, "_test.go")
	}, 0)
	if err != nil {
		return nil, err
	}
	var files []*ast.File
	for name, pkg := range pkgs {
		if name == "main" {
			continue
		}
		for _, f := range pkg.Files {
			files = append(files, f)
		}
	}
	if len(files) == 0 {
		return nil, fmt.Errorf("no Go files in %s", dir)
	}
	return files, nil
}

func walkExportedObjects(t *testing.T, pkgPath string, pkg *types.Package, seen map[types.Type]struct{}) {
	t.Helper()
	scope := pkg.Scope()
	for _, name := range scope.Names() {
		obj := scope.Lookup(name)
		if obj == nil || !obj.Exported() {
			continue
		}
		switch o := obj.(type) {
		case *types.Func:
			checkTypesSignature(t, pkgPath+"."+name, o.Type(), seen)
		case *types.TypeName:
			typ := o.Type()
			checkTypeNoInternal(t, pkgPath+"."+name, typ, seen)
			walkNamedTypeMethods(t, pkgPath+"."+name, typ, seen)
			if ptr := types.NewPointer(typ); ptr != nil {
				walkNamedTypeMethods(t, pkgPath+"."+name, ptr, seen)
			}
		case *types.Var, *types.Const:
			checkTypeNoInternal(t, pkgPath+"."+name, obj.Type(), seen)
		}
	}
}

func walkNamedTypeMethods(t *testing.T, where string, typ types.Type, seen map[types.Type]struct{}) {
	named, ok := typ.(*types.Named)
	if !ok {
		return
	}
	for i := 0; i < named.NumMethods(); i++ {
		m := named.Method(i)
		if !m.Exported() {
			continue
		}
		checkTypesSignature(t, where+"."+m.Name(), m.Type(), seen)
	}
}

func checkTypesSignature(t *testing.T, where string, sig types.Type, seen map[types.Type]struct{}) {
	if sig, ok := sig.(*types.Signature); ok {
		if sig.Params() != nil {
			for i := 0; i < sig.Params().Len(); i++ {
				checkTypeNoInternal(t, where, sig.Params().At(i).Type(), seen)
			}
		}
		if sig.Results() != nil {
			for i := 0; i < sig.Results().Len(); i++ {
				checkTypeNoInternal(t, where, sig.Results().At(i).Type(), seen)
			}
		}
		return
	}
	checkTypeNoInternal(t, where, sig, seen)
}

func checkTypeNoInternal(t *testing.T, where string, typ types.Type, seen map[types.Type]struct{}) {
	if typ == nil {
		return
	}
	if _, ok := seen[typ]; ok {
		return
	}
	seen[typ] = struct{}{}

	switch tt := typ.(type) {
	case *types.Pointer:
		checkTypeNoInternal(t, where, tt.Elem(), seen)
	case *types.Slice:
		checkTypeNoInternal(t, where, tt.Elem(), seen)
	case *types.Array:
		checkTypeNoInternal(t, where, tt.Elem(), seen)
	case *types.Map:
		checkTypeNoInternal(t, where, tt.Key(), seen)
		checkTypeNoInternal(t, where, tt.Elem(), seen)
	case *types.Chan:
		checkTypeNoInternal(t, where, tt.Elem(), seen)
	case *types.Struct:
		for i := 0; i < tt.NumFields(); i++ {
			field := tt.Field(i)
			if !field.Exported() && !field.Embedded() {
				continue
			}
			checkTypeNoInternal(t, where+"."+field.Name(), field.Type(), seen)
		}
	case *types.Interface:
		for i := 0; i < tt.NumMethods(); i++ {
			m := tt.Method(i)
			checkTypesSignature(t, where+"."+m.Name(), m.Type(), seen)
		}
	case *types.Signature:
		checkTypesSignature(t, where, tt, seen)
	case *types.Named:
		if pkg := tt.Obj().Pkg(); pkg != nil && strings.Contains(pkg.Path(), "/internal/") {
			t.Errorf("%s: exported signature reaches internal type %s", where, tt)
		}
		checkTypeNoInternal(t, where, tt.Underlying(), seen)
	}
}

func TestAPISurfaceDocComments(t *testing.T) {
	for _, pkg := range apiPackages {
		t.Run(pkg.importPath, func(t *testing.T) {
			missing := exportedMissingDocs(pkg.dir, pkg.importPath)
			if len(missing) > 0 {
				t.Errorf("exported symbols missing doc comments:\n  %s", strings.Join(missing, "\n  "))
			}
		})
	}
}

func TestAPISurfaceKeyedLiteralDocs(t *testing.T) {
	for _, pkg := range apiPackages {
		t.Run(pkg.importPath, func(t *testing.T) {
			missing := exportedStructsMissingKeyedNote(pkg.dir)
			if len(missing) > 0 {
				t.Errorf("caller-constructed structs missing keyed-literal doc note:\n  %s", strings.Join(missing, "\n  "))
			}
		})
	}
}

func TestAPISurfaceContextFirst(t *testing.T) {
	t.Run("imapclient", func(t *testing.T) {
		var violations []string
		clientType := reflect.TypeOf((*imapclient.Client)(nil))
		checkReflectMethods("Client", clientType, &violations)

		commandTypes := []reflect.Type{
			reflect.TypeOf((*imapclient.Command)(nil)),
			reflect.TypeOf((*imapclient.FetchCommand)(nil)),
			reflect.TypeOf((*imapclient.SearchCommand)(nil)),
			reflect.TypeOf((*imapclient.AppendCommand)(nil)),
			reflect.TypeOf((*imapclient.IdleCommand)(nil)),
			reflect.TypeOf((*imapclient.ListCommand)(nil)),
			reflect.TypeOf((*imapclient.SelectCommand)(nil)),
			reflect.TypeOf((*imapclient.StatusCommand)(nil)),
			reflect.TypeOf((*imapclient.CopyCommand)(nil)),
			reflect.TypeOf((*imapclient.EnableCommand)(nil)),
			reflect.TypeOf((*imapclient.NamespaceCommand)(nil)),
			reflect.TypeOf((*imapclient.ESearchCommand)(nil)),
			reflect.TypeOf((*imapclient.MultiAppendCommand)(nil)),
			reflect.TypeOf((*imapclient.MultiSearchCommand)(nil)),
			reflect.TypeOf((*imapclient.SyncSelectCommand)(nil)),
			reflect.TypeOf((*imapclient.SyncStoreCommand)(nil)),
		}
		for _, typ := range commandTypes {
			checkReflectMethods(typ.Elem().Name(), typ, &violations)
		}

		for _, fn := range []struct {
			name string
			typ  reflect.Type
		}{
			{"Dial", reflect.TypeOf(imapclient.Dial)},
			{"DialTLS", reflect.TypeOf(imapclient.DialTLS)},
			{"DialStartTLS", reflect.TypeOf(imapclient.DialStartTLS)},
		} {
			checkFuncContext(fn.name, fn.typ, true, &violations)
		}

		if len(violations) > 0 {
			t.Errorf("context rule violations:\n  %s", strings.Join(violations, "\n  "))
		}
	})
}

func checkReflectMethods(typeName string, typ reflect.Type, violations *[]string) {
	for i := 0; i < typ.NumMethod(); i++ {
		m := typ.Method(i)
		if !m.IsExported() {
			continue
		}
		if isCommandHandleType(typeName) {
			if isBlockingBoundary(m.Name) && !reflectFirstParamContext(m.Type) {
				*violations = append(*violations, fmt.Sprintf("(%s).%s must take context.Context first", typeName, m.Name))
			}
			continue
		}
		if typeName == "Client" {
			checkReflectClientMethod(m.Name, m.Type, violations)
		}
	}
}

func checkReflectClientMethod(name string, typ reflect.Type, violations *[]string) {
	if nonBlockingClientMethods[name] {
		return
	}
	if isReflectCommandHandleConstructor(typ) {
		if streamingCommandConstructor(name) {
			if !reflectFirstParamContext(typ) {
				*violations = append(*violations, fmt.Sprintf("(*Client).%s streaming constructor must take context.Context first", name))
			}
		} else if reflectHasContextParam(typ) {
			*violations = append(*violations, fmt.Sprintf("(*Client).%s command-handle constructor must not take context.Context", name))
		}
		return
	}
	if !looksReflectBlockingClientMethod(name, typ) {
		return
	}
	if !reflectFirstParamContext(typ) {
		*violations = append(*violations, fmt.Sprintf("(*Client).%s blocking method must take context.Context first", name))
	}
}

func checkFuncContext(name string, typ reflect.Type, want bool, violations *[]string) {
	if reflectFirstParamContext(typ) != want {
		if want {
			*violations = append(*violations, fmt.Sprintf("%s must take context.Context first", name))
		} else {
			*violations = append(*violations, fmt.Sprintf("%s must not take context.Context", name))
		}
	}
}

func isReflectCommandHandleConstructor(typ reflect.Type) bool {
	if typ.NumOut() == 0 {
		return false
	}
	out := typ.Out(0)
	for out.Kind() == reflect.Ptr {
		out = out.Elem()
	}
	if out.Kind() != reflect.Struct {
		return false
	}
	n := out.Name()
	return n == "Command" || strings.HasSuffix(n, "Command")
}

func looksReflectBlockingClientMethod(name string, typ reflect.Type) bool {
	if isReflectCommandHandleConstructor(typ) {
		return false
	}
	if typ.NumOut() == 0 {
		return name == "Logout"
	}
	for i := 0; i < typ.NumOut(); i++ {
		if typ.Out(i).String() == "error" {
			return true
		}
	}
	return false
}

func reflectFirstParamContext(typ reflect.Type) bool {
	if typ.NumIn() == 0 {
		return false
	}
	start := 0
	if typ.NumIn() > 0 && typ.In(0).Kind() == reflect.Ptr {
		start = 1
	}
	if typ.NumIn() <= start {
		return false
	}
	return typ.In(start).String() == "context.Context"
}

func reflectHasContextParam(typ reflect.Type) bool {
	for i := 0; i < typ.NumIn(); i++ {
		if typ.In(i).String() == "context.Context" {
			return true
		}
	}
	return false
}

var nonBlockingClientMethods = map[string]bool{
	"Capabilities":               true,
	"CapabilityValues":           true,
	"Close":                      true,
	"Compressed":                 true,
	"CondStoreEnabled":           true,
	"EnabledCapabilities":        true,
	"I18NLevel":                  true,
	"IMAPSieveScripts":           true,
	"MessageLimit":               true,
	"QResyncEnabled":             true,
	"QuotaResources":             true,
	"RightsSets":                 true,
	"SaveLimit":                  true,
	"State":                      true,
	"Supports":                   true,
	"SupportsAnnotateExperiment": true,
	"SupportsContextSearch":      true,
	"SupportsContextSort":        true,
	"SupportsConvert":            true,
	"SupportsESort":              true,
	"SupportsFilters":            true,
	"SupportsInProgress":         true,
	"SupportsLoginReferrals":     true,
	"SupportsMailboxReferrals":   true,
	"SupportsURLPartial":         true,
	"UIDOnlyEnabled":             true,
}

func isCommandHandleType(name string) bool {
	return name == "Command" || strings.HasSuffix(name, "Command")
}

func isBlockingBoundary(name string) bool {
	switch name {
	case "Wait", "Next", "Collect", "All", "AllUID", "WaitReady":
		return true
	default:
		return false
	}
}

func streamingCommandConstructor(name string) bool {
	switch name {
	case "Append", "MultiAppend", "CatenateAppend", "Replace", "ReplaceUID":
		return true
	default:
		return false
	}
}

func exportedMissingDocs(dir, importPath string) []string {
	fset := token.NewFileSet()
	pkgs, err := parser.ParseDir(fset, dir, func(fi os.FileInfo) bool {
		n := fi.Name()
		return strings.HasSuffix(n, ".go") && !strings.HasSuffix(n, "_test.go")
	}, parser.ParseComments)
	if err != nil {
		return []string{err.Error()}
	}
	var missing []string
	for _, pkg := range pkgs {
		for _, f := range pkg.Files {
			for _, decl := range f.Decls {
				switch d := decl.(type) {
				case *ast.GenDecl:
					doc := d.Doc
					for _, spec := range d.Specs {
						switch s := spec.(type) {
						case *ast.TypeSpec:
							if !ast.IsExported(s.Name.Name) {
								continue
							}
							if doc == nil && s.Doc == nil {
								missing = append(missing, "type "+s.Name.Name)
							}
						case *ast.ValueSpec:
							for _, n := range s.Names {
								if !ast.IsExported(n.Name) {
									continue
								}
								if doc == nil && s.Doc == nil {
									missing = append(missing, exportedDeclName(d.Tok, n.Name))
								}
							}
						}
					}
				case *ast.FuncDecl:
					if d.Recv != nil {
						recv := d.Recv.List[0].Type
						var recvName string
						switch rt := recv.(type) {
						case *ast.StarExpr:
							if id, ok := rt.X.(*ast.Ident); ok {
								recvName = id.Name
							}
						case *ast.Ident:
							recvName = rt.Name
						}
						if recvName == "" || !ast.IsExported(recvName) || !ast.IsExported(d.Name.Name) {
							continue
						}
						if d.Doc == nil {
							missing = append(missing, "method (*"+recvName+")."+d.Name.Name)
						}
					} else {
						if !ast.IsExported(d.Name.Name) {
							continue
						}
						if d.Doc == nil {
							missing = append(missing, "func "+d.Name.Name)
						}
					}
				}
			}
		}
	}
	return missing
}

func exportedDeclName(tok token.Token, name string) string {
	if tok == token.CONST {
		return "const " + name
	}
	return "var " + name
}

func exportedStructsMissingKeyedNote(dir string) []string {
	fset := token.NewFileSet()
	pkgs, err := parser.ParseDir(fset, dir, func(fi os.FileInfo) bool {
		n := fi.Name()
		return strings.HasSuffix(n, ".go") && !strings.HasSuffix(n, "_test.go")
	}, parser.ParseComments)
	if err != nil {
		return []string{err.Error()}
	}
	var missing []string
	for _, pkg := range pkgs {
		for _, f := range pkg.Files {
			for _, decl := range f.Decls {
				gen, ok := decl.(*ast.GenDecl)
				if !ok || gen.Tok != token.TYPE {
					continue
				}
				for _, spec := range gen.Specs {
					ts, ok := spec.(*ast.TypeSpec)
					if !ok || !ast.IsExported(ts.Name.Name) {
						continue
					}
					st, ok := ts.Type.(*ast.StructType)
					if !ok || !structHasExportedField(st) {
						continue
					}
					if !hasKeyedLiteralNote(gen.Doc, ts.Doc) {
						missing = append(missing, "type "+ts.Name.Name)
					}
				}
			}
		}
	}
	return missing
}

func structHasExportedField(st *ast.StructType) bool {
	for _, f := range st.Fields.List {
		for _, n := range f.Names {
			if ast.IsExported(n.Name) {
				return true
			}
		}
	}
	return false
}

func hasKeyedLiteralNote(docs ...*ast.CommentGroup) bool {
	for _, d := range docs {
		if d == nil {
			continue
		}
		text := strings.ToLower(d.Text())
		if strings.Contains(text, "keyed") {
			return true
		}
	}
	return false
}

func filesFromASTPkg(pkg *ast.Package) []*ast.File {
	var out []*ast.File
	for _, f := range pkg.Files {
		out = append(out, f)
	}
	return out
}

func TestExampleProgramsPresent(t *testing.T) {
	examplesDir := filepath.Join(mustRepoRoot(t), "examples")
	entries, err := os.ReadDir(examplesDir)
	if err != nil {
		t.Fatalf("examples directory: %v", err)
	}
	var found int
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".go") {
			continue
		}
		found++
	}
	if found < 8 {
		t.Fatalf("expected at least 8 example programs, found %d", found)
	}
}

func mustRepoRoot(t *testing.T) string {
	t.Helper()
	wd, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	return wd
}
