package maildir_test

import (
	"context"
	"testing"
	"time"

	"github.com/kiliant/go-imap/imapserver"
	"github.com/kiliant/go-imap/imapserver/backendtest"
	"github.com/kiliant/go-imap/imapserver/maildir"
)

func TestBackendConformance(t *testing.T) {
	backendtest.Run(t, &backendtest.Harness{New: func(ctx context.Context, _ *backendtest.InstanceOptions) (*backendtest.Instance, error) {
		backend, err := maildir.Open(ctx, t.TempDir(), &maildir.Options{Accounts: []maildir.AccountOptions{{Username: "alice", Password: "secret"}}})
		if err != nil {
			return nil, err
		}
		t.Cleanup(func() {
			ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
			defer cancel()
			if err := backend.Close(ctx, nil); err != nil {
				t.Error(err)
			}
		})
		return &backendtest.Instance{Backend: backend, Credentials: imapserver.Credentials{Mechanism: "PLAIN", Username: "alice", Password: "secret"}}, nil
	}})
}
