package imapserver

import (
	"context"
	"errors"
	"testing"
	"time"
)

func TestCommandWaitsForReaderSettlement(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	c := &conn{ctx: ctx, server: New(&expungeOrderBackend{}, nil)}
	c.inputPending.Store(true) // reader raised it before handing off this command
	settled := make(chan struct{})
	executed := make(chan bool, 1)
	command := &queuedCommand{
		name: "UID",
		descriptor: &commandDescriptor{states: stateMaskAny, handle: func(context.Context, *conn, *queuedCommand) error {
			executed <- c.inputPending.Load()
			return nil
		}},
		readerSettled: settled,
	}
	done := make(chan error, 1)
	go func() { done <- c.execute(command) }()
	select {
	case pending := <-executed:
		close(settled)
		t.Fatalf("command ran before reader settled; inputPending = %v", pending)
	case <-time.After(50 * time.Millisecond):
	}
	c.inputPending.Store(false)
	close(settled)
	select {
	case pending := <-executed:
		if pending {
			t.Fatal("command saw stale pending-input state")
		}
	case <-ctx.Done():
		t.Fatal(ctx.Err())
	}
	if err := <-done; err != nil {
		t.Fatal(err)
	}
}

func TestCommandReaderSettlementIsCancelable(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	c := &conn{ctx: ctx, server: New(&expungeOrderBackend{}, nil)}
	command := &queuedCommand{
		readerSettled: make(chan struct{}),
		descriptor: &commandDescriptor{states: stateMaskAny, handle: func(context.Context, *conn, *queuedCommand) error {
			t.Error("canceled handoff executed the handler")
			return nil
		}},
	}
	cancel()
	if err := c.execute(command); !errors.Is(err, context.Canceled) {
		t.Fatalf("execute while handoff canceled = %v, want context canceled", err)
	}
}
