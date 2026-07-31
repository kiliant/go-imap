package imapclient

import (
	"bufio"
	"context"
	"errors"
	"net"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func TestIdleDoneRoutesResponsesUntilTaggedCompletion(t *testing.T) {
	clientConn, serverConn := net.Pipe()
	defer serverConn.Close()
	updates := make(chan uint32, 2)
	go func() {
		_, _ = serverConn.Write([]byte("* PREAUTH [CAPABILITY IMAP4rev1 IDLE] ready\r\n"))
		r := bufio.NewReader(serverConn)
		line, _ := r.ReadString('\n')
		tag := strings.Fields(line)[0]
		_, _ = serverConn.Write([]byte("+ idling\r\n* 2 EXISTS\r\n"))
		_, _ = r.ReadString('\n') // DONE
		_, _ = serverConn.Write([]byte("* 3 EXISTS\r\n" + tag + " OK idle terminated\r\n"))
	}()
	c := NewClient(clientConn, &Options{UnilateralData: &UnilateralDataHandler{
		Exists: func(n uint32) { updates <- n },
	}})
	defer c.Close()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	if err := c.WaitGreeting(context.Background()); err != nil {
		t.Fatal(err)
	}
	idle := c.Idle()
	result := make(chan error, 1)
	go func() { result <- idle.Wait(ctx) }()
	select {
	case got := <-updates:
		if got != 2 {
			t.Fatalf("first EXISTS = %d", got)
		}
	case <-time.After(time.Second):
		t.Fatal("IDLE did not route unsolicited EXISTS")
	}
	cancel()
	if err := <-result; !errors.Is(err, context.Canceled) {
		t.Fatalf("IDLE Wait() = %v", err)
	}
	select {
	case got := <-updates:
		if got != 3 {
			t.Fatalf("EXISTS racing DONE = %d", got)
		}
	case <-time.After(time.Second):
		t.Fatal("response racing DONE was not routed")
	}
	if c.State() == StateLogout {
		t.Fatal("IDLE cancellation poisoned the connection")
	}
}

func TestIdleWaitReady(t *testing.T) {
	t.Run("accepted", func(t *testing.T) {
		clientConn, serverConn := net.Pipe()
		defer serverConn.Close()
		go func() {
			_, _ = serverConn.Write([]byte("* PREAUTH [CAPABILITY IDLE] ready\r\n"))
			_, _ = bufio.NewReader(serverConn).ReadString('\n')
			_, _ = serverConn.Write([]byte("+ idling\r\n"))
		}()
		c := NewClient(clientConn, nil)
		defer c.Close()
		ctx, cancel := context.WithTimeout(context.Background(), time.Second)
		defer cancel()
		if err := c.WaitGreeting(ctx); err != nil {
			t.Fatal(err)
		}
		if err := c.Idle().WaitReady(ctx); err != nil {
			t.Fatalf("WaitReady() = %v", err)
		}
	})

	t.Run("rejected", func(t *testing.T) {
		clientConn, serverConn := net.Pipe()
		defer serverConn.Close()
		go func() {
			_, _ = serverConn.Write([]byte("* PREAUTH [CAPABILITY IDLE] ready\r\n"))
			line, _ := bufio.NewReader(serverConn).ReadString('\n')
			tag := strings.Fields(line)[0]
			_, _ = serverConn.Write([]byte(tag + " NO IDLE disabled\r\n"))
		}()
		c := NewClient(clientConn, nil)
		defer c.Close()
		ctx, cancel := context.WithTimeout(context.Background(), time.Second)
		defer cancel()
		if err := c.WaitGreeting(ctx); err != nil {
			t.Fatal(err)
		}
		if err := c.Idle().WaitReady(ctx); err == nil || !strings.Contains(err.Error(), "IDLE disabled") {
			t.Fatalf("WaitReady() = %v", err)
		}
	})

	t.Run("cancellation leaves idle active", func(t *testing.T) {
		clientConn, serverConn := net.Pipe()
		defer serverConn.Close()
		allowReady := make(chan struct{})
		go func() {
			_, _ = serverConn.Write([]byte("* PREAUTH [CAPABILITY IDLE] ready\r\n"))
			r := bufio.NewReader(serverConn)
			line, _ := r.ReadString('\n')
			<-allowReady
			_, _ = serverConn.Write([]byte("+ idling\r\n"))
			_, _ = r.ReadString('\n') // DONE from test cleanup
			tag := strings.Fields(line)[0]
			_, _ = serverConn.Write([]byte(tag + " OK done\r\n"))
		}()
		c := NewClient(clientConn, nil)
		defer c.Close()
		if err := c.WaitGreeting(context.Background()); err != nil {
			t.Fatal(err)
		}
		idle := c.Idle()
		readyCtx, cancel := context.WithCancel(context.Background())
		cancel()
		if err := idle.WaitReady(readyCtx); !errors.Is(err, context.Canceled) {
			t.Fatalf("cancelled WaitReady() = %v", err)
		}
		if c.State() == StateLogout {
			t.Fatal("WaitReady cancellation poisoned the connection")
		}
		close(allowReady)
		if err := idle.WaitReady(context.Background()); err != nil {
			t.Fatalf("second WaitReady() = %v", err)
		}
		if err := idle.Done(); err != nil {
			t.Fatalf("Done() after WaitReady cancellation = %v", err)
		}
	})
}

func TestIdleReissuesBeforeConfiguredTimeout(t *testing.T) {
	clientConn, serverConn := net.Pipe()
	defer serverConn.Close()
	second := make(chan struct{})
	go func() {
		defer serverConn.Close()
		_, _ = serverConn.Write([]byte("* PREAUTH [CAPABILITY IDLE] ready\r\n"))
		r := bufio.NewReader(serverConn)
		for cycle := 0; cycle < 2; cycle++ {
			line, _ := r.ReadString('\n')
			tag := strings.Fields(line)[0]
			_, _ = serverConn.Write([]byte("+ idling\r\n"))
			if cycle == 1 {
				close(second)
			}
			_, _ = r.ReadString('\n')
			_, _ = serverConn.Write([]byte(tag + " OK idle terminated\r\n"))
		}
	}()
	c := NewClient(clientConn, &Options{IdleTimeout: 10 * time.Millisecond})
	defer c.Close()
	if err := c.WaitGreeting(context.Background()); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	idle := c.Idle()
	result := make(chan error, 1)
	go func() { result <- idle.Wait(ctx) }()
	select {
	case <-second:
	case <-time.After(time.Second):
		t.Fatal("IDLE was not re-issued")
	}
	cancel()
	if err := <-result; !errors.Is(err, context.Canceled) {
		t.Fatalf("IDLE Wait() = %v", err)
	}
}

func TestIdleFallsBackToNoopPolling(t *testing.T) {
	clientConn, serverConn := net.Pipe()
	defer serverConn.Close()
	var noops atomic.Int32
	twoNoops := make(chan struct{})
	go func() {
		defer serverConn.Close()
		_, _ = serverConn.Write([]byte("* PREAUTH [CAPABILITY IMAP4rev1] ready\r\n"))
		r := bufio.NewReader(serverConn)
		for {
			line, err := r.ReadString('\n')
			if err != nil {
				return
			}
			count := noops.Add(1)
			if count == 2 {
				close(twoNoops)
			}
			tag := strings.Fields(line)[0]
			_, _ = serverConn.Write([]byte(tag + " OK noop\r\n"))
		}
	}()
	c := NewClient(clientConn, &Options{IdlePollInterval: 5 * time.Millisecond})
	defer c.Close()
	if err := c.WaitGreeting(context.Background()); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	result := make(chan error, 1)
	go func() { result <- c.Idle().Wait(ctx) }()
	select {
	case <-twoNoops:
	case <-time.After(time.Second):
		t.Fatal("IDLE fallback did not poll with NOOP")
	}
	cancel()
	if err := <-result; !errors.Is(err, context.Canceled) {
		t.Fatalf("polling IDLE Wait() = %v", err)
	}
}

func TestIdlePollingCancellationWaitsForInFlightNoop(t *testing.T) {
	clientConn, serverConn := net.Pipe()
	defer serverConn.Close()
	noopRead := make(chan string, 1)
	allowCompletion := make(chan struct{})
	go func() {
		_, _ = serverConn.Write([]byte("* PREAUTH [CAPABILITY IMAP4rev1] ready\r\n"))
		line, _ := bufio.NewReader(serverConn).ReadString('\n')
		noopRead <- line
		<-allowCompletion
		tag := strings.Fields(line)[0]
		_, _ = serverConn.Write([]byte(tag + " OK noop\r\n"))
	}()
	c := NewClient(clientConn, nil)
	defer c.Close()
	if err := c.WaitGreeting(context.Background()); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	result := make(chan error, 1)
	go func() { result <- c.Idle().Wait(ctx) }()
	select {
	case <-noopRead:
	case <-time.After(time.Second):
		t.Fatal("NOOP fallback was not issued")
	}
	cancel()
	select {
	case err := <-result:
		t.Fatalf("polling IDLE returned before tagged NOOP completion: %v", err)
	case <-time.After(20 * time.Millisecond):
	}
	close(allowCompletion)
	if err := <-result; !errors.Is(err, context.Canceled) {
		t.Fatalf("polling IDLE Wait() = %v", err)
	}
	if c.State() == StateLogout {
		t.Fatal("polling cancellation poisoned the connection")
	}
}

func TestIdleDoneStopsNoopPollingAfterInFlightCompletion(t *testing.T) {
	clientConn, serverConn := net.Pipe()
	defer serverConn.Close()
	noopRead := make(chan string, 1)
	allowCompletion := make(chan struct{})
	go func() {
		_, _ = serverConn.Write([]byte("* PREAUTH [CAPABILITY IMAP4rev1] ready\r\n"))
		line, _ := bufio.NewReader(serverConn).ReadString('\n')
		noopRead <- line
		<-allowCompletion
		tag := strings.Fields(line)[0]
		_, _ = serverConn.Write([]byte(tag + " OK noop\r\n"))
	}()
	c := NewClient(clientConn, nil)
	defer c.Close()
	if err := c.WaitGreeting(context.Background()); err != nil {
		t.Fatal(err)
	}
	idle := c.Idle()
	result := make(chan error, 1)
	go func() { result <- idle.Wait(context.Background()) }()
	select {
	case <-noopRead:
	case <-time.After(time.Second):
		t.Fatal("NOOP fallback was not issued")
	}
	if err := idle.Done(); err != nil {
		t.Fatalf("Idle Done() = %v", err)
	}
	select {
	case err := <-result:
		t.Fatalf("polling IDLE returned before tagged NOOP completion: %v", err)
	case <-time.After(20 * time.Millisecond):
	}
	close(allowCompletion)
	if err := <-result; err != nil {
		t.Fatalf("polling IDLE Wait() after Done = %v", err)
	}
	if c.State() == StateLogout {
		t.Fatal("polling Done poisoned the connection")
	}
}

func TestIdleFirstDoneIntentWinsOverConcurrentDone(t *testing.T) {
	clientConn, serverConn := net.Pipe()
	defer serverConn.Close()
	firstDone := make(chan struct{})
	allowFirstCompletion := make(chan struct{})
	secondIdle := make(chan struct{})
	go func() {
		defer serverConn.Close()
		_, _ = serverConn.Write([]byte("* PREAUTH [CAPABILITY IDLE] ready\r\n"))
		r := bufio.NewReader(serverConn)
		line, _ := r.ReadString('\n')
		tag := strings.Fields(line)[0]
		_, _ = serverConn.Write([]byte("+ idling\r\n"))
		_, _ = r.ReadString('\n') // DONE from timeout renewal
		close(firstDone)
		<-allowFirstCompletion
		_, _ = serverConn.Write([]byte(tag + " OK idle terminated\r\n"))
		line, _ = r.ReadString('\n')
		if strings.Contains(line, " IDLE\r\n") {
			close(secondIdle)
		}
		tag = strings.Fields(line)[0]
		_, _ = serverConn.Write([]byte("+ idling\r\n"))
		_, _ = r.ReadString('\n')
		_, _ = serverConn.Write([]byte(tag + " OK idle terminated\r\n"))
	}()
	c := NewClient(clientConn, &Options{IdleTimeout: 5 * time.Millisecond})
	defer c.Close()
	if err := c.WaitGreeting(context.Background()); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	idle := c.Idle()
	result := make(chan error, 1)
	go func() { result <- idle.Wait(ctx) }()
	select {
	case <-firstDone:
	case <-time.After(time.Second):
		t.Fatal("timeout renewal did not send DONE")
	}
	if err := idle.Done(); err != nil {
		t.Fatalf("concurrent Done() = %v", err)
	}
	close(allowFirstCompletion)
	select {
	case <-secondIdle:
	case <-time.After(time.Second):
		t.Fatal("final Done changed the timeout renewal intent")
	}
	cancel()
	if err := <-result; !errors.Is(err, context.Canceled) {
		t.Fatalf("IDLE Wait() = %v", err)
	}
}

func TestIdleCancellationWhileRenewalWaitsForTaggedCompletion(t *testing.T) {
	clientConn, serverConn := net.Pipe()
	defer serverConn.Close()
	firstDone := make(chan struct{})
	allowCompletion := make(chan struct{})
	serverRead := make(chan string, 1)
	go func() {
		_, _ = serverConn.Write([]byte("* PREAUTH [CAPABILITY IDLE] ready\r\n"))
		r := bufio.NewReader(serverConn)
		line, _ := r.ReadString('\n')
		tag := strings.Fields(line)[0]
		_, _ = serverConn.Write([]byte("+ idling\r\n"))
		_, _ = r.ReadString('\n') // DONE from timeout renewal
		close(firstDone)
		<-allowCompletion
		_, _ = serverConn.Write([]byte(tag + " OK idle terminated\r\n"))
		_ = serverConn.SetReadDeadline(time.Now().Add(100 * time.Millisecond))
		line, _ = r.ReadString('\n')
		serverRead <- line
	}()
	c := NewClient(clientConn, &Options{IdleTimeout: 5 * time.Millisecond})
	defer c.Close()
	if err := c.WaitGreeting(context.Background()); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	idle := c.Idle()
	result := make(chan error, 1)
	go func() { result <- idle.Wait(ctx) }()
	select {
	case <-firstDone:
	case <-time.After(time.Second):
		t.Fatal("timeout renewal did not send DONE")
	}
	cancel()
	close(allowCompletion)
	if err := <-result; !errors.Is(err, context.Canceled) {
		t.Fatalf("IDLE Wait() = %v", err)
	}
	select {
	case line := <-serverRead:
		if strings.Contains(line, " IDLE\r\n") {
			t.Fatalf("IDLE renewed after cancellation: %q", line)
		}
	case <-time.After(time.Second):
		t.Fatal("server did not observe the absence of a renewal")
	}
	if c.State() == StateLogout {
		t.Fatal("cancellation while renewing poisoned the connection")
	}
}

func TestIdleRejectsConcurrentWait(t *testing.T) {
	clientConn, serverConn := net.Pipe()
	defer serverConn.Close()
	continued := make(chan struct{})
	go func() {
		_, _ = serverConn.Write([]byte("* PREAUTH [CAPABILITY IDLE] ready\r\n"))
		r := bufio.NewReader(serverConn)
		_, _ = r.ReadString('\n')
		_, _ = serverConn.Write([]byte("+ idling\r\n"))
		close(continued)
		_, _ = r.ReadString('\n') // DONE
	}()
	c := NewClient(clientConn, nil)
	defer c.Close()
	if err := c.WaitGreeting(context.Background()); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	idle := c.Idle()
	first := make(chan error, 1)
	go func() { first <- idle.Wait(ctx) }()
	select {
	case <-continued:
	case <-time.After(time.Second):
		t.Fatal("IDLE did not enter the idle state")
	}
	deadline := time.After(time.Second)
	for {
		idle.mu.Lock()
		waiting := idle.waiting
		idle.mu.Unlock()
		if waiting && errors.Is(idle.Wait(context.Background()), errIdleWaitInProgress) {
			break
		}
		select {
		case <-deadline:
			t.Fatal("concurrent IDLE Wait was not rejected")
		default:
			time.Sleep(time.Millisecond)
		}
	}
	cancel()
	// The test server intentionally does not complete DONE; close the transport
	// after asserting waiter ownership so the first waiter can finish.
	_ = c.Close()
	<-first
}

func TestIdleCancellationBeforeContinuationClosesConnection(t *testing.T) {
	clientConn, serverConn := net.Pipe()
	defer serverConn.Close()
	go func() {
		_, _ = serverConn.Write([]byte("* PREAUTH [CAPABILITY IDLE] ready\r\n"))
		_, _ = bufio.NewReader(serverConn).ReadString('\n') // IDLE; deliberately no continuation
	}()
	c := NewClient(clientConn, nil)
	defer c.Close()
	if err := c.WaitGreeting(context.Background()); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := c.Idle().Wait(ctx); !errors.Is(err, context.Canceled) {
		t.Fatalf("IDLE Wait() = %v", err)
	}
	if c.State() != StateLogout {
		t.Fatalf("state after cancellation before IDLE continuation = %s, want logout", c.State())
	}
}

func TestIdleInvalidStateAndClosedReturnStartError(t *testing.T) {
	t.Run("not authenticated", func(t *testing.T) {
		clientConn, serverConn := net.Pipe()
		defer serverConn.Close()
		go func() { _, _ = serverConn.Write([]byte("* OK ready\r\n")) }()
		c := NewClient(clientConn, nil)
		defer c.Close()
		ctx, cancel := context.WithTimeout(context.Background(), time.Second)
		defer cancel()
		if err := c.WaitGreeting(ctx); err != nil {
			t.Fatal(err)
		}
		idle := c.Idle()
		if err := idle.Wait(ctx); err == nil || !strings.Contains(err.Error(), "not valid in not-authenticated state") {
			t.Fatalf("Idle Wait() = %v", err)
		}
		if err := idle.Done(); err == nil || !strings.Contains(err.Error(), "not valid in not-authenticated state") {
			t.Fatalf("Idle Done() = %v", err)
		}
	})

	t.Run("closed", func(t *testing.T) {
		clientConn, serverConn := net.Pipe()
		defer serverConn.Close()
		c := NewClient(clientConn, nil)
		if err := c.Close(); err != nil {
			t.Fatal(err)
		}
		if err := c.Idle().Wait(context.Background()); !errors.Is(err, net.ErrClosed) {
			t.Fatalf("Idle Wait() after Close = %v", err)
		}
	})
}
