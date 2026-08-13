package imapserver

import (
	"go/ast"
	"go/parser"
	"go/token"
	"io/fs"
	"reflect"
	"slices"
	"strings"
	"testing"
)

func TestBackendInterfaceMethodSets(t *testing.T) {
	want := map[string][]string{
		"Backend":           {"Authenticate"},
		"CapabilitySupport": {"SupportsCapability"},
		"CondStoreMailbox":  {"StoreCondStore"},
		"MoveMailbox":       {"Move"},
		"MoveSupport":       {"SupportsMove"},
		"QResyncMailbox":    {"Resync"},
		"SelectedMailbox":   {"Copy", "Expunge", "Fetch", "Search", "Status", "Store", "Unselect"},
		"Session":           {"Append", "Close", "Create", "Delete", "List", "Rename", "Select", "Status", "Subscribe", "Unsubscribe"},
		"Update":            {"update"},
	}

	fset := token.NewFileSet()
	packages, err := parser.ParseDir(fset, ".", func(info fs.FileInfo) bool {
		name := info.Name()
		return strings.HasSuffix(name, ".go") && !strings.HasSuffix(name, "_test.go")
	}, 0)
	if err != nil {
		t.Fatal(err)
	}
	got := make(map[string][]string)
	for _, file := range packages["imapserver"].Files {
		for _, declaration := range file.Decls {
			general, ok := declaration.(*ast.GenDecl)
			if !ok || general.Tok != token.TYPE {
				continue
			}
			for _, spec := range general.Specs {
				typeSpec := spec.(*ast.TypeSpec)
				iface, ok := typeSpec.Type.(*ast.InterfaceType)
				if !ok || !ast.IsExported(typeSpec.Name.Name) {
					continue
				}
				for _, method := range iface.Methods.List {
					for _, name := range method.Names {
						got[typeSpec.Name.Name] = append(got[typeSpec.Name.Name], name.Name)
					}
				}
				slices.Sort(got[typeSpec.Name.Name])
			}
		}
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("exported interface method sets changed\n got: %#v\nwant: %#v\nA new RFC adds an optional interface or options field; it never adds a method to an existing interface.", got, want)
	}
}

func TestExtensionOptionFieldsHaveFeatureBinding(t *testing.T) {
	baseline := map[string]bool{
		"MutationOptions.Origin":         true,
		"StatusOptions.Items":            true,
		"AppendOptions.MutationOptions":  true,
		"AppendOptions.Flags":            true,
		"AppendOptions.InternalDate":     true,
		"SelectOptions.ReadOnly":         true,
		"FetchOptions.Items":             true,
		"SearchOptions.Charset":          true,
		"StoreOptions.MutationOptions":   true,
		"StoreOptions.Silent":            true,
		"CopyOptions.MutationOptions":    true,
		"MoveOptions.MutationOptions":    true,
		"ExpungeOptions.MutationOptions": true,
		"Options.TLSConfig":              true,
		"Options.RequireTLS":             true,
		"Options.AllowInsecureAuth":      true,
		"Options.Greeting":               true,
		"Options.ServerID":               true,
		"Options.Limits":                 true,
	}
	knownFeatures := make(map[string]bool)
	for _, descriptor := range featureDescriptors {
		knownFeatures[string(descriptor.ID)] = true
	}

	fset := token.NewFileSet()
	packages, err := parser.ParseDir(fset, ".", nil, parser.ParseComments)
	if err != nil {
		t.Fatal(err)
	}
	for _, file := range packages["imapserver"].Files {
		for _, declaration := range file.Decls {
			general, ok := declaration.(*ast.GenDecl)
			if !ok || general.Tok != token.TYPE {
				continue
			}
			for _, spec := range general.Specs {
				typeSpec := spec.(*ast.TypeSpec)
				if !strings.HasSuffix(typeSpec.Name.Name, "Options") {
					continue
				}
				structure, ok := typeSpec.Type.(*ast.StructType)
				if !ok {
					continue
				}
				for _, field := range structure.Fields.List {
					name := ""
					if len(field.Names) > 0 {
						name = field.Names[0].Name
					} else if ident, ok := field.Type.(*ast.Ident); ok {
						name = ident.Name
					}
					if name == "" || name == "_" || !ast.IsExported(name) {
						continue
					}
					key := typeSpec.Name.Name + "." + name
					if baseline[key] {
						continue
					}
					feature := reflect.StructTag(strings.Trim(field.Tag.Value, "`")).Get("imapfeature")
					if feature == "" {
						t.Errorf("%s is not a baseline field and has no imapfeature binding", key)
					} else if !knownFeatures[feature] {
						t.Errorf("%s binds unknown feature %q", key, feature)
					}
				}
			}
		}
	}
}

func TestGrowableConfigurationStructsAreGuarded(t *testing.T) {
	for _, value := range []any{
		ConnInfo{}, Credentials{}, Options{}, Limits{}, AuthenticateOptions{}, MutationOptions{}, ListOptions{}, StatusOptions{},
		CreateOptions{}, DeleteOptions{}, RenameOptions{}, SubscribeOptions{}, UnsubscribeOptions{},
		AppendOptions{}, SelectOptions{}, FetchOptions{}, SearchOptions{}, StoreFlags{}, StoreOptions{}, CopyOptions{},
		MoveOptions{}, ExpungeOptions{}, SelectResult{}, SelectSnapshot{}, UpdateBatch{}, UpdateAdd{}, UpdateFlags{},
		UpdateExpunge{}, UpdateVanished{}, SearchResult{}, QResyncSelect{}, CondStoreResult{}, QResyncResult{},
	} {
		typeOf := reflect.TypeOf(value)
		field, ok := typeOf.FieldByName("_")
		if !ok || field.Type != reflect.TypeOf(struct{}{}) {
			t.Errorf("%s has no unexported sentinel guard", typeOf.Name())
		}
	}
}
