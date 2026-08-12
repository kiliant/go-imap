package imapserver

import (
	"bufio"
	"context"
	"crypto/tls"
	"errors"
	"net"
	"strings"
	"testing"
	"time"

	"github.com/kiliant/go-imap"
	"github.com/kiliant/go-imap/internal/imapwire"
)

func TestReaderCancelsBlockedEventLoopOnDisconnect(t *testing.T) {
	entered := make(chan struct{})
	cancelled := make(chan error, 1)
	previous, existed := commandDescriptors["BLOCK"]
	commandDescriptors["BLOCK"] = &commandDescriptor{
		name:   "BLOCK",
		states: stateMaskAny,
		parse:  parseNoArgs,
		handle: func(ctx context.Context, _ *conn, _ *queuedCommand) error {
			close(entered)
			<-ctx.Done()
			cancelled <- context.Cause(ctx)
			return ctx.Err()
		},
	}
	defer func() {
		if existed {
			commandDescriptors["BLOCK"] = previous
		} else {
			delete(commandDescriptors, "BLOCK")
		}
	}()

	serverSide, clientSide := net.Pipe()
	server := New(nil, nil)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- server.ServeConn(ctx, serverSide) }()
	reader := bufio.NewReader(clientSide)
	if _, err := reader.ReadString('\n'); err != nil {
		t.Fatal(err)
	}
	if _, err := clientSide.Write([]byte("A1 BLOCK\r\n")); err != nil {
		t.Fatal(err)
	}
	select {
	case <-entered:
	case <-ctx.Done():
		t.Fatal(ctx.Err())
	}
	if err := clientSide.Close(); err != nil {
		t.Fatal(err)
	}
	select {
	case cause := <-cancelled:
		if cause == nil {
			t.Fatal("backend command received nil cancellation cause")
		}
	case <-time.After(time.Second):
		t.Fatal("disconnect did not cancel blocked event-loop command")
	}
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("connection did not terminate after disconnect")
	}
}

func TestAuthenticationStopsWholeConnectionPreAuthDeadline(t *testing.T) {
	previous, existed := commandDescriptors["AUTHOK"]
	commandDescriptors["AUTHOK"] = &commandDescriptor{
		name:    "AUTHOK",
		states:  stateMaskNotAuthenticated,
		barrier: true,
		parse:   parseNoArgs,
		handle: func(_ context.Context, c *conn, command *queuedCommand) error {
			if !c.state.authenticate(&stubSession{}) {
				return c.writeBad(command.tag, "authentication failed")
			}
			return c.writeTagged(command.tag, "OK", "authenticated")
		},
	}
	defer func() {
		if existed {
			commandDescriptors["AUTHOK"] = previous
		} else {
			delete(commandDescriptors, "AUTHOK")
		}
	}()

	serverSide, clientSide := net.Pipe()
	server := New(nil, &Options{Limits: Limits{PreAuthTimeout: 40 * time.Millisecond}})
	done := make(chan error, 1)
	go func() { done <- server.ServeConn(context.Background(), serverSide) }()
	reader := bufio.NewReader(clientSide)
	if _, err := reader.ReadString('\n'); err != nil {
		t.Fatal(err)
	}
	if _, err := clientSide.Write([]byte("A1 AUTHOK\r\n")); err != nil {
		t.Fatal(err)
	}
	if line, err := reader.ReadString('\n'); err != nil || !strings.HasPrefix(line, "A1 OK ") {
		t.Fatalf("authentication completion = %q, %v", line, err)
	}
	time.Sleep(80 * time.Millisecond)
	if _, err := clientSide.Write([]byte("A2 NOOP\r\n")); err != nil {
		t.Fatal(err)
	}
	if line, err := reader.ReadString('\n'); err != nil || !strings.HasPrefix(line, "A2 OK ") {
		t.Fatalf("post-deadline NOOP = %q, %v", line, err)
	}
	_ = clientSide.Close()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("authenticated connection did not stop")
	}
}

func TestBarrierContinuationUsesReaderAndPreservesBufferedCommands(t *testing.T) {
	previous, existed := commandDescriptors["EXCHANGE"]
	commandDescriptors["EXCHANGE"] = &commandDescriptor{
		name:    "EXCHANGE",
		states:  stateMaskAny,
		barrier: true,
		parse:   parseNoArgs,
		handle: func(ctx context.Context, c *conn, command *queuedCommand) error {
			line, err := c.continueLine(ctx, "send answer")
			if err != nil {
				return err
			}
			if line != "answer" {
				return c.writeBad(command.tag, "wrong answer")
			}
			return c.writeTagged(command.tag, "OK", "exchange completed")
		},
	}
	defer func() {
		if existed {
			commandDescriptors["EXCHANGE"] = previous
		} else {
			delete(commandDescriptors, "EXCHANGE")
		}
	}()

	serverSide, clientSide := net.Pipe()
	server := New(nil, nil)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- server.ServeConn(ctx, serverSide) }()
	reader := bufio.NewReader(clientSide)
	if _, err := reader.ReadString('\n'); err != nil {
		t.Fatal(err)
	}
	if _, err := clientSide.Write([]byte("A1 EXCHANGE\r\n")); err != nil {
		t.Fatal(err)
	}
	if line, err := reader.ReadString('\n'); err != nil || !strings.HasPrefix(line, "+ send answer") {
		t.Fatalf("continuation = %q, %v", line, err)
	}
	// The next command is already buffered when the continuation completes.
	// A non-transport barrier must retain it.
	if _, err := clientSide.Write([]byte("answer\r\nA2 NOOP\r\n")); err != nil {
		t.Fatal(err)
	}
	if line, err := reader.ReadString('\n'); err != nil || !strings.HasPrefix(line, "A1 OK ") {
		t.Fatalf("exchange completion = %q, %v", line, err)
	}
	if line, err := reader.ReadString('\n'); err != nil || !strings.HasPrefix(line, "A2 OK ") {
		t.Fatalf("buffered command completion = %q, %v", line, err)
	}
	_ = clientSide.Close()
	select {
	case <-done:
	case <-ctx.Done():
		t.Fatal(ctx.Err())
	}
}

func TestSASLContinuationExchange(t *testing.T) {
	previous, existed := commandDescriptors["SASLISH"]
	commandDescriptors["SASLISH"] = &commandDescriptor{
		name:    "SASLISH",
		states:  stateMaskNotAuthenticated,
		barrier: true,
		parse:   parseNoArgs,
		handle: func(ctx context.Context, c *conn, command *queuedCommand) error {
			server, err := newSASLServer("LOGIN")
			if err != nil {
				return err
			}
			response, aborted, err := c.continueSASL(ctx, server.initialChallenge())
			if err != nil || aborted {
				return err
			}
			credentials, challenge, err := server.next(response)
			if err != nil || credentials != nil {
				return err
			}
			response, aborted, err = c.continueSASL(ctx, challenge)
			if err != nil || aborted {
				return err
			}
			credentials, _, err = server.next(response)
			if err != nil {
				return err
			}
			if credentials.Username != "alice" || credentials.Password != "secret" {
				return errors.New("unexpected credentials")
			}
			return c.writeTagged(command.tag, "OK", "SASL completed")
		},
	}
	defer func() {
		if existed {
			commandDescriptors["SASLISH"] = previous
		} else {
			delete(commandDescriptors, "SASLISH")
		}
	}()

	serverSide, clientSide := net.Pipe()
	server := New(nil, nil)
	done := make(chan error, 1)
	go func() { done <- server.ServeConn(context.Background(), serverSide) }()
	reader := bufio.NewReader(clientSide)
	if _, err := reader.ReadString('\n'); err != nil {
		t.Fatal(err)
	}
	if _, err := clientSide.Write([]byte("A1 SASLISH\r\n")); err != nil {
		t.Fatal(err)
	}
	if line, err := reader.ReadString('\n'); err != nil || line != "+ VXNlcm5hbWU6\r\n" {
		t.Fatalf("username challenge = %q, %v", line, err)
	}
	if _, err := clientSide.Write([]byte("YWxpY2U=\r\n")); err != nil {
		t.Fatal(err)
	}
	if line, err := reader.ReadString('\n'); err != nil || line != "+ UGFzc3dvcmQ6\r\n" {
		t.Fatalf("password challenge = %q, %v", line, err)
	}
	if _, err := clientSide.Write([]byte("c2VjcmV0\r\n")); err != nil {
		t.Fatal(err)
	}
	if line, err := reader.ReadString('\n'); err != nil || !strings.HasPrefix(line, "A1 OK ") {
		t.Fatalf("SASL completion = %q, %v", line, err)
	}
	_ = clientSide.Close()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("SASL connection did not stop")
	}
}

func TestIdleContinuationDrainsUpdatesBeforeDone(t *testing.T) {
	previous, existed := commandDescriptors["IDLEISH"]
	commandDescriptors["IDLEISH"] = &commandDescriptor{
		name:    "IDLEISH",
		states:  stateMaskSelected,
		barrier: true,
		parse:   parseNoArgs,
		handle: func(ctx context.Context, c *conn, command *queuedCommand) error {
			if err := c.idleUntilDone(ctx); err != nil {
				return err
			}
			return c.writeTagged(command.tag, "OK", "IDLE completed")
		},
	}
	defer func() {
		if existed {
			commandDescriptors["IDLEISH"] = previous
		} else {
			delete(commandDescriptors, "IDLEISH")
		}
	}()

	serverSide, clientSide := net.Pipe()
	server := New(nil, nil)
	c, err := newConn(context.Background(), server, serverSide)
	if err != nil {
		t.Fatal(err)
	}
	queue := newUpdateQueue(8, 1024, c.updateOverflow)
	updater := newUpdater(queue)
	c.state.state = stateSelected
	c.state.session = &stubSession{}
	c.state.selected = &selectedState{
		mailbox: &stubSelectedMailbox{},
		uids:    []imap.UID{1}, revision: "r1", queue: queue, updater: updater,
	}
	done := make(chan error, 1)
	go func() { done <- c.serve() }()
	reader := bufio.NewReader(clientSide)
	if _, err := reader.ReadString('\n'); err != nil {
		t.Fatal(err)
	}
	if _, err := clientSide.Write([]byte("A1 IDLEISH\r\n")); err != nil {
		t.Fatal(err)
	}
	if line, err := reader.ReadString('\n'); err != nil || !strings.HasPrefix(line, "+ idling") {
		t.Fatalf("IDLE continuation = %q, %v", line, err)
	}
	if err := updater.Push(&UpdateBatch{Before: "r1", After: "r2", Changes: []Update{&UpdateAdd{UIDs: []imap.UID{2}}}}); err != nil {
		t.Fatal(err)
	}
	if line, err := reader.ReadString('\n'); err != nil || line != "* 2 EXISTS\r\n" {
		t.Fatalf("IDLE update = %q, %v", line, err)
	}
	if _, err := clientSide.Write([]byte("done\r\n")); err != nil {
		t.Fatal(err)
	}
	if line, err := reader.ReadString('\n'); err != nil || !strings.HasPrefix(line, "A1 OK ") {
		t.Fatalf("IDLE completion = %q, %v", line, err)
	}
	_ = clientSide.Close()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("IDLE connection did not stop")
	}
}

func TestCompressDeflateReplacesTransportAtBarrier(t *testing.T) {
	serverSide, clientSide := net.Pipe()
	server := New(nil, nil)
	c, err := newConn(context.Background(), server, serverSide)
	if err != nil {
		t.Fatal(err)
	}
	c.state.state = stateAuthenticated
	done := make(chan error, 1)
	go func() { done <- c.serve() }()

	clearReader := bufio.NewReader(clientSide)
	line, err := clearReader.ReadString('\n')
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(line, "COMPRESS=DEFLATE") {
		t.Fatalf("greeting omitted compression: %q", line)
	}
	if _, err := clientSide.Write([]byte("A1 COMPRESS DEFLATE\r\n")); err != nil {
		t.Fatal(err)
	}
	if line, err := clearReader.ReadString('\n'); err != nil || !strings.HasPrefix(line, "A1 OK ") {
		t.Fatalf("COMPRESS completion = %q, %v", line, err)
	}

	compressed, err := newCompressedConn(clientSide)
	if err != nil {
		t.Fatal(err)
	}
	reader := bufio.NewReader(compressed)
	if _, err := compressed.Write([]byte("A2 CAPABILITY\r\n")); err != nil {
		t.Fatal(err)
	}
	if line, err := reader.ReadString('\n'); err != nil || !strings.HasPrefix(line, "* CAPABILITY ") || strings.Contains(line, "COMPRESS=DEFLATE") {
		t.Fatalf("compressed capabilities = %q, %v", line, err)
	}
	if line, err := reader.ReadString('\n'); err != nil || !strings.HasPrefix(line, "A2 OK ") {
		t.Fatalf("compressed CAPABILITY completion = %q, %v", line, err)
	}
	if _, err := compressed.Write([]byte("A3 LOGOUT\r\n")); err != nil {
		t.Fatal(err)
	}
	_, _ = reader.ReadString('\n')
	_, _ = reader.ReadString('\n')
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("compressed connection did not stop")
	}
}

func TestCommandQueueByteBudgetBlocksUntilReleased(t *testing.T) {
	serverSide, clientSide := net.Pipe()
	defer clientSide.Close()
	server := New(nil, &Options{Limits: Limits{MaxQueuedCommandBytes: 8}})
	c, err := newConn(context.Background(), server, serverSide)
	if err != nil {
		t.Fatal(err)
	}
	defer c.close()
	if err := c.acquireCommandBytes(8); err != nil {
		t.Fatal(err)
	}
	acquired := make(chan error, 1)
	go func() { acquired <- c.acquireCommandBytes(1) }()
	select {
	case err := <-acquired:
		t.Fatalf("byte budget did not block: %v", err)
	case <-time.After(20 * time.Millisecond):
	}
	c.releaseCommandBytes(8)
	select {
	case err := <-acquired:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(time.Second):
		t.Fatal("releasing byte budget did not wake reader")
	}
}

func TestLiteralMinusSizeLimit(t *testing.T) {
	serverSide, clientSide := net.Pipe()
	defer clientSide.Close()
	server := New(nil, nil)
	c, err := newConn(context.Background(), server, serverSide)
	if err != nil {
		t.Fatal(err)
	}
	defer c.close()

	request := literalRequest{
		descriptor: commandDescriptors["ID"],
		info:       imapwire.LiteralInfo{Size: maxLiteralMinusSize + 1, NonSynchronising: true},
		reply:      make(chan error, 1),
	}
	c.handleLiteralRequest(request)
	if err := <-request.reply; err == nil {
		t.Fatal("oversized LITERAL- payload accepted without LITERAL+")
	}
}

func TestServerCloseWaitsForDirectServeConn(t *testing.T) {
	serverSide, clientSide := net.Pipe()
	server := New(nil, nil)
	serveDone := make(chan error, 1)
	go func() { serveDone <- server.ServeConn(context.Background(), serverSide) }()
	reader := bufio.NewReader(clientSide)
	if _, err := reader.ReadString('\n'); err != nil {
		t.Fatal(err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	if err := server.Close(ctx); err != nil {
		t.Fatal(err)
	}
	select {
	case <-serveDone:
	case <-ctx.Done():
		t.Fatal("Close returned before ServeConn stopped")
	}
	_ = clientSide.Close()
}

func TestNewCopiesOptionsAndKeepsLimitsBounded(t *testing.T) {
	serverID := map[string]string{"name": "original"}
	tlsConfig := &tls.Config{MinVersion: tls.VersionTLS10}
	server := New(nil, &Options{
		TLSConfig: tlsConfig,
		ServerID:  serverID,
		Limits: Limits{
			MaxQueuedCommands:    -1,
			MaxQueuedUpdateBytes: -1,
			CommandTimeout:       -1,
		},
	})
	serverID["name"] = "mutated"
	tlsConfig.MinVersion = tls.VersionTLS13

	if got := server.options.ServerID["name"]; got != "original" {
		t.Fatalf("server ID alias retained: %q", got)
	}
	if got := server.options.TLSConfig.MinVersion; got != tls.VersionTLS12 {
		t.Fatalf("TLS minimum = %x, want TLS 1.2", got)
	}
	if server.options.Limits.MaxQueuedCommands <= 0 || server.options.Limits.MaxQueuedUpdateBytes <= 0 || server.options.Limits.CommandTimeout <= 0 {
		t.Fatalf("non-positive limits were not defaulted: %#v", server.options.Limits)
	}
}

func TestSyntaxErrorDoesNotLoseNextCommand(t *testing.T) {
	serverSide, clientSide := net.Pipe()
	server := New(nil, nil)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- server.ServeConn(ctx, serverSide) }()
	reader := bufio.NewReader(clientSide)
	if _, err := reader.ReadString('\n'); err != nil {
		t.Fatal(err)
	}
	if _, err := clientSide.Write([]byte("A1 NOOP extra\r\nA2 NOOP\r\n")); err != nil {
		t.Fatal(err)
	}
	line, err := reader.ReadString('\n')
	if err != nil || !strings.HasPrefix(line, "A1 BAD ") {
		t.Fatalf("syntax response = %q, %v", line, err)
	}
	line, err = reader.ReadString('\n')
	if err != nil || !strings.HasPrefix(line, "A2 OK ") {
		t.Fatalf("recovered response = %q, %v", line, err)
	}
	_ = clientSide.Close()
	select {
	case err := <-done:
		if err != nil && !errors.Is(err, net.ErrClosed) {
			t.Fatal(err)
		}
	case <-time.After(time.Second):
		t.Fatal("server did not observe close")
	}
}
