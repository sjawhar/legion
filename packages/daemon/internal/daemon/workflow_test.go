package daemon

import (
	"context"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/sjawhar/legion/daemon/internal/appauth"
	"github.com/sjawhar/legion/daemon/internal/config"
	"github.com/sjawhar/legion/daemon/internal/ghrepo"
)

// linger_hours reaches the workflow engine as the duration it names, a decimal included: the
// Stage 4b proof lingers 0.3 hours so a tree closes inside one run, and an hour count truncated to
// zero would reach the engine as no linger at all, closing a finished tree the moment it ends.
func TestLingerHoursReachesTheEngineAsItsDuration(t *testing.T) {
	path := filepath.Join(t.TempDir(), "legion.yaml")
	body := "project: demo\nstate_dir: /var/lib/legion\npostgres_dsn: postgres://legion@127.0.0.1:5432/legion\nlinger_hours: 0.3\n"
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg, err := config.LoadForValidation(path, func(string) string { return "" })
	if err != nil {
		t.Fatalf("LoadForValidation: %v", err)
	}

	if got := engineConfig(cfg, "legion-reviewer[bot]").Linger; got != 18*time.Minute {
		t.Errorf("the engine's linger = %s, want 18m0s", got)
	}
}

// loginTokens answers each App role's lease, and its login, with the bot login named for it.
type loginTokens map[appauth.AppRole]string

func (t loginTokens) Token(_ context.Context, role appauth.AppRole, _ string) (appauth.Lease, error) {
	return appauth.Lease{Token: "token", ExpiresAt: time.Now().Add(time.Hour), Identity: appauth.GitIdentity{Name: t[role]}}, nil
}

func (t loginTokens) AppLogin(_ context.Context, role appauth.AppRole) (string, error) {
	return t[role], nil
}

func (loginTokens) InstalledOn(context.Context, appauth.AppRole, ghrepo.Repository) (bool, error) {
	return true, nil
}

// Forget drops nothing: every lease is minted afresh.
func (loginTokens) Forget(appauth.AppRole, string) {}

func (t loginTokens) ReadToken(ctx context.Context, role appauth.AppRole, owner string) (appauth.Lease, error) {
	lease, err := t.Token(ctx, role, owner)
	lease.Token += "-read"
	return lease, err
}

// The engine judges a push by its pusher against the review App's login, so a configuration whose
// two roles resolve to one App would count no fix attempt at all: boot refuses it, and a review App
// with no login, before anything else opens.
func TestOpenWorkflowRefusesAReviewLoginItCannotTellFromTheImplementers(t *testing.T) {
	dispatchServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !servedMapping(t, w, r, "demo") {
			t.Errorf("Dispatch request = %s %s", r.Method, r.URL.Path)
		}
	}))
	t.Cleanup(dispatchServer.Close)
	cfg := config.Config{
		Project: "demo", DispatchURL: dispatchServer.URL,
		Projects: map[string]config.Project{"demo": {}},
	}
	github := repositoriesGitHub(t)
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	for name, tokens := range map[string]loginTokens{
		"one App for both roles": {appauth.Implement: "legion-bot[bot]", appauth.Review: "legion-bot[bot]"},
		"no review login":        {appauth.Implement: "legion-implementer[bot]"},
	} {
		t.Run(name, func(t *testing.T) {
			_, err := openWorkflow(context.Background(), cfg, nil, "project-id", "dispatch-token", newRepositories(cfg.Project), log, tokens, github)
			if err == nil || !strings.Contains(err.Error(), "the workflow needs two different Apps") {
				t.Fatalf("openWorkflow = %v, want the two-Apps refusal", err)
			}
		})
	}
}

// The boot reads the review App's bot login, the login the engine judges a push's pusher against,
// under the App's own JWT, with no owner to mint for.
func TestAppLoginsAtBootReturnsTheReviewAppLogin(t *testing.T) {
	tokens := loginTokens{appauth.Implement: "legion-implementer[bot]", appauth.Review: "legion-reviewer[bot]"}
	login, err := appLoginsAtBoot(context.Background(), tokens, slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err != nil {
		t.Fatalf("appLoginsAtBoot: %v", err)
	}
	if login != "legion-reviewer[bot]" {
		t.Fatalf("appLoginsAtBoot = %q, want the review App's login %q", login, "legion-reviewer[bot]")
	}
}
