package harness

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/kiliant/go-imap/interop/definition"
)

// recordingRunner fails the first n build attempts and records every command.
type recordingRunner struct {
	failures int
	commands [][]string
	err      error
}

func (r *recordingRunner) Run(_ context.Context, args ...string) (string, error) {
	r.commands = append(r.commands, args)
	if r.failures > 0 {
		r.failures--
		return "", r.err
	}
	return "", nil
}

func buildCount(runner *recordingRunner) int {
	count := 0
	for _, command := range runner.commands {
		if len(command) != 0 && command[0] == "build" {
			count++
		}
	}
	return count
}

// TestImageBuildRetriesOnceThenReportsUnavailable pins the two halves of the
// rule separately, because they fail in opposite directions.
//
// The retry exists for a podman storage race — a layer unpacked into a
// per-attempt temporary directory that a rename then cannot find — which is
// transient by construction and cost a whole scheduled Interop run. The skip
// exists for the case a retry cannot fix, a registry outage, where failing
// would report go-imap red for something go-imap did not do.
//
// Neither half may quietly become the other: a retry that never stops would
// hide a genuinely broken image reference behind a long timeout, and a skip
// without the retry spends a run's Dovecot coverage on a transient error.
func TestImageBuildRetriesOnceThenReportsUnavailable(t *testing.T) {
	t.Run("transient failure is retried and succeeds", func(t *testing.T) {
		runner := &recordingRunner{failures: 1, err: errors.New("rename tar-split.gz: no such file or directory")}
		manager := NewManager()
		manager.runner = runner
		if err := manager.buildImage(context.Background(), "img", "ctx"); err != nil {
			t.Fatalf("a failure the retry clears must not surface: %v", err)
		}
		if got := buildCount(runner); got != 2 {
			t.Errorf("build attempts = %d, want 2", got)
		}
	})

	t.Run("persistent failure is unavailable, not a hard error", func(t *testing.T) {
		cause := errors.New("connecting to registry: no route to host")
		runner := &recordingRunner{failures: 99, err: cause}
		manager := NewManager()
		manager.runner = runner
		err := manager.buildImage(context.Background(), "img", "ctx")
		if !errors.Is(err, ErrImageUnavailable) {
			t.Fatalf("a profile whose image cannot be obtained must be skippable, got %v", err)
		}
		if !errors.Is(err, cause) {
			t.Errorf("the underlying cause must survive wrapping, got %v", err)
		}
		if !strings.Contains(err.Error(), "img") {
			t.Errorf("the message must name the image, got %v", err)
		}
		if got := buildCount(runner); got != imageBuildAttempts {
			t.Errorf("build attempts = %d, want %d — an unbounded retry hides a broken image reference",
				got, imageBuildAttempts)
		}
	})

	t.Run("a cancelled context is not retried", func(t *testing.T) {
		ctx, cancel := context.WithCancel(context.Background())
		cancel()
		runner := &recordingRunner{failures: 99, err: context.Canceled}
		manager := NewManager()
		manager.runner = runner
		if err := manager.buildImage(ctx, "img", "ctx"); errors.Is(err, ErrImageUnavailable) {
			t.Error("a cancelled run is the caller giving up, not an unavailable image, " +
				"and reporting it as skippable would hide a teardown bug")
		}
		if got := buildCount(runner); got != 1 {
			t.Errorf("build attempts after cancellation = %d, want 1", got)
		}
	})
}

// TestStartProfilesSkipsUnavailableImages checks the half that decides whether
// the suite goes red: an unavailable image drops its profile, while any other
// start failure still fails the run.
//
// The two cases differ by which command the fake fails, not by the error value,
// because that is the real distinction. A profile with a BuildContext fails
// inside buildImage and is wrapped; a profile with a pinned Image never builds,
// so its failure comes from "run" and must stay fatal — a container that starts
// and dies is a server problem, which is exactly what this matrix is for.
func TestStartProfilesSkipsUnavailableImages(t *testing.T) {
	building := definition.Profile{
		Name:          "builds",
		BuildContext:  "testdata/does-not-matter",
		ContainerPort: 143,
		Tier:          definition.TierNativeBuild,
	}
	pinned := definition.Profile{
		Name:          "pinned",
		Image:         "example.test/server:v1",
		ContainerPort: 143,
		Tier:          definition.TierNativeImage,
	}

	t.Run("unavailable image is skipped", func(t *testing.T) {
		manager := NewManager()
		manager.runner = &recordingRunner{failures: 99, err: errors.New("no route to host")}
		servers, err := startProfiles(context.Background(), manager, []definition.Profile{building})
		if err != nil {
			t.Fatalf("an unavailable image must not fail the suite: %v", err)
		}
		if len(servers) != 0 {
			t.Fatalf("servers = %d, want 0", len(servers))
		}
		for _, server := range servers {
			if server == nil {
				t.Fatal("a skipped profile must be compacted out, not left as a nil hole " +
					"for every caller that ranges over the result to dereference")
			}
		}
	})

	t.Run("other start failures still fail", func(t *testing.T) {
		manager := NewManager()
		manager.runner = &recordingRunner{failures: 99, err: errors.New("container exited immediately")}
		if _, err := startProfiles(context.Background(), manager, []definition.Profile{pinned}); err == nil {
			t.Fatal("a start failure that is not an image problem must not be skipped")
		} else if errors.Is(err, ErrImageUnavailable) {
			t.Fatalf("a run failure was misreported as an image problem: %v", err)
		}
	})
}
