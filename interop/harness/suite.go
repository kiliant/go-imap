package harness

import (
	"context"
	"fmt"
	"os"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/kiliant/go-imap/interop/definition"
)

var (
	runningMu      sync.RWMutex
	runningServers []*Server
	runningCaps    map[string]map[string]bool
)

// Run starts each container once, provisions identical fixtures, runs the
// package tests, prints the capability table, and tears everything down. It is
// intended to be returned from TestMain via os.Exit.
func Run(m *testing.M, profiles []definition.Profile) int {
	manager := NewManager()
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()

	selected, err := selectProfiles(profiles, os.Getenv("GO_IMAP_INTEROP_SERVERS"))
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 2
	}
	capabilities := make(map[string]map[string]bool, len(selected))
	servers, err := startProfiles(ctx, manager, selected)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		closeManager(manager)
		return 1
	}
	for _, server := range servers {
		profile := server.Profile
		trace := new(Trace)
		session, err := DialSession(ctx, server.Address, trace)
		if err == nil {
			err = session.Login(ctx)
		}
		var caps map[string]bool
		if err == nil {
			caps, err = session.Capabilities(ctx)
		}
		if err == nil {
			missing := MissingExpectedCapabilities(profile, caps)
			if len(missing) != 0 {
				err = fmt.Errorf("profile claims capabilities not advertised: %s", strings.Join(missing, ", "))
			}
		}
		if err == nil {
			err = Seed(ctx, session, profile)
		}
		if session != nil {
			_ = session.Close()
		}
		if err != nil {
			fmt.Fprintf(os.Stderr, "interop: provision %s: %v\n", profile.Name, err)
			server.DumpDiagnostics(context.Background(), os.Stderr, trace)
			closeManager(manager)
			return 1
		}
		capabilities[profile.Name] = caps
	}

	runningMu.Lock()
	runningServers = servers
	runningCaps = capabilities
	runningMu.Unlock()

	code := m.Run()
	fmt.Fprintln(os.Stderr, "\ninterop capability table:")
	fmt.Fprint(os.Stderr, CapabilityTable(capabilities))
	closeManager(manager)
	return code
}

type startResult struct {
	index  int
	server *Server
	err    error
}

func startProfiles(ctx context.Context, manager *Manager, profiles []definition.Profile) ([]*Server, error) {
	startCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	results := make(chan startResult, len(profiles))
	for index, profile := range profiles {
		index, profile := index, profile
		go func() {
			fmt.Fprintf(os.Stderr, "interop: starting %s\n", profile.Name)
			server, err := manager.Start(startCtx, profile)
			results <- startResult{index: index, server: server, err: err}
		}()
	}
	servers := make([]*Server, len(profiles))
	var firstErr error
	for range profiles {
		result := <-results
		if result.err != nil {
			if firstErr == nil {
				firstErr = result.err
				cancel()
			}
			continue
		}
		servers[result.index] = result.server
	}
	if firstErr != nil {
		return nil, firstErr
	}
	return servers, nil
}

func closeManager(manager *Manager) {
	ctx, cancel := context.WithTimeout(context.Background(), defaultStopTimeout)
	defer cancel()
	if err := manager.Close(ctx); err != nil {
		fmt.Fprintf(os.Stderr, "interop: cleanup: %v\n", err)
	}
}

func selectProfiles(profiles []definition.Profile, selection string) ([]definition.Profile, error) {
	if strings.TrimSpace(selection) == "" {
		return profiles, nil
	}
	wanted := make(map[string]bool)
	for _, name := range strings.Split(selection, ",") {
		wanted[strings.TrimSpace(name)] = true
	}
	var selected []definition.Profile
	for _, profile := range profiles {
		if wanted[profile.Name] {
			selected = append(selected, profile)
			delete(wanted, profile.Name)
		}
	}
	if len(wanted) != 0 {
		unknown := make([]string, 0, len(wanted))
		for name := range wanted {
			unknown = append(unknown, name)
		}
		sort.Strings(unknown)
		return nil, fmt.Errorf("interop: unknown servers in GO_IMAP_INTEROP_SERVERS: %s", strings.Join(unknown, ", "))
	}
	return selected, nil
}

// RunningServers returns the servers owned by the current package's TestMain.
func RunningServers() []*Server {
	runningMu.RLock()
	defer runningMu.RUnlock()
	return append([]*Server(nil), runningServers...)
}

// CapabilitiesFor returns a copy of a live server's capability set.
func CapabilitiesFor(name string) map[string]bool {
	runningMu.RLock()
	defer runningMu.RUnlock()
	copy := make(map[string]bool, len(runningCaps[name]))
	for capability, present := range runningCaps[name] {
		copy[capability] = present
	}
	return copy
}
