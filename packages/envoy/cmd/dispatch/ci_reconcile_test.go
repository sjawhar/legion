package main

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/sjawhar/envoy/internal/cistore"
	"github.com/sjawhar/envoy/internal/dispatch/githubapp/githubapptest"
	"github.com/sjawhar/envoy/internal/dispatch/store"
	"github.com/sjawhar/envoy/internal/dispatch/store/storetest"
	"github.com/sjawhar/envoy/internal/logging"
	"github.com/sjawhar/envoy/internal/testnats"
)

// What the reconcile stand-in below serves: the row repo_projects holds, lower-cased as every
// mapping is stored, the name and id GitHub itself answers with, and the one open pull request's
// head the pass reads the checks of.
const (
	ciReconcileRow        = "acme/widgets"
	ciReconcileOwner      = "Acme"
	ciReconcileName       = "Widgets"
	ciReconcileRepository = ciReconcileOwner + "/" + ciReconcileName
	ciReconcileHeadSHA    = "abcdef1234567890abcdef1234567890abcdef12"
	// ciReconcileReviewerApp is DISPATCH_REVIEWER_APP_ID for the process, the App that created
	// the `tester` check run below; a pass that never read the variable would drop that run.
	ciReconcileReviewerApp = "5341243"
)

// The CI reconcile pass is a loop of the real envoy-dispatch binary, so this proves it there:
// the actual process, against a real NATS whose envoy_ci_state bucket the test made as the
// listener makes it (the pass creates no bucket), a loopback GitHub serving the installation
// lookup, the token mint and the four reads one pass makes, and one repo_projects row for the
// repository it serves. The process's own log is the evidence: the duty loop's leased pass under
// the ci-reconcile lease, and the pass's own INFO line with the three records it wrote -- an
// ordinary completed check run, the review App's `tester` verdict and the head's check suite.
func TestTheCIReconcilePassRunsInTheDispatchProcess(t *testing.T) {
	database := storetest.Open(t)
	mapCIReconcileRepository(t, database)
	_, natsURL := testnats.Start(t)
	makeCIStateBucket(t, natsURL)
	github := startCIReconcileGitHub(t)
	_, pemText := githubapptest.Key(t)

	process := startDispatchProcessWith(t, database.Pool.Config().ConnString(), dispatchOptions{
		NATSURL: natsURL,
		Env: map[string]string{
			"DISPATCH_APP_CLIENT_ID":   "Iv1.cireconcile",
			"DISPATCH_APP_PEM_B64":     pemText,
			"DISPATCH_GITHUB_API_BASE": github.URL,
			"DISPATCH_REVIEWER_APP_ID": ciReconcileReviewerApp,
			// Five minutes is longer than any test may wait for a pass.
			"DISPATCH_TEST_CI_RECONCILE_INTERVAL": "1s",
		},
	})
	process.waitHealthy(t)
	process.WaitForOutput(t,
		`msg="dispatch: reconciling the CI records GitHub's webhook never delivered" client_id=Iv1.cireconcile interval=1s`,
		`msg="ci reconcile pass" list_pages=1 heads=1 head_requests=2 heads_not_modified=0 records=3 mappings_skipped=0 heads_too_large=0 rate_limited=false`,
		`msg="dispatch duty: leased pass ran" lease=ci-reconcile`,
	)
}

// mapCIReconcileRepository maps the stand-in's repository to a project of its own, as Settings →
// Repositories does: the pass reads GitHub for the repositories repo_projects names and no other.
func mapCIReconcileRepository(t *testing.T, database *store.Store) {
	t.Helper()
	if _, err := database.Pool.Exec(t.Context(), `
		insert into projects (key, name) values ('CIR', 'CI reconcile') on conflict (key) do nothing
	`); err != nil {
		t.Fatalf("seed the project: %v", err)
	}
	if _, err := database.Pool.Exec(t.Context(), `
		insert into repo_projects (repo, project, created_by)
		values ($1, 'CIR', '{"kind":"system","id":"test"}'::jsonb)
	`, ciReconcileRow); err != nil {
		t.Fatalf("map %s to a project: %v", ciReconcileRow, err)
	}
}

// makeCIStateBucket makes envoy_ci_state on the test's NATS, as the Envoy listener makes it. The
// pass binds that bucket through cistore.OpenWriter and creates none, so a process whose NATS
// has no bucket writes nothing at all.
func makeCIStateBucket(t *testing.T, natsURL string) {
	t.Helper()
	conn := testnats.Connect(t, natsURL)
	t.Cleanup(conn.Close)
	if _, err := cistore.Open(conn, logging.New("ci-reconcile-test"), cistore.WithTTL(time.Hour)); err != nil {
		t.Fatalf("make the CI state bucket: %v", err)
	}
}

// startCIReconcileGitHub is the loopback GitHub the process's App calls go to, so none leaves
// this machine: the installation lookup and the token mint every installation token needs, and
// the four reads one pass makes of one repository. The webhook redelivery sweep's own listing
// answers empty, and every other path answers GitHub's 404, as GitHub answers a path the App
// cannot see.
func startCIReconcileGitHub(t *testing.T) *httptest.Server {
	t.Helper()
	installation := map[string]any{
		"id":          1,
		"app_slug":    "cireconcile",
		"permissions": map[string]string{"checks": "read", "pull_requests": "read", "contents": "read"},
	}
	write := func(w http.ResponseWriter, status int, payload any) {
		body, err := json.Marshal(payload)
		if err != nil {
			t.Errorf("encode the stand-in's answer: %v", err)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(status)
		_, _ = w.Write(body)
	}
	mux := http.NewServeMux()
	mux.HandleFunc("GET /repos/{owner}/{repo}/installation", func(w http.ResponseWriter, _ *http.Request) {
		write(w, http.StatusOK, installation)
	})
	mux.HandleFunc("POST /app/installations/{id}/access_tokens", func(w http.ResponseWriter, _ *http.Request) {
		write(w, http.StatusCreated, map[string]any{
			"token":      "ghs_cireconcile",
			"expires_at": time.Now().Add(time.Hour).Format(time.RFC3339),
		})
	})
	mux.HandleFunc("GET /repos/{owner}/{repo}", func(w http.ResponseWriter, _ *http.Request) {
		// The name GitHub itself gives the repository, which every record is keyed by, rather
		// than the lower-cased row the pass read it from.
		write(w, http.StatusOK, map[string]any{"full_name": ciReconcileRepository, "id": 4242})
	})
	mux.HandleFunc("GET /repos/{owner}/{repo}/pulls", func(w http.ResponseWriter, _ *http.Request) {
		write(w, http.StatusOK, []map[string]any{
			{"number": 7, "state": "open", "head": map[string]any{"sha": ciReconcileHeadSHA}},
		})
	})
	mux.HandleFunc("GET /repos/{owner}/{repo}/commits/{sha}/check-runs", func(w http.ResponseWriter, _ *http.Request) {
		write(w, http.StatusOK, map[string]any{"total_count": 2, "check_runs": []map[string]any{
			{
				"id": 11, "name": "build", "head_sha": ciReconcileHeadSHA,
				"status": "completed", "conclusion": "success",
				"completed_at": "2026-01-02T03:04:05Z",
				"app":          map[string]any{"id": 777},
			},
			{
				"id": 12, "name": "tester", "head_sha": ciReconcileHeadSHA,
				"status": "completed", "conclusion": "success",
				"completed_at": "2026-01-02T03:04:06Z",
				"app":          map[string]any{"id": ciReconcileReviewerApp},
			},
		}})
	})
	mux.HandleFunc("GET /repos/{owner}/{repo}/commits/{sha}/check-suites", func(w http.ResponseWriter, _ *http.Request) {
		write(w, http.StatusOK, map[string]any{"total_count": 1, "check_suites": []map[string]any{
			{
				"id": 21, "head_sha": ciReconcileHeadSHA,
				"status": "completed", "conclusion": "success",
				"updated_at": "2026-01-02T03:04:07Z",
			},
		}})
	})
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/app/hook/deliveries" {
			write(w, http.StatusOK, []any{})
			return
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusNotFound)
		_, _ = io.WriteString(w, `{"message":"Not Found"}`)
	})
	server := httptest.NewServer(mux)
	t.Cleanup(server.Close)
	return server
}
