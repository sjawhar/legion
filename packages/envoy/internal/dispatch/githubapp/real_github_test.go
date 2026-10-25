package githubapp_test

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/url"
	"os"
	"strings"
	"testing"
	"time"

	"gopkg.in/yaml.v3"

	"github.com/sjawhar/envoy/internal/dispatch/auth"
	"github.com/sjawhar/envoy/internal/dispatch/githubapp"
	"github.com/sjawhar/envoy/internal/dispatch/githubapp/githubapptest"
)

// TestRealGitHubRefReadsALargeHead resolves, against GitHub itself, the head of
// DISPATCH_REAL_GITHUB_BRANCH of DISPATCH_REAL_GITHUB_REPO (owner/repo), a branch whose commit
// answer in JSON is over the 1 MiB response cap, with the installation token of the gh hosts file
// DISPATCH_REAL_GITHUB_HOSTS_FILE names. It first reads that JSON answer whole, so the fixture is
// proven large rather than assumed, then Client.Ref must answer the same SHA. It is gated on
// DISPATCH_REAL_GITHUB_BRANCH: CI holds no App token.
func TestRealGitHubRefReadsALargeHead(t *testing.T) {
	branch := os.Getenv("DISPATCH_REAL_GITHUB_BRANCH")
	if branch == "" {
		t.Skip("set DISPATCH_REAL_GITHUB_BRANCH, DISPATCH_REAL_GITHUB_REPO and DISPATCH_REAL_GITHUB_HOSTS_FILE to run the real GitHub large-head proof")
	}
	get := func(name string) string {
		value := os.Getenv(name)
		if value == "" {
			t.Fatalf("%s is required with DISPATCH_REAL_GITHUB_BRANCH", name)
		}
		return value
	}
	repository := get("DISPATCH_REAL_GITHUB_REPO")
	owner, repo, ok := strings.Cut(repository, "/")
	if !ok || owner == "" || repo == "" || strings.Contains(repo, "/") {
		t.Fatalf("DISPATCH_REAL_GITHUB_REPO = %q, want owner/repo", repository)
	}
	token := hostsFileToken(t, get("DISPATCH_REAL_GITHUB_HOSTS_FILE"))
	ctx := context.Background()

	// The precondition: GitHub's JSON answer for the commit, read without a cap, is over 1 MiB.
	target := "https://api.github.com/repos/" + url.PathEscape(owner) + "/" + url.PathEscape(repo) +
		"/commits/" + url.PathEscape(branch)
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, target, nil)
	if err != nil {
		t.Fatalf("build GET %s: %v", target, err)
	}
	request.Header.Set("Authorization", "Bearer "+token)
	request.Header.Set("Accept", "application/vnd.github+json")
	response, err := (&http.Client{Timeout: 60 * time.Second}).Do(request)
	if err != nil {
		t.Fatalf("GET %s: %v", target, err)
	}
	body, err := io.ReadAll(response.Body)
	_ = response.Body.Close()
	if err != nil {
		t.Fatalf("read GET %s: %v", target, err)
	}
	if response.StatusCode != http.StatusOK {
		t.Fatalf("GET %s: status %d", target, response.StatusCode)
	}
	if len(body) <= 1<<20 {
		t.Fatalf("GET %s answered %d bytes of JSON, want more than 1048576: the fixture is too small", target, len(body))
	}
	var commit struct {
		SHA string `json:"sha"`
	}
	if err := json.Unmarshal(body, &commit); err != nil || commit.SHA == "" {
		t.Fatalf("decode the commit of %s@%s: sha=%q err=%v", repository, branch, commit.SHA, err)
	}
	t.Logf("GET %s: %d bytes of JSON, sha %s", target, len(body), commit.SHA)

	// The App key is never used: Ref reads under the installation token it is given.
	_, keyPEM := githubapptest.Key(t)
	client, err := githubapp.New(&auth.AppConfig{ClientID: "Iv1.real-github-ref", PEM: keyPEM}, "")
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	sha, err := client.Ref(ctx, token, owner, repo, branch)
	if err != nil {
		t.Fatalf("Ref: %v", err)
	}
	if sha != commit.SHA {
		t.Fatalf("Ref = %q, want %q", sha, commit.SHA)
	}
	t.Logf("Ref(%s@%s) = %s", repository, branch, sha)
}

// hostsFileToken returns the oauth_token of the github.com block of a gh hosts.yml. The token
// is never logged.
func hostsFileToken(t *testing.T, path string) string {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("DISPATCH_REAL_GITHUB_HOSTS_FILE: %v", err)
	}
	var hosts struct {
		GitHub struct {
			OAuthToken string `yaml:"oauth_token"`
		} `yaml:"github.com"`
	}
	// The decode error is not printed: yaml errors can quote the file, and the file holds a token.
	if err := yaml.Unmarshal(data, &hosts); err != nil {
		t.Fatalf("DISPATCH_REAL_GITHUB_HOSTS_FILE %s does not parse as YAML", path)
	}
	if hosts.GitHub.OAuthToken == "" {
		t.Fatalf("DISPATCH_REAL_GITHUB_HOSTS_FILE %s holds no oauth_token for github.com", path)
	}
	return hosts.GitHub.OAuthToken
}
