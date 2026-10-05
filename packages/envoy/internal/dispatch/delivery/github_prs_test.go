package delivery

import (
	"context"
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"encoding/json"
	"encoding/pem"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/sjawhar/envoy/internal/dispatch/auth"
	"github.com/sjawhar/envoy/internal/dispatch/githubapp"
)

// fakeGitHub is a minimal httptest stand-in for the GitHub App and REST APIs this package's
// fetchers call: installation resolution, token minting, and whatever read handlers a test
// installs on mux. Shared by github_prs_test.go and github_runs_test.go.
type fakeGitHub struct {
	t          *testing.T
	mux        *http.ServeMux
	installID  int64
	tokenMints int
}

func newFakeGitHub(t *testing.T) *fakeGitHub {
	t.Helper()
	f := &fakeGitHub{t: t, mux: http.NewServeMux(), installID: 1}
	f.mux.HandleFunc("GET /repos/{owner}/{repo}/installation", func(w http.ResponseWriter, r *http.Request) {
		if err := json.NewEncoder(w).Encode(map[string]any{
			"id":          f.installID,
			"app_slug":    "delivery-test",
			"permissions": map[string]string{"contents": "read"},
		}); err != nil {
			f.t.Errorf("encode installation: %v", err)
		}
	})
	f.mux.HandleFunc("POST /app/installations/{id}/access_tokens", func(w http.ResponseWriter, r *http.Request) {
		f.tokenMints++
		w.WriteHeader(http.StatusCreated)
		if err := json.NewEncoder(w).Encode(map[string]any{
			"token":      fmt.Sprintf("ghs_fake_%d", f.tokenMints),
			"expires_at": time.Now().Add(time.Hour).Format(time.RFC3339),
		}); err != nil {
			f.t.Errorf("encode token: %v", err)
		}
	})
	return f
}

func (f *fakeGitHub) handle(pattern string, handler http.HandlerFunc) {
	f.mux.HandleFunc(pattern, handler)
}

func (f *fakeGitHub) newTestClient() *githubapp.Client {
	t := f.t
	t.Helper()
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatalf("generate fixture key: %v", err)
	}
	pemText := string(pem.EncodeToMemory(&pem.Block{Type: "RSA PRIVATE KEY", Bytes: x509.MarshalPKCS1PrivateKey(key)}))
	server := httptest.NewServer(f.mux)
	t.Cleanup(server.Close)
	client, err := githubapp.New(&auth.AppConfig{ClientID: "Iv1.testclient", PEM: pemText}, server.URL)
	if err != nil {
		t.Fatalf("new client: %v", err)
	}
	if client == nil {
		t.Fatal("new client: got nil for a configured app")
	}
	return client
}

func mustEncode(t *testing.T, w http.ResponseWriter, v any) {
	t.Helper()
	if err := json.NewEncoder(w).Encode(v); err != nil {
		t.Errorf("encode response: %v", err)
	}
}

func ptrTime(s string) *time.Time {
	v, err := time.Parse(time.RFC3339, s)
	if err != nil {
		panic(err)
	}
	return &v
}

func TestFetchPullRequestMapsEveryField(t *testing.T) {
	fake := newFakeGitHub(t)
	fake.handle("GET /repos/acme/widgets/pulls/42", func(w http.ResponseWriter, r *http.Request) {
		mustEncode(t, w, map[string]any{
			"number":           42,
			"title":            "Add retries",
			"html_url":         "https://github.com/acme/widgets/pull/42",
			"user":             map[string]any{"login": "octocat"},
			"labels":           []map[string]any{{"name": "bug"}, {"name": "needs-review"}},
			"body":             "Fixes #7",
			"created_at":       "2024-01-01T00:00:00Z",
			"merged_at":        "2024-01-02T00:00:00Z",
			"merge_commit_sha": "deadbeef",
			"additions":        12,
			"deletions":        3,
		})
	})
	fake.handle("GET /repos/acme/widgets/pulls/42/commits", func(w http.ResponseWriter, r *http.Request) {
		if got := r.URL.Query().Get("per_page"); got != "1" {
			t.Errorf("commits request per_page = %q, want 1", got)
		}
		mustEncode(t, w, []map[string]any{
			{
				"sha": "c0ffee",
				"commit": map[string]any{
					"author":    map[string]any{"date": "2023-12-30T10:00:00Z"},
					"committer": map[string]any{"date": "2023-12-30T11:00:00Z"},
				},
			},
			{
				"sha": "later",
				"commit": map[string]any{
					"author": map[string]any{"date": "2023-12-31T10:00:00Z"},
				},
			},
		})
	})
	client := fake.newTestClient()

	pr, err := FetchPullRequest(context.Background(), client, "acme", "widgets", 42)
	if err != nil {
		t.Fatalf("FetchPullRequest: %v", err)
	}
	want := FetchedPullRequest{
		Number:         42,
		Title:          "Add retries",
		URL:            "https://github.com/acme/widgets/pull/42",
		Author:         "octocat",
		Labels:         []string{"bug", "needs-review"},
		CreatedAt:      *ptrTime("2024-01-01T00:00:00Z"),
		MergedAt:       ptrTime("2024-01-02T00:00:00Z"),
		MergeCommitSHA: new("deadbeef"),
		Additions:      new(12),
		Deletions:      new(3),
		Body:           "Fixes #7",
		FirstCommitAt:  ptrTime("2023-12-30T10:00:00Z"),
	}
	assertPullRequestsEqual(t, pr, want)
}

func TestFetchPullRequestUsesCommitterDateWhenAuthorIsAbsent(t *testing.T) {
	fake := newFakeGitHub(t)
	fake.handle("GET /repos/acme/widgets/pulls/7", func(w http.ResponseWriter, r *http.Request) {
		mustEncode(t, w, map[string]any{
			"number": 7, "title": "x", "html_url": "u", "user": map[string]any{"login": "a"},
			"created_at": "2024-01-01T00:00:00Z", "merged_at": "2024-01-02T00:00:00Z",
		})
	})
	fake.handle("GET /repos/acme/widgets/pulls/7/commits", func(w http.ResponseWriter, r *http.Request) {
		mustEncode(t, w, []map[string]any{
			{"sha": "nouser", "commit": map[string]any{"committer": map[string]any{"date": "2023-12-29T00:00:00Z"}}},
		})
	})
	client := fake.newTestClient()

	pr, err := FetchPullRequest(context.Background(), client, "acme", "widgets", 7)
	if err != nil {
		t.Fatalf("FetchPullRequest: %v", err)
	}
	if pr.FirstCommitAt == nil || !pr.FirstCommitAt.Equal(*ptrTime("2023-12-29T00:00:00Z")) {
		t.Fatalf("FirstCommitAt = %v, want the committer date", pr.FirstCommitAt)
	}
}

func TestFetchPullRequestNoCommitsIsAnError(t *testing.T) {
	fake := newFakeGitHub(t)
	fake.handle("GET /repos/acme/widgets/pulls/9", func(w http.ResponseWriter, r *http.Request) {
		mustEncode(t, w, map[string]any{"number": 9, "title": "x", "html_url": "u", "user": map[string]any{"login": "a"}})
	})
	fake.handle("GET /repos/acme/widgets/pulls/9/commits", func(w http.ResponseWriter, r *http.Request) {
		mustEncode(t, w, []map[string]any{})
	})
	client := fake.newTestClient()

	_, err := FetchPullRequest(context.Background(), client, "acme", "widgets", 9)
	if err == nil {
		t.Fatal("FetchPullRequest: err = nil, want a malformed-commits error")
	}
}

func TestFetchPullRequestUpstream404IsWrappedNotPanic(t *testing.T) {
	fake := newFakeGitHub(t)
	fake.handle("GET /repos/acme/widgets/pulls/404", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNotFound)
		fmt.Fprint(w, `{"message":"Not Found"}`)
	})
	client := fake.newTestClient()

	_, err := FetchPullRequest(context.Background(), client, "acme", "widgets", 404)
	if err == nil || !strings.Contains(err.Error(), "404") {
		t.Fatalf("FetchPullRequest: err = %v, want an error naming the 404 status", err)
	}
}

func TestFetchPullRequestUpstream500IsWrapped(t *testing.T) {
	fake := newFakeGitHub(t)
	fake.handle("GET /repos/acme/widgets/pulls/500", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
		fmt.Fprint(w, `{"message":"boom"}`)
	})
	client := fake.newTestClient()

	_, err := FetchPullRequest(context.Background(), client, "acme", "widgets", 500)
	if err == nil || !strings.Contains(err.Error(), "500") {
		t.Fatalf("FetchPullRequest: err = %v, want an error naming the 500 status", err)
	}
}

func searchIssueItemJSON(number int, title, author string, createdAt, mergedAt string) map[string]any {
	return map[string]any{
		"number":     number,
		"title":      title,
		"html_url":   fmt.Sprintf("https://github.com/acme/widgets/pull/%d", number),
		"user":       map[string]any{"login": author},
		"created_at": createdAt,
		"body":       "",
		"pull_request": map[string]any{
			"merged_at": mergedAt,
		},
	}
}

func TestSearchMergedPullRequestsBuildsTheQuery(t *testing.T) {
	fake := newFakeGitHub(t)
	var gotQuery string
	fake.handle("GET /search/issues", func(w http.ResponseWriter, r *http.Request) {
		gotQuery = r.URL.Query().Get("q")
		mustEncode(t, w, map[string]any{"total_count": 0, "items": []any{}})
	})
	client := fake.newTestClient()

	since := time.Date(2024, 1, 1, 0, 0, 0, 0, time.UTC)
	until := time.Date(2024, 1, 2, 0, 0, 0, 0, time.UTC)
	if _, err := SearchMergedPullRequests(context.Background(), client, "acme", "widgets", []string{"alice", "bob"}, since, until); err != nil {
		t.Fatalf("SearchMergedPullRequests: %v", err)
	}

	want := "repo:acme/widgets is:pr is:merged merged:2024-01-01T00:00:00Z..2024-01-02T00:00:00Z author:alice author:bob"
	if gotQuery != want {
		t.Fatalf("search query = %q, want %q", gotQuery, want)
	}
}

func TestSearchMergedPullRequestsAcrossInstallationOmitsRepoQualifier(t *testing.T) {
	fake := newFakeGitHub(t)
	var gotQuery string
	fake.handle("GET /search/issues", func(w http.ResponseWriter, r *http.Request) {
		gotQuery = r.URL.Query().Get("q")
		mustEncode(t, w, map[string]any{"total_count": 0, "items": []any{}})
	})
	client := fake.newTestClient()

	since := time.Date(2024, 1, 1, 0, 0, 0, 0, time.UTC)
	until := time.Date(2024, 1, 2, 0, 0, 0, 0, time.UTC)
	// tokenOwner/tokenRepo (acme/widgets) only resolves which installation to mint a token
	// from -- LEGION-294's population spans every repository that installation covers, so the
	// query itself must carry no repo: qualifier.
	if _, err := SearchMergedPullRequestsAcrossInstallation(context.Background(), client, "acme", "widgets", []string{"alice", "bob"}, since, until); err != nil {
		t.Fatalf("SearchMergedPullRequestsAcrossInstallation: %v", err)
	}

	if strings.Contains(gotQuery, "repo:") {
		t.Fatalf("search query = %q, want no repo: qualifier", gotQuery)
	}
	want := "is:pr is:merged merged:2024-01-01T00:00:00Z..2024-01-02T00:00:00Z author:alice author:bob"
	if gotQuery != want {
		t.Fatalf("search query = %q, want %q", gotQuery, want)
	}
}

func TestSearchMergedPullRequestsPaginatesAcrossPages(t *testing.T) {
	fake := newFakeGitHub(t)
	var pagesSeen []string
	fake.handle("GET /search/issues", func(w http.ResponseWriter, r *http.Request) {
		page := r.URL.Query().Get("page")
		pagesSeen = append(pagesSeen, page)
		switch page {
		case "1":
			mustEncode(t, w, map[string]any{
				"total_count": 150,
				"items":       repeatSearchItems(100, 1),
			})
		case "2":
			mustEncode(t, w, map[string]any{
				"total_count": 150,
				"items":       repeatSearchItems(50, 101),
			})
		default:
			t.Fatalf("unexpected page %q", page)
		}
	})
	client := fake.newTestClient()

	results, err := SearchMergedPullRequests(context.Background(), client, "acme", "widgets", []string{"alice"},
		time.Date(2024, 1, 1, 0, 0, 0, 0, time.UTC), time.Date(2024, 1, 2, 0, 0, 0, 0, time.UTC))
	if err != nil {
		t.Fatalf("SearchMergedPullRequests: %v", err)
	}
	if len(results) != 150 {
		t.Fatalf("len(results) = %d, want 150", len(results))
	}
	if len(pagesSeen) != 2 || pagesSeen[0] != "1" || pagesSeen[1] != "2" {
		t.Fatalf("pages fetched = %v, want [1 2]", pagesSeen)
	}
	if results[0].Number != 1 || results[149].Number != 150 {
		t.Fatalf("results not in page order: first=%d last=%d", results[0].Number, results[149].Number)
	}
}

func repeatSearchItems(n, startNumber int) []map[string]any {
	items := make([]map[string]any, n)
	for i := range n {
		number := startNumber + i
		items[i] = searchIssueItemJSON(number, "t", "alice", "2024-01-01T00:00:00Z", "2024-01-01T01:00:00Z")
	}
	return items
}

func TestSearchMergedPullRequestsHalvesOnOverflowWithoutGapOrOverlap(t *testing.T) {
	fake := newFakeGitHub(t)
	type window struct{ since, until string }
	var windows []window
	fake.handle("GET /search/issues", func(w http.ResponseWriter, r *http.Request) {
		q := r.URL.Query().Get("q")
		// q contains "merged:<since>..<until>"; extract it for the window assertion.
		const marker = "merged:"
		idx := strings.Index(q, marker)
		if idx < 0 {
			t.Fatalf("query %q has no merged: qualifier", q)
		}
		rangeText := strings.Fields(q[idx+len(marker):])[0]
		parts := strings.SplitN(rangeText, "..", 2)
		if len(parts) != 2 {
			t.Fatalf("merged range %q is not a..b", rangeText)
		}
		windows = append(windows, window{parts[0], parts[1]})

		if rangeText == "2024-01-01T00:00:00Z..2024-01-02T00:00:00Z" {
			// The original, full window: answer with an overflowing total_count so the caller
			// halves and recurses instead of paginating past 1,000.
			mustEncode(t, w, map[string]any{"total_count": 1001, "items": repeatSearchItems(100, 1)})
			return
		}
		// Either half answers a small, final result.
		mustEncode(t, w, map[string]any{"total_count": 1, "items": repeatSearchItems(1, 1)})
	})
	client := fake.newTestClient()

	since := time.Date(2024, 1, 1, 0, 0, 0, 0, time.UTC)
	until := time.Date(2024, 1, 2, 0, 0, 0, 0, time.UTC)
	results, err := SearchMergedPullRequests(context.Background(), client, "acme", "widgets", []string{"alice"}, since, until)
	if err != nil {
		t.Fatalf("SearchMergedPullRequests: %v", err)
	}
	if len(results) != 2 {
		t.Fatalf("len(results) = %d, want 2 (one per half)", len(results))
	}
	if len(windows) != 3 {
		t.Fatalf("requests made = %d, want 3 (original + two halves)", len(windows))
	}
	mid := since.Add(until.Sub(since) / 2).UTC().Format(time.RFC3339)
	first, second := windows[1], windows[2]
	if first.since != since.UTC().Format(time.RFC3339) || first.until != mid {
		t.Fatalf("first half = %+v, want [%s, %s)", first, since.UTC().Format(time.RFC3339), mid)
	}
	if second.since != mid || second.until != until.UTC().Format(time.RFC3339) {
		t.Fatalf("second half = %+v, want [%s, %s)", second, mid, until.UTC().Format(time.RFC3339))
	}
	if first.until != second.since {
		t.Fatalf("halves do not meet at the midpoint: first ends %s, second starts %s", first.until, second.since)
	}
}

func assertPullRequestsEqual(t *testing.T, got, want FetchedPullRequest) {
	t.Helper()
	if got.Number != want.Number || got.Title != want.Title || got.URL != want.URL || got.Author != want.Author ||
		got.Body != want.Body || !got.CreatedAt.Equal(want.CreatedAt) {
		t.Fatalf("got = %+v, want = %+v", got, want)
	}
	if (got.MergedAt == nil) != (want.MergedAt == nil) || (got.MergedAt != nil && !got.MergedAt.Equal(*want.MergedAt)) {
		t.Fatalf("MergedAt = %v, want %v", got.MergedAt, want.MergedAt)
	}
	if (got.MergeCommitSHA == nil) != (want.MergeCommitSHA == nil) || (got.MergeCommitSHA != nil && *got.MergeCommitSHA != *want.MergeCommitSHA) {
		t.Fatalf("MergeCommitSHA = %v, want %v", got.MergeCommitSHA, want.MergeCommitSHA)
	}
	if (got.Additions == nil) != (want.Additions == nil) || (got.Additions != nil && *got.Additions != *want.Additions) {
		t.Fatalf("Additions = %v, want %v", got.Additions, want.Additions)
	}
	if (got.Deletions == nil) != (want.Deletions == nil) || (got.Deletions != nil && *got.Deletions != *want.Deletions) {
		t.Fatalf("Deletions = %v, want %v", got.Deletions, want.Deletions)
	}
	if (got.FirstCommitAt == nil) != (want.FirstCommitAt == nil) || (got.FirstCommitAt != nil && !got.FirstCommitAt.Equal(*want.FirstCommitAt)) {
		t.Fatalf("FirstCommitAt = %v, want %v", got.FirstCommitAt, want.FirstCommitAt)
	}
	if len(got.Labels) != len(want.Labels) {
		t.Fatalf("Labels = %v, want %v", got.Labels, want.Labels)
	}
	for i := range got.Labels {
		if got.Labels[i] != want.Labels[i] {
			t.Fatalf("Labels = %v, want %v", got.Labels, want.Labels)
		}
	}
}

func TestSearchMergedPullRequestsLeavesCompletingFieldsNil(t *testing.T) {
	fake := newFakeGitHub(t)
	fake.handle("GET /search/issues", func(w http.ResponseWriter, r *http.Request) {
		mustEncode(t, w, map[string]any{
			"total_count": 1,
			"items":       []map[string]any{searchIssueItemJSON(5, "t", "alice", "2024-01-01T00:00:00Z", "2024-01-01T01:00:00Z")},
		})
	})
	client := fake.newTestClient()

	results, err := SearchMergedPullRequests(context.Background(), client, "acme", "widgets", []string{"alice"},
		time.Date(2024, 1, 1, 0, 0, 0, 0, time.UTC), time.Date(2024, 1, 2, 0, 0, 0, 0, time.UTC))
	if err != nil {
		t.Fatalf("SearchMergedPullRequests: %v", err)
	}
	if len(results) != 1 {
		t.Fatalf("len(results) = %d, want 1", len(results))
	}
	pr := results[0]
	if pr.FirstCommitAt != nil || pr.MergeCommitSHA != nil || pr.Additions != nil || pr.Deletions != nil {
		t.Fatalf("search result carries a completing-fetch-only field: %+v", pr)
	}
	if pr.MergedAt == nil {
		t.Fatal("search result MergedAt is nil, want the pull_request.merged_at value")
	}
}
