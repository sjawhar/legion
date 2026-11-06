package githubapp

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"reflect"
	"strings"
	"testing"
)

// HasCommit reads `GET /repos/{owner}/{repo}/commits/{sha}` for the smallest answer GitHub
// sends and proves the repository holds no such commit from one answer alone: the 422 whose
// message begins "No commit found for SHA". GitHub answers the same status for a validation
// failure and for an endpoint it has throttled, so every other 422 is the error that says
// Dispatch cannot tell, as a refusal, a rate limit, a repository the App cannot see and a
// failure of GitHub's own are.
func TestHasCommitProvesAbsenceFromGitHubsNoCommitMessageAlone(t *testing.T) {
	for _, tc := range []struct {
		name   string
		status int
		header map[string]string
		body   string
		// held is the answer HasCommit must give; errorText, unreadable and limited say which
		// error must come with it, and an empty errorText with neither flag wants no error.
		held       bool
		unreadable bool
		limited    bool
		errorText  string
	}{
		{
			name:   "the repository holds the commit",
			status: http.StatusOK,
			body:   mainCommit + "\n",
			held:   true,
		},
		{
			name:   "GitHub found no commit for the SHA",
			status: http.StatusUnprocessableEntity,
			body:   fmt.Sprintf(`{"message":"No commit found for SHA: %s","documentation_url":"https://docs.github.com/rest/commits/commits#get-a-commit"}`, mainCommit),
		},
		{
			name:      "another 422",
			status:    http.StatusUnprocessableEntity,
			body:      `{"message":"Validation failed, or the endpoint has been spammed."}`,
			errorText: "status 422",
		},
		{
			name:      "a 422 whose body is not JSON",
			status:    http.StatusUnprocessableEntity,
			body:      "No commit found for SHA: " + mainCommit,
			errorText: "status 422",
		},
		{
			name:       "the token is refused the resource",
			status:     http.StatusForbidden,
			body:       `{"message":"Resource not accessible by integration"}`,
			unreadable: true,
		},
		{
			name:    "the installation's rate limit is exhausted",
			status:  http.StatusForbidden,
			header:  map[string]string{"X-RateLimit-Remaining": "0", "Retry-After": "60"},
			body:    `{"message":"API rate limit exceeded for installation ID 77."}`,
			limited: true,
		},
		{
			name:       "the App cannot see the repository",
			status:     http.StatusNotFound,
			body:       `{"message":"Not Found"}`,
			unreadable: true,
		},
		{
			name:      "GitHub failed",
			status:    http.StatusInternalServerError,
			body:      `{"message":"Server Error"}`,
			errorText: "status 500",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var asked []string
			client := branchServer(t, func(w http.ResponseWriter, r *http.Request) {
				asked = append(asked, r.Method+" "+r.URL.EscapedPath()+" "+r.Header.Get("Authorization")+" "+r.Header.Get("Accept"))
				for name, value := range tc.header {
					w.Header().Set(name, value)
				}
				w.WriteHeader(tc.status)
				fmt.Fprint(w, tc.body)
			})

			held, err := client.HasCommit(context.Background(), "ghs_installation", "legion", "arch", mainCommit)
			if held != tc.held {
				t.Fatalf("HasCommit on a %d = %v, %v; want %v", tc.status, held, err, tc.held)
			}
			limited, isLimit := AsRateLimit(err)
			switch {
			case tc.limited:
				if !isLimit || limited.Wait <= 0 {
					t.Fatalf("HasCommit on a rate-limited %d: err=%v, want the rate limit with a wait", tc.status, err)
				}
			case tc.unreadable:
				if !errors.Is(err, ErrRepositoryUnreadable) || !strings.Contains(err.Error(), "legion/arch") {
					t.Fatalf("HasCommit on a %d: err=%v, want ErrRepositoryUnreadable naming legion/arch", tc.status, err)
				}
			case tc.errorText != "":
				if err == nil || errors.Is(err, ErrRepositoryUnreadable) || isLimit {
					t.Fatalf("HasCommit on a %d: err=%v, want a failed read, neither unreadable nor a rate limit", tc.status, err)
				}
				if !strings.Contains(err.Error(), tc.errorText) || !strings.Contains(err.Error(), "/repos/legion/arch/commits/"+mainCommit) {
					t.Fatalf("HasCommit on a %d: err=%v, want %q and the target named", tc.status, err, tc.errorText)
				}
			default:
				if err != nil {
					t.Fatalf("HasCommit on a %d: err=%v, want none", tc.status, err)
				}
			}
			want := []string{"GET /repos/legion/arch/commits/" + mainCommit + " Bearer ghs_installation application/vnd.github.sha"}
			if !reflect.DeepEqual(asked, want) {
				t.Fatalf("GitHub saw %q, want %q", asked, want)
			}
		})
	}

	// A deployment holding no App key has no client, and the read says so rather than that the
	// repository does not hold the commit.
	var none *Client
	if held, err := none.HasCommit(context.Background(), "ghs_installation", "legion", "arch", mainCommit); held || !errors.Is(err, ErrNoAppKey) {
		t.Fatalf("HasCommit without a client = %v, %v; want false and ErrNoAppKey", held, err)
	}
}
