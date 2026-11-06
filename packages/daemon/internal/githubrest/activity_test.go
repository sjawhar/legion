package githubrest

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

// BranchActivity reads the branch's own ref, refs/heads/<branch>, one page of a hundred, and keeps
// what each entry says as GitHub answers it: the kind, the commits before and after, the time and
// the actor's login, an App's <slug>[bot]. It reads no second page, whatever the Link header names,
// and an answer other than 2xx is its Answer.
func TestBranchActivityReadsOnePageOfTheBranchsRef(t *testing.T) {
	var reads int
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		reads++
		if r.URL.Path != "/repos/acme/widgets/activity" {
			http.Error(w, `{"message":"Not Found"}`, http.StatusNotFound)
			return
		}
		if ref := r.URL.Query().Get("ref"); ref != "refs/heads/legion/A-1" {
			http.Error(w, `{"message":"No activity"}`, http.StatusUnprocessableEntity)
			return
		}
		if got := r.URL.Query().Get("per_page"); got != "100" {
			t.Errorf("read the activity with per_page=%q, want 100", got)
		}
		w.Header().Set("Link", `<`+"http://"+r.Host+`/repos/acme/widgets/activity?ref=refs%2Fheads%2Flegion%2FA-1&per_page=100&after=x>; rel="next"`)
		w.Write([]byte(`[{"id":2,"node_id":"RA_2","before":"aaa","after":"bbb","ref":"refs/heads/legion/A-1","timestamp":"2026-10-28T14:20:00Z","activity_type":"force_push",` +
			`"actor":{"login":"legion-implementer[bot]","id":7,"type":"Bot"}},` +
			`{"id":1,"node_id":"RA_1","before":"0000000000000000000000000000000000000000","after":"aaa","ref":"refs/heads/legion/A-1","timestamp":"2026-10-28T14:10:00Z","activity_type":"branch_creation",` +
			`"actor":{"login":"octocat","id":1,"type":"User"}}]`))
	}))
	t.Cleanup(server.Close)
	github := Client{Token: "token", API: server.URL + "/repos/acme/widgets"}

	got, err := github.BranchActivity(context.Background(), "legion/A-1")
	if err != nil || len(got) != 2 || reads != 1 {
		t.Fatalf("BranchActivity = %+v, %v after %d reads; want two entries from one read", got, err, reads)
	}
	newest := got[0]
	if newest.Type != "force_push" || newest.Before != "aaa" || newest.After != "bbb" || newest.Ref != "refs/heads/legion/A-1" ||
		!newest.Timestamp.Equal(time.Date(2026, 10, 28, 14, 20, 0, 0, time.UTC)) || newest.Actor.Login != "legion-implementer[bot]" || newest.Actor.Type != "Bot" {
		t.Fatalf("the newest entry is %+v, want the App's force push aaa..bbb", newest)
	}
	if got[1].Type != "branch_creation" || got[1].Actor.Login != "octocat" {
		t.Fatalf("the older entry is %+v, want octocat's branch creation", got[1])
	}
	var answer *Answer
	if _, err := github.BranchActivity(context.Background(), "legion/A-2"); !errors.As(err, &answer) || answer.Status != http.StatusUnprocessableEntity {
		t.Fatalf("BranchActivity(legion/A-2) = %v, want GitHub's 422", err)
	}
}
