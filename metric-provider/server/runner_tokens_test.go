package server

import (
	"bytes"
	"context"
	"crypto"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"encoding/pem"
	"fmt"
	"io"
	"math/big"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	ghinstallation "github.com/bradleyfalzon/ghinstallation/v2"
	"google.golang.org/api/idtoken"
	"google.golang.org/api/option"
)

type brokerTestTransport func(*http.Request) (*http.Response, error)

func (transport brokerTestTransport) RoundTrip(request *http.Request) (*http.Response, error) {
	return transport(request)
}

func TestRunnerTokenBrokerTrustBoundary(t *testing.T) {
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	encode := func(value any) string {
		encoded, err := json.Marshal(value)
		if err != nil {
			t.Fatal(err)
		}
		return base64.RawURLEncoding.EncodeToString(encoded)
	}
	certificates, err := json.Marshal(map[string]any{"keys": []any{map[string]string{
		"kty": "RSA", "alg": "RS256", "kid": "test-key", "use": "sig",
		"n": base64.RawURLEncoding.EncodeToString(key.N.Bytes()),
		"e": base64.RawURLEncoding.EncodeToString(big.NewInt(int64(key.E)).Bytes()),
	}}})
	if err != nil {
		t.Fatal(err)
	}
	validator, err := idtoken.NewValidator(context.Background(), option.WithHTTPClient(&http.Client{
		Transport: brokerTestTransport(func(request *http.Request) (*http.Response, error) {
			return &http.Response{StatusCode: 200, Header: http.Header{"Cache-Control": {"max-age=3600"}}, Body: io.NopCloser(bytes.NewReader(certificates))}, nil
		}),
	}))
	if err != nil {
		t.Fatal(err)
	}
	for _, scenario := range []struct {
		name       string
		audience   string
		subject    string
		issuer     string
		verified   bool
		expired    bool
		noToken    bool
		invalid    bool
		path       string
		body       string
		upstream   int
		wantStatus int
	}{
		{name: "registration", path: "/runner-tokens/registration", wantStatus: 200},
		{name: "removal", path: "/runner-tokens/removal", wantStatus: 200},
		{name: "missing identity", noToken: true, wantStatus: 401},
		{name: "forged signature", invalid: true, wantStatus: 401},
		{name: "wrong audience", audience: "https://another-service", wantStatus: 401},
		{name: "expired identity", expired: true, wantStatus: 401},
		{name: "other caller", subject: "other-service-account", wantStatus: 403},
		{name: "unverified identity", verified: true, wantStatus: 403},
		{name: "other issuer", issuer: "https://untrusted.example", wantStatus: 403},
		{name: "repository override", body: `{"repository":"Finni-Health/other"}`, wantStatus: 400},
		{name: "query override", path: "/runner-tokens/registration?repository=other", wantStatus: 400},
		{name: "arbitrary token endpoint", path: "/runner-tokens/installation", wantStatus: 404},
		{name: "upstream failure", upstream: 403, wantStatus: 502},
	} {
		t.Run(scenario.name, func(t *testing.T) {
			audience := "https://broker.example"
			claims := map[string]any{
				"iss": "https://accounts.google.com", "aud": audience,
				"sub": "worker-subject", "email_verified": true, "exp": time.Now().Add(time.Hour).Unix(),
			}
			if scenario.audience != "" {
				claims["aud"] = scenario.audience
			}
			if scenario.subject != "" {
				claims["sub"] = scenario.subject
			}
			if scenario.issuer != "" {
				claims["iss"] = scenario.issuer
			}
			if scenario.verified {
				claims["email_verified"] = false
			}
			if scenario.expired {
				claims["exp"] = time.Now().Add(-time.Minute).Unix()
			}
			unsigned := encode(map[string]string{"alg": "RS256", "kid": "test-key"}) + "." + encode(claims)
			digest := sha256.Sum256([]byte(unsigned))
			signature, err := rsa.SignPKCS1v15(rand.Reader, key, crypto.SHA256, digest[:])
			if err != nil {
				t.Fatal(err)
			}
			if scenario.invalid {
				signature[0] ^= 1
			}
			credential := unsigned + "." + base64.RawURLEncoding.EncodeToString(signature)
			issued := 0
			broker := &runnerTokenBroker{
				audience: audience, callerSubject: "worker-subject", repository: "Finni-Health/finni",
				validateToken: validator.Validate,
				githubClient: &http.Client{Transport: brokerTestTransport(func(request *http.Request) (*http.Response, error) {
					issued++
					if request.Method != http.MethodPost || request.URL.Host != "api.github.com" ||
						!strings.HasPrefix(request.URL.Path, "/repos/Finni-Health/finni/actions/runners/") {
						t.Fatalf("unexpected GitHub request: %s %s", request.Method, request.URL)
					}
					status := http.StatusCreated
					if scenario.upstream != 0 {
						status = scenario.upstream
					}
					return &http.Response{StatusCode: status, Body: io.NopCloser(strings.NewReader(
						fmt.Sprintf(`{"token":"runner-only","expires_at":%q,"installation_token":"must-not-leak"}`, time.Now().Add(time.Hour).Format(time.RFC3339)),
					))}, nil
				})},
			}
			path := scenario.path
			if path == "" {
				path = "/runner-tokens/registration"
			}
			request := httptest.NewRequest(http.MethodPost, path, strings.NewReader(scenario.body))
			if !scenario.noToken {
				request.Header.Set("Authorization", "Bearer "+credential)
			}
			response := httptest.NewRecorder()
			broker.ServeHTTP(response, request)
			if response.Code != scenario.wantStatus {
				t.Fatalf("got %d, want %d: %s", response.Code, scenario.wantStatus, response.Body.String())
			}
			if response.Header().Get("Cache-Control") != "no-store" || strings.Contains(response.Body.String(), "must-not-leak") {
				t.Fatal("credential response must not be cached or expose extra upstream fields")
			}
			if scenario.wantStatus < 500 && scenario.wantStatus != 200 && issued != 0 {
				t.Fatal("rejected request reached GitHub")
			}
			if scenario.wantStatus == 200 {
				var token runnerToken
				if json.Unmarshal(response.Body.Bytes(), &token) != nil || token.Token != "runner-only" || issued != 1 {
					t.Fatal("authorized request did not return one runner token")
				}
			}
		})
	}
}

type runnerTestSecretReader struct{ key string }

func (reader runnerTestSecretReader) ReadSecret(context.Context, string, string) (string, error) {
	return reader.key, nil
}

func TestRunnerBrokerInstallationScope(t *testing.T) {
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	t.Setenv("RUNNER_TOKEN_AUDIENCE", "https://broker.example")
	t.Setenv("RUNNER_TOKEN_CALLER_SUBJECT", "worker-subject")
	t.Setenv("RUNNER_TOKEN_REPOSITORY", "Finni-Health/finni")
	t.Setenv("RUNNER_TOKEN_KEY_SECRET", "app-key")
	t.Setenv("RUNNER_TOKEN_APP_ID", "5806192")
	t.Setenv("RUNNER_TOKEN_INSTALLATION_ID", "183069151")
	broker, err := newRunnerTokenBroker(context.Background(), runnerTestSecretReader{key: string(pem.EncodeToMemory(&pem.Block{Type: "RSA PRIVATE KEY", Bytes: x509.MarshalPKCS1PrivateKey(key)}))})
	if err != nil {
		t.Fatal(err)
	}
	transport := broker.githubClient.Transport.(*ghinstallation.Transport)
	options := transport.InstallationTokenOptions
	if len(options.Repositories) != 1 || options.Repositories[0] != "finni" || options.Permissions.GetAdministration() != "write" || options.Permissions.GetActions() != "" {
		t.Fatalf("installation token is not restricted to runner administration: %#v", options)
	}
}
