//go:build interop

package interop

// Third-party software driven against our server — the outward-facing half of
// T24, and the half our own tests structurally cannot replace.
//
// Everything else that exercises imapserver was written by whoever wrote
// imapserver, from the same reading of the RFCs. That catches regressions and
// misses a shared misreading, which is exactly the failure the client-side
// interop matrix exists to catch. These tests invert the matrix: instead of our
// client against real servers, real clients against our server.
//
// The shape is the inverse of every other interop entry, and the harness in
// interop/harness models servers-in-containers only, so these do not go through
// it. What they do keep is its two standing rules:
//
//   - Absent tooling SKIPS, never fails. A permanently red matrix is a matrix
//     nobody reads (CLAUDE.md). podman missing, an image that will not build,
//     no network for the base image — all skips.
//   - A protocol failure once the client is running is OUR bug and fails, since
//     unlike a third-party server container we control both halves.

import (
	"bufio"
	"context"
	"encoding/base64"
	"fmt"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/kiliant/go-imap/interop/harness"
)

// containerRuntime is the engine these tests drive and, more importantly, how a
// container reaches back out to the host. The server under test runs in this
// process rather than in a container, so every client here has to dial the
// host, and the two engines spell that differently.
type containerRuntime struct {
	binary string
	// hostAlias resolves to the host from inside a container.
	hostAlias string
	// runArgs are engine-specific arguments needed for hostAlias to resolve.
	runArgs []string
}

// runtimeForTests resolves the container engine the same way interop/harness
// does — the IMAP_INTEROP_ENGINE override first, then podman, then docker — so
// these tests and the matrix cannot end up driving different engines on the
// same machine. The resolution is duplicated rather than shared because the
// harness keeps it unexported, and widening an exported API to serve a test is
// the wrong trade in a repository whose deliverable is API stability.
func runtimeForTests(t *testing.T) containerRuntime {
	t.Helper()
	candidates := []string{"podman", "docker"}
	if override := os.Getenv(harness.EngineEnv); override != "" {
		candidates = []string{override}
	}
	for _, candidate := range candidates {
		binary, err := exec.LookPath(candidate)
		if err != nil {
			continue
		}
		if strings.Contains(filepath.Base(candidate), "docker") {
			// Docker resolves the host under a different name, and on Linux
			// only when the mapping is asked for explicitly.
			return containerRuntime{
				binary:    binary,
				hostAlias: "host.docker.internal",
				runArgs:   []string{"--add-host", "host.docker.internal:host-gateway"},
			}
		}
		return containerRuntime{binary: binary, hostAlias: "host.containers.internal"}
	}
	t.Skipf("no container engine on PATH (looked for %s); set %s to pin one",
		strings.Join(candidates, ", "), harness.EngineEnv)
	return containerRuntime{}
}

// serverForClients starts the same server the capability matrix measures, bound
// so a container can reach it, and returns the port. Binding every interface
// rather than loopback is the one difference from the profile's own listener.
func serverForClients(t *testing.T) string {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	server, err := startOn(ctx, "0.0.0.0:0")
	if err != nil {
		cancel()
		t.Fatalf("starting the server: %v", err)
	}
	t.Cleanup(func() {
		stopCtx, stopCancel := context.WithTimeout(context.Background(), 15*time.Second)
		defer stopCancel()
		if err := server.Stop(stopCtx); err != nil {
			t.Errorf("stopping the server: %v", err)
		}
		cancel()
	})
	_, port, err := net.SplitHostPort(server.Address)
	if err != nil {
		t.Fatalf("server address %q: %v", server.Address, err)
	}
	t.Logf("server listening on %s (port %s from a container)", server.Address, port)
	return port
}

// imageOverrideEnv names a prebuilt image to use instead of building one, as
// TAG=REF pairs: "go-imap-imaptest:local=docker.io/dovecot/imaptest@sha256:…".
//
// For local troubleshooting only, and deliberately not the default. The imaptest
// image builds Dovecot from source — see its Containerfile for why no
// distribution package can substitute — which costs minutes natively and far
// more under emulation, and that turnaround is the difference between iterating
// on a race and guessing at it. CI keeps building from source, because the
// version pair the Containerfile pins is the thing the matrix is supposed to
// assert against, and dovecot/imaptest publishes only a moving :latest.
//
// Pin by digest when using this. A moving tag makes a run that passed
// unreproducible, which is the property the whole harness exists to have.
const imageOverrideEnv = "GO_IMAP_INTEROP_IMAGES"

// overriddenImage returns the replacement for tag, if one was named.
func overriddenImage(tag string) (string, bool) {
	for _, pair := range strings.Split(os.Getenv(imageOverrideEnv), ",") {
		name, ref, ok := strings.Cut(strings.TrimSpace(pair), "=")
		if ok && strings.TrimSpace(name) == tag {
			return strings.TrimSpace(ref), true
		}
	}
	return "", false
}

// buildImage builds one client image, skipping the test when it cannot. A build
// needs a working engine and network access to a base image; neither is a
// property of the server under test, so neither may turn the suite red.
func buildImage(t *testing.T, runtime containerRuntime, tag, dir string) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 40*time.Minute)
	defer cancel()
	if ref, ok := overriddenImage(tag); ok {
		t.Logf("using prebuilt %s in place of a build from %s (%s)", ref, dir, imageOverrideEnv)
		// Only pull what is not already here, and give the pull minutes rather
		// than the build's 40. The point of this path is a fast edit-run loop;
		// inheriting a timeout sized for compiling Dovecot means a registry
		// having a bad minute stalls the run instead of failing it, which cost
		// one debugging cycle before the timeout was separated out.
		if !imagePresent(ctx, runtime, ref) {
			pullCtx, cancelPull := context.WithTimeout(ctx, 5*time.Minute)
			defer cancelPull()
			pull := exec.CommandContext(pullCtx, runtime.binary, "pull", ref)
			if out, err := pull.CombinedOutput(); err != nil {
				t.Skipf("pulling %s failed:\n%s", ref, out)
			}
		}
		retag := exec.CommandContext(ctx, runtime.binary, "tag", ref, tag)
		if out, err := retag.CombinedOutput(); err != nil {
			t.Skipf("tagging %s as %s failed:\n%s", ref, tag, out)
		}
		return
	}
	command := exec.CommandContext(ctx, runtime.binary, "build", "-t", tag,
		"-f", dir+"/Containerfile", dir)
	if out, err := command.CombinedOutput(); err != nil {
		t.Skipf("building %s failed, skipping rather than reporting our server red:\n%s", tag, out)
	}
}

// runInImage runs a shell script inside an image and returns its combined
// output. The script is passed base64-encoded rather than as a bind mount:
// podman on macOS runs in a VM, so which host paths are mountable is a property
// of the developer's machine, and a test that depends on it fails for reasons
// having nothing to do with IMAP.
func runInImage(t *testing.T, runtime containerRuntime, tag, script string, timeout time.Duration) (string, error) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()
	encoded := base64.StdEncoding.EncodeToString([]byte(script))
	args := []string{"run", "--rm", "--entrypoint", "/bin/sh"}
	args = append(args, runtime.runArgs...)
	args = append(args, tag, "-c",
		"echo "+encoded+" | base64 -d > /tmp/script.sh && sh /tmp/script.sh")
	command := exec.CommandContext(ctx, runtime.binary, args...)
	out, err := command.CombinedOutput()
	if ctx.Err() != nil {
		return string(out), fmt.Errorf("timed out after %s: %w", timeout, ctx.Err())
	}
	return string(out), err
}

// seed stores count messages in mailbox over the wire, so a synchronising
// client has something to synchronise. Raw protocol rather than imapclient:
// this file is about what a foreign client sees, and seeding through our own
// client would put our own encoder on both sides of the test.
func seed(t *testing.T, port, mailbox string, count int) {
	t.Helper()
	conn, err := net.Dial("tcp", net.JoinHostPort("127.0.0.1", port))
	if err != nil {
		t.Fatalf("dialing the server: %v", err)
	}
	defer conn.Close()
	_ = conn.SetDeadline(time.Now().Add(60 * time.Second))
	reader := bufio.NewReader(conn)

	readTagged := func(tag string) string {
		for {
			line, err := reader.ReadString('\n')
			if err != nil {
				t.Fatalf("reading response for %s: %v", tag, err)
			}
			if strings.HasPrefix(line, tag+" ") {
				return strings.TrimSpace(line)
			}
		}
	}
	send := func(tag, command string) {
		if _, err := fmt.Fprintf(conn, "%s %s\r\n", tag, command); err != nil {
			t.Fatalf("writing %s: %v", command, err)
		}
		if line := readTagged(tag); !strings.HasPrefix(line, tag+" OK") {
			t.Fatalf("%s = %q", command, line)
		}
	}

	if _, err := reader.ReadString('\n'); err != nil { // greeting
		t.Fatalf("greeting: %v", err)
	}
	send("l", fmt.Sprintf("LOGIN %s %s", interopUser, interopPassword))
	if mailbox != "INBOX" {
		send("c", "CREATE "+mailbox)
	}
	for i := range count {
		message := fmt.Sprintf(
			"From: sender@example.test\r\nTo: %s\r\nSubject: seeded message %d\r\n"+
				"Message-ID: <seed-%d@example.test>\r\nDate: Mon, 10 Aug 2026 12:00:00 +0000\r\n"+
				"\r\nBody of seeded message %d.\r\n", interopUser, i, i, i)
		tag := fmt.Sprintf("a%d", i)
		if _, err := fmt.Fprintf(conn, "%s APPEND %s {%d}\r\n", tag, mailbox, len(message)); err != nil {
			t.Fatalf("APPEND: %v", err)
		}
		line, err := reader.ReadString('\n')
		if err != nil || !strings.HasPrefix(line, "+") {
			t.Fatalf("APPEND continuation = %q, %v", line, err)
		}
		if _, err := conn.Write([]byte(message + "\r\n")); err != nil {
			t.Fatalf("APPEND payload: %v", err)
		}
		if got := readTagged(tag); !strings.HasPrefix(got, tag+" OK") {
			t.Fatalf("APPEND = %q", got)
		}
	}
	send("o", "LOGOUT")
}

// imagePresent reports whether an image reference is already in local storage.
func imagePresent(ctx context.Context, runtime containerRuntime, ref string) bool {
	lookCtx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	// image inspect is shared by Docker and Podman; Podman's image exists is
	// convenient but would make the Docker path pull on every run.
	return exec.CommandContext(lookCtx, runtime.binary, "image", "inspect", ref).Run() == nil
}
