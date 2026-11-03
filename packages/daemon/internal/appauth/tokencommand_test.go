package appauth

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/sjawhar/legion/daemon/internal/config"
	"github.com/sjawhar/legion/daemon/internal/ghrepo"
)

// fakeBroker is a token_command that answers as `agent-secrets <NAME> [--github-owner O
// [--github-read-only]] -- sh -c 'printf %s "$NAME"'` does: the release of every installation it
// holds, or of the owner LEGION_GITHUB_OWNER names alone (a non-zero exit, as the broker's
// refusal, for an owner it holds none on), each token naming the owner and whether it is
// read-only. Every run appends its owner and read-only flag to the calls file.
type fakeBroker struct {
	dir     string
	command string
	calls   string
}

func newFakeBroker(t *testing.T, owners []string, expires time.Time) fakeBroker {
	t.Helper()
	dir := t.TempDir()
	calls := filepath.Join(dir, "calls")
	var cases strings.Builder
	var every []string
	for i, owner := range owners {
		entry := fmt.Sprintf(`"%s":{"installation_id":%d,"token":"ghs_%s_$mode","expires_at":"%s"}`, owner, 100+i, strings.ToLower(owner), expires.Format(time.RFC3339))
		every = append(every, entry)
		fmt.Fprintf(&cases, "  %s) printf '{\"installations\":{%%s}}' \"%s\" ;;\n", strings.ToLower(owner), strings.ReplaceAll(entry, `"`, `\"`))
	}
	script := fmt.Sprintf(`#!/bin/sh
printf '%%s|%%s\n' "$LEGION_GITHUB_OWNER" "$LEGION_GITHUB_READ_ONLY" >>%q
mode=write
[ -z "$LEGION_GITHUB_READ_ONLY" ] || mode=read
[ -z "$FAKE_BROKER_FAIL" ] || { echo "broker: refused" >&2; exit 3; }
owner=$(printf %%s "$LEGION_GITHUB_OWNER" | tr A-Z a-z)
case "$owner" in
  "") printf '{"installations":{%%s}}' "%s" ;;
%s  *) echo "agent-secrets: github app has no installation on $LEGION_GITHUB_OWNER" >&2; exit 1 ;;
esac
`, calls, strings.ReplaceAll(strings.Join(every, ","), `"`, `\"`), cases.String())
	command := filepath.Join(dir, "broker")
	if err := os.WriteFile(command, []byte(script), 0o700); err != nil {
		t.Fatal(err)
	}
	return fakeBroker{dir: dir, command: command, calls: calls}
}

func (b fakeBroker) runs(t *testing.T) []string {
	t.Helper()
	data, err := os.ReadFile(b.calls)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		t.Fatal(err)
	}
	return strings.Split(strings.TrimSuffix(string(data), "\n"), "\n")
}

// fakeTokenGitHub answers what token mode asks of GitHub under an installation token: GraphQL's
// viewer, GET /apps/<slug>, GET /users/<login> and GET /installation/repositories. It refuses any
// JWT-only route (GET /app, the installation listing and exchange), since token mode signs none.
type fakeTokenGitHub struct {
	mu        sync.Mutex
	slug      string
	appID     string
	repos     []string
	asked     []string
	viewerErr int
}

func (g *fakeTokenGitHub) serve(t *testing.T) *httptest.Server {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		g.mu.Lock()
		defer g.mu.Unlock()
		auth := r.Header.Get("Authorization")
		if !strings.HasPrefix(auth, "token ghs_") {
			t.Errorf("%s %s with authorization %q, want an installation token", r.Method, r.URL.Path, auth)
		}
		g.asked = append(g.asked, r.Method+" "+r.URL.Path)
		switch {
		case r.Method == http.MethodPost && r.URL.Path == "/graphql":
			if g.viewerErr != 0 {
				w.WriteHeader(g.viewerErr)
				return
			}
			fmt.Fprintf(w, `{"data":{"viewer":{"login":"%s[bot]"}}}`, g.slug)
		case r.URL.Path == "/apps/"+g.slug:
			fmt.Fprintf(w, `{"id":%s,"slug":%q}`, g.appID, g.slug)
		case r.URL.Path == "/users/"+g.slug+"[bot]":
			fmt.Fprint(w, `{"id":271566630}`)
		case r.URL.Path == "/installation/repositories":
			var listed []string
			for _, repo := range g.repos {
				listed = append(listed, fmt.Sprintf(`{"full_name":%q}`, repo))
			}
			fmt.Fprintf(w, `{"total_count":%d,"repositories":[%s]}`, len(g.repos), strings.Join(listed, ","))
		default:
			t.Errorf("token mode asked GitHub %s %s", r.Method, r.URL.Path)
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	t.Cleanup(server.Close)
	return server
}

func tokenModeManager(server *httptest.Server, broker fakeBroker, now *time.Time, stderr *bytes.Buffer) *Manager {
	return New(config.GitHubApps{
		Implement: config.GitHubApp{AppID: "12345", TokenCommand: broker.command},
		Review:    config.GitHubApp{AppID: "67890", TokenCommand: broker.command + " review"},
	}, Options{BaseURL: server.URL, HTTPClient: server.Client(), Now: func() time.Time { return *now }, Stderr: stderr})
}

// A Token in token mode is the owner's entry of the command's release for that owner, matched
// without regard to case; its identity comes from the token alone (GraphQL viewer, GET /apps/<slug>
// checked against app_id, GET /users/<login>), read once; it is served from the cache until fewer
// than twenty minutes remain, then the command runs again.
func TestTokenModeMintsTheOwnersEntryAndRefreshes(t *testing.T) {
	now := time.Date(2026, 11, 3, 12, 0, 0, 0, time.UTC)
	broker := newFakeBroker(t, []string{"Sjawhar", "acme"}, now.Add(time.Hour))
	github := &fakeTokenGitHub{slug: "legion-implementer", appID: "12345"}
	var stderr bytes.Buffer
	tokens := tokenModeManager(github.serve(t), broker, &now, &stderr)

	lease, err := tokens.Token(context.Background(), Implement, "SJAWHAR")
	if err != nil {
		t.Fatalf("Token: %v", err)
	}
	if lease.Token != "ghs_sjawhar_write" || !lease.ExpiresAt.Equal(now.Add(time.Hour)) ||
		lease.Identity.Name != "legion-implementer[bot]" || lease.Identity.Email != "271566630+legion-implementer[bot]@users.noreply.github.com" {
		t.Fatalf("lease = %+v", lease)
	}
	if _, err := tokens.Token(context.Background(), Implement, "sjawhar"); err != nil {
		t.Fatalf("cached Token: %v", err)
	}
	if got := broker.runs(t); len(got) != 1 || got[0] != "SJAWHAR|" {
		t.Fatalf("command runs = %q, want one for SJAWHAR, write", got)
	}

	read, err := tokens.ReadToken(context.Background(), Implement, "acme")
	if err != nil || read.Token != "ghs_acme_read" {
		t.Fatalf("ReadToken = %+v, %v; want the read-only release's token", read, err)
	}
	if got := broker.runs(t); len(got) != 2 || got[1] != "acme|1" {
		t.Fatalf("command runs = %q, want the read token's run with LEGION_GITHUB_READ_ONLY=1", got)
	}

	now = now.Add(40*time.Minute + time.Second)
	if _, err := tokens.Token(context.Background(), Implement, "sjawhar"); err != nil {
		t.Fatalf("refreshed Token: %v", err)
	}
	if got := broker.runs(t); len(got) != 3 {
		t.Fatalf("command runs = %q, want a third run once fewer than twenty minutes remain", got)
	}
	github.mu.Lock()
	defer github.mu.Unlock()
	viewers := 0
	for _, asked := range github.asked {
		if asked == "POST /graphql" {
			viewers++
		}
	}
	if viewers != 1 {
		t.Fatalf("GitHub was asked %v, want the identity read once", github.asked)
	}
}

// An owner the command releases nothing for is ErrNotInstalled, an answer about the owner, whether
// the narrowed run fails (as the broker refuses an owner without an installation) or its release
// lacks the owner; a command that fails for every owner is its own error, never ErrNotInstalled,
// and carries its stderr.
func TestTokenModeAnOwnerWithoutAnInstallationIsNotInstalled(t *testing.T) {
	now := time.Date(2026, 11, 3, 12, 0, 0, 0, time.UTC)
	broker := newFakeBroker(t, []string{"sjawhar"}, now.Add(time.Hour))
	github := &fakeTokenGitHub{slug: "legion-implementer", appID: "12345"}
	var stderr bytes.Buffer
	tokens := tokenModeManager(github.serve(t), broker, &now, &stderr)

	if _, err := tokens.Token(context.Background(), Implement, "stranger"); !errors.Is(err, ErrNotInstalled) {
		t.Fatalf("Token for an owner with no installation = %v, want ErrNotInstalled", err)
	}
	if !strings.Contains(stderr.String(), "no installation on stranger") {
		t.Errorf("the command's stderr did not reach the daemon's: %q", stderr.String())
	}

	t.Setenv("FAKE_BROKER_FAIL", "1")
	_, err := tokens.Token(context.Background(), Implement, "sjawhar")
	if err == nil || errors.Is(err, ErrNotInstalled) || !strings.Contains(err.Error(), "github_apps.implement.token_command failed (exit 3): broker: refused") {
		t.Fatalf("Token with a failing command = %v, want the command's failure with its stderr", err)
	}
}

// What a command prints is never quoted in an error: output that is no release, an entry without
// a token or with an expired one, all fail naming the field and the account alone.
func TestTokenModeRefusesAMalformedReleaseWithoutQuotingIt(t *testing.T) {
	now := time.Date(2026, 11, 3, 12, 0, 0, 0, time.UTC)
	for _, tc := range []struct{ name, output, want string }{
		{"not JSON", `ghs_secret_value`, "printed no release"},
		{"no installations", `{"token":"ghs_secret_value"}`, "printed no release"},
		{"no token", `{"installations":{"acme":{"installation_id":1,"expires_at":"2026-11-03T13:00:00Z"}}}`, "released acme's installation without a token"},
		{"expired", `{"installations":{"acme":{"installation_id":1,"token":"ghs_secret_value","expires_at":"2026-11-03T11:00:00Z"}}}`, "released acme's installation with a token that expired"},
		{"no installation id", `{"installations":{"acme":{"token":"ghs_secret_value","expires_at":"2026-11-03T13:00:00Z"}}}`, "released acme's installation with no installation id"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, err := parseRelease([]byte(tc.output), "github_apps.implement.token_command", now)
			if err == nil || !strings.Contains(err.Error(), tc.want) || strings.Contains(err.Error(), "ghs_secret_value") {
				t.Fatalf("parseRelease = %v, want %q and no token", err, tc.want)
			}
		})
	}
}

// AppLogin in token mode reads the bot login under a token of any installation (the command run
// with no owner), and refuses a token whose App is not the configured app_id; GitHub's trouble on
// the viewer read is transient.
func TestTokenModeAppLoginReadsTheViewerAndChecksTheAppID(t *testing.T) {
	now := time.Date(2026, 11, 3, 12, 0, 0, 0, time.UTC)
	broker := newFakeBroker(t, []string{"sjawhar"}, now.Add(time.Hour))
	github := &fakeTokenGitHub{slug: "legion-reviewer", appID: "67890"}
	var stderr bytes.Buffer
	tokens := tokenModeManager(github.serve(t), broker, &now, &stderr)

	login, err := tokens.AppLogin(context.Background(), Review)
	if err != nil || login != "legion-reviewer[bot]" {
		t.Fatalf("AppLogin = %q, %v; want legion-reviewer[bot]", login, err)
	}
	if got := broker.runs(t); len(got) != 1 || got[0] != "|" {
		t.Fatalf("command runs = %q, want one with no owner", got)
	}

	other := tokenModeManager(github.serve(t), broker, &now, &stderr)
	_, err = other.AppLogin(context.Background(), Implement)
	if err == nil || !strings.Contains(err.Error(), "released a token of App 67890 (legion-reviewer), not of the configured app_id 12345") {
		t.Fatalf("AppLogin of a token of another App = %v, want the app_id refusal", err)
	}

	github.mu.Lock()
	github.viewerErr = http.StatusBadGateway
	github.mu.Unlock()
	fresh := tokenModeManager(github.serve(t), broker, &now, &stderr)
	var transient *TransientError
	if _, err := fresh.AppLogin(context.Background(), Review); !errors.As(err, &transient) {
		t.Fatalf("AppLogin on GitHub's 502 = %v, want a transient error", err)
	}
}

// InstalledOn in token mode reads the owner's installation's repositories: a repository it lists
// is installed, one it does not list, or an owner with no installation, is not; GitHub's trouble is
// transient. An installation token reads any public repository, so GET /repos/{owner}/{repo} could
// not say.
func TestTokenModeInstalledOnReadsTheInstallationsRepositories(t *testing.T) {
	now := time.Date(2026, 11, 3, 12, 0, 0, 0, time.UTC)
	broker := newFakeBroker(t, []string{"acme"}, now.Add(time.Hour))
	github := &fakeTokenGitHub{slug: "legion-implementer", appID: "12345", repos: []string{"acme/Widgets"}}
	var stderr bytes.Buffer
	tokens := tokenModeManager(github.serve(t), broker, &now, &stderr)

	for _, tc := range []struct {
		repo string
		want bool
	}{{"acme/widgets", true}, {"acme/gadgets", false}, {"stranger/widgets", false}} {
		got, err := tokens.InstalledOn(context.Background(), Implement, ghrepo.MustParse(tc.repo))
		if err != nil || got != tc.want {
			t.Errorf("InstalledOn(%s) = %v, %v; want %v", tc.repo, got, err, tc.want)
		}
	}

	t.Setenv("FAKE_BROKER_FAIL", "1")
	fresh := tokenModeManager(github.serve(t), broker, &now, &stderr)
	if _, err := fresh.InstalledOn(context.Background(), Implement, ghrepo.MustParse("acme/widgets")); err == nil || !strings.Contains(err.Error(), "acme/widgets") {
		t.Fatalf("InstalledOn with a failing command = %v, want an error naming the repository", err)
	}
}

// The command sees the daemon's environment less the NATS seeds, with the two variables the daemon
// sets replacing any it inherited; a command cut off by its context is transient.
func TestTokenModeCommandEnvironmentAndDeadline(t *testing.T) {
	now := time.Date(2026, 11, 3, 12, 0, 0, 0, time.UTC)
	dir := t.TempDir()
	seen := filepath.Join(dir, "env")
	t.Setenv("NATS_NKEY_SEED", "seed-value")
	t.Setenv(TokenCommandOwner, "inherited")
	t.Setenv(TokenCommandReadOnly, "inherited")
	command := fmt.Sprintf(`env >%q; printf '{"installations":{"acme":{"installation_id":1,"token":"ghs_x","expires_at":"%s"}}}'`, seen, now.Add(time.Hour).Format(time.RFC3339))
	tokens := New(config.GitHubApps{Implement: config.GitHubApp{AppID: "1", TokenCommand: command}}, Options{Now: func() time.Time { return now }})
	if _, err := tokens.runTokenCommand(context.Background(), Implement, tokens.apps.Implement, "acme", false); err != nil {
		t.Fatal(err)
	}
	env, _ := os.ReadFile(seen)
	if strings.Contains(string(env), "seed-value") || !strings.Contains(string(env), "\nLEGION_GITHUB_OWNER=acme\n") ||
		!strings.Contains(string(env), "\nLEGION_GITHUB_READ_ONLY=\n") || strings.Contains(string(env), "inherited") {
		t.Fatalf("the command's environment:\n%s", env)
	}

	slow := New(config.GitHubApps{Implement: config.GitHubApp{AppID: "1", TokenCommand: "sleep 30"}}, Options{})
	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()
	var transient *TransientError
	if _, err := slow.runTokenCommand(ctx, Implement, slow.apps.Implement, "acme", false); !errors.As(err, &transient) {
		t.Fatalf("a command past its deadline = %v, want a transient error", err)
	}
}
