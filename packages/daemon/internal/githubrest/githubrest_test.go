package githubrest

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"slices"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

// Post sends its body as JSON with the installation token, reads a 2xx answer such as GitHub's
// 201 Created, and gives any other answer back as an *Answer naming the method, the path and the
// status, so a caller can tell GitHub's refusal from a failed request.
func TestPostSendsJSONAndAnswersWithTheStatus(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var sent struct {
			Ref string `json:"ref"`
		}
		if r.Method != http.MethodPost || r.Header.Get("Authorization") != "Bearer token" || r.Header.Get("Content-Type") != "application/json" ||
			json.NewDecoder(r.Body).Decode(&sent) != nil {
			http.Error(w, `{"message":"bad request"}`, http.StatusBadRequest)
			return
		}
		if sent.Ref == "refs/heads/taken" {
			http.Error(w, `{"message":"Reference already exists"}`, http.StatusUnprocessableEntity)
			return
		}
		w.WriteHeader(http.StatusCreated)
		w.Write([]byte(`{"ref":"` + sent.Ref + `"}`))
	}))
	defer server.Close()
	client := Client{Token: "token", API: server.URL + "/repos/acme/widgets"}

	var created struct {
		Ref string `json:"ref"`
	}
	if err := client.Post(context.Background(), "/git/refs", map[string]string{"ref": "refs/heads/new"}, &created); err != nil || created.Ref != "refs/heads/new" {
		t.Fatalf("Post = %+v, %v; want the 201 answer read", created, err)
	}
	err := client.Post(context.Background(), "/git/refs?x=1", map[string]string{"ref": "refs/heads/taken"}, nil)
	var answer *Answer
	if !errors.As(err, &answer) || answer.Method != http.MethodPost || answer.Path != "/git/refs" || answer.Status != http.StatusUnprocessableEntity {
		t.Fatalf("Post of a taken ref = %v, want an *Answer for POST /git/refs with 422", err)
	}
}

// GetPages and GetListPages follow GitHub's next pages wherever GitHub names them on the API's own
// origin, its /repositories/<id>/ form included, and an answer on such a page names that page's
// path. A next page on another host is refused before the token is sent there.
func TestGetPagesFollowsNextPagesOnTheAPIsOriginOnly(t *testing.T) {
	var elsewhere atomic.Int32
	other := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		elsewhere.Add(1)
		if strings.HasSuffix(r.URL.Path, "/check-runs") {
			w.Write([]byte(`{"total_count":1,"check_runs":["leaked"]}`))
			return
		}
		w.Write([]byte(`["leaked"]`))
	}))
	defer other.Close()
	var nextHost atomic.Pointer[string]
	var pageTwo atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/repos/acme/widgets/rules/branches/main":
			next := *nextHost.Load() + "/repositories/42/rules/branches/main?per_page=100&page=2"
			w.Header().Set("Link", "<"+next+`>; rel="next", <`+next+`>; rel="last"`)
			w.Write([]byte(`["first"]`))
		case "/repositories/42/rules/branches/main":
			if status := int(pageTwo.Load()); status != http.StatusOK {
				http.Error(w, `{"message":"Resource not accessible by integration"}`, status)
				return
			}
			w.Write([]byte(`["second"]`))
		case "/repos/acme/widgets/commits/head/check-runs":
			next := *nextHost.Load() + "/repositories/42/commits/head/check-runs?per_page=100&page=2"
			w.Header().Set("Link", "<"+next+`>; rel="next"`)
			w.Write([]byte(`{"total_count":2,"check_runs":["first"]}`))
		case "/repositories/42/commits/head/check-runs":
			w.Write([]byte(`{"total_count":2,"check_runs":["second"]}`))
		default:
			http.Error(w, `{"message":"Not Found"}`, http.StatusNotFound)
		}
	}))
	defer server.Close()
	client := Client{Token: "token", API: server.URL + "/repos/acme/widgets"}

	nextHost.Store(&server.URL)
	pageTwo.Store(http.StatusOK)
	if got, err := GetPages[string](context.Background(), client, "/rules/branches/main"); err != nil || !slices.Equal(got, []string{"first", "second"}) {
		t.Fatalf("GetPages = %v, %v; want both pages", got, err)
	}
	if got, err := GetListPages[string](context.Background(), client, "/commits/head/check-runs", "check_runs"); err != nil || !slices.Equal(got, []string{"first", "second"}) {
		t.Fatalf("GetListPages = %v, %v; want both pages' check runs", got, err)
	}

	pageTwo.Store(http.StatusForbidden)
	_, err := GetPages[string](context.Background(), client, "/rules/branches/main")
	var answer *Answer
	if !errors.As(err, &answer) || answer.Path != "/repositories/42/rules/branches/main" || answer.Status != http.StatusForbidden {
		t.Fatalf("GetPages with page 2 refused = %v, want an *Answer naming /repositories/42/rules/branches/main with 403", err)
	}

	nextHost.Store(&other.URL)
	got, err := GetPages[string](context.Background(), client, "/rules/branches/main")
	if err == nil || !strings.Contains(err.Error(), other.URL) || got != nil {
		t.Fatalf("GetPages with a next page on another host = %v, %v; want an error naming it and no items", got, err)
	}
	list, err := GetListPages[string](context.Background(), client, "/commits/head/check-runs", "check_runs")
	if err == nil || !strings.Contains(err.Error(), other.URL) || list != nil {
		t.Fatalf("GetListPages with a next page on another host = %v, %v; want an error naming it and no items", list, err)
	}
	if n := elsewhere.Load(); n != 0 {
		t.Fatalf("the other host saw %d requests, want none", n)
	}
}

// A list GitHub answers as an object's field is read from that field, and an answer without it is
// an error naming the field, never an empty list a caller would read as nothing required or run.
func TestGetListPagesRefusesAnAnswerWithoutTheField(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/repos/acme/widgets/actions/runs" {
			w.Write([]byte(`{"total_count":1,"workflow_runs":[{"id":37}]}`))
			return
		}
		w.Write([]byte(`{"total_count":0,"message":"no list here"}`))
	}))
	defer server.Close()
	client := Client{Token: "token", API: server.URL + "/repos/acme/widgets"}
	type run struct {
		ID int `json:"id"`
	}
	if runs, err := GetListPages[run](context.Background(), client, "/actions/runs", "workflow_runs"); err != nil || len(runs) != 1 || runs[0].ID != 37 {
		t.Fatalf("GetListPages = %+v, %v; want the one run read from workflow_runs", runs, err)
	}
	runs, err := GetListPages[run](context.Background(), client, "/commits/head/check-runs", "check_runs")
	if err == nil || runs != nil || err.Error() != `GitHub answered GET /commits/head/check-runs with no "check_runs" list` {
		t.Fatalf("GetListPages of an answer without the field = %+v, %v; want an error naming check_runs", runs, err)
	}
}

// GitHub's secondary rate limit can answer a 403 with only its message to say so: no retry-after,
// and x-ratelimit-remaining well above 0. That answer is a rate limit, waited the minute GitHub's
// documentation names for a limit that names no wait. A 403 whose message is anything else is not.
func TestASecondaryRateLimitNamedOnlyByItsMessageIsARateLimit(t *testing.T) {
	for _, tc := range []struct {
		name      string
		body      string
		limited   bool
		wantAfter time.Duration
	}{
		{name: "a secondary rate limit", body: `{"message":"You have exceeded a secondary rate limit. Please wait a few minutes before you try again."}`,
			limited: true, wantAfter: time.Minute},
		{name: "an installation refused", body: `{"message":"Resource not accessible by integration"}`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				w.Header().Set("X-RateLimit-Remaining", "4321")
				http.Error(w, tc.body, http.StatusForbidden)
			}))
			defer server.Close()
			client := Client{Token: "token", API: server.URL + "/repos/acme/widgets"}

			var answer *Answer
			if err := client.Get(context.Background(), "/collaborators/a-writer/permission", nil); !errors.As(err, &answer) {
				t.Fatalf("Get = %v, want an *Answer", err)
			}
			if answer.RateLimited != tc.limited || answer.RetryAfter != tc.wantAfter {
				t.Fatalf("the 403 = rate limited %v, retry after %s; want %v, %s", answer.RateLimited, answer.RetryAfter, tc.limited, tc.wantAfter)
			}
		})
	}
}

// A 403 naming x-ratelimit-reset far in the future - a host clock behind GitHub's, or a limit
// GitHub's documentation allows up to an hour out - waits no longer than an hour, GitHub's primary
// rate limit's window, rather than stretching the nak delay and the held limit out past it.
func TestARateLimitsResetFarInTheFutureWaitsNoLongerThanAnHour(t *testing.T) {
	reset := strconv.FormatInt(time.Now().Add(6*time.Hour).Unix(), 10)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("X-RateLimit-Remaining", "0")
		w.Header().Set("X-RateLimit-Reset", reset)
		http.Error(w, `{"message":"rate limited"}`, http.StatusForbidden)
	}))
	defer server.Close()
	client := Client{Token: "token", API: server.URL + "/repos/acme/widgets"}

	var answer *Answer
	if err := client.Get(context.Background(), "/collaborators/a-writer/permission", nil); !errors.As(err, &answer) {
		t.Fatalf("Get = %v, want an *Answer", err)
	}
	if !answer.RateLimited || answer.RetryAfter != time.Hour {
		t.Fatalf("a reset six hours out = rate limited %v, retry after %s; want true and an hour's cap", answer.RateLimited, answer.RetryAfter)
	}
}
