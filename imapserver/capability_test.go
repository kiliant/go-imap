package imapserver

import (
	"go/ast"
	"go/parser"
	"go/printer"
	"go/token"
	"io/fs"
	"reflect"
	"slices"
	"strings"
	"testing"
)

func TestBackendInterfaceMethodSets(t *testing.T) {
	// want is the full signature of every exported interface method, not merely
	// the method names.
	//
	// Names alone let a signature change through silently, and one went through:
	// ComparatorSession.Comparators returned (string, []string, error) and was
	// reshaped to (*ComparatorData, error) with this gate green throughout. Adding
	// a return value to an existing method is exactly the breaking change this
	// table exists to make visible, so it has to compare what actually breaks.
	want := map[string][]string{
		"ACLSession": {
			"GetACL(ctx context.Context, mailbox string, options *ACLOptions) (*imap.ACLData, error)",
			"ListRights(ctx context.Context, mailbox, identifier string, options *ACLOptions) (*imap.ListRightsData, error)",
			"MyRights(ctx context.Context, mailbox string, options *ACLOptions) (imap.ACLRights, error)",
		},
		"ACLSetSession": {
			"DeleteACL(ctx context.Context, mailbox, identifier string, options *ACLOptions) error",
			"SetACL(ctx context.Context, mailbox, identifier string, rights imap.ACLRights, options *ACLSetOptions) error",
		},
		"Backend": {
			"Authenticate(ctx context.Context, conn *ConnInfo, credentials *Credentials, options *AuthenticateOptions) (Session, error)",
		},
		"CapabilitySupport": {
			"SupportsCapability(name string) bool",
		},
		"CatenateSession": {
			"ResolveCatenateURL(ctx context.Context, url string, options *CatenateOptions) (io.ReadCloser, error)",
		},
		"ComparatorSession": {
			"Comparators(ctx context.Context, options *ComparatorOptions) (*ComparatorData, error)",
			"SetComparator(ctx context.Context, order []string, options *ComparatorOptions) (*ComparatorResult, error)",
		},
		"CondStoreMailbox": {
			"StoreCondStore(ctx context.Context, writer *FetchWriter, uids imap.UIDSet, flags *StoreFlags, options *StoreOptions) (*CondStoreResult, error)",
		},
		"FilterSession": {
			"Filter(ctx context.Context, name string, options *FilterOptions) (imap.SearchCriteria, error)",
		},
		"LanguageSession": {
			"Languages(ctx context.Context, options *LanguageOptions) ([]string, error)",
			"SetLanguage(ctx context.Context, tag string, options *LanguageOptions) (*LanguageResult, error)",
		},
		"MessageLimitSession": {
			"MessageLimits(ctx context.Context, options *MessageLimitOptions) (*MessageLimits, error)",
		},
		"MetadataSession": {
			"GetMetadata(ctx context.Context, mailbox string, entries []imap.MetadataEntryName, options *MetadataOptions) (*imap.MailboxMetadata, error)",
			"SetMetadata(ctx context.Context, mailbox string, entries []imap.MetadataEntry, options *MetadataOptions) error",
		},
		"MoveMailbox": {
			"Move(ctx context.Context, uids imap.UIDSet, destination string, options *MoveOptions) (*imap.CopyData, error)",
		},
		"MoveSupport": {
			"SupportsMove() bool",
		},
		"MultiSearchSession": {
			"MultiSearch(ctx context.Context, mailboxes []string, criteria imap.SearchCriteria, options *MultiSearchOptions) ([]MultiSearchMailboxResult, error)",
		},
		"NamespaceSession": {
			"Namespace(ctx context.Context, options *NamespaceOptions) (*imap.NamespaceData, error)",
		},
		"NotifySession": {
			"Notify(ctx context.Context, updater *SessionUpdater, config *NotifyConfig, options *NotifyOptions) error",
		},
		"QResyncMailbox": {
			"Resync(ctx context.Context, params *QResyncSelect, options *QResyncOptions) (*QResyncResult, error)",
		},
		"QuotaSession": {
			"GetQuota(ctx context.Context, root string, options *QuotaOptions) (*imap.QuotaData, error)",
			"QuotaRoots(ctx context.Context, mailbox string, options *QuotaOptions) ([]string, error)",
		},
		"QuotaSetSession": {
			"SetQuota(ctx context.Context, root string, limits []imap.QuotaResourceLimit, options *QuotaOptions) error",
		},
		"ReplaceMailbox": {
			"Replace(ctx context.Context, uid imap.UID, mailbox string, literal io.Reader, options *ReplaceOptions) (*imap.AppendData, error)",
		},
		"SCRAMCredentials": {
			"SCRAMCredentials(ctx context.Context, mechanism, username string, options *SCRAMCredentialsOptions) (*SCRAMStoredCredentials, error)",
		},
		"SelectedMailbox": {
			"Copy(ctx context.Context, uids imap.UIDSet, destination string, options *CopyOptions) (*imap.CopyData, error)",
			"Expunge(ctx context.Context, writer *ExpungeWriter, uids *imap.UIDSet, options *ExpungeOptions) error",
			"Fetch(ctx context.Context, writer *FetchWriter, uids imap.UIDSet, options *FetchOptions) error",
			"Search(ctx context.Context, query *SearchQuery, options *SearchOptions) (*SearchResult, error)",
			"Status(ctx context.Context, options *StatusOptions) (*imap.MailboxStatus, error)",
			"Store(ctx context.Context, writer *FetchWriter, uids imap.UIDSet, flags *StoreFlags, options *StoreOptions) error",
			"Unselect(ctx context.Context) error",
		},
		"Session": {
			"Append(ctx context.Context, mailbox string, literal io.Reader, options *AppendOptions) (*imap.AppendData, error)",
			"Close(ctx context.Context) error",
			"Create(ctx context.Context, mailbox string, options *CreateOptions) error",
			"Delete(ctx context.Context, mailbox string, options *DeleteOptions) error",
			"List(ctx context.Context, writer *ListWriter, reference string, patterns []string, options *ListOptions) error",
			"Rename(ctx context.Context, oldName, newName string, options *RenameOptions) error",
			"Select(ctx context.Context, mailbox string, updater *Updater, options *SelectOptions) (*SelectResult, error)",
			"Status(ctx context.Context, mailbox string, options *StatusOptions) (*imap.StatusData, error)",
			"Subscribe(ctx context.Context, mailbox string, options *SubscribeOptions) error",
			"Unsubscribe(ctx context.Context, mailbox string, options *UnsubscribeOptions) error",
		},
		"SortMailbox": {
			"Sort(ctx context.Context, query *SearchQuery, keys []imap.SortKeySpec, options *SortOptions) ([]imap.UID, error)",
		},
		"ThreadMailbox": {
			"Thread(ctx context.Context, query *SearchQuery, algorithm imap.ThreadAlgorithm, options *ThreadOptions) ([]imap.ThreadNode, error)",
		},
		"URLAuthSession": {
			"FetchURLAuth(ctx context.Context, url string, options *URLAuthOptions) (io.ReadCloser, error)",
			"GenerateURLAuth(ctx context.Context, url, mechanism string, options *URLAuthOptions) (string, error)",
			"ResetURLAuthKey(ctx context.Context, mailbox string, options *URLAuthOptions) error",
		},
		"UnauthenticateSession": {
			"Unauthenticate(ctx context.Context, options *UnauthenticateOptions) error",
		},
		"Update": {
			"update()",
		},
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
						var rendered strings.Builder
						if err := printer.Fprint(&rendered, fset, method.Type); err != nil {
							t.Fatal(err)
						}
						// The printed FuncType leads with "func"; the golden entry
						// reads better as "Method(args) results".
						signature := name.Name + strings.TrimPrefix(rendered.String(), "func")
						got[typeSpec.Name.Name] = append(got[typeSpec.Name.Name], signature)
					}
				}
				slices.Sort(got[typeSpec.Name.Name])
			}
		}
	}
	// Report only what moved. Dumping both maps whole makes the reader diff 28
	// entries by eye to find the one that changed, and a gate that is painful to
	// read gets "fixed" by pasting got over want — which is the one repair that
	// defeats it entirely.
	if !reflect.DeepEqual(got, want) {
		for name, wantMethods := range want {
			gotMethods, present := got[name]
			if !present {
				t.Errorf("interface %s was removed or renamed", name)
				continue
			}
			if !slices.Equal(gotMethods, wantMethods) {
				t.Errorf("interface %s changed:\n  got:  %s\n  want: %s",
					name, strings.Join(gotMethods, "\n        "), strings.Join(wantMethods, "\n        "))
			}
		}
		for name := range got {
			if _, present := want[name]; !present {
				t.Errorf("new exported interface %s is not in the golden table; add it here so "+
					"its method set is reviewed rather than assumed", name)
			}
		}
		t.Log("A new RFC adds an optional interface or options field; it never adds a method to an existing interface.")
	}
}

// frameworkRequestStruct reports whether a struct carries a client's request
// into a backend, and therefore needs every field bound to a feature.
//
// The `*Options` suffix covers almost all of them, and covering only those was a
// loophole: a field on a struct named otherwise needed no binding at all.
// NotifyConfig is the live example — the framework fills it from the wire, hands
// it to NotifySession, and it grew a field without tripping this gate. A
// successor RFC adding a registration parameter would land there for the same
// reason, and an older backend would ignore it silently while the server went on
// advertising the capability.
//
// The extras are listed rather than detected because "struct the framework
// populates for a backend" has no syntactic signature. A list is a judgement
// call, so it is kept short and each entry names the interface it reaches.
func frameworkRequestStruct(name string) bool {
	if strings.HasSuffix(name, "Options") {
		return true
	}
	switch name {
	case "NotifyConfig", // -> NotifySession.Notify
		"NotifyWatch",   // -> NotifyConfig.Watches
		"QResyncSelect", // -> QResyncMailbox.Resync
		"StoreFlags":    // -> SelectedMailbox.Store, CondStoreMailbox.StoreCondStore
		return true
	}
	return false
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
		// STORE's operation and flag list are the rev1 command itself, not an
		// extension of it.
		"StoreFlags.Op":    true,
		"StoreFlags.Flags": true,
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
				if !frameworkRequestStruct(typeSpec.Name.Name) {
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
