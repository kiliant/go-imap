package imapserver

import (
	"go/ast"
	"go/parser"
	"go/printer"
	"go/token"
	"io/fs"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strconv"
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
			"Unselect(ctx context.Context, options *UnselectOptions) error",
		},
		"Session": {
			"Append(ctx context.Context, mailbox string, literal io.Reader, options *AppendOptions) (*imap.AppendData, error)",
			"Close(ctx context.Context, options *SessionCloseOptions) error",
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

// TestBackendMethodsTakeOptions is rule 3 applied to the backend surface: every
// blocking backend method ends in a pointer to an options struct, so a future
// RFC adds a field rather than a parameter.
//
// The client half of this has been gated since T14 (TestAPISurfaceOptionsStruct
// in the root module). The server half had no gate, and shipped Session.Close
// and SelectedMailbox.Unselect without options — the two methods on the frozen
// mandatory interfaces, whose own doc comment promises "an option field, not a
// method here" as the extension route. That promise was false for exactly the
// two methods nothing checked.
//
// docs/API-STABILITY.md section 3 records the same defect one layer down: three
// imapclient methods shipped without options and were caught only at the v1.0
// freeze review. This is the gate that stops it being a third time.
func TestBackendMethodsTakeOptions(t *testing.T) {
	// Witnesses and markers are not blocking calls: they answer from state the
	// backend already holds, take no context, and write nothing to the wire.
	// Each exemption is a decision, so each is named rather than pattern-matched.
	exempt := map[string]bool{
		"MoveSupport.SupportsMove":             true,
		"CapabilitySupport.SupportsCapability": true,
		"Update.update":                        true,

		// Accessor over state the caller already handed us: no context, no
		// wire, nothing to configure.
		"SearchQuery.Criteria": true,

		// The payload is itself a growable struct, which is the same guarantee
		// an options struct gives: a new RFC adds a field to imap.ListData,
		// imap.FetchMessageData, UpdateBatch or SessionUpdate. Contrast
		// ExpungeWriter.WriteExpunge, whose payload is a bare imap.UID with
		// nowhere to grow — that one takes options for exactly this reason.
		"ListWriter.WriteList":     true,
		"FetchWriter.WriteMessage": true,
		"Updater.Push":             true,
		"SessionUpdater.Push":      true,
	}

	fset := token.NewFileSet()
	packages, err := parser.ParseDir(fset, ".", func(info fs.FileInfo) bool {
		name := info.Name()
		return strings.HasSuffix(name, ".go") && !strings.HasSuffix(name, "_test.go")
	}, 0)
	if err != nil {
		t.Fatal(err)
	}
	// Exported methods on exported concrete types, not only interface methods.
	// The three a user actually calls — Serve, ServeConn, Close — are methods on
	// *Server, so an interface-only walk left the entry points outside the gate
	// while this comment claimed to cover "the backend surface". Server.Close
	// takes a context and so is not io.Closer; the root module's Close exemption
	// does not reach it.
	for _, file := range packages["imapserver"].Files {
		for _, declaration := range file.Decls {
			if fn, ok := declaration.(*ast.FuncDecl); ok {
				receiver, isMethod := receiverTypeName(fn)
				if !isMethod || !ast.IsExported(receiver) || !ast.IsExported(fn.Name.Name) {
					continue
				}
				qualified := receiver + "." + fn.Name.Name
				if exempt[qualified] || optionsParameter(fset, fn.Type) {
					continue
				}
				t.Errorf("%s does not end in a pointer to an options struct.\n"+
					"A new RFC must be able to add a field; adding a parameter breaks every caller. "+
					"If this method genuinely cannot block or grow, add it to the exempt list above "+
					"with a reason.", qualified)
				continue
			}
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
					fn, ok := method.Type.(*ast.FuncType)
					if !ok {
						continue
					}
					for _, name := range method.Names {
						qualified := typeSpec.Name.Name + "." + name.Name
						if exempt[qualified] {
							continue
						}
						if optionsParameter(fset, fn) {
							continue
						}
						t.Errorf("%s does not end in a pointer to an options struct.\n"+
							"A new RFC must be able to add a field; adding a parameter breaks every backend. "+
							"If this method genuinely cannot block or grow, add it to the exempt list above "+
							"with a reason.", qualified)
					}
				}
			}
		}
	}
}

// receiverTypeName returns the type a method is declared on, without its
// pointer star, and whether the declaration is a method at all.
func receiverTypeName(fn *ast.FuncDecl) (string, bool) {
	if fn.Recv == nil || len(fn.Recv.List) != 1 {
		return "", false
	}
	expr := fn.Recv.List[0].Type
	if star, ok := expr.(*ast.StarExpr); ok {
		expr = star.X
	}
	if index, ok := expr.(*ast.IndexExpr); ok { // generic receiver
		expr = index.X
	}
	ident, ok := expr.(*ast.Ident)
	if !ok {
		return "", false
	}
	return ident.Name, true
}

// optionsParameter reports whether fn's last parameter is a *…Options pointer.
func optionsParameter(fset *token.FileSet, fn *ast.FuncType) bool {
	if fn.Params == nil || len(fn.Params.List) == 0 {
		return false
	}
	last := fn.Params.List[len(fn.Params.List)-1]
	star, ok := last.Type.(*ast.StarExpr)
	if !ok {
		return false
	}
	var rendered strings.Builder
	if err := printer.Fprint(&rendered, fset, star.X); err != nil {
		return false
	}
	return strings.HasSuffix(rendered.String(), "Options")
}

// TestEverySearchKeyAndFetchItemIsClassified reads the declarations in package
// imap and fails when one of them is missing from capability_keys.go.
//
// This is the gate that makes docs/API-STABILITY.md §10 branch (a) enforced
// rather than promised. §10 says a criterion or item added to package imap may
// only reach a backend if "the framework guarantees it never reaches a consumer
// that predates it *and* a test enforces it for every path". This is that test.
//
// It matters because forgetting fails open. An unclassified criterion that
// nobody notices is one the framework hands to a backend ungated — which is the
// state the whole file was written to end — so the check cannot be "does the
// switch compile", it has to be "does the switch mention everything that
// exists".
func TestEverySearchKeyAndFetchItemIsClassified(t *testing.T) {
	root := rootPackageDir(t)
	fset := token.NewFileSet()
	packages, err := parser.ParseDir(fset, root, func(info fs.FileInfo) bool {
		name := info.Name()
		return strings.HasSuffix(name, ".go") && !strings.HasSuffix(name, "_test.go")
	}, 0)
	if err != nil {
		t.Fatal(err)
	}
	pkg, ok := packages["imap"]
	if !ok {
		t.Fatalf("package imap not found under %s", root)
	}

	// A type implements the marker by declaring the unexported method.
	criteria, items := implementorsOf(pkg.Files, "searchCriteria"), implementorsOf(pkg.Files, "fetchItem")
	if len(criteria) == 0 || len(items) == 0 {
		t.Fatalf("found %d criteria and %d fetch items; the marker-method scan is broken",
			len(criteria), len(items))
	}

	for _, name := range criteria {
		if !classifiedCriterion(name) {
			t.Errorf("imap.%s is a SearchCriteria that capability_keys.go does not classify.\n"+
				"Unclassified means refused at the wire, so a key this framework "+
				"supports would stop working; unlisted-and-forgotten means it reaches "+
				"backends ungated. Add it to criterionCapability.", name)
		}
	}
	for _, name := range items {
		if !classifiedFetchItem(name) {
			t.Errorf("imap.%s is a FetchItem that capability_keys.go does not classify. "+
				"Add it to fetchItemCapability.", name)
		}
	}
}

// classifiedCriterion and classifiedFetchItem report whether the named type
// appears in the classification source. Reading the source rather than calling
// the function keeps the check honest for types that cannot be constructed
// here without knowing their shape.
func classifiedCriterion(name string) bool { return classificationMentions("imap." + name) }
func classifiedFetchItem(name string) bool { return classificationMentions("imap." + name) }

func classificationMentions(qualified string) bool {
	source, err := os.ReadFile("capability_keys.go")
	if err != nil {
		return false
	}
	// Word boundary: imap.SearchUID must not be satisfied by imap.SearchUIDPlus.
	for _, line := range strings.Split(string(source), "\n") {
		for _, token := range strings.FieldsFunc(line, func(r rune) bool {
			return r == ' ' || r == '\t' || r == ',' || r == ':' || r == '(' || r == ')' || r == '*' || r == '{'
		}) {
			if token == qualified {
				return true
			}
		}
	}
	return false
}

// implementorsOf returns the exported types declaring the given marker method.
//
// It takes the file map rather than the enclosing package: ast.Package is
// deprecated, and nothing here needs it.
func implementorsOf(files map[string]*ast.File, marker string) []string {
	var names []string
	for _, file := range files {
		for _, declaration := range file.Decls {
			fn, ok := declaration.(*ast.FuncDecl)
			if !ok || fn.Name.Name != marker || fn.Recv == nil || len(fn.Recv.List) == 0 {
				continue
			}
			receiver := fn.Recv.List[0].Type
			if star, ok := receiver.(*ast.StarExpr); ok {
				receiver = star.X
			}
			ident, ok := receiver.(*ast.Ident)
			if !ok || !ast.IsExported(ident.Name) {
				continue
			}
			names = append(names, ident.Name)
		}
	}
	slices.Sort(names)
	return slices.Compact(names)
}

// rootPackageDir locates the root module's package imap from inside the nested
// imapserver module.
func rootPackageDir(t *testing.T) string {
	t.Helper()
	dir, err := filepath.Abs("..")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(dir, "search.go")); err != nil {
		// Fatal, not Skip. This is the gate that makes API-STABILITY §10
		// branch (a) enforced rather than promised, and its failure mode is to
		// let an unclassified key through — so it disappearing quietly is the
		// one outcome it must not have.
		t.Fatalf("root package sources are not beside this module, so the "+
			"classification gate cannot run: %v", err)
	}
	return dir
}

// TestEveryKeyGateResolves checks that every gate in capability_keys.go names
// something the framework actually declares — a featureID for requiresFeature,
// a capability descriptor for requiresToken.
//
// Both constructors fail in the same direction. requiresFeature returns a
// deny-all gate for an id it cannot resolve; requiresToken reads
// advertised[name], which is false forever for a name matching no descriptor.
// A typo, or a descriptor later renamed, therefore silently withdraws a key the
// framework supports — the BINARY defect re-created in the direction nobody
// notices, because the wire symptom is a NO rather than a wrong answer.
//
// The token half is the one that will actually be hit. RFC 5257 spells its
// search key and fetch item ANNOTATION while the capability token is
// ANNOTATE-EXPERIMENT-1, so whoever adds that row will reach for the spelling
// in front of them and gate on a token no backend can ever witness.
//
// The first version of this test covered only requiresFeature, leaving eight of
// the ten non-baseline gates unchecked. Before that, capability_keys.go cited
// the test while it did not exist at all. Both times the reviewer who checked
// is the only reason the gap closed, which is the argument for the scan being
// over the source rather than over a list maintained here.
func TestEveryKeyGateResolves(t *testing.T) {
	fset := token.NewFileSet()
	file, err := parser.ParseFile(fset, "capability_keys.go", nil, 0)
	if err != nil {
		t.Fatal(err)
	}
	declared := make(map[string]bool)
	for _, name := range featureIDConstantNames(t) {
		declared[name] = true
	}

	// Capability names come from the registry itself rather than from a second
	// scan for Name: fields. registerCapabilities has run by the time a test
	// does, so capabilityDescriptors is the same set CAPABILITY is derived
	// from — comparing a gate against anything else would just be a third
	// spelling to keep in step.
	advertisable := make(map[string]bool, len(capabilityDescriptors))
	for _, descriptor := range capabilityDescriptors {
		advertisable[descriptor.Name] = true
	}

	features, tokens := 0, 0
	ast.Inspect(file, func(n ast.Node) bool {
		call, ok := n.(*ast.CallExpr)
		if !ok {
			return true
		}
		ident, ok := call.Fun.(*ast.Ident)
		if !ok || len(call.Args) != 1 {
			return true
		}
		switch ident.Name {
		case "requiresFeature":
			arg, ok := call.Args[0].(*ast.Ident)
			if !ok {
				t.Errorf("%s: requiresFeature takes a featureID constant, so this gate cannot be checked",
					fset.Position(call.Pos()))
				return true
			}
			features++
			if !declared[arg.Name] {
				t.Errorf("%s: requiresFeature(%s) names no entry in featureDescriptors, so the gate "+
					"denies every session — silently, because the symptom is a NO rather than a wrong answer",
					fset.Position(call.Pos()), arg.Name)
			}
		case "requiresToken":
			arg, ok := call.Args[0].(*ast.BasicLit)
			if !ok || arg.Kind != token.STRING {
				t.Errorf("%s: requiresToken takes a literal capability name, so this gate cannot be checked",
					fset.Position(call.Pos()))
				return true
			}
			name, err := strconv.Unquote(arg.Value)
			if err != nil {
				t.Errorf("%s: %v", fset.Position(call.Pos()), err)
				return true
			}
			tokens++
			if !advertisable[name] {
				t.Errorf("%s: requiresToken(%q) names no capability descriptor, so no backend can "+
					"ever satisfy it and the key is refused to every session — silently, because the "+
					"symptom is a NO rather than a wrong answer",
					fset.Position(call.Pos()), name)
			}
		}
		return true
	})
	if features == 0 || tokens == 0 {
		t.Errorf("gates found: %d by feature, %d by token; a zero means either the gates stopped "+
			"using that constructor or this scan no longer matches it, and both make the test vacuous",
			features, tokens)
	}
}

// featureIDConstantNames returns the featureID constants declared with a
// descriptor in featureDescriptors, read from the source rather than from a
// list here.
func featureIDConstantNames(t *testing.T) []string {
	t.Helper()
	fset := token.NewFileSet()
	file, err := parser.ParseFile(fset, "capability.go", nil, 0)
	if err != nil {
		t.Fatal(err)
	}
	var names []string
	ast.Inspect(file, func(n ast.Node) bool {
		kv, ok := n.(*ast.KeyValueExpr)
		if !ok {
			return true
		}
		key, ok := kv.Key.(*ast.Ident)
		if !ok || key.Name != "ID" {
			return true
		}
		if value, ok := kv.Value.(*ast.Ident); ok {
			names = append(names, value.Name)
		}
		return true
	})
	if len(names) == 0 {
		t.Fatal("no featureDescriptors entries found; the scan is broken")
	}
	return names
}
