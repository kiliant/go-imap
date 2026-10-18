package maildir

import (
	"context"
	"errors"
	"io"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/kiliant/go-imap"
	"github.com/kiliant/go-imap/imapserver"
)

func TestFetchReleasesAccountWhileStreaming(t *testing.T) {
	ctx := context.Background()
	b := openTest(t, t.TempDir())
	reading, mutating := loginTest(t, b), loginTest(t, b)
	if err := reading.Create(ctx, "source", nil); err != nil {
		t.Fatal(err)
	}
	first := appendTest(t, reading, "source").UID
	second := appendTest(t, reading, "source").UID
	third := appendTest(t, reading, "source").UID
	sel, other := selectTest(t, reading, "source"), selectTest(t, mutating, "source")
	var got []imap.UID
	writer := &imapserver.FetchWriter{WriteFunc: func(ctx context.Context, data *imap.FetchMessageData) error {
		uid := imap.UID(data.Items["UID"][0].(imap.FetchDataUID))
		got = append(got, uid)
		if uid == first {
			// The writer stands in for a slow network client. A different
			// session must make progress before this literal is consumed.
			mutationCtx, cancel := context.WithTimeout(ctx, time.Second)
			defer cancel()
			if err := other.Store(mutationCtx, discardFetch(), imap.UIDSetNum(first, second), &imapserver.StoreFlags{Op: imapserver.StoreFlagsAdd, Flags: []imap.Flag{imap.FlagDeleted}}, nil); err != nil {
				return err
			}
			if err := mutating.Rename(mutationCtx, "source", "renamed", nil); err != nil {
				return err
			}
			if err := other.Expunge(mutationCtx, discardExpunge(), nil, nil); err != nil {
				return err
			}
			if _, err := mutating.Append(mutationCtx, "renamed", strings.NewReader(sampleMessage), nil); err != nil {
				return err
			}
			if flags := []imap.Flag(data.Items["FLAGS"][0].(imap.FetchDataFlags)); len(flags) != 0 {
				t.Errorf("in-flight FETCH flags changed with STORE: %v", flags)
			}
		}
		section := data.Items["BODY[]"][0].(*imap.FetchDataBodySection)
		body, err := io.ReadAll(section.Literal)
		if err == nil && string(body) != sampleMessage {
			t.Errorf("UID %d body = %q", uid, body)
		}
		return err
	}}
	options := &imapserver.FetchOptions{Items: []imap.FetchItem{imap.FetchItemUID, imap.FetchItemFlags, &imap.FetchItemBodySection{Peek: true}}}
	if err := sel.Fetch(ctx, writer, imap.UIDSetNum(first, second, third), options); err != nil {
		t.Fatal(err)
	}
	// The open first source survived rename and unlink, the second vanished
	// before opening, the third followed the mailbox rename, and a new UID
	// was not pulled into the request's original membership.
	if want := []imap.UID{first, third}; !reflect.DeepEqual(got, want) {
		t.Fatalf("FETCH UIDs = %v, want %v", got, want)
	}
}

func TestCloseWaitsForUnlockedFetch(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	b := openTest(t, t.TempDir())
	t.Cleanup(cancel)
	s := loginTest(t, b)
	first := appendTest(t, s, "INBOX").UID
	second := appendTest(t, s, "INBOX").UID
	sel := selectTest(t, s, "INBOX")
	started, resume := make(chan struct{}), make(chan struct{})
	var got []imap.UID
	writer := &imapserver.FetchWriter{WriteFunc: func(ctx context.Context, data *imap.FetchMessageData) error {
		got = append(got, imap.UID(data.Items["UID"][0].(imap.FetchDataUID)))
		if len(got) == 1 {
			close(started)
			select {
			case <-resume:
			case <-ctx.Done():
				return ctx.Err()
			}
		}
		_, err := io.Copy(io.Discard, data.Items["BODY[]"][0].(*imap.FetchDataBodySection).Literal)
		return err
	}}
	done := make(chan error, 1)
	go func() {
		done <- sel.Fetch(ctx, writer, imap.UIDSetNum(first, second), &imapserver.FetchOptions{Items: []imap.FetchItem{imap.FetchItemUID, &imap.FetchItemBodySection{Peek: true}}})
	}()
	select {
	case <-started:
	case <-ctx.Done():
		t.Fatal(ctx.Err())
	}
	closeCtx, closeCancel := context.WithTimeout(ctx, 50*time.Millisecond)
	err := b.Close(closeCtx, nil)
	closeCancel()
	close(resume)
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("Close while FETCH writer blocked = %v, want deadline exceeded", err)
	}
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-ctx.Done():
		t.Fatal(ctx.Err())
	}
	if want := []imap.UID{first, second}; !reflect.DeepEqual(got, want) {
		t.Fatalf("in-flight FETCH UIDs during Close = %v, want %v", got, want)
	}
	if err := b.Close(ctx, nil); err != nil {
		t.Fatal(err)
	}
}

func TestFetchWriterFailureLeavesAccountUsable(t *testing.T) {
	for _, canceled := range []bool{false, true} {
		t.Run(map[bool]string{false: "writer-error", true: "canceled-body"}[canceled], func(t *testing.T) {
			b := openTest(t, t.TempDir())
			s := loginTest(t, b)
			uid := appendTest(t, s, "INBOX").UID
			sel := selectTest(t, s, "INBOX")
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			want := errors.New("writer failed")
			if canceled {
				want = context.Canceled
			}
			writer := &imapserver.FetchWriter{WriteFunc: func(ctx context.Context, data *imap.FetchMessageData) error {
				if !canceled {
					return want
				}
				cancel()
				_, err := io.Copy(io.Discard, data.Items["BODY[]"][0].(*imap.FetchDataBodySection).Literal)
				return err
			}}
			if err := sel.Fetch(ctx, writer, imap.UIDSetNum(uid), &imapserver.FetchOptions{Items: []imap.FetchItem{&imap.FetchItemBodySection{Peek: true}}}); !errors.Is(err, want) {
				t.Fatalf("Fetch = %v, want %v", err, want)
			}
			mutationCtx, mutationCancel := context.WithTimeout(context.Background(), time.Second)
			defer mutationCancel()
			if _, err := s.Append(mutationCtx, "INBOX", strings.NewReader(sampleMessage), nil); err != nil {
				t.Fatalf("account unusable after FETCH failure: %v", err)
			}
		})
	}
}
