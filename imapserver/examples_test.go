package imapserver_test

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// The example programs under examples/ all carry a //go:build ignore
// constraint, so `go build ./...`, `go vet ./...` and `go test ./...` skip them
// entirely — `go list ./examples/...` matches no packages. Nothing compiles
// them unless something here does, and an example nobody compiles is an example
// that rots into a lie about the API the first time a signature changes. That
// is not hypothetical: it is why the root module grew the same gate at T14.

// examplePrograms returns the example programs and the shared helper file they
// all compile against.
func examplePrograms(t *testing.T) (programs []string, shared string) {
	t.Helper()
	entries, err := os.ReadDir("examples")
	if err != nil {
		t.Fatalf("examples directory: %v", err)
	}
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".go") {
			continue
		}
		path := filepath.Join("examples", e.Name())
		if e.Name() == "config.go" {
			shared = path
			continue
		}
		programs = append(programs, path)
	}
	return programs, shared
}

func TestExampleProgramsPresent(t *testing.T) {
	programs, shared := examplePrograms(t)
	if shared == "" {
		t.Fatal("examples/config.go is missing; the example programs share it")
	}
	// A floor rather than an exact count, so adding an example is not a test
	// edit. T25 ships five: minimal, tls, and one per optional interface
	// demonstrated.
	if len(programs) < 4 {
		t.Fatalf("expected at least 4 example programs, found %d", len(programs))
	}
}

// TestExampleProgramsCompile type-checks every runnable example against the
// current API. Each program is a package main with its own func main, so they
// must be compiled one at a time, each paired with the shared config.go.
func TestExampleProgramsCompile(t *testing.T) {
	if testing.Short() {
		t.Skip("compiling the examples invokes the go tool per program")
	}
	programs, shared := examplePrograms(t)
	if shared == "" {
		t.Fatal("examples/config.go is missing; the example programs share it")
	}
	goTool, err := exec.LookPath("go")
	if err != nil {
		t.Skipf("go tool not available: %v", err)
	}
	for _, program := range programs {
		t.Run(filepath.Base(program), func(t *testing.T) {
			t.Parallel()
			cmd := exec.Command(goTool, "vet", program, shared)
			if out, err := cmd.CombinedOutput(); err != nil {
				t.Errorf("example does not compile: %v\n%s", err, out)
			}
		})
	}
}
