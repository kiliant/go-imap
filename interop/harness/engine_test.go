package harness

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func lookPathIn(available ...string) func(string) (string, error) {
	set := make(map[string]bool, len(available))
	for _, name := range available {
		set[name] = true
	}
	return func(name string) (string, error) {
		if set[name] {
			return filepath.Join("/usr/bin", name), nil
		}
		return "", errors.New("not found")
	}
}

func TestResolveEnginePrefersPodman(t *testing.T) {
	engine, err := resolveEngine("", lookPathIn("podman", "docker"))
	if err != nil {
		t.Fatal(err)
	}
	if engine.binary != "/usr/bin/podman" || engine.kind != enginePodman {
		t.Fatalf("engine = %+v, want podman", engine)
	}
}

func TestResolveEngineFallsBackToDocker(t *testing.T) {
	engine, err := resolveEngine("", lookPathIn("docker"))
	if err != nil {
		t.Fatal(err)
	}
	if engine.binary != "/usr/bin/docker" || engine.kind != engineDocker {
		t.Fatalf("engine = %+v, want docker", engine)
	}
}

func TestResolveEngineOverride(t *testing.T) {
	// An explicit override beats the probe order, even when podman is present.
	engine, err := resolveEngine("docker", lookPathIn("podman", "docker"))
	if err != nil {
		t.Fatal(err)
	}
	if engine.binary != "/usr/bin/docker" || engine.kind != engineDocker {
		t.Fatalf("engine = %+v, want docker", engine)
	}

	// A path is taken as-is: it need not be on PATH, and its base name still
	// decides the dialect.
	absolute := filepath.Join(string(os.PathSeparator), "opt", "bin", "docker")
	engine, err = resolveEngine(absolute, lookPathIn())
	if err != nil {
		t.Fatal(err)
	}
	if engine.binary != absolute || engine.kind != engineDocker {
		t.Fatalf("engine = %+v, want %s/docker", engine, absolute)
	}

	// An unknown binary name is assumed podman-compatible, which is the dialect
	// the harness emits natively.
	engine, err = resolveEngine("nerdctl", lookPathIn("nerdctl"))
	if err != nil {
		t.Fatal(err)
	}
	if engine.kind != enginePodman {
		t.Fatalf("kind = %v, want podman dialect", engine.kind)
	}
}

func TestResolveEngineErrors(t *testing.T) {
	if _, err := resolveEngine("", lookPathIn()); err == nil {
		t.Fatal("missing engine accepted")
	} else if !strings.Contains(err.Error(), EngineEnv) {
		t.Fatalf("error does not name the override: %v", err)
	}
	if _, err := resolveEngine("podperson", lookPathIn("podman")); err == nil {
		t.Fatal("unresolvable override accepted")
	}
}

func TestTranslateArgsPodmanIsIdentity(t *testing.T) {
	// Hosts that had podman before this indirection existed must keep executing
	// exactly the same command line.
	args := []string{"run", "--detach", "--arch", "amd64", "img", "--time", "5"}
	got := translateArgs(enginePodman, args)
	if len(got) != len(args) {
		t.Fatalf("podman args rewritten: %q", got)
	}
	for i := range args {
		if got[i] != args[i] {
			t.Fatalf("podman args rewritten: %q", got)
		}
	}
}

func TestTranslateForDocker(t *testing.T) {
	for _, tc := range []struct {
		name string
		in   []string
		want []string
	}{{
		name: "arch becomes platform",
		in:   []string{"run", "--detach", "--name", "c", "--publish", "127.0.0.1::143", "--arch", "amd64", "--env", "A=b", "img"},
		want: []string{"run", "--detach", "--name", "c", "--publish", "127.0.0.1::143", "--platform", "linux/amd64", "--env", "A=b", "img"},
	}, {
		name: "inline arch",
		in:   []string{"run", "--arch=amd64", "img"},
		want: []string{"run", "--platform", "linux/amd64", "img"},
	}, {
		name: "rm time dropped",
		in:   []string{"rm", "--force", "--time", "2", "abc123"},
		want: []string{"rm", "--force", "abc123"},
	}, {
		name: "inline rm time dropped",
		in:   []string{"rm", "--force", "--time=2", "abc123"},
		want: []string{"rm", "--force", "abc123"},
	}, {
		name: "native spellings untouched",
		in:   []string{"inspect", "--format", "{{.State.Status}}", "abc123"},
		want: []string{"inspect", "--format", "{{.State.Status}}", "abc123"},
	}, {
		name: "build untouched",
		in:   []string{"build", "--tag", "localhost/x:v1", "/ctx"},
		want: []string{"build", "--tag", "localhost/x:v1", "/ctx"},
	}, {
		name: "container arguments are not reinterpreted",
		in:   []string{"run", "--detach", "--arch", "amd64", "img", "serve", "--arch", "riscv", "--time", "9"},
		want: []string{"run", "--detach", "--platform", "linux/amd64", "img", "serve", "--arch", "riscv", "--time", "9"},
	}, {
		name: "exec command untouched",
		in:   []string{"exec", "abc123", "sh", "-c", "doveadm reload --time 1"},
		want: []string{"exec", "abc123", "sh", "-c", "doveadm reload --time 1"},
	}, {
		name: "time outside rm survives",
		in:   []string{"stop", "--time", "2", "abc123"},
		want: []string{"stop", "--time", "2", "abc123"},
	}, {
		name: "empty",
		in:   nil,
		want: nil,
	}} {
		t.Run(tc.name, func(t *testing.T) {
			got := translateForDocker(tc.in)
			if strings.Join(got, "\x00") != strings.Join(tc.want, "\x00") {
				t.Fatalf("got %q, want %q", got, tc.want)
			}
		})
	}
}

// TestTranslateForDockerCoversHarnessCommands pins the translation against the
// command lines the harness actually builds. If Start grows a podman-only flag,
// this is the test that should be extended alongside it.
func TestTranslateForDockerCoversHarnessCommands(t *testing.T) {
	for _, args := range [][]string{
		{"logs", "abc123"},
		{"port", "abc123", "143/tcp"},
		{"inspect", "--format", "{{.State.Status}}", "abc123"},
		{"build", "--tag", "localhost/go-imap-interop-cyrus:v1", "/repo/interop/servers/cyrus"},
	} {
		got := translateForDocker(args)
		if strings.Join(got, "\x00") != strings.Join(args, "\x00") {
			t.Fatalf("%q rewritten to %q; docker spells it the same", args, got)
		}
	}
}
