package githubrest

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

// CheckRuns reads every run on the commit, filter=all so a check's earlier runs are listed beside
// its latest, a hundred to a page and every page GitHub names, and keeps what the pass judges each
// run by: its id, name, status, conclusion (empty while it runs, GitHub's null), completion time
// (nil while it runs) and the App that created it. An answer other than 2xx is its Answer.
func TestCheckRunsReadsEveryRunOfTheCommitOnEveryPage(t *testing.T) {
	var reads int
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		reads++
		if r.URL.Path != "/repos/acme/widgets/commits/abc123/check-runs" {
			http.Error(w, `{"message":"No commit found for SHA"}`, http.StatusUnprocessableEntity)
			return
		}
		if r.URL.Query().Get("filter") != "all" || r.URL.Query().Get("per_page") != "100" {
			t.Errorf("read the check runs with %q, want filter=all and per_page=100", r.URL.RawQuery)
		}
		if r.URL.Query().Get("page") != "2" {
			w.Header().Set("Link", `<http://`+r.Host+`/repos/acme/widgets/commits/abc123/check-runs?filter=all&per_page=100&page=2>; rel="next"`)
			w.Write([]byte(`{"total_count":2,"check_runs":[{"id":11,"name":"test","head_sha":"abc123","status":"completed","conclusion":"timed_out",` +
				`"started_at":"2026-10-28T14:00:00Z","completed_at":"2026-10-28T14:05:00Z","app":{"id":15368,"slug":"github-actions"},"pull_requests":[]}]}`))
			return
		}
		w.Write([]byte(`{"total_count":2,"check_runs":[{"id":12,"name":"tester","head_sha":"abc123","status":"in_progress","conclusion":null,` +
			`"started_at":"2026-10-28T14:06:00Z","completed_at":null,"app":{"id":271000,"slug":"legion-review"},"pull_requests":[]}]}`))
	}))
	t.Cleanup(server.Close)
	github := Client{Token: "token", API: server.URL + "/repos/acme/widgets"}

	runs, err := github.CheckRuns(context.Background(), "abc123")
	if err != nil || len(runs) != 2 || reads != 2 {
		t.Fatalf("CheckRuns = %+v, %v after %d reads; want both pages' runs from two reads", runs, err, reads)
	}
	completed := runs[0]
	if completed.ID != 11 || completed.Name != "test" || completed.Status != "completed" || completed.Conclusion != "timed_out" ||
		completed.CompletedAt == nil || !completed.CompletedAt.Equal(time.Date(2026, 10, 28, 14, 5, 0, 0, time.UTC)) || completed.App.ID != 15368 {
		t.Fatalf("the first run is %+v, want test's run 11 timed out at 14:05 by App 15368", completed)
	}
	running := runs[1]
	if running.ID != 12 || running.Name != "tester" || running.Status != "in_progress" || running.Conclusion != "" || running.CompletedAt != nil || running.App.ID != 271000 {
		t.Fatalf("the second run is %+v, want tester's run 12 in progress by App 271000", running)
	}
	var answer *Answer
	if _, err := github.CheckRuns(context.Background(), "unknown"); !errors.As(err, &answer) || answer.Status != http.StatusUnprocessableEntity {
		t.Fatalf("CheckRuns(unknown) = %v, want GitHub's 422", err)
	}
}
