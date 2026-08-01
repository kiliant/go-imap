//go:build ignore

// Stream a message attachment to disk without buffering the entire part in memory.
//
// Run:
//
//	IMAP_ADDR=localhost:1143 IMAP_USER=user IMAP_PASS=secret \
//	  go run ./examples/stream_attachment.go ./examples/config.go /tmp/part.bin 1 2
//
// Arguments: output path, message sequence number, MIME part number (1-based).
package main

import (
	"fmt"
	"io"
	"os"
	"strconv"

	"github.com/kiliant/go-imap"
)

func main() {
	if len(os.Args) != 4 {
		fmt.Fprintf(os.Stderr, "usage: %s outfile seq part\n", os.Args[0])
		os.Exit(2)
	}
	outPath := os.Args[1]
	seq, err := strconv.ParseUint(os.Args[2], 10, 32)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(2)
	}
	partNum, err := strconv.Atoi(os.Args[3])
	if err != nil || partNum < 1 {
		fmt.Fprintln(os.Stderr, "part must be a positive integer")
		os.Exit(2)
	}

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
	if _, err := client.Select("INBOX", nil).Wait(ctx); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}

	section := &imap.FetchItemBodySection{
		Part: []int{partNum},
		Peek: true,
	}
	cmd := client.Fetch(imap.SeqSetNum(imap.SeqNum(seq)), nil, section)
	var copied int64
	for {
		data, err := cmd.Next(ctx)
		if err == io.EOF {
			break
		}
		if err != nil {
			fmt.Fprintln(os.Stderr, err)
			os.Exit(1)
		}
		for key, values := range data.Items {
			_ = key
			for _, v := range values {
				body, ok := v.(*imap.FetchDataBodySection)
				if !ok {
					continue
				}
				out, err := os.Create(outPath)
				if err != nil {
					drainLiteral(body.Literal)
					fmt.Fprintln(os.Stderr, err)
					os.Exit(1)
				}
				copied, err = io.Copy(out, body.Literal)
				out.Close()
				if err != nil {
					drainLiteral(body.Literal)
					fmt.Fprintln(os.Stderr, err)
					os.Exit(1)
				}
				drainLiteral(body.Literal)
			}
		}
	}
	if copied == 0 {
		fmt.Fprintln(os.Stderr, "no matching body section found")
		os.Exit(1)
	}
	fmt.Printf("wrote %d bytes to %s\n", copied, outPath)
}

func drainLiteral(r io.Reader) {
	if r == nil {
		return
	}
	if c, ok := r.(io.Closer); ok {
		_, _ = io.Copy(io.Discard, r)
		_ = c.Close()
		return
	}
	_, _ = io.Copy(io.Discard, r)
}
