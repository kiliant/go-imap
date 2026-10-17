package imapserver_test

import (
	"os"
	"slices"
	"testing"
)

// Capture the shared namespace before TestMain installs the package's private
// one. A different package process may leave a spool here while this suite runs.
var sharedTestTempDir = os.TempDir()

func TestSpoolScannerUsesPrivateNamespace(t *testing.T) {
	foreign, err := os.CreateTemp(sharedTestTempDir, "go-imap-fetch-foreign-*")
	if err != nil {
		t.Fatal(err)
	}
	if err := foreign.Close(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Remove(foreign.Name()) })
	if slices.Contains(tempSpoolFiles(), foreign.Name()) {
		t.Error("scanner sees a spool owned by another package process")
	}
	own, err := os.CreateTemp("", "go-imap-fetch-owned-*")
	if err != nil {
		t.Fatal(err)
	}
	if err := own.Close(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Remove(own.Name()) })
	if !slices.Contains(tempSpoolFiles(), own.Name()) {
		t.Error("scanner no longer detects this package's spool files")
	}
}
