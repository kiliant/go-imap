//go:build ignore

// Incremental mailbox sync with CONDSTORE and QRESYNC (RFC 7162).
//
// First run caches uidvalidity and highestmodseq. Second run supplies the cached
// anchor via UIDVALIDITY and MODSEQ and replays changes since that mod-sequence.
//
// Run:
//
//	# first run — print anchors to cache
//	IMAP_ADDR=localhost:1143 IMAP_USER=user IMAP_PASS=secret \
//	  go run ./examples/sync_condstore.go ./examples/config.go
//
//	# second run — resync since cached MODSEQ
//	IMAP_ADDR=localhost:1143 IMAP_USER=user IMAP_PASS=secret \
//	  UIDVALIDITY=1 MODSEQ=42 \
//	  go run ./examples/sync_condstore.go ./examples/config.go
package main

import (
	"fmt"
	"io"
	"os"
	"strconv"

	"github.com/kiliant/go-imap"
	"github.com/kiliant/go-imap/imapclient"
)

func main() {
	ctx, cancel := exampleContext()
	defer cancel()

	client, err := dialExample(ctx)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	defer client.Close()

	if err := authenticate(ctx, client); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}

	if _, err := client.Enable("CONDSTORE", "QRESYNC").Wait(ctx); err != nil {
		fmt.Fprintln(os.Stderr, "enable CONDSTORE/QRESYNC:", err)
		os.Exit(1)
	}

	var cachedModseq uint64
	syncOpts := &imapclient.SyncSelectOptions{CondStore: true}
	if uv := os.Getenv("UIDVALIDITY"); uv != "" {
		modseqStr := os.Getenv("MODSEQ")
		if modseqStr == "" {
			fmt.Fprintln(os.Stderr, "MODSEQ required when UIDVALIDITY is set")
			os.Exit(2)
		}
		uidValidity, err := strconv.ParseUint(uv, 10, 32)
		if err != nil {
			fmt.Fprintln(os.Stderr, err)
			os.Exit(2)
		}
		modseq, err := strconv.ParseUint(modseqStr, 10, 64)
		if err != nil {
			fmt.Fprintln(os.Stderr, err)
			os.Exit(2)
		}
		cachedModseq = modseq
		syncOpts.QResync = &imapclient.QResyncOptions{
			UIDValidity: uint32(uidValidity),
			ModSeq:      modseq,
		}
	}

	status, err := client.SelectSync("INBOX", syncOpts).Wait(ctx)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	fmt.Printf("uidvalidity=%d highestmodseq=%d\n", status.Status.UIDValidity, status.Status.HighestModSeq)

	if syncOpts.QResync != nil {
		for _, v := range status.Vanished {
			fmt.Printf("vanished uids=%s earlier=%v\n", v.UIDs, v.Earlier)
		}

		// CHANGEDSINCE uses the cached mod-sequence from before this session,
		// not the fresh HIGHESTMODSEQ returned by the current SELECT.
		fetch := client.FetchUIDSync(imap.UIDSetRange(1, 0), &imapclient.SyncFetchOptions{
			ChangedSince: cachedModseq,
		}, imap.FetchItemUID, imap.FetchItemFlags, imap.FetchItemModSeq)
		for {
			data, err := fetch.Next(ctx)
			if err == io.EOF {
				break
			}
			if err != nil {
				fmt.Fprintln(os.Stderr, err)
				os.Exit(1)
			}
			var uid imap.UID
			if uids, ok := data.Items[imap.FetchDataKey("UID")]; ok && len(uids) > 0 {
				uid = imap.UID(uids[0].(imap.FetchDataUID))
			}
			fmt.Printf("changed uid=%d\n", uid)
		}
	}
}
