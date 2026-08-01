//go:build ignore

// Package main provides shared helpers for the runnable examples under
// examples/. Each program reads connection settings from the environment:
//
//   - IMAP_ADDR (required): host:port, e.g. localhost:1143
//   - IMAP_USER (required): account name
//   - IMAP_PASS: password for LOGIN / PLAIN / SCRAM
//   - IMAP_TOKEN: bearer token for OAUTHBEARER / XOAUTH2
//   - IMAP_TLS: when "1", use implicit TLS ([imapclient.DialTLS])
//   - IMAP_STARTTLS: when "1", upgrade with STARTTLS ([imapclient.DialStartTLS])
//   - IMAP_INSECURE: when "1", skip TLS verification (test servers only)
package main

import (
	"context"
	"fmt"
	"os"
	"time"

	"github.com/kiliant/go-imap/imapclient"
)

func mustEnv(key string) string {
	v := os.Getenv(key)
	if v == "" {
		fmt.Fprintf(os.Stderr, "missing required environment variable %s\n", key)
		os.Exit(2)
	}
	return v
}

func envBool(key string) bool {
	return os.Getenv(key) == "1" || os.Getenv(key) == "true"
}

// dialExample connects using the environment variables above.
func dialExample(ctx context.Context) (*imapclient.Client, error) {
	return dialExampleWithOptions(ctx, nil)
}

func dialExampleWithOptions(ctx context.Context, opts *imapclient.Options) (*imapclient.Client, error) {
	if opts == nil {
		opts = &imapclient.Options{}
	}
	if envBool("IMAP_INSECURE") {
		opts.InsecureSkipVerify = true
	}
	addr := mustEnv("IMAP_ADDR")
	switch {
	case envBool("IMAP_TLS"):
		return imapclient.DialTLS(ctx, addr, opts)
	case envBool("IMAP_STARTTLS"):
		return imapclient.DialStartTLS(ctx, addr, opts)
	default:
		return imapclient.Dial(ctx, addr, opts)
	}
}

func exampleContext() (context.Context, context.CancelFunc) {
	return context.WithTimeout(context.Background(), 2*time.Minute)
}

func authenticate(ctx context.Context, c *imapclient.Client) error {
	user := mustEnv("IMAP_USER")
	if token := os.Getenv("IMAP_TOKEN"); token != "" {
		return c.Authenticate(ctx, user, "", &imapclient.AuthenticateOptions{
			Mechanism: "OAUTHBEARER",
			Token:     token,
		})
	}
	pass := mustEnv("IMAP_PASS")
	return c.Authenticate(ctx, user, pass, nil)
}
