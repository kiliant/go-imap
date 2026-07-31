//go:build interop

package imapclient_test

import (
	"context"
	"os"
	"testing"
	"time"

	"github.com/kiliant/go-imap/imapclient"
	"github.com/kiliant/go-imap/interop/harness"
)

func TestMain(m *testing.M) {
	os.Exit(harness.Run(m, harness.Profiles()))
}

func TestConnectionLifecycle(t *testing.T) {
	for _, server := range harness.RunningServers() {
		server := server
		t.Run(server.Profile.Name, func(t *testing.T) {
			t.Parallel()
			ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
			defer cancel()

			client, err := imapclient.Dial(ctx, server.Address, nil)
			if err == nil {
				err = client.Noop().Wait(ctx)
			}
			if err == nil {
				err = client.Logout(ctx)
			}
			if err != nil {
				if client != nil {
					_ = client.Close()
				}
				server.LogDiagnostics(context.Background(), t, nil)
				t.Fatal(err)
			}
		})
	}
}
