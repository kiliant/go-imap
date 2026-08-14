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
	"os/exec"
	"strings"
	"testing"
	"time"
)

// containerHost is the name podman resolves to the host from inside a
// container. The server under test runs in this process, not in a container, so
// every client here has to reach back out.
const containerHost = "host.containers.internal"

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
	t.Logf("server listening on %s (%s:%s from a container)", server.Address, containerHost, port)
	return port
}

// buildImage builds one client image, skipping the test when it cannot. A build
// needs a working podman and network access to a base image; neither is a
// property of the server under test, so neither may turn the suite red.
func buildImage(t *testing.T, tag, dir string) {
	t.Helper()
	if _, err := exec.LookPath("podman"); err != nil {
		t.Skipf("podman is not installed: %v", err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Minute)
	defer cancel()
	command := exec.CommandContext(ctx, "podman", "build", "-t", tag,
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
func runInImage(t *testing.T, tag, script string, timeout time.Duration) (string, error) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()
	encoded := base64.StdEncoding.EncodeToString([]byte(script))
	command := exec.CommandContext(ctx, "podman", "run", "--rm",
		"--entrypoint", "/bin/sh", tag, "-c",
		"echo "+encoded+" | base64 -d > /tmp/script.sh && sh /tmp/script.sh")
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
