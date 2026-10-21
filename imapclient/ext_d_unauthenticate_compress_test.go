package imapclient

import (
	"bufio"
	"compress/flate"
	"fmt"
	"io"
	"net"
	"strings"
	"testing"

	"github.com/kiliant/go-imap/internal/imapdeflate"
)

func TestUnauthenticateEndsCompression(t *testing.T) {
	for _, finalBlock := range []bool{false, true} {
		t.Run(fmt.Sprint(finalBlock), func(t *testing.T) {
			client, server := net.Pipe()
			done := make(chan error, 1)
			go func() {
				defer server.Close()
				run := func() error {
					if _, err := io.WriteString(server, "* PREAUTH ready\r\n"); err != nil {
						return err
					}
					line, err := readClearLine(server)
					if err != nil {
						return err
					}
					if !strings.HasSuffix(line, " COMPRESS DEFLATE") {
						return fmt.Errorf("COMPRESS = %q", line)
					}
					if _, err = io.WriteString(server, strings.Fields(line)[0]+" OK compression active\r\n"); err != nil {
						return err
					}
					incoming := imapdeflate.NewReader(server)
					outgoing, _ := flate.NewWriter(server, flate.DefaultCompression)
					line, err = bufio.NewReader(incoming).ReadString('\n')
					if err != nil {
						return err
					}
					if !strings.HasSuffix(line, " UNAUTHENTICATE\r\n") {
						return fmt.Errorf("compressed command = %q", line)
					}
					if err = incoming.Finish(); err != nil {
						return err
					}
					if _, err = io.WriteString(outgoing, strings.Fields(line)[0]+" OK reset\r\n"); err != nil {
						return err
					}
					if err = outgoing.Flush(); err != nil {
						return err
					}
					if finalBlock {
						if err = outgoing.Close(); err != nil {
							return err
						}
					}
					plain := bufio.NewReader(incoming.PlainReader())
					for _, command := range []string{"CAPABILITY", "NOOP"} {
						line, err = plain.ReadString('\n')
						if err != nil {
							return err
						}
						if !strings.HasSuffix(line, " "+command+"\r\n") {
							return fmt.Errorf("plaintext %s = %q", command, line)
						}
						if command == "CAPABILITY" {
							if _, err = io.WriteString(server, "* CAPABILITY IMAP4rev1 UNAUTHENTICATE\r\n"); err != nil {
								return err
							}
						}
						if _, err = io.WriteString(server, strings.Fields(line)[0]+" OK done\r\n"); err != nil {
							return err
						}
					}
					return nil
				}
				done <- run()
			}()
			c := NewClient(client, nil)
			defer c.Close()
			ctx := extCContext(t)
			if err := c.WaitGreeting(ctx, nil); err != nil {
				t.Fatal(err)
			}
			extCReady(c, []string{"IMAP4REV1", "COMPRESS=DEFLATE", "UNAUTHENTICATE"}, nil, true)
			if err := c.Compress(ctx, nil); err != nil {
				t.Fatal(err)
			}
			if err := c.Unauthenticate(ctx, nil); err != nil {
				t.Fatal(err)
			}
			if c.Compressed() {
				t.Fatal("compression remains active")
			}
			if err := c.Noop(nil).Wait(ctx); err != nil {
				t.Fatal(err)
			}
			if err := <-done; err != nil {
				t.Fatal(err)
			}
		})
	}
}
