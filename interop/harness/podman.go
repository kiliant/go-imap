package harness

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"sort"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/kiliant/go-imap/interop/definition"
)

const (
	defaultStartTimeout = 3 * time.Minute
	defaultStopTimeout  = 15 * time.Second
)

type commandRunner interface {
	Run(context.Context, ...string) (string, error)
}

type podmanRunner struct{}

func (podmanRunner) Run(ctx context.Context, args ...string) (string, error) {
	cmd := exec.CommandContext(ctx, "podman", args...)
	out, err := cmd.CombinedOutput()
	if err != nil {
		return string(out), fmt.Errorf("podman %s: %w\n%s", strings.Join(args, " "), err, out)
	}
	return strings.TrimSpace(string(out)), nil
}

// Manager owns the containers started for one test package.
type Manager struct {
	runner       commandRunner
	interopRoot  string
	startTimeout time.Duration

	mu      sync.Mutex
	servers []*Server
}

// NewManager constructs a podman-backed container manager.
func NewManager() *Manager {
	return &Manager{
		runner:       podmanRunner{},
		interopRoot:  interopRoot(),
		startTimeout: defaultStartTimeout,
	}
}

// Server is one running interoperability server.
type Server struct {
	Profile definition.Profile
	ID      string
	Address string

	additionalAddresses map[int]string

	manager *Manager
	closed  atomic.Bool
}

var containerSequence atomic.Uint64

// Start builds (when needed), starts, and greeting-polls a profile.
func (m *Manager) Start(ctx context.Context, profile definition.Profile) (_ *Server, err error) {
	if err := validateProfile(profile); err != nil {
		return nil, err
	}
	ctx, cancel := context.WithTimeout(ctx, m.startTimeout)
	defer cancel()

	image := profile.Image
	if profile.BuildContext != "" {
		image = "localhost/go-imap-interop-" + profile.Name + ":v1"
		buildContext := profile.BuildContext
		if !filepath.IsAbs(buildContext) {
			buildContext = filepath.Join(m.interopRoot, buildContext)
		}
		if _, err := m.runner.Run(ctx, "build", "--tag", image, buildContext); err != nil {
			return nil, err
		}
	}

	// Every package with a TestMain owns an independent harness lifecycle and
	// runs as its own OS process (see docs/INTEROP.md); "go test ./interop/..."
	// can start several of those processes concurrently. Two packages that
	// both start, say, "dovecot" within the same wall-clock second would
	// otherwise generate identical container names from a per-process
	// sequence counter and a second-resolution timestamp alone. The PID
	// disambiguates them; discovered when interop/saslprep and interop/smoke
	// collided on "go-imap-stalwart-<ts>-2" in the same second.
	name := fmt.Sprintf("go-imap-%s-%d-%d-%d", profile.Name, os.Getpid(), time.Now().Unix(), containerSequence.Add(1))
	args := []string{"run", "--detach", "--name", name, "--publish", fmt.Sprintf("127.0.0.1::%d", profile.ContainerPort)}
	for _, port := range profile.AdditionalPorts {
		args = append(args, "--publish", fmt.Sprintf("127.0.0.1::%d", port))
	}
	if profile.Tier == definition.TierEmulated {
		args = append(args, "--arch", "amd64")
	}
	keys := make([]string, 0, len(profile.Environment))
	for key := range profile.Environment {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	for _, key := range keys {
		args = append(args, "--env", key+"="+profile.Environment[key])
	}
	args = append(args, image)
	args = append(args, profile.Arguments...)

	runOutput, err := m.runner.Run(ctx, args...)
	if err != nil {
		return nil, err
	}
	id, err := extractContainerID(runOutput)
	if err != nil {
		return nil, err
	}
	server := &Server{Profile: profile, ID: id, manager: m, additionalAddresses: make(map[int]string)}
	defer func() {
		if err != nil {
			stopCtx, stopCancel := context.WithTimeout(context.Background(), defaultStopTimeout)
			defer stopCancel()
			_ = server.Stop(stopCtx)
		}
	}()

	address, err := m.waitReady(ctx, server)
	if err != nil {
		logCtx, logCancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer logCancel()
		logs, logErr := server.Logs(logCtx)
		if logErr != nil {
			logs += "\n(log collection failed: " + logErr.Error() + ")"
		}
		return nil, fmt.Errorf("interop: %s did not become ready: %w\nserver log:\n%s", profile.Name, err, logs)
	}
	server.Address = address
	for _, port := range profile.AdditionalPorts {
		address, err := m.publishedAddress(ctx, server, port)
		if err != nil {
			return nil, err
		}
		server.additionalAddresses[port] = address
	}
	for _, command := range profile.ProvisionCommands {
		if len(command) == 0 {
			return nil, fmt.Errorf("interop: %s has an empty provision command", profile.Name)
		}
		args := append([]string{"exec", server.ID}, command...)
		if _, err := m.runner.Run(ctx, args...); err != nil {
			return nil, fmt.Errorf("interop: provision %s with %q: %w", profile.Name, command, err)
		}
	}

	m.mu.Lock()
	m.servers = append(m.servers, server)
	m.mu.Unlock()
	return server, nil
}

func extractContainerID(output string) (string, error) {
	lines := strings.Split(strings.TrimSpace(output), "\n")
	for i := len(lines) - 1; i >= 0; i-- {
		candidate := strings.TrimSpace(lines[i])
		if len(candidate) < 12 || len(candidate) > 64 {
			continue
		}
		valid := true
		for _, r := range candidate {
			if (r < '0' || r > '9') && (r < 'a' || r > 'f') {
				valid = false
				break
			}
		}
		if valid {
			return candidate, nil
		}
	}
	return "", fmt.Errorf("interop: podman run returned no container ID:\n%s", output)
}

func (m *Manager) waitReady(ctx context.Context, server *Server) (string, error) {
	ticker := time.NewTicker(100 * time.Millisecond)
	defer ticker.Stop()
	var lastErr error
	for {
		address, err := m.publishedAddress(ctx, server, server.Profile.ContainerPort)
		if err == nil {
			probeCtx, cancel := context.WithTimeout(ctx, 500*time.Millisecond)
			err = probeGreeting(probeCtx, address)
			cancel()
			if err == nil {
				return address, nil
			}
		}
		lastErr = err
		status, statusErr := m.runner.Run(ctx, "inspect", "--format", "{{.State.Status}}", server.ID)
		if statusErr == nil && status != "running" && status != "created" {
			return "", fmt.Errorf("container entered state %q before its IMAP greeting", status)
		}
		select {
		case <-ctx.Done():
			return "", errors.Join(ctx.Err(), lastErr)
		case <-ticker.C:
		}
	}
}

func (m *Manager) publishedAddress(ctx context.Context, server *Server, port int) (string, error) {
	out, err := m.runner.Run(ctx, "port", server.ID, strconv.Itoa(port)+"/tcp")
	if err != nil {
		return "", err
	}
	line := strings.TrimSpace(strings.Split(out, "\n")[0])
	if line == "" {
		return "", fmt.Errorf("interop: podman returned no published port")
	}
	host, publishedPort, err := net.SplitHostPort(line)
	if err != nil {
		return "", fmt.Errorf("interop: parse published port %q: %w", line, err)
	}
	if host == "0.0.0.0" || host == "::" {
		host = "127.0.0.1"
	}
	return net.JoinHostPort(host, publishedPort), nil
}

// AddressForPort returns the host address for a port published by this
// profile. The primary IMAP port is also available as Address.
func (s *Server) AddressForPort(port int) (string, bool) {
	if port == s.Profile.ContainerPort {
		return s.Address, s.Address != ""
	}
	address, ok := s.additionalAddresses[port]
	return address, ok
}

// Logs returns the server's complete container log.
func (s *Server) Logs(ctx context.Context) (string, error) {
	return s.manager.runner.Run(ctx, "logs", s.ID)
}

// DumpDiagnostics writes both server logs and the client-side trace.
func (s *Server) DumpDiagnostics(ctx context.Context, dst io.Writer, trace fmt.Stringer) {
	fmt.Fprintf(dst, "\n=== %s server log ===\n", s.Profile.Name)
	if logs, err := s.Logs(ctx); err != nil {
		fmt.Fprintf(dst, "log error: %v\n", err)
	} else {
		fmt.Fprintln(dst, logs)
	}
	if trace != nil {
		fmt.Fprintf(dst, "=== %s client wire trace ===\n%s", s.Profile.Name, trace.String())
	}
}

// LogDiagnostics writes DumpDiagnostics' output to the test log.
//
// It buffers and emits one t.Log rather than taking a writer from the testing
// package, because testing.T.Output — the obvious destination for an io.Writer —
// requires go1.25, and go.mod deliberately keeps the floor a major lower so
// consumers on the previous release build without a toolchain download. A single
// Log call is also better output than a streaming writer would give: t.Log
// indents a multi-line block uniformly, so the aligned tables in a diagnostic
// dump survive, where a line-by-line writer prefixes every line with its own
// file:line and destroys the columns.
func (s *Server) LogDiagnostics(ctx context.Context, t testing.TB, trace fmt.Stringer) {
	t.Helper()
	var buf bytes.Buffer
	s.DumpDiagnostics(ctx, &buf, trace)
	t.Log(buf.String())
}

// Stop removes a running container. It is idempotent.
func (s *Server) Stop(ctx context.Context) error {
	if !s.closed.CompareAndSwap(false, true) {
		return nil
	}
	_, err := s.manager.runner.Run(ctx, "rm", "--force", "--time", "2", s.ID)
	return err
}

// Close stops all containers owned by the manager, in reverse start order.
func (m *Manager) Close(ctx context.Context) error {
	m.mu.Lock()
	servers := append([]*Server(nil), m.servers...)
	m.servers = nil
	m.mu.Unlock()
	var errs []error
	for i := len(servers) - 1; i >= 0; i-- {
		if err := servers[i].Stop(ctx); err != nil {
			errs = append(errs, err)
		}
	}
	return errors.Join(errs...)
}

func probeGreeting(ctx context.Context, address string) error {
	conn, err := (&net.Dialer{}).DialContext(ctx, "tcp", address)
	if err != nil {
		return err
	}
	defer conn.Close()
	if deadline, ok := ctx.Deadline(); ok {
		if err := conn.SetDeadline(deadline); err != nil {
			return err
		}
	}
	line, err := readBoundedLine(conn, 64<<10)
	if err != nil {
		return err
	}
	upper := strings.ToUpper(string(line))
	if !strings.HasPrefix(upper, "* OK") && !strings.HasPrefix(upper, "* PREAUTH") {
		return fmt.Errorf("unexpected IMAP greeting %q", line)
	}
	return nil
}

func interopRoot() string {
	_, file, _, ok := runtime.Caller(0)
	if !ok {
		return "."
	}
	return filepath.Dir(filepath.Dir(file))
}
