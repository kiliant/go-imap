package maildir

import (
	"bufio"
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/kiliant/go-imap/imapserver"
)

func TestCloseLifecycle(t *testing.T) {
	ctx := context.Background()
	root := t.TempDir()
	openCtx, cancelOpen := context.WithCancel(ctx)
	b, err := Open(openCtx, root, testOptions())
	if err != nil {
		t.Fatal(err)
	}
	cancelOpen()
	s := loginTest(t, b)
	sel := selectTest(t, s, "INBOX")
	canceled, cancel := context.WithCancel(ctx)
	cancel()
	if err := b.Close(canceled, nil); !errors.Is(err, context.Canceled) {
		t.Fatal(err)
	}
	appendTest(t, s, "INBOX") // Neither Open's old context nor pre-canceled Close stopped it.
	if other, err := Open(ctx, root, testOptions()); err == nil {
		other.Close(ctx, nil)
		t.Fatal("second root owner accepted")
	}
	entered, release := make(chan struct{}), make(chan struct{})
	finished := make(chan error, 1)
	go func() {
		_, err := s.Append(ctx, "INBOX", &blockingReader{entered: entered, release: release}, nil)
		finished <- err
	}()
	<-entered
	deadline, cancelDeadline := context.WithTimeout(ctx, 20*time.Millisecond)
	defer cancelDeadline()
	if err := b.Close(deadline, nil); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("Close did not bound its wait: %v", err)
	}
	if _, err := s.Status(ctx, "INBOX", nil); err == nil {
		t.Fatal("new work accepted during shutdown")
	}
	if other, err := Open(ctx, root, testOptions()); err == nil {
		other.Close(ctx, nil)
		t.Fatal("lock released before operation finished")
	}
	close(release)
	if err := <-finished; err != nil {
		t.Fatal(err)
	}
	if err := b.Close(ctx, nil); err != nil {
		t.Fatal(err)
	}
	if err := sel.Unselect(ctx, nil); err != nil {
		t.Fatal(err)
	}
	if err := s.Close(ctx, nil); err != nil {
		t.Fatal(err)
	}
	reopened := openTest(t, root)
	loginTest(t, reopened)
}

type blockingReader struct {
	entered, release chan struct{}
	once             sync.Once
}

func (r *blockingReader) Read(p []byte) (int, error) {
	r.once.Do(func() { close(r.entered); <-r.release })
	return 0, io.EOF
}

func TestPollDeliveryConcurrentSelectAndCallbackShutdown(t *testing.T) {
	ctx := context.Background()
	root := t.TempDir()
	callbackEntered, callbackRelease := make(chan struct{}), make(chan struct{})
	var once sync.Once
	opts := testOptions()
	opts.PollInterval = 5 * time.Millisecond
	opts.OnPollError = func(error) { once.Do(func() { close(callbackEntered) }); <-callbackRelease }
	b, err := Open(ctx, root, opts)
	if err != nil {
		t.Fatal(err)
	}
	defer b.Close(ctx, nil)
	var wg sync.WaitGroup
	for i := 0; i < 16; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			s := loginTest(t, b)
			var mu sync.Mutex
			var batches []*imapserver.UpdateBatch
			result, err := s.Select(ctx, "INBOX", &imapserver.Updater{PushFunc: func(batch *imapserver.UpdateBatch) error {
				mu.Lock()
				defer mu.Unlock()
				batches = append(batches, batch)
				return nil
			}}, nil)
			if err != nil {
				t.Error(err)
				return
			}
			deadline := time.Now().Add(3 * time.Second)
			for time.Now().Before(deadline) {
				mu.Lock()
				got := len(batches)
				mu.Unlock()
				if len(result.Snapshot.UIDs) > 0 || got > 0 {
					break
				}
				time.Sleep(time.Millisecond)
			}
			mu.Lock()
			defer mu.Unlock()
			revision := result.Snapshot.Revision
			count := len(result.Snapshot.UIDs)
			for _, batch := range batches {
				if batch.Before != revision {
					t.Errorf("broken revision chain: %s != %s", batch.Before, revision)
				}
				revision = batch.After
				for _, update := range batch.Changes {
					if add, ok := update.(*imapserver.UpdateAdd); ok {
						count += len(add.UIDs)
					}
				}
			}
			if count != 1 {
				t.Errorf("snapshot+updates contains %d deliveries, want 1", count)
			}
			if err := s.Close(ctx, nil); err != nil {
				t.Error(err)
			}
		}()
	}
	deliveryDir := filepath.Join(accountPath(root), mailboxPath("INBOX"), "new")
	if err := os.WriteFile(filepath.Join(deliveryDir, "external"), []byte("Subject: external\n\nbody\n"), 0600); err != nil {
		t.Fatal(err)
	}
	wg.Wait()
	if err := os.Symlink(t.TempDir(), filepath.Join(deliveryDir, "bad-link")); err != nil {
		t.Fatal(err)
	}
	select {
	case <-callbackEntered:
	case <-time.After(3 * time.Second):
		t.Fatal("poll error was not reported")
	}
	if b.LastPollError() == nil {
		t.Fatal("nil cached polling error")
	}
	// The callback runs outside account/backend locks: an operation still succeeds.
	s := loginTest(t, b)
	if _, err := s.Status(ctx, "INBOX", nil); err != nil {
		t.Fatal(err)
	}
	deadline, cancel := context.WithTimeout(ctx, 20*time.Millisecond)
	defer cancel()
	if err := b.Close(deadline, nil); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("Close did not wait for callback: %v", err)
	}
	close(callbackRelease)
	if err := b.Close(ctx, nil); err != nil {
		t.Fatal(err)
	}
}

func TestLockChild(t *testing.T) {
	root := os.Getenv("GO_IMAP_MAILDIR_LOCK_CHILD")
	if root == "" {
		return
	}
	b, err := Open(context.Background(), root, testOptions())
	if err != nil {
		t.Fatal(err)
	}
	defer b.Close(context.Background(), nil)
	fmt.Println("LOCKED")
	select {}
}
func TestRootLockReleasedAfterProcessDeath(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	root := t.TempDir()
	cmd := exec.CommandContext(ctx, os.Args[0], "-test.run=^TestLockChild$")
	cmd.Env = append(os.Environ(), "GO_IMAP_MAILDIR_LOCK_CHILD="+root)
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	cmd.Stderr = os.Stderr
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	defer func() {
		if cmd.Process != nil {
			cmd.Process.Kill()
			cmd.Wait()
		}
	}()
	line, err := bufio.NewReader(stdout).ReadString('\n')
	if err != nil || line != "LOCKED\n" {
		t.Fatalf("child startup: %q, %v", line, err)
	}
	if value, err := Open(ctx, root, testOptions()); err == nil {
		value.Close(ctx, nil)
		t.Fatal("lock not exclusive across processes")
	}
	if err := cmd.Process.Kill(); err != nil {
		t.Fatal(err)
	}
	_ = cmd.Wait()
	cmd.Process = nil
	b := openTest(t, root)
	loginTest(t, b)
}

// TestSlowAppendDoesNotStallOtherAccountsDelivery pins delivery isolation. A
// single poller waited on each account's lock in turn, and APPEND held its
// account's lock while reading the client's literal, so one slow client
// stopped new-mail delivery for every account.
func TestSlowAppendDoesNotStallOtherAccountsDelivery(t *testing.T) {
	ctx := context.Background()
	root := t.TempDir()
	opts := testOptions()
	opts.Accounts = append(opts.Accounts, AccountOptions{Username: "bob", Password: "bob-secret"})
	opts.PollInterval = 5 * time.Millisecond
	b, err := Open(ctx, root, opts)
	if err != nil {
		t.Fatal(err)
	}
	defer b.Close(ctx, nil)
	bob, err := b.Authenticate(ctx, nil, &imapserver.Credentials{Mechanism: "PLAIN", Username: "bob", Password: "bob-secret"}, nil)
	if err != nil {
		t.Fatal(err)
	}
	added := make(chan struct{}, 1)
	if _, err := bob.Select(ctx, "INBOX", &imapserver.Updater{PushFunc: func(batch *imapserver.UpdateBatch) error {
		for _, change := range batch.Changes {
			if _, ok := change.(*imapserver.UpdateAdd); ok {
				select {
				case added <- struct{}{}:
				default:
				}
			}
		}
		return nil
	}}, nil); err != nil {
		t.Fatal(err)
	}

	entered, release := make(chan struct{}), make(chan struct{})
	finished := make(chan error, 1)
	go func() {
		_, err := loginTest(t, b).Append(ctx, "INBOX", &blockingReader{entered: entered, release: release}, nil)
		finished <- err
	}()
	<-entered
	defer func() {
		close(release)
		if err := <-finished; err != nil {
			t.Error(err)
		}
	}()

	// Several poll intervals, so a poller that waits on alice's lock is already
	// stuck there: map order would otherwise let one tick reach bob first.
	time.Sleep(20 * opts.PollInterval)
	bobINBOX := filepath.Join(root, fmt.Sprintf("%x", sha256.Sum256([]byte("bob"))), mailboxPath("INBOX"), "new")
	if err := os.WriteFile(filepath.Join(bobINBOX, "external"), []byte("Subject: external\n\nbody\n"), 0600); err != nil {
		t.Fatal(err)
	}
	select {
	case <-added:
	case <-time.After(3 * time.Second):
		t.Fatal("bob's delivery waited for alice's APPEND")
	}
}
