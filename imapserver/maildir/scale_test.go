package maildir

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/kiliant/go-imap"
	"github.com/kiliant/go-imap/imapserver"
)

func BenchmarkMaildirScale(b *testing.B) {
	for _, count := range []int{1000, 20000} {
		b.Run(fmt.Sprint(count), func(b *testing.B) {
			ctx := context.Background()
			backend, err := Open(ctx, b.TempDir(), testOptions())
			if err != nil {
				b.Fatal(err)
			}
			b.Cleanup(func() {
				if err := backend.Close(ctx, nil); err != nil {
					b.Error(err)
				}
			})
			sessionAPI, err := backend.Authenticate(ctx, nil, &imapserver.Credentials{Mechanism: "PLAIN", Username: "alice", Password: "secret"}, nil)
			if err != nil {
				b.Fatal(err)
			}
			s := sessionAPI.(*session)
			if err := s.Create(ctx, "Archive", nil); err != nil {
				b.Fatal(err)
			}
			a := s.account
			if err := a.enter(ctx); err != nil {
				b.Fatal(err)
			}
			m := a.state.Mailboxes["INBOX"]
			for i := 1; i <= count; i++ {
				msg := &messageRecord{UID: imap.UID(i), File: fmt.Sprintf("bench-%08d:2,", i), Size: int64(len(sampleMessage)), Date: time.Unix(1700000000, 0), ModSeq: uint64(i + 1)}
				f, err := a.root.OpenFile(filepath.Join(mailboxPath("INBOX"), "cur", msg.File), os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0600)
				if err != nil {
					b.Fatal(err)
				}
				if _, err := f.WriteString(sampleMessage); err != nil {
					b.Fatal(err)
				}
				if err := f.Close(); err != nil {
					b.Fatal(err)
				}
				m.Messages = append(m.Messages, msg)
			}
			m.UIDNext, m.HighestModSeq = imap.UID(count+1), uint64(count+1)
			if err := a.commit(a.state, nil); err != nil {
				b.Fatal(err)
			}
			a.leave()
			selection, err := s.Select(ctx, "INBOX", &imapserver.Updater{PushFunc: func(*imapserver.UpdateBatch) error { return nil }}, nil)
			if err != nil {
				b.Fatal(err)
			}
			sel := selection.Mailbox.(*selected)
			archiveSelection, err := s.Select(ctx, "Archive", &imapserver.Updater{PushFunc: func(*imapserver.UpdateBatch) error { return nil }}, nil)
			if err != nil {
				b.Fatal(err)
			}
			archiveSel := archiveSelection.Mailbox.(*selected)
			b.Run("idle-poll", func(b *testing.B) {
				b.ReportAllocs()
				for i := 0; i < b.N; i++ {
					if err := a.enter(ctx); err != nil {
						b.Fatal(err)
					}
					err := a.reconcileContext(ctx)
					a.leave()
					if err != nil {
						b.Fatal(err)
					}
				}
			})
			b.Run("single-store", func(b *testing.B) {
				b.ReportAllocs()
				for i := 0; i < b.N; i++ {
					if err := a.enter(ctx); err != nil {
						b.Fatal(err)
					}
					msg := a.state.Mailboxes["INBOX"].Messages[0]
					uid := msg.UID
					op := imapserver.StoreFlagsAdd
					if imap.ContainsFlag(msg.Flags, imap.FlagSeen) {
						op = imapserver.StoreFlagsRemove
					}
					a.leave()
					if err := sel.Store(ctx, discardFetch(), imap.UIDSetNum(uid), &imapserver.StoreFlags{Op: op, Flags: []imap.Flag{imap.FlagSeen}}, nil); err != nil {
						b.Fatal(err)
					}
				}
			})
			b.Run("single-move", func(b *testing.B) {
				b.ReportAllocs()
				for i := 0; i < b.N; i++ {
					// Move a fresh remaining source message on every iteration.
					uid := a.state.Mailboxes["INBOX"].Messages[0].UID
					result, err := sel.Move(ctx, imap.UIDSetNum(uid), "Archive", nil)
					if err != nil {
						b.Fatal(err)
					}
					b.StopTimer()
					if _, err := archiveSel.Move(ctx, result.DestinationUIDs, "INBOX", nil); err != nil {
						b.Fatal(err)
					}
					b.StartTimer()
				}
			})
		})
	}
}
