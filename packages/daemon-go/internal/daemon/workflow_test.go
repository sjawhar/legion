package daemon

import (
	"context"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/sjawhar/legion/daemon/internal/appauth"
	"github.com/sjawhar/legion/daemon/internal/config"
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

// loginTokens answers each App role's lease with the bot login named for it.
type loginTokens map[appauth.AppRole]string

func (t loginTokens) Token(_ context.Context, role appauth.AppRole, _ string) (appauth.Lease, error) {
	return appauth.Lease{Token: "token", ExpiresAt: time.Now().Add(time.Hour), Identity: appauth.GitIdentity{Name: t[role]}}, nil
}

// The engine judges a push by its pusher against the review App's login, so a configuration whose
// two roles resolve to one App would count no fix attempt at all: boot refuses it, and a review
// lease with no login, before anything else opens.
func TestOpenWorkflowRefusesAReviewLoginItCannotTellFromTheImplementers(t *testing.T) {
	cfg := config.Config{
		Project: "demo", DispatchURL: "http://127.0.0.1:1",
		Projects: map[string]config.Project{"demo": {Repo: "acme/widgets"}},
	}
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	for name, tokens := range map[string]loginTokens{
		"one App for both roles": {appauth.Implement: "legion-bot[bot]", appauth.Review: "legion-bot[bot]"},
		"no review login":        {appauth.Implement: "legion-implementer[bot]"},
	} {
		t.Run(name, func(t *testing.T) {
			_, err := openWorkflow(context.Background(), cfg, nil, "project-id", log, tokens)
			if err == nil || !strings.Contains(err.Error(), "the workflow needs two different Apps") {
				t.Fatalf("openWorkflow = %v, want the two-Apps refusal", err)
			}
		})
	}
}
