package imapserver

import (
	"go/ast"
	"go/parser"
	"go/token"
	"strconv"
	"strings"
	"testing"
)

// TestCapabilityWitnessCatalogue compares the public contract catalogue with
// the compiled descriptors and backend-only registry in both directions. A
// source walk also catches a new spoken routing token outside either registry.
func TestCapabilityWitnessCatalogue(t *testing.T) {
	fset := token.NewFileSet()
	file, err := parser.ParseFile(fset, "backend.go", nil, parser.ParseComments)
	if err != nil {
		t.Fatal(err)
	}
	var doc string
	for _, declaration := range file.Decls {
		decl, ok := declaration.(*ast.GenDecl)
		if !ok {
			continue
		}
		for _, spec := range decl.Specs {
			if typ, ok := spec.(*ast.TypeSpec); ok && typ.Name.Name == "CapabilitySupport" {
				doc = decl.Doc.Text()
			}
		}
	}
	if doc == "" {
		t.Fatal("CapabilitySupport has no public documentation")
	}
	type row struct{ mode, contract, states, obligation string }
	rows := make(map[string]row)
	for _, line := range strings.Split(doc, "\n") {
		fields := strings.Split(strings.TrimSpace(line), " | ")
		if len(fields) != 5 || fields[0] == "Token" {
			continue
		}
		if _, exists := rows[fields[0]]; exists {
			t.Errorf("duplicate witness row %q", fields[0])
		}
		rows[fields[0]] = row{fields[1], fields[2], fields[3], fields[4]}
	}
	type witness struct{ mode, states string }
	want := make(map[string]witness)
	declared := make(map[string]bool)
	for _, descriptor := range capabilityDescriptors {
		declared[descriptor.Name] = true
		if descriptor.RequiresBackend == nil && descriptor.Name != listRecursiveMatchWitness {
			continue
		}
		mode := "gate"
		if descriptor.RequiresBackend == nil {
			mode = "route"
		}
		var states string
		for _, phase := range []struct {
			mask stateMask
			name string
		}{{stateMaskNotAuthenticated, "N"}, {stateMaskAuthenticated, "A"}, {stateMaskSelected, "S"}} {
			if descriptor.States&phase.mask != 0 {
				states += phase.name
			}
		}
		if descriptor.RequiresTLS == tlsOnly {
			states += "/TLS"
		}
		want[descriptor.Name] = witness{mode, states}
	}
	for name := range backendFeatureWitnesses {
		want[name] = witness{"backend-only", "NAS"}
		declared[name] = true
	}
	for name, expected := range want {
		documented, ok := rows[name]
		if !ok {
			t.Errorf("backend witness %s has no public catalogue row", name)
			continue
		}
		if documented.mode != expected.mode || documented.states != expected.states {
			t.Errorf("%s documents %s/%s; registry requires %s/%s", name, documented.mode, documented.states, expected.mode, expected.states)
		}
		if documented.contract == "" || documented.obligation == "" {
			t.Errorf("%s is missing its contract or obligation", name)
		}
	}
	for name := range rows {
		if _, ok := want[name]; !ok {
			t.Errorf("catalogue names %s, which has no backend witness or routing contract", name)
		}
	}

	files := parsePublicServerFiles(t, fset)
	constants := make(map[string]ast.Expr)
	for _, file := range files {
		if file.Name.Name != "imapserver" {
			continue
		}
		for _, declaration := range file.Decls {
			decl, ok := declaration.(*ast.GenDecl)
			if !ok || decl.Tok != token.CONST {
				continue
			}
			for _, spec := range decl.Specs {
				value := spec.(*ast.ValueSpec)
				for i, name := range value.Names {
					if i < len(value.Values) {
						constants[name.Name] = value.Values[i]
					}
				}
			}
		}
	}
	for _, file := range files {
		if file.Name.Name != "imapserver" {
			continue
		}
		ast.Inspect(file, func(node ast.Node) bool {
			call, ok := node.(*ast.CallExpr)
			if !ok || len(call.Args) != 1 {
				return true
			}
			function, ok := call.Fun.(*ast.Ident)
			if !ok || function.Name != "backendSupportsCapability" {
				return true
			}
			value := call.Args[0]
			if name, ok := value.(*ast.Ident); ok {
				value = constants[name.Name]
			}
			literal, ok := value.(*ast.BasicLit)
			if !ok || literal.Kind != token.STRING {
				// Dynamic SCRAM names are covered by the compiled registry.
				return true
			}
			name, err := strconv.Unquote(literal.Value)
			if err != nil {
				t.Error(err)
				return true
			}
			if !declared[name] {
				t.Errorf("spoken token %s is not a registered capability or backend feature", name)
			}
			if _, ok := rows[name]; !ok {
				t.Errorf("spoken token %s is missing from the public catalogue", name)
			}
			return true
		})
	}
}
