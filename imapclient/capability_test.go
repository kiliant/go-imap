package imapclient

import (
	"bufio"
	"context"
	"net"
	"strings"
	"testing"
	"time"
)

func TestCapabilityValuesAndRefresh(t *testing.T) {
	clientConn, serverConn := net.Pipe()
	defer serverConn.Close()
	go func() {
		defer serverConn.Close()
		_, _ = serverConn.Write([]byte("* OK [CAPABILITY IMAP4rev1 AUTH=PLAIN AUTH=SCRAM-SHA-256 APPENDLIMIT=1234 X-FUTURE] ready\r\n"))
		line, _ := bufio.NewReader(serverConn).ReadString('\n')
		tag := strings.Fields(line)[0]
		_, _ = serverConn.Write([]byte("* CAPABILITY IMAP4rev1 AUTH=OAUTHBEARER APPENDLIMIT=9999 X-NEW\r\n" + tag + " OK refreshed\r\n"))
	}()
	c := NewClient(clientConn, nil)
	defer c.Close()
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	if err := c.WaitGreeting(ctx, nil); err != nil {
		t.Fatal(err)
	}
	if got := c.CapabilityValues("AUTH"); strings.Join(got, ",") != "PLAIN,SCRAM-SHA-256" {
		t.Fatalf("AUTH values = %#v", got)
	}
	if got := c.CapabilityValues("APPENDLIMIT"); len(got) != 1 || got[0] != "1234" {
		t.Fatalf("APPENDLIMIT values = %#v", got)
	}
	if err := c.Capability(ctx, nil); err != nil {
		t.Fatal(err)
	}
	caps := c.Capabilities()
	if caps["AUTH=PLAIN"] || caps["X-FUTURE"] || !caps["AUTH=OAUTHBEARER"] || !caps["X-NEW"] {
		t.Fatalf("refreshed capabilities = %#v", caps)
	}
	if got := c.CapabilityValues("APPENDLIMIT"); len(got) != 1 || got[0] != "9999" {
		t.Fatalf("refreshed APPENDLIMIT values = %#v", got)
	}
}

func TestCapabilityResponseCodeAddsCapabilities(t *testing.T) {
	clientConn, serverConn := net.Pipe()
	defer serverConn.Close()
	go func() {
		defer serverConn.Close()
		_, _ = serverConn.Write([]byte("* OK [CAPABILITY IMAP4rev1] ready\r\n"))
		line, _ := bufio.NewReader(serverConn).ReadString('\n')
		tag := strings.Fields(line)[0]
		_, _ = serverConn.Write([]byte("* OK [CAPABILITY IDLE UTF8=ACCEPT] capability update\r\n" + tag + " OK [CAPABILITY MOVE] done\r\n"))
	}()
	c := NewClient(clientConn, nil)
	defer c.Close()
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	if err := c.WaitGreeting(ctx, nil); err != nil {
		t.Fatal(err)
	}
	if err := c.Noop(nil).Wait(ctx); err != nil {
		t.Fatal(err)
	}
	if caps := c.Capabilities(); !caps["IMAP4REV1"] || !caps["IDLE"] || !caps["UTF8=ACCEPT"] || !caps["MOVE"] {
		t.Fatalf("capabilities after response code = %#v", caps)
	}
}
