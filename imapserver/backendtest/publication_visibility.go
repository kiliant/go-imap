package backendtest

import (
	"context"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/kiliant/go-imap"
	"github.com/kiliant/go-imap/imapserver"
)

func runCopyPublication(t *testing.T, harness *Harness) {
	instance, source := newSession(t, harness)
	defer closeSession(t, instance, source)
	name := populate(t, source, "copy-publication")
	selected := selectMailbox(t, source, name, discardUpdater())
	defer selected.Mailbox.Unselect(t.Context(), nil)
	dest := name + "-dest"
	if err := source.Create(t.Context(), dest, nil); err != nil {
		t.Fatal(err)
	}
	var mu sync.Mutex
	var batches [2][]*imapserver.UpdateBatch
	for i := range batches {
		_, watcher := authenticate(t, instance)
		defer closeSession(t, instance, watcher)
		result := selectMailbox(t, watcher, dest, &imapserver.Updater{PushFunc: func(batch *imapserver.UpdateBatch) error {
			mu.Lock()
			defer mu.Unlock()
			batches[i] = append(batches[i], cloneBatch(batch))
			return nil
		}})
		defer result.Mailbox.Unselect(t.Context(), nil)
	}
	const origin imapserver.ChangeToken = 750
	if _, err := selected.Mailbox.Copy(t.Context(), imap.UIDSetNum(selected.Snapshot.UIDs[0]), dest, &imapserver.CopyOptions{MutationOptions: imapserver.MutationOptions{Origin: origin}}); err != nil {
		t.Fatal(err)
	}
	mu.Lock()
	defer mu.Unlock()
	for i, updates := range batches {
		found := false
		for _, batch := range updates {
			if batch.Origin == origin && soleAddedUID(batch) != 0 {
				found = true
			}
		}
		if !found {
			t.Fatalf("COPY returned before publishing its destination batch to observer %d", i)
		}
	}
}

// Hold the publication callback open while independent sessions read STATUS
// and SELECT. New state must not escape the backend's publication fence.
// The mutation has no framework origin, as with an externally initiated write.
func runPublicationVisibility(t *testing.T, harness *Harness) {
	instance, watcher := newSession(t, harness)
	defer closeSession(t, instance, watcher)
	_, mutator := authenticate(t, instance)
	defer closeSession(t, instance, mutator)
	_, statusReader := authenticate(t, instance)
	defer closeSession(t, instance, statusReader)
	_, selectReader := authenticate(t, instance)
	defer closeSession(t, instance, selectReader)
	name := populate(t, watcher, "visibility")
	entered, release := make(chan struct{}), make(chan struct{})
	var started, published atomic.Bool
	var armed atomic.Bool
	var once sync.Once
	unblock := func() { once.Do(func() { close(release) }) }
	defer unblock()
	selection := selectMailbox(t, watcher, name, &imapserver.Updater{PushFunc: func(*imapserver.UpdateBatch) error {
		if armed.Load() && started.CompareAndSwap(false, true) {
			close(entered)
			<-release
			published.Store(true)
		}
		return nil
	}})
	defer selection.Mailbox.Unselect(t.Context(), nil)
	defer unblock()
	armed.Store(true)
	mutation := make(chan error, 1)
	go func() {
		_, err := mutator.Append(t.Context(), name, strings.NewReader("Subject: external\r\n\r\nbody\r\n"), nil)
		mutation <- err
	}()
	select {
	case <-entered:
	case <-time.After(5 * time.Second):
		t.Fatal("mutation did not publish")
	}
	type observed struct {
		operation string
		count     uint64
		published bool
		err       error
		result    *imapserver.SelectResult
	}
	reads := make(chan observed, 2)
	go func() {
		data, err := statusReader.Status(t.Context(), name, &imapserver.StatusOptions{Items: []imap.StatusItem{imap.StatusItemMessages}})
		n, _ := data.Number(imap.StatusItemMessages)
		reads <- observed{operation: "STATUS", count: n, published: published.Load(), err: err}
	}()
	go func() {
		result, err := selectReader.Select(t.Context(), name, discardUpdater(), nil)
		var n uint64
		if result != nil {
			n = uint64(len(result.Snapshot.UIDs))
		}
		reads <- observed{operation: "SELECT", count: n, published: published.Load(), err: err, result: result}
	}()
	var observations []observed
	timer := time.NewTimer(20 * time.Millisecond)
	defer timer.Stop()
wait:
	for len(observations) < 2 {
		select {
		case r := <-reads:
			observations = append(observations, r)
		case <-timer.C:
			break wait
		}
	}
	unblock()
	if err := <-mutation; err != nil {
		t.Fatal(err)
	}
	for len(observations) < 2 {
		observations = append(observations, <-reads)
	}
	for _, r := range observations {
		if r.result != nil && r.result.Mailbox != nil {
			_ = r.result.Mailbox.Unselect(context.Background(), nil)
		}
		if r.err != nil {
			t.Errorf("%s: %v", r.operation, r.err)
		}
		if r.count > 2 && !r.published {
			t.Errorf("%s exposed a commit before publication completed", r.operation)
		}
	}
}
