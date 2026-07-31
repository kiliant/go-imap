//go:build interop

package imapclient_test

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/kiliant/go-imap/imapclient"
	"github.com/kiliant/go-imap/interop/harness"
)

const (
	authInteropUsername = "interop@example.test"
	authInteropPassword = "interop-pw"
)

type authInteropCase struct {
	mechanism string
}

func TestAuthenticationCapabilityProfiles(t *testing.T) {
	for _, server := range harness.RunningServers() {
		server := server
		t.Run(server.Profile.Name, func(t *testing.T) {
			harness.AssertProfile(t, server.Profile, harness.CapabilitiesFor(server.Profile.Name))
			var mechanisms []string
			switch server.Profile.Name {
			case "dovecot":
				mechanisms = []string{"PLAIN", "CRAM-MD5", "SCRAM-SHA-256"}
			case "stalwart":
				mechanisms = []string{"OAUTHBEARER", "PLAIN"}
			default:
				return
			}

			ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
			defer cancel()
			client, err := imapclient.Dial(ctx, server.Address, nil)
			if err != nil {
				server.LogDiagnostics(context.Background(), t, nil)
				t.Fatal(err)
			}
			defer client.Close()

			for _, mechanism := range mechanisms {
				if !client.Capabilities()["AUTH="+mechanism] {
					t.Errorf("%s profile does not advertise AUTH=%s before authentication: %s", server.Profile.Name, mechanism, formatCapabilities(client.Capabilities()))
				}
			}
		})
	}
}

func TestAuthenticationInterop(t *testing.T) {
	for _, server := range harness.RunningServers() {
		server := server
		t.Run(server.Profile.Name, func(t *testing.T) {
			harness.AssertProfile(t, server.Profile, harness.CapabilitiesFor(server.Profile.Name))
			cases := authCasesFor(server.Profile.Name)
			if len(cases) == 0 {
				t.Skip("no T04 authentication mechanism is configured for this profile")
			}
			for _, test := range cases {
				test := test
				t.Run(test.mechanism, func(t *testing.T) {
					testAuthenticationInterop(t, server, test)
				})
			}
		})
	}
}

func authCasesFor(profile string) []authInteropCase {
	switch profile {
	case "dovecot":
		return []authInteropCase{
			{mechanism: "PLAIN"},
			{mechanism: "CRAM-MD5"},
			{mechanism: "SCRAM-SHA-256"},
		}
	case "stalwart":
		return []authInteropCase{{mechanism: "OAUTHBEARER"}, {mechanism: "PLAIN"}}
	default:
		return nil
	}
}

func testAuthenticationInterop(t *testing.T, server *harness.Server, test authInteropCase) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()

	trace := new(authInteropTrace)
	options := &imapclient.Options{Trace: trace.Add}
	if test.mechanism == "PLAIN" {
		// The harness exposes the Dovecot test listener without TLS.  This is an
		// explicit test-only opt-in; downgrade refusal is covered by auth_test.go.
		options.AllowInsecureAuth = true
	}
	client, err := imapclient.Dial(ctx, server.Address, options)
	if err != nil {
		server.LogDiagnostics(context.Background(), t, trace)
		t.Fatal(err)
	}
	defer client.Close()

	// AUTH= mechanisms are deliberately pre-authentication capabilities.  The
	// harness capability table is post-authentication, so gate against the
	// production client's greeting-derived snapshot.
	harness.RequireCapabilities(t, client.Capabilities(), "AUTH="+test.mechanism)

	token := ""
	if server.Profile.Name == "stalwart" && test.mechanism == "OAUTHBEARER" {
		token, err = stalwartOAuthToken(ctx, server)
		if err != nil {
			server.LogDiagnostics(context.Background(), t, trace)
			t.Fatal(err)
		}
	}

	err = client.Authenticate(ctx, authInteropUsername, authInteropPassword, &imapclient.AuthenticateOptions{
		Mechanism: test.mechanism,
		Token:     token,
	})
	if err == nil && client.State() != imapclient.StateAuthenticated {
		err = fmt.Errorf("state after %s authentication = %s, want authenticated", test.mechanism, client.State())
	}
	if err == nil {
		err = client.Noop().Wait(ctx)
	}
	if err == nil {
		err = client.Logout(ctx)
	}
	if err != nil {
		server.LogDiagnostics(context.Background(), t, trace)
		t.Fatal(err)
	}
}

type authInteropTrace struct {
	mu sync.Mutex
	b  strings.Builder
}

func (trace *authInteropTrace) Add(event imapclient.TraceEvent) {
	trace.mu.Lock()
	defer trace.mu.Unlock()
	fmt.Fprintf(&trace.b, "%s %s\n", event.Direction, event.Data)
}

func (trace *authInteropTrace) String() string {
	trace.mu.Lock()
	defer trace.mu.Unlock()
	return trace.b.String()
}

func formatCapabilities(capabilities map[string]bool) string {
	return harness.FormatCapabilities(capabilities)
}

func stalwartOAuthToken(ctx context.Context, server *harness.Server) (string, error) {
	address, ok := server.AddressForPort(8080)
	if !ok {
		return "", fmt.Errorf("Stalwart profile does not publish its HTTP OAuth listener")
	}
	baseURL := "http://" + address

	var metadata stalwartOAuthMetadata
	if err := getOAuthJSON(ctx, baseURL+"/.well-known/oauth-authorization-server", &metadata); err != nil {
		return "", fmt.Errorf("get OAuth metadata: %w", err)
	}
	registrationURL, err := localOAuthEndpoint(baseURL, metadata.RegistrationEndpoint)
	if err != nil {
		return "", fmt.Errorf("registration endpoint: %w", err)
	}
	var registration stalwartOAuthRegistration
	if err := postOAuthJSON(ctx, registrationURL, nil, stalwartOAuthRegistrationRequest{
		RedirectURIs: []string{"https://localhost"},
	}, &registration); err != nil {
		return "", fmt.Errorf("register OAuth client: %w", err)
	}
	if registration.ClientID == "" {
		return "", fmt.Errorf("register OAuth client: empty client ID")
	}

	var codeResponse stalwartOAuthCodeResponse
	if err := postOAuthJSON(ctx, baseURL+"/api/oauth", &oauthBasicCredentials, stalwartOAuthCodeRequest{
		Type:        "code",
		ClientID:    registration.ClientID,
		RedirectURI: "https://localhost",
		Nonce:       "go-imap-interop",
	}, &codeResponse); err != nil {
		return "", fmt.Errorf("authorize OAuth code: %w", err)
	}
	if codeResponse.Data.Code == "" {
		return "", fmt.Errorf("authorize OAuth code: empty code")
	}

	tokenURL, err := localOAuthEndpoint(baseURL, metadata.TokenEndpoint)
	if err != nil {
		return "", fmt.Errorf("token endpoint: %w", err)
	}
	var token stalwartOAuthTokenResponse
	if err := postOAuthForm(ctx, tokenURL, url.Values{
		"client_id":    {registration.ClientID},
		"redirect_uri": {"https://localhost"},
		"grant_type":   {"authorization_code"},
		"code":         {codeResponse.Data.Code},
	}, &token); err != nil {
		return "", fmt.Errorf("exchange OAuth code: %w", err)
	}
	if token.AccessToken == "" {
		return "", fmt.Errorf("exchange OAuth code: server returned no access token")
	}
	return token.AccessToken, nil
}

var oauthBasicCredentials = struct{ username, password string }{
	username: authInteropUsername,
	password: authInteropPassword,
}

type stalwartOAuthMetadata struct {
	TokenEndpoint        string `json:"token_endpoint"`
	RegistrationEndpoint string `json:"registration_endpoint"`
}

type stalwartOAuthRegistrationRequest struct {
	RedirectURIs []string `json:"redirect_uris"`
}

type stalwartOAuthRegistration struct {
	ClientID string `json:"client_id"`
}

type stalwartOAuthCodeRequest struct {
	Type        string `json:"type"`
	ClientID    string `json:"client_id"`
	RedirectURI string `json:"redirect_uri"`
	Nonce       string `json:"nonce"`
}

type stalwartOAuthCodeResponse struct {
	Data struct {
		Code string `json:"code"`
	} `json:"data"`
}

type stalwartOAuthTokenResponse struct {
	AccessToken string `json:"access_token"`
}

func localOAuthEndpoint(baseURL, endpoint string) (string, error) {
	base, err := url.Parse(baseURL)
	if err != nil {
		return "", err
	}
	parsed, err := url.Parse(endpoint)
	if err != nil {
		return "", err
	}
	if !parsed.IsAbs() {
		return base.ResolveReference(parsed).String(), nil
	}
	parsed.Scheme = base.Scheme
	parsed.Host = base.Host
	return parsed.String(), nil
}

func getOAuthJSON(ctx context.Context, endpoint string, output any) error {
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, nil)
	if err != nil {
		return err
	}
	return doOAuthJSON(request, output)
}

func postOAuthJSON(ctx context.Context, endpoint string, basic *struct{ username, password string }, input, output any) error {
	body, err := json.Marshal(input)
	if err != nil {
		return err
	}
	request, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, strings.NewReader(string(body)))
	if err != nil {
		return err
	}
	request.Header.Set("Content-Type", "application/json")
	if basic != nil {
		request.SetBasicAuth(basic.username, basic.password)
	}
	return doOAuthJSON(request, output)
}

func postOAuthForm(ctx context.Context, endpoint string, values url.Values, output any) error {
	request, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, strings.NewReader(values.Encode()))
	if err != nil {
		return err
	}
	request.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	return doOAuthJSON(request, output)
}

func doOAuthJSON(request *http.Request, output any) error {
	response, err := (&http.Client{Timeout: 10 * time.Second}).Do(request)
	if err != nil {
		return err
	}
	defer response.Body.Close()
	if response.StatusCode < http.StatusOK || response.StatusCode >= http.StatusMultipleChoices {
		return fmt.Errorf("HTTP %s", response.Status)
	}
	if err := json.NewDecoder(response.Body).Decode(output); err != nil {
		return err
	}
	return nil
}
