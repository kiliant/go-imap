package harness

import (
	"context"
	"fmt"
	"time"

	"github.com/kiliant/go-imap/interop/definition"
)

// The in-process branch of the harness.
//
// T12 built the matrix around one assumption: a profile is a container image,
// and starting it means asking podman. That is true of every third-party server
// and false of the one server this repository ships, which is a Go value in the
// test process. Making our own coverage comparable with Dovecot's and
// Stalwart's in the same table means the harness has to be able to hold both.
//
// Everything after startup is deliberately shared with the container path.
// A native profile is seeded over APPEND, asserted against its declared
// capabilities and reported in the same table by the same code — because the
// entire value of the entry is that its row is produced the same way as the
// rows it is compared against. Only acquisition and teardown differ.

// startNative runs a TierInProcess profile and applies the same readiness
// probe the container path uses.
func (m *Manager) startNative(ctx context.Context, profile definition.Profile) (_ *Server, err error) {
	// nil rather than an empty struct: the harness configures nothing today, and
	// the nil-means-defaults contract is worth having a caller that takes it —
	// otherwise the branch is written for the first time on the day a field
	// lands, having never run.
	instance, err := profile.Native(ctx, nil)
	if err != nil {
		return nil, fmt.Errorf("interop: start native %s: %w", profile.Name, err)
	}
	if instance == nil {
		return nil, fmt.Errorf("interop: native %s returned no server", profile.Name)
	}
	if instance.Stop == nil {
		return nil, fmt.Errorf("interop: native %s returned no Stop function", profile.Name)
	}
	server := &Server{
		Profile:             profile,
		Address:             instance.Address,
		additionalAddresses: make(map[int]string),
		native:              instance,
		manager:             m,
	}
	defer func() {
		if err != nil {
			stopCtx, cancel := context.WithTimeout(context.Background(), defaultStopTimeout)
			defer cancel()
			_ = server.Stop(stopCtx)
		}
	}()
	if instance.Address == "" {
		return nil, fmt.Errorf("interop: native %s returned no address", profile.Name)
	}

	// The same greeting probe as a container, for the same reason: a listener
	// that accepts before its backend is ready would otherwise hand the suite a
	// server that fails its first command. In-process startup is near-instant,
	// so this normally succeeds on the first attempt.
	if err := waitGreeting(ctx, instance.Address); err != nil {
		return nil, fmt.Errorf("interop: native %s did not become ready: %w\nserver log:\n%s",
			profile.Name, err, server.nativeLogs())
	}

	m.mu.Lock()
	m.servers = append(m.servers, server)
	m.mu.Unlock()
	return server, nil
}

// waitGreeting polls until the address answers with an IMAP greeting.
func waitGreeting(ctx context.Context, address string) error {
	ticker := time.NewTicker(20 * time.Millisecond)
	defer ticker.Stop()
	var lastErr error
	for {
		probeCtx, cancel := context.WithTimeout(ctx, 500*time.Millisecond)
		lastErr = probeGreeting(probeCtx, address)
		cancel()
		if lastErr == nil {
			return nil
		}
		select {
		case <-ctx.Done():
			return fmt.Errorf("%w (last attempt: %v)", ctx.Err(), lastErr)
		case <-ticker.C:
		}
	}
}

func (s *Server) nativeLogs() string {
	if s.native == nil || s.native.Logs == nil {
		return "(this server records no log)"
	}
	return s.native.Logs()
}
