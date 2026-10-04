package daemon

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/sjawhar/legion/daemon/internal/ghrepo"
)

// A review decides a round only from an account with write access or higher, which the daemon
// reads from GitHub's collaborator permission before the fact is applied. GitHub's legacy
// `permission` answers `admin` for an admin, `write` for a maintainer as well as a writer, and
// `read` or `none` below that. An account GitHub does not know on the repository (404), or one
// this installation may not read (403), has no write access and decides nothing. Any other failure
// is returned, so intake retries the review rather than applying it with a permission nobody read.
// The review App's own login is answered without a GitHub call: its reviews decide by login.
func TestTheReviewersRepositoryPermissionDecidesWhoMayEndARound(t *testing.T) {
	repository := ghrepo.MustParse("acme/widgets")
	for _, tc := range []struct {
		name       string
		login      string
		permission string
		status     int
		want       bool
		wantErr    string
		wantCalls  int
	}{
		{name: "an admin", login: "an-admin", permission: "admin", want: true, wantCalls: 1},
		{name: "a writer", login: "a-writer", permission: "write", want: true, wantCalls: 1},
		{name: "a maintainer, which GitHub answers as write", login: "a-maintainer", permission: "write", want: true, wantCalls: 1},
		{name: "a reader", login: "a-reader", permission: "read", wantCalls: 1},
		{name: "a triage collaborator, which GitHub answers as read", login: "a-triager", permission: "read", wantCalls: 1},
		{name: "an account with no permission", login: "a-stranger", permission: "none", wantCalls: 1},
		{name: "an account GitHub does not know", login: "a-ghost", status: http.StatusNotFound, wantCalls: 1},
		{name: "a permission this installation may not read", login: "a-member", status: http.StatusForbidden, wantCalls: 1},
		{name: "GitHub failing", login: "a-writer", status: http.StatusBadGateway, wantErr: "502", wantCalls: 1},
		{name: "the review App itself", login: "legion-reviewer[bot]", wantCalls: 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			calls := 0
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				calls++
				if got, want := r.URL.Path, "/repos/acme/widgets/collaborators/"+tc.login+"/permission"; got != want {
					t.Errorf("GitHub was asked %s, want %s", got, want)
				}
				if tc.status != 0 {
					http.Error(w, "refused", tc.status)
					return
				}
				fmt.Fprintf(w, `{"permission":%q}`, tc.permission)
			}))
			defer server.Close()
			w := &workflowRuntime{tokens: outboxTokens{}, githubAPI: server.URL, log: quietLogger(), reviewAppLogin: "legion-reviewer[bot]"}

			got, err := w.reviewerCanWrite(context.Background(), repository, tc.login)
			switch {
			case tc.wantErr != "" && (err == nil || !strings.Contains(err.Error(), tc.wantErr)):
				t.Fatalf("%s's permission = %v, %v; want an error naming %s", tc.login, got, err, tc.wantErr)
			case tc.wantErr == "" && err != nil:
				t.Fatalf("%s's permission = %v, %v; want no error", tc.login, got, err)
			}
			if got != tc.want {
				t.Errorf("%s can write = %v, want %v", tc.login, got, tc.want)
			}
			if calls != tc.wantCalls {
				t.Errorf("GitHub was asked %d times, want %d", calls, tc.wantCalls)
			}
		})
	}
}
