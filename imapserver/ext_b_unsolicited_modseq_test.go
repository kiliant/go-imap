package imapserver_test

import (
	"context"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/kiliant/go-imap"
	"github.com/kiliant/go-imap/imapclient"
)

func TestLoopbackUnsolicitedModSeqRequiresEnablement(t *testing.T) {
	for _, enabled := range []bool{false, true} {
		t.Run(fmt.Sprint(enabled), func(t *testing.T) {
			ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
			defer cancel()
			server, connection, reader := newGroupARawSessionIn(t, ctx, true)
			parameter := ""
			if enabled {
				parameter = " (CONDSTORE)"
			}
			writeRawCommand(t, connection, "S SELECT INBOX"+parameter+"\r\n")
			collectUntilTag(t, reader, "S ")
			writer, _ := openLoopbackClient(t, ctx, server, &imapclient.Options{AllowInsecureAuth: true})
			if err := writer.Login(ctx, "alice", "secret", nil); err != nil {
				t.Fatal(err)
			}
			if _, err := writer.Select("INBOX", nil).Wait(ctx); err != nil {
				t.Fatal(err)
			}
			if err := writer.StoreUID(imap.UIDSetNum(1), []imap.Flag{imap.FlagFlagged}, &imapclient.StoreOptions{Op: imapclient.StoreFlagsAdd}).Wait(ctx); err != nil {
				t.Fatal(err)
			}
			if err := connection.SetReadDeadline(time.Now().Add(5 * time.Second)); err != nil {
				t.Fatal(err)
			}
			line, err := reader.ReadString('\n')
			if err != nil {
				t.Fatal(err)
			}
			if !strings.Contains(line, " FETCH ") || !strings.Contains(line, "\\Flagged") || strings.Contains(line, "MODSEQ") != enabled {
				t.Errorf("CONDSTORE enabled=%t: unsolicited response %q", enabled, line)
			}
		})
	}
}
