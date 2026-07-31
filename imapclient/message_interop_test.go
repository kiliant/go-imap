//go:build interop

package imapclient_test

import (
	"context"
	"fmt"
	"io"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/kiliant/go-imap"
	"github.com/kiliant/go-imap/imapclient"
	"github.com/kiliant/go-imap/interop/harness"
)

const t06LargeBodySize = 5 << 20

// TestMessageCommandsRoundTrip exercises the production client, rather than
// the small raw client used to provision the interoperability fixtures. Each
// server gets a private source and destination mailbox so this test remains
// independent of the seeded corpus and of parallel package tests.
func TestMessageCommandsRoundTrip(t *testing.T) {
	for _, server := range harness.RunningServers() {
		if server.Profile.Name != "dovecot" && server.Profile.Name != "greenmail" {
			continue
		}
		server := server
		t.Run(server.Profile.Name, func(t *testing.T) {
			ctx, cancel := context.WithTimeout(context.Background(), 45*time.Second)
			defer cancel()

			client, err := imapclient.Dial(ctx, server.Address, &imapclient.Options{AllowInsecureAuth: true})
			if err == nil {
				err = client.Login(ctx, "interop@example.test", "interop-pw")
			}
			if err != nil {
				if client != nil {
					_ = client.Close()
				}
				server.DumpDiagnostics(context.Background(), t.Output(), nil)
				t.Fatal(err)
			}
			defer client.Close()

			source := harness.UniqueMailbox("t06-source")
			destination := harness.UniqueMailbox("t06-destination")
			cleanup := func() {
				cleanupCtx, cleanupCancel := context.WithTimeout(context.Background(), 10*time.Second)
				defer cleanupCancel()
				if client.State() == imapclient.StateSelected {
					_ = client.CloseMailbox().Wait(cleanupCtx)
				}
				_ = client.Delete(destination).Wait(cleanupCtx)
				_ = client.Delete(source).Wait(cleanupCtx)
			}
			defer cleanup()

			if err := client.Create(source).Wait(ctx); err != nil {
				t.Fatal(err)
			}
			if err := client.Create(destination).Wait(ctx); err != nil {
				t.Fatal(err)
			}

			const subject = "go-imap T06 round trip"
			message := "From: sender@example.test\r\nTo: receiver@example.test\r\nSubject: " + subject + "\r\nX-Excluded: do not return\r\n\r\nmessage command round trip\r\n"
			if _, err := client.Append(ctx, source, nil, int64(len(message)), strings.NewReader(message)).Wait(ctx); err != nil {
				t.Fatal(err)
			}
			if _, err := client.Select(source, nil).Wait(ctx); err != nil {
				t.Fatal(err)
			}

			seqNums, err := client.Search(imap.SearchHeaderField{Field: "Subject", Value: subject}, nil).All(ctx)
			if err != nil {
				t.Fatal(err)
			}
			if len(seqNums) != 1 {
				t.Fatalf("SEARCH matched %v, want one message", seqNums)
			}
			seqNum := seqNums[0]

			uid, flags := t06FetchUIDAndFlags(t, ctx, client, seqNum)
			if imap.ContainsFlag(flags, imap.FlagSeen) {
				t.Fatalf("freshly appended message already has %s: %v", imap.FlagSeen, flags)
			}

			uidMatches, err := client.SearchUID(imap.SearchHeaderField{Field: "Subject", Value: subject}, nil).AllUID(ctx)
			if err != nil {
				t.Fatal(err)
			}
			if len(uidMatches) != 1 || uidMatches[0] != uid {
				t.Fatalf("UID SEARCH = %v, want [%d]", uidMatches, uid)
			}

			t06CheckHeaderFields(t, ctx, client, seqNum, subject)
			t06CheckBodyPeekDoesNotSetSeen(t, ctx, client, seqNum)

			if err := client.Store(imap.SeqSetNum(seqNum), []imap.Flag{imap.FlagFlagged}, &imapclient.StoreOptions{Op: imapclient.StoreFlagsAdd}).Wait(ctx); err != nil {
				t.Fatal(err)
			}
			_, flags = t06FetchUIDAndFlags(t, ctx, client, seqNum)
			if !imap.ContainsFlag(flags, imap.FlagFlagged) {
				t.Fatalf("STORE did not add %s: %v", imap.FlagFlagged, flags)
			}
			if err := client.StoreUID(imap.UIDSetNum(uid), []imap.Flag{imap.FlagFlagged}, &imapclient.StoreOptions{Op: imapclient.StoreFlagsRemove, Silent: true}).Wait(ctx); err != nil {
				t.Fatal(err)
			}
			uidData, err := t06FetchOne(ctx, client.FetchUID(imap.UIDSetNum(uid), imap.FetchItemUID, imap.FetchItemFlags))
			if err != nil {
				t.Fatal(err)
			}
			if err := dataWait(ctx, uidData); err != nil {
				t.Fatal(err)
			}
			gotUID, gotFlags := t06UIDAndFlags(t, uidData.data)
			if gotUID != uid || imap.ContainsFlag(gotFlags, imap.FlagFlagged) {
				t.Fatalf("UID STORE/FETCH got UID %d flags %v, want UID %d without %s", gotUID, gotFlags, uid, imap.FlagFlagged)
			}

			if _, err := client.Copy(imap.SeqSetNum(seqNum), destination).Wait(ctx); err != nil {
				t.Fatal(err)
			}
			if _, err := client.CopyUID(imap.UIDSetNum(uid), destination).Wait(ctx); err != nil {
				t.Fatal(err)
			}
			if _, err := client.Select(destination, nil).Wait(ctx); err != nil {
				t.Fatal(err)
			}
			copied, err := client.Search(imap.SearchHeaderField{Field: "Subject", Value: subject}, nil).All(ctx)
			if err != nil {
				t.Fatal(err)
			}
			if len(copied) != 2 {
				t.Fatalf("COPY and UID COPY produced %d messages, want 2", len(copied))
			}

			if _, err := client.Select(source, nil).Wait(ctx); err != nil {
				t.Fatal(err)
			}
			if err := client.StoreUID(imap.UIDSetNum(uid), []imap.Flag{imap.FlagDeleted}, &imapclient.StoreOptions{Op: imapclient.StoreFlagsAdd, Silent: true}).Wait(ctx); err != nil {
				t.Fatal(err)
			}
			if err := client.Expunge().Wait(ctx); err != nil {
				t.Fatal(err)
			}
			remaining, err := client.Search(imap.SearchHeaderField{Field: "Subject", Value: subject}, nil).All(ctx)
			if err != nil {
				t.Fatal(err)
			}
			if len(remaining) != 0 {
				t.Fatalf("EXPUNGE left matching messages %v", remaining)
			}
			t06CheckLargeFetchStreaming(t, ctx, client, source)
		})
	}
}

func t06CheckHeaderFields(t *testing.T, ctx context.Context, client *imapclient.Client, seqNum imap.SeqNum, subject string) {
	t.Helper()
	data, err := t06FetchOne(ctx, client.Fetch(imap.SeqSetNum(seqNum), &imap.FetchItemBodySection{
		Specifier:    imap.PartSpecifierHeader,
		HeaderFields: []string{"From", "Subject"},
		Peek:         true,
	}))
	if err != nil {
		t.Fatal(err)
	}
	section := t06BodySection(t, data)
	if !t06SameStringsFold(section.HeaderFields, []string{"From", "Subject"}) {
		t.Fatalf("HEADER.FIELDS metadata = %v, want [From Subject]", section.HeaderFields)
	}
	header, err := io.ReadAll(section.Literal)
	if err != nil {
		t.Fatal(err)
	}
	if err := dataWait(ctx, data); err != nil {
		t.Fatal(err)
	}
	got := string(header)
	if !strings.Contains(got, "From: sender@example.test\r\n") || !strings.Contains(got, "Subject: "+subject+"\r\n") || strings.Contains(got, "To: receiver@example.test") || strings.Contains(got, "X-Excluded:") {
		t.Fatalf("HEADER.FIELDS literal = %q", got)
	}
}

func t06CheckBodyPeekDoesNotSetSeen(t *testing.T, ctx context.Context, client *imapclient.Client, seqNum imap.SeqNum) {
	t.Helper()
	data, err := t06FetchOne(ctx, client.Fetch(imap.SeqSetNum(seqNum), &imap.FetchItemBodySection{Peek: true}))
	if err != nil {
		t.Fatal(err)
	}
	section := t06BodySection(t, data)
	if _, err := io.Copy(io.Discard, section.Literal); err != nil {
		t.Fatal(err)
	}
	if err := dataWait(ctx, data); err != nil {
		t.Fatal(err)
	}
	_, flags := t06FetchUIDAndFlags(t, ctx, client, seqNum)
	if imap.ContainsFlag(flags, imap.FlagSeen) {
		t.Fatalf("BODY.PEEK set %s: %v", imap.FlagSeen, flags)
	}
}

func t06CheckLargeFetchStreaming(t *testing.T, ctx context.Context, client *imapclient.Client, source string) {
	t.Helper()
	const subject = "go-imap T06 five megabyte stream"
	header := "From: sender@example.test\r\nTo: receiver@example.test\r\nSubject: " + subject + "\r\nContent-Type: application/octet-stream\r\n\r\n"
	size := int64(len(header) + t06LargeBodySize)
	if _, err := client.Append(ctx, source, nil, size, io.MultiReader(strings.NewReader(header), io.LimitReader(t06RepeatedByte('x'), t06LargeBodySize))).Wait(ctx); err != nil {
		t.Fatal(err)
	}
	seqNums, err := client.Search(imap.SearchHeaderField{Field: "Subject", Value: subject}, nil).All(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(seqNums) != 1 {
		t.Fatalf("large-message SEARCH matched %v, want one message", seqNums)
	}

	// TotalAlloc records allocation over the transfer, not retained heap. A
	// literal buffered as []byte would exceed this budget by the five MiB body;
	// the streaming decoder normally stays well below it.
	runtime.GC()
	var before, after runtime.MemStats
	runtime.ReadMemStats(&before)
	data, err := t06FetchOne(ctx, client.Fetch(imap.SeqSetNum(seqNums[0]), &imap.FetchItemBodySection{Peek: true}))
	if err != nil {
		t.Fatal(err)
	}
	section := t06BodySection(t, data)
	n, err := io.Copy(io.Discard, section.Literal)
	if err != nil {
		t.Fatal(err)
	}
	if err := dataWait(ctx, data); err != nil {
		t.Fatal(err)
	}
	runtime.ReadMemStats(&after)
	if n != size {
		t.Fatalf("streamed %d bytes, want %d", n, size)
	}
	if allocated := after.TotalAlloc - before.TotalAlloc; allocated > 1<<20 {
		t.Fatalf("FETCH allocated %d bytes for a %d-byte literal; want <= %d (streaming)", allocated, size, 1<<20)
	}
}

// fetchResult keeps the command alongside its first response so callers can
// drain a literal before awaiting the tagged completion.
type fetchResult struct {
	command *imapclient.FetchCommand
	data    *imap.FetchMessageData
}

func t06FetchOne(ctx context.Context, command *imapclient.FetchCommand) (*fetchResult, error) {
	data, err := command.Next(ctx)
	if err != nil {
		return nil, err
	}
	return &fetchResult{command: command, data: data}, nil
}

func dataWait(ctx context.Context, result *fetchResult) error { return result.command.Wait(ctx) }

func t06FetchUIDAndFlags(t *testing.T, ctx context.Context, client *imapclient.Client, seqNum imap.SeqNum) (imap.UID, []imap.Flag) {
	t.Helper()
	result, err := t06FetchOne(ctx, client.Fetch(imap.SeqSetNum(seqNum), imap.FetchItemUID, imap.FetchItemFlags))
	if err != nil {
		t.Fatal(err)
	}
	if err := dataWait(ctx, result); err != nil {
		t.Fatal(err)
	}
	return t06UIDAndFlags(t, result.data)
}

func t06UIDAndFlags(t *testing.T, data *imap.FetchMessageData) (imap.UID, []imap.Flag) {
	t.Helper()
	var uid imap.UID
	var flags []imap.Flag
	hasFlags := false
	for _, values := range data.Items {
		for _, value := range values {
			switch value := value.(type) {
			case imap.FetchDataUID:
				uid = imap.UID(value)
			case imap.FetchDataFlags:
				flags = append([]imap.Flag(nil), value...)
				hasFlags = true
			}
		}
	}
	if uid == 0 {
		t.Fatal("FETCH response did not contain UID")
	}
	if !hasFlags {
		t.Fatalf("FETCH response did not contain FLAGS: %s", t06DescribeFetch(data))
	}
	return uid, flags
}

func t06BodySection(t *testing.T, result *fetchResult) *imap.FetchDataBodySection {
	t.Helper()
	for _, values := range result.data.Items {
		for _, value := range values {
			if section, ok := value.(*imap.FetchDataBodySection); ok {
				return section
			}
		}
	}
	t.Fatalf("FETCH response has no BODY section: %s", t06DescribeFetch(result.data))
	return nil
}

func t06SameStringsFold(got, want []string) bool {
	if len(got) != len(want) {
		return false
	}
	for i := range got {
		if !strings.EqualFold(got[i], want[i]) {
			return false
		}
	}
	return true
}

func t06DescribeFetch(data *imap.FetchMessageData) string {
	var keys []string
	for key := range data.Items {
		keys = append(keys, string(key))
	}
	return fmt.Sprintf("seq=%d keys=%v", data.SeqNum, keys)
}

type t06RepeatedByte byte

func (b t06RepeatedByte) Read(dst []byte) (int, error) {
	for i := range dst {
		dst[i] = byte(b)
	}
	return len(dst), nil
}

var _ io.Reader = t06RepeatedByte(0)
