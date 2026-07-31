package imapclient

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"time"

	"github.com/kiliant/go-imap"
)

const (
	defaultIdleTimeout      = 25 * time.Minute
	maxIdleTimeout          = 29 * time.Minute
	defaultIdlePollInterval = time.Minute
)

// IdleCommand is a long-lived IDLE session. Its Wait method transparently
// renews IDLE before the server's recommended 29-minute limit. Cancelling the
// Wait context sends DONE and preserves connection synchronisation.
//
// When IDLE is not advertised, Idle uses periodic NOOP commands instead. That
// fallback has polling latency controlled by [Options.IdlePollInterval].
type IdleCommand struct {
	client   *Client
	startErr error
	fallback bool
	timeout  time.Duration
	poll     time.Duration

	mu           sync.Mutex
	cycle        *idleCycle
	reissue      bool
	closed       bool
	waiting      bool
	pollDone     chan struct{}
	pollDoneOnce sync.Once
}

var errIdleWaitInProgress = errors.New("imapclient: IDLE wait is already in progress")

type idleCycle struct {
	command  *Command
	ready    chan struct{}
	entered  chan struct{}
	release  func()
	doneSent bool
}

// Idle begins receiving mailbox updates. The command is valid in the
// authenticated and selected states. While an IDLE command is active, the
// protocol permits no other command until DONE is sent; the client enforces
// that rule locally.
func (c *Client) Idle() *IdleCommand {
	cmd := &IdleCommand{client: c, timeout: c.idleTimeout(), poll: c.idlePollInterval()}
	c.mu.Lock()
	if c.closed {
		err := c.closeErr
		if err == nil {
			err = netClosedError{}
		}
		c.mu.Unlock()
		cmd.startErr = err
		return cmd
	}
	if c.idle != nil {
		c.mu.Unlock()
		cmd.startErr = &imap.Error{Type: imap.ErrorTypeProtocol, Text: "an IDLE command is already active"}
		return cmd
	}
	if state := c.state; state != StateAuthenticated && state != StateSelected {
		c.mu.Unlock()
		cmd.startErr = &imap.Error{Type: imap.ErrorTypeProtocol, Text: fmt.Sprintf("IDLE is not valid in %s state", state)}
		return cmd
	}
	_, idleAdvertised := c.caps["IDLE"]
	_, rev2Enabled := c.enabled["IMAP4REV2"]
	if !idleAdvertised && !rev2Enabled {
		c.mu.Unlock()
		cmd.fallback = true
		cmd.pollDone = make(chan struct{})
		return cmd
	}
	c.idle = cmd
	c.mu.Unlock()
	cmd.startCycle()
	return cmd
}

// WaitReady waits until the server has accepted IDLE by sending its
// continuation response. It reports a server rejection before that point. It
// is intended for callers that must know IDLE is active before causing a
// change from another connection.
//
// Cancelling ctx only stops this wait. It does not send DONE, cancel IDLE, or
// invalidate the connection; the caller remains responsible for calling Done
// or for using [IdleCommand.Wait] with its own lifetime context. WaitReady is
// unavailable when Idle is using its NOOP polling fallback.
func (cmd *IdleCommand) WaitReady(ctx context.Context) error {
	if cmd == nil || cmd.client == nil {
		return fmt.Errorf("imapclient: nil idle command")
	}
	if ctx == nil {
		return fmt.Errorf("imapclient: IDLE readiness requires a non-nil context")
	}
	if cmd.startErr != nil {
		return cmd.startErr
	}
	if cmd.fallback {
		return &imap.Error{Type: imap.ErrorTypeProtocol, Text: "IDLE readiness is unavailable while using NOOP polling"}
	}
	cmd.mu.Lock()
	cycle := cmd.cycle
	cmd.mu.Unlock()
	if cycle == nil {
		return fmt.Errorf("imapclient: IDLE did not start")
	}
	select {
	case <-cycle.ready:
	case <-ctx.Done():
		return ctx.Err()
	}
	// Prefer a received continuation over a concurrent tagged completion: the
	// server did accept IDLE, even if it subsequently ended the session.
	select {
	case <-cycle.entered:
		return nil
	default:
	}
	select {
	case <-cycle.entered:
		return nil
	case <-cycle.command.done:
		return cycle.command.err
	case <-ctx.Done():
		return ctx.Err()
	}
}

// Wait keeps the IDLE session active until ctx is cancelled, the server ends
// it, or the connection fails. A cancelled context sends DONE, waits for the
// tagged completion, and returns ctx.Err without poisoning the connection.
// Only one Wait may drive an IdleCommand at a time; a concurrent call returns
// an error rather than racing two renewal timers against the same session.
// If cancellation happens before the server's continuation, IDLE has not yet
// entered the RFC 2177 cancellation state and the connection is closed, just
// as it is for cancellation of any other command.
func (cmd *IdleCommand) Wait(ctx context.Context) error {
	if cmd == nil || cmd.client == nil {
		return fmt.Errorf("imapclient: nil idle command")
	}
	if ctx == nil {
		return fmt.Errorf("imapclient: IDLE requires a non-nil context")
	}
	if cmd.startErr != nil {
		return cmd.startErr
	}
	cmd.mu.Lock()
	if cmd.waiting {
		cmd.mu.Unlock()
		return errIdleWaitInProgress
	}
	cmd.waiting = true
	cmd.mu.Unlock()
	defer func() {
		cmd.mu.Lock()
		cmd.waiting = false
		cmd.mu.Unlock()
	}()
	if cmd.fallback {
		return cmd.waitPolling(ctx)
	}
	for {
		cycle := cmd.currentCycle()
		if cycle == nil || cycle.command == nil {
			return fmt.Errorf("imapclient: IDLE did not start")
		}
		// DONE is valid only after the continuation tells us the server has
		// entered IDLE. A server that rejects IDLE before then simply completes
		// the command normally.
		select {
		case <-cycle.entered:
		case <-cycle.command.done:
			cmd.finish()
			return cycle.command.err
		case <-ctx.Done():
			select {
			case <-cycle.entered:
				if err := cmd.stop(false); err != nil {
					return err
				}
				<-cycle.command.done
				return ctx.Err()
			default:
				// Before the continuation, IDLE has no clean cancellation token.
				// Treat this exactly like cancellation of another in-flight command.
				cmd.client.poison(ctx.Err())
				return ctx.Err()
			}
		}

		timer := time.NewTimer(cmd.timeout)
		select {
		case <-cycle.command.done:
			stopTimer(timer)
			cmd.finish()
			return cycle.command.err
		case <-ctx.Done():
			stopTimer(timer)
			if err := cmd.stop(false); err != nil {
				return err
			}
			<-cycle.command.done
			return ctx.Err()
		case <-timer.C:
			if err := cmd.stop(true); err != nil {
				return err
			}
			<-cycle.command.done
			if err := cycle.command.err; err != nil {
				cmd.finish()
				return err
			}
			// The timeout has already sent DONE. If the caller cancelled while
			// waiting for that old cycle's tagged completion, do not start a new
			// IDLE only to immediately treat that pre-continuation cancellation as
			// connection-poisoning.
			if err := ctx.Err(); err != nil {
				cmd.finish()
				return err
			}
			cmd.startCycle()
		}
	}
}

// Done sends the RFC 2177 DONE continuation. It is useful when the caller
// wants to end IDLE before cancelling its Wait context. Wait still reports the
// tagged completion. In NOOP polling fallback mode, Done stops future polls
// and Wait returns after any in-flight NOOP has received its tagged completion.
// Calling Done more than once is harmless.
func (cmd *IdleCommand) Done() error {
	if cmd == nil || cmd.client == nil {
		return fmt.Errorf("imapclient: nil idle command")
	}
	if cmd.startErr != nil {
		return cmd.startErr
	}
	if cmd.fallback {
		cmd.pollDoneOnce.Do(func() { close(cmd.pollDone) })
		return nil
	}
	cycle := cmd.currentCycle()
	if cycle == nil {
		return fmt.Errorf("imapclient: IDLE did not start")
	}
	select {
	case <-cycle.entered:
	default:
		return fmt.Errorf("imapclient: IDLE has not entered the idle state")
	}
	return cmd.stop(false)
}

func (cmd *IdleCommand) currentCycle() *idleCycle {
	cmd.mu.Lock()
	cycle := cmd.cycle
	cmd.mu.Unlock()
	if cycle != nil {
		<-cycle.ready
	}
	return cycle
}

func (cmd *IdleCommand) startCycle() {
	cycle := &idleCycle{ready: make(chan struct{}), entered: make(chan struct{})}
	cmd.mu.Lock()
	if cmd.closed {
		cmd.mu.Unlock()
		return
	}
	cmd.cycle = cycle
	cmd.reissue = false
	cmd.mu.Unlock()

	cmd.client.continuationOwnerMu.Lock()
	var once sync.Once
	var enteredOnce sync.Once
	var clear func()
	var clearMu sync.Mutex
	cycle.release = func() {
		once.Do(func() {
			clearMu.Lock()
			if clear != nil {
				clear()
			}
			clearMu.Unlock()
			cmd.client.continuationOwnerMu.Unlock()
		})
	}
	clearMu.Lock()
	clear = cmd.client.setContinuation(func(string) error {
		cycle.release()
		enteredOnce.Do(func() { close(cycle.entered) })
		return nil
	})
	clearMu.Unlock()
	cycle.command = cmd.client.issue("IDLE", commandOptions{
		allowed: stateAuthenticated | stateSelected,
		onComplete: func(success bool) {
			cycle.release()
			cmd.cycleComplete(cycle)
		},
		ownsContinuation: true,
	})
	close(cycle.ready)
	select {
	case <-cycle.command.done:
		cycle.release()
		cmd.cycleComplete(cycle)
	default:
	}
}

func (cmd *IdleCommand) cycleComplete(cycle *idleCycle) {
	cmd.mu.Lock()
	if cmd.cycle != cycle || cmd.reissue || cmd.closed {
		cmd.mu.Unlock()
		return
	}
	cmd.closed = true
	cmd.mu.Unlock()
	cmd.clearClientIdle()
}

func (cmd *IdleCommand) stop(reissue bool) error {
	cmd.mu.Lock()
	if cmd.closed {
		cmd.mu.Unlock()
		return nil
	}
	cycle := cmd.cycle
	if cycle != nil && cycle.doneSent {
		cmd.mu.Unlock()
		return nil
	}
	if cycle != nil {
		cycle.doneSent = true
		// The first DONE decides whether this completion starts a renewal.
		// A concurrent caller must not convert a timeout renewal into a final
		// shutdown (or vice versa) after DONE is already on the wire.
		cmd.reissue = reissue
	}
	cmd.mu.Unlock()
	if cycle == nil || cycle.command == nil {
		return fmt.Errorf("imapclient: IDLE did not start")
	}
	cmd.client.writeMu.Lock()
	cmd.client.mu.Lock()
	if cmd.client.closed {
		err := cmd.client.closeErr
		cmd.client.mu.Unlock()
		cmd.client.writeMu.Unlock()
		if err != nil {
			return err
		}
		return netClosedError{}
	}
	enc := cmd.client.enc
	cmd.client.mu.Unlock()
	enc.Atom("DONE").CRLF()
	err := enc.Flush()
	cmd.client.writeMu.Unlock()
	if err != nil {
		cmd.client.poison(protocolError(err))
		return protocolError(err)
	}
	cmd.client.trace(TraceClient, "DONE")
	return nil
}

func (cmd *IdleCommand) finish() {
	cmd.mu.Lock()
	if cmd.closed {
		cmd.mu.Unlock()
		return
	}
	cmd.closed = true
	cmd.mu.Unlock()
	cmd.clearClientIdle()
}

func (cmd *IdleCommand) clearClientIdle() {
	cmd.client.mu.Lock()
	if cmd.client.idle == cmd {
		cmd.client.idle = nil
	}
	cmd.client.mu.Unlock()
}

func (cmd *IdleCommand) waitPolling(ctx context.Context) error {
	for {
		select {
		case <-cmd.pollDone:
			return nil
		default:
		}
		noop := cmd.client.Noop()
		select {
		case <-noop.done:
			if err := noop.err; err != nil {
				return err
			}
		case <-ctx.Done():
			// NOOP has no cancellation token. Unlike Command.Wait, deliberately
			// wait for its tagged completion so the polling fallback preserves
			// stream synchronisation. This can add up to one NOOP round trip to
			// cancellation latency.
			<-noop.done
			return ctx.Err()
		case <-cmd.pollDone:
			<-noop.done
			return noop.err
		}
		timer := time.NewTimer(cmd.poll)
		select {
		case <-ctx.Done():
			stopTimer(timer)
			return ctx.Err()
		case <-cmd.pollDone:
			stopTimer(timer)
			return nil
		case <-timer.C:
		}
	}
}

func (c *Client) idleTimeout() time.Duration {
	d := c.opts.IdleTimeout
	if d <= 0 || d >= maxIdleTimeout {
		return defaultIdleTimeout
	}
	return d
}

func (c *Client) idlePollInterval() time.Duration {
	d := c.opts.IdlePollInterval
	if d <= 0 {
		return defaultIdlePollInterval
	}
	return d
}

func stopTimer(timer *time.Timer) {
	if !timer.Stop() {
		select {
		case <-timer.C:
		default:
		}
	}
}
