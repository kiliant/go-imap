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
		"ACLSession":            {"GetACL", "ListRights", "MyRights"},
		"ACLSetSession":         {"DeleteACL", "SetACL"},
		"Backend":               {"Authenticate"},
		"CapabilitySupport":     {"SupportsCapability"},
		"CatenateSession":       {"ResolveCatenateURL"},
		"CondStoreMailbox":      {"StoreCondStore"},
		"LanguageSession":       {"Languages", "SetLanguage"},
		"MessageLimitSession":   {"MessageLimits"},
		"MetadataSession":       {"GetMetadata", "SetMetadata"},
		"MoveMailbox":           {"Move"},
		"MoveSupport":           {"SupportsMove"},
		"MultiSearchSession":    {"MultiSearch"},
		"NamespaceSession":      {"Namespace"},
		"NotifySession":         {"Notify"},
		"QResyncMailbox":        {"Resync"},
		"QuotaSession":          {"GetQuota", "QuotaRoots"},
		"QuotaSetSession":       {"SetQuota"},
		"ReplaceMailbox":        {"Replace"},
		"SCRAMCredentials":      {"SCRAMCredentials"},
		"SelectedMailbox":       {"Copy", "Expunge", "Fetch", "Search", "Status", "Store", "Unselect"},
		"Session":               {"Append", "Close", "Create", "Delete", "List", "Rename", "Select", "Status", "Subscribe", "Unsubscribe"},
		"SortMailbox":           {"Sort"},
		"ThreadMailbox":         {"Thread"},
		"URLAuthSession":        {"FetchURLAuth", "GenerateURLAuth", "ResetURLAuthKey"},
		"UnauthenticateSession": {"Unauthenticate"},
		"Update":                {"update"},
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
		// REPLACE carries the same mutation origin as every other mutating
		// command; only its extension-specific fields bind to a feature.
		"ReplaceOptions.MutationOptions": true,
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
					// An untagged field has a nil Tag, which is the most likely
					// way for this gate to be tripped: someone added a field and
					// no binding at all. Report that rather than panicking on it.
					feature := ""
					if field.Tag != nil {
						feature = reflect.StructTag(strings.Trim(field.Tag.Value, "`")).Get("imapfeature")
					}
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

// framework-constructed exported structs are exempt from the unkeyed-literal
// guard: a caller never builds one, so adding a field to it cannot break any
// caller's literal. Each needs a reason, because "it seemed fine" is how an
// exception list stops meaning anything.
var unguardedByDesign = map[string]string{
	"Server":        "constructed by New, never by a caller",
	"SearchQuery":   "constructed by the framework from parsed criteria; has no exported fields",
	"ListWriter":    "framework-provided writer; callers receive one, never build one",
	"FetchWriter":   "framework-provided writer",
	"ExpungeWriter": "framework-provided writer",
	"Updater":       "framework-provided; callers receive one from Select",
}

// TestGrowableConfigurationStructsAreGuarded requires every exported struct in
// the package to carry an unexported sentinel field.
//
// It walks the package rather than iterating a hand-written list. A list is a
// judgement call per struct, and API-STABILITY.md section 7 says the point of
// the rule is that it is not a judgement call: a future group adding an
// unguarded options struct and forgetting to list it would otherwise pass
// green, which is exactly the failure the guard exists to prevent.
func TestGrowableConfigurationStructsAreGuarded(t *testing.T) {
	files := parseServerFiles(t, 0)
	for _, file := range files {
		for _, declaration := range file.Decls {
			general, ok := declaration.(*ast.GenDecl)
			if !ok || general.Tok != token.TYPE {
				continue
			}
			for _, spec := range general.Specs {
				typeSpec := spec.(*ast.TypeSpec)
				if !ast.IsExported(typeSpec.Name.Name) {
					continue
				}
				structure, ok := typeSpec.Type.(*ast.StructType)
				if !ok {
					continue
				}
				if _, exempt := unguardedByDesign[typeSpec.Name.Name]; exempt {
					continue
				}
				if !hasSentinelField(structure) {
					t.Errorf("%s has no unexported sentinel guard; add `_ struct{}` or an entry in unguardedByDesign with a reason",
						typeSpec.Name.Name)
				}
			}
		}
	}
	// An exemption for a struct that no longer exists is a stale claim, so the
	// list is checked in both directions.
	declared := make(map[string]bool)
	for _, file := range files {
		for _, declaration := range file.Decls {
			general, ok := declaration.(*ast.GenDecl)
			if !ok || general.Tok != token.TYPE {
				continue
			}
			for _, spec := range general.Specs {
				declared[spec.(*ast.TypeSpec).Name.Name] = true
			}
		}
	}
	for name := range unguardedByDesign {
		if !declared[name] {
			t.Errorf("unguardedByDesign names %s, which no longer exists", name)
		}
	}
}

func hasSentinelField(structure *ast.StructType) bool {
	for _, field := range structure.Fields.List {
		for _, name := range field.Names {
			if name.Name != "_" {
				continue
			}
			if inner, ok := field.Type.(*ast.StructType); ok && len(inner.Fields.List) == 0 {
				return true
			}
		}
	}
	return false
}
