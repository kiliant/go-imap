package harness

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
)

// EngineEnv names the environment variable that pins the container engine.
//
// Its value is either a bare binary name ("docker", "podman") or an absolute
// path to one. When unset, the harness probes PATH for podman first and docker
// second, so a machine with both keeps the podman behaviour it had before this
// override existed.
const EngineEnv = "IMAP_INTEROP_ENGINE"

// engineKind selects the CLI dialect spoken to the resolved binary.
//
// The two dialects are near-identical for everything this harness invokes —
// build --tag, run --detach/--name/--publish/--env, exec, inspect --format,
// port, logs, rm --force are spelled the same — but not identical, so the
// binary name alone is not enough. See translateForDocker.
type engineKind int

const (
	enginePodman engineKind = iota
	engineDocker
)

func (k engineKind) String() string {
	if k == engineDocker {
		return "docker"
	}
	return "podman"
}

// containerEngine is a resolved engine binary plus the dialect it speaks.
type containerEngine struct {
	binary string
	kind   engineKind
}

// preferredEngines is the probe order: podman first, so that a host with both
// installed behaves exactly as it did before docker was supported at all.
var preferredEngines = []string{"podman", "docker"}

var detectEngineOnce = sync.OnceValues(func() (containerEngine, error) {
	return resolveEngine(os.Getenv(EngineEnv), exec.LookPath)
})

// resolveEngine picks the engine binary. It is separated from the environment
// and from PATH so that it is testable without either.
func resolveEngine(override string, lookPath func(string) (string, error)) (containerEngine, error) {
	if override = strings.TrimSpace(override); override != "" {
		binary := override
		if !strings.ContainsRune(override, os.PathSeparator) {
			resolved, err := lookPath(override)
			if err != nil {
				return containerEngine{}, fmt.Errorf("interop: %s=%q not found on PATH: %w", EngineEnv, override, err)
			}
			binary = resolved
		}
		return containerEngine{binary: binary, kind: kindForBinary(binary)}, nil
	}
	for _, candidate := range preferredEngines {
		if resolved, err := lookPath(candidate); err == nil {
			return containerEngine{binary: resolved, kind: kindForBinary(candidate)}, nil
		}
	}
	return containerEngine{}, fmt.Errorf("interop: no container engine on PATH (looked for %s); install one or set %s",
		strings.Join(preferredEngines, ", "), EngineEnv)
}

// kindForBinary infers the CLI dialect from the binary's name. Anything that is
// not recognisably docker is assumed podman-compatible, which is the dialect
// the harness writes natively.
func kindForBinary(binary string) engineKind {
	name := strings.ToLower(filepath.Base(binary))
	name = strings.TrimSuffix(name, ".exe")
	if name == "docker" || strings.HasPrefix(name, "docker-") {
		return engineDocker
	}
	return enginePodman
}

// valueFlags lists the flags this harness passes that take a separate value
// argument. Translation walks the leading flag block of a command line and
// needs to know which tokens are flag values rather than the first positional
// argument (an image name, a container ID, or the start of an exec command).
var valueFlags = map[string]bool{
	"--arch":     true,
	"--env":      true,
	"--format":   true,
	"--name":     true,
	"--platform": true,
	"--publish":  true,
	"--tag":      true,
	"--time":     true,
}

// translateArgs rewrites a podman command line for the target dialect.
//
// For podman it is the identity function: the harness speaks podman natively,
// and existing hosts must execute byte-for-byte what they executed before.
func translateArgs(kind engineKind, args []string) []string {
	if kind != engineDocker {
		return args
	}
	return translateForDocker(args)
}

// translateForDocker applies the two incompatibilities that matter here:
//
//	run --arch <a>       ->  run --platform linux/<a>   (docker has no --arch)
//	rm --force --time N  ->  rm --force                 (docker rm has no --time;
//	                                                     its removal is immediate)
//
// Translation stops at the first positional argument, so a profile's own
// container arguments — which follow the image name and are not ours to
// reinterpret — are passed through untouched.
func translateForDocker(args []string) []string {
	if len(args) == 0 {
		return args
	}
	subcommand := args[0]
	out := make([]string, 0, len(args)+1)
	out = append(out, subcommand)

	i := 1
	for ; i < len(args); i++ {
		arg := args[i]
		if !strings.HasPrefix(arg, "-") {
			break // image name, container ID, or the start of an exec command.
		}
		name, inlineValue, hasInline := strings.Cut(arg, "=")
		switch {
		case name == "--arch":
			value := inlineValue
			if !hasInline {
				if i+1 >= len(args) {
					out = append(out, arg)
					continue
				}
				i++
				value = args[i]
			}
			out = append(out, "--platform", "linux/"+value)
		case name == "--time" && subcommand == "rm":
			if !hasInline {
				i++ // drop the value too
			}
		default:
			out = append(out, arg)
			if !hasInline && valueFlags[name] && i+1 < len(args) {
				i++
				out = append(out, args[i])
			}
		}
	}
	return append(out, args[i:]...)
}
