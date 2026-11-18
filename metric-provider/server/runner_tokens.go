package server

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"strconv"
	"strings"
	"time"

	ghinstallation "github.com/bradleyfalzon/ghinstallation/v2"
	"github.com/google/go-github/v75/github"
	"google.golang.org/api/idtoken"
)

// runnerTokenBroker holds signing credentials outside workers. Its API cannot choose a
// repository, installation, permission set, or GitHub URL supplied by the caller.
type runnerTokenBroker struct {
	audience      string
	callerSubject string
	repository    string
	githubClient  *http.Client
	validateToken func(context.Context, string, string) (*idtoken.Payload, error)
}

type runnerToken struct {
	Token     string    `json:"token"`
	ExpiresAt time.Time `json:"expires_at"`
}

func newRunnerTokenBroker(ctx context.Context, secrets interface {
	ReadSecret(context.Context, string, string) (string, error)
}) (*runnerTokenBroker, error) {
	audience := os.Getenv("RUNNER_TOKEN_AUDIENCE")
	if audience == "" {
		return nil, nil
	}
	subject := os.Getenv("RUNNER_TOKEN_CALLER_SUBJECT")
	repository := os.Getenv("RUNNER_TOKEN_REPOSITORY")
	secret := os.Getenv("RUNNER_TOKEN_KEY_SECRET")
	appID, appErr := strconv.ParseInt(os.Getenv("RUNNER_TOKEN_APP_ID"), 10, 64)
	installationID, installationErr := strconv.ParseInt(os.Getenv("RUNNER_TOKEN_INSTALLATION_ID"), 10, 64)
	owner, repositoryName, found := strings.Cut(repository, "/")
	if subject == "" || secret == "" || !found || owner == "" || repositoryName == "" ||
		strings.Contains(repositoryName, "/") || appErr != nil || installationErr != nil || appID <= 0 || installationID <= 0 {
		return nil, fmt.Errorf("invalid runner token broker configuration")
	}
	key, err := secrets.ReadSecret(ctx, secret, "latest")
	if err != nil {
		return nil, fmt.Errorf("cannot read runner token signing key: %w", err)
	}
	transport, err := ghinstallation.New(http.DefaultTransport, appID, installationID, []byte(key))
	if err != nil {
		return nil, fmt.Errorf("invalid runner token signing key")
	}
	transport.InstallationTokenOptions = &github.InstallationTokenOptions{
		Repositories: []string{repositoryName},
		Permissions:  &github.InstallationPermissions{Administration: github.Ptr("write")},
	}
	validator, err := idtoken.NewValidator(ctx)
	if err != nil {
		return nil, fmt.Errorf("cannot initialize runner identity validator: %w", err)
	}
	return &runnerTokenBroker{
		audience: audience, callerSubject: subject, repository: repository,
		githubClient:  &http.Client{Transport: transport, Timeout: 20 * time.Second},
		validateToken: validator.Validate,
	}, nil
}

func (broker *runnerTokenBroker) ServeHTTP(writer http.ResponseWriter, request *http.Request) {
	writer.Header().Set("Cache-Control", "no-store")
	if request.Method != http.MethodPost {
		http.Error(writer, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	operation := ""
	switch request.URL.Path {
	case "/runner-tokens/registration":
		operation = "registration-token"
	case "/runner-tokens/removal":
		operation = "remove-token"
	default:
		http.NotFound(writer, request)
		return
	}
	credential, found := strings.CutPrefix(request.Header.Get("Authorization"), "Bearer ")
	if !found || credential == "" {
		http.Error(writer, "identity token required", http.StatusUnauthorized)
		return
	}
	identity, err := broker.validateToken(request.Context(), credential, broker.audience)
	if err != nil || identity == nil {
		http.Error(writer, "invalid identity token", http.StatusUnauthorized)
		return
	}
	if (identity.Issuer != "https://accounts.google.com" && identity.Issuer != "accounts.google.com") ||
		identity.Subject != broker.callerSubject || identity.Claims["email_verified"] != true {
		http.Error(writer, "worker identity required", http.StatusForbidden)
		return
	}
	body, err := io.ReadAll(io.LimitReader(request.Body, 1))
	if err != nil || len(body) > 0 || request.URL.RawQuery != "" {
		http.Error(writer, "request parameters are not accepted", http.StatusBadRequest)
		return
	}
	githubRequest, err := http.NewRequestWithContext(request.Context(), http.MethodPost,
		"https://api.github.com/repos/"+broker.repository+"/actions/runners/"+operation, nil)
	if err != nil {
		http.Error(writer, "token issuance unavailable", http.StatusInternalServerError)
		return
	}
	githubRequest.Header.Set("Accept", "application/vnd.github+json")
	githubRequest.Header.Set("X-GitHub-Api-Version", "2022-11-28")
	response, err := broker.githubClient.Do(githubRequest)
	if err != nil {
		http.Error(writer, "GitHub token issuance failed", http.StatusBadGateway)
		return
	}
	defer response.Body.Close()
	var token runnerToken
	if response.StatusCode != http.StatusCreated ||
		json.NewDecoder(io.LimitReader(response.Body, 16384)).Decode(&token) != nil ||
		token.Token == "" || !token.ExpiresAt.After(time.Now()) {
		// Do not forward upstream errors, which may include credential material.
		http.Error(writer, "GitHub token issuance failed", http.StatusBadGateway)
		return
	}
	writer.Header().Set("Content-Type", "application/json")
	json.NewEncoder(writer).Encode(token)
}
