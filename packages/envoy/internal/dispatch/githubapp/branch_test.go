package githubapp

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strconv"
	"strings"
	"testing"

	"github.com/sjawhar/envoy/internal/dispatch/auth"
)

// branchServer serves handler as the GitHub API and answers a client pointed at it.
func branchServer(t *testing.T, handler http.HandlerFunc) *Client {
	t.Helper()
	server := httptest.NewServer(handler)
	t.Cleanup(server.Close)
	_, pemText := testKeyPEM(t)
	client, err := New(&auth.AppConfig{ClientID: "Iv1.testclient", PEM: pemText}, server.URL)
	if err != nil {
		t.Fatalf("new client: %v", err)
	}
	return client
}

// Branch reads the exact ref of the branch under the installation token, its name path-escaped
// so a `/` in it reaches GitHub as %2F (GitHub answers `infra-391%2Farchitecture` with
// refs/heads/infra-391/architecture), and answers the head commit.
func TestBranchReadsTheExactRefOfTheBranch(t *testing.T) {
	var seen []string
	client := branchServer(t, func(w http.ResponseWriter, r *http.Request) {
		seen = append(seen, r.Method+" "+r.URL.EscapedPath()+" "+r.Header.Get("Authorization")+" "+r.Header.Get("Accept"))
		fmt.Fprintf(w, `{"ref":"refs/heads/infra-391/architecture","node_id":"REF_x","object":{"sha":%q,"type":"commit"}}`, mainCommit)
	})

	sha, err := client.Branch(context.Background(), "ghs_installation", "legion", "arch", "infra-391/architecture")
	if err != nil || sha != mainCommit {
		t.Fatalf("Branch = %q, %v; want %q", sha, err, mainCommit)
	}
	want := []string{"GET /repos/legion/arch/git/ref/heads/infra-391%2Farchitecture Bearer ghs_installation application/vnd.github+json"}
	if !reflect.DeepEqual(seen, want) {
		t.Fatalf("GitHub saw %q, want %q", seen, want)
	}
}

// GitHub's exact read answers 404 for a name that is no branch's (a SHA, `main~1`, `HEAD`,
// `refs/heads/main`, a prefix of a branch's name), 409 for an empty repository, and 422 for a
// name it refuses; each is ErrNoBranch naming the branch and the repository, and no SHA.
func TestBranchMissingIsErrNoBranch(t *testing.T) {
	for _, status := range []int{http.StatusNotFound, http.StatusConflict, http.StatusUnprocessableEntity} {
		t.Run(strconv.Itoa(status), func(t *testing.T) {
			client := branchServer(t, func(w http.ResponseWriter, _ *http.Request) {
				w.WriteHeader(status)
				fmt.Fprint(w, `{"message":"Not Found"}`)
			})
			sha, err := client.Branch(context.Background(), "ghs_installation", "legion", "arch", "main~1")
			if !errors.Is(err, ErrNoBranch) || !strings.Contains(err.Error(), "main~1 on legion/arch") {
				t.Fatalf("Branch answered %d: err=%v, want ErrNoBranch naming main~1 on legion/arch", status, err)
			}
			if sha != "" {
				t.Fatalf("Branch answered %d: sha=%q, want none", status, sha)
			}
		})
	}
}

// A 200 that names another ref than the branch's, or carries no 40-hex SHA, proves no branch:
// GitHub's exact read never answers one, and the check does not trust that it cannot.
func TestBranchRefusesAnAnswerForAnotherRef(t *testing.T) {
	for _, tc := range []struct{ name, branch, body string }{
		{"another branch", "infra-391", `{"ref":"refs/heads/infra-391/architecture","object":{"sha":"` + mainCommit + `","type":"commit"}}`},
		{"a tag", "main", `{"ref":"refs/tags/main","object":{"sha":"` + mainCommit + `","type":"tag"}}`},
		{"no sha", "main", `{"ref":"refs/heads/main","object":{"sha":"c0ffee","type":"commit"}}`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			branch := tc.branch
			client := branchServer(t, func(w http.ResponseWriter, _ *http.Request) {
				fmt.Fprint(w, tc.body)
			})
			sha, err := client.Branch(context.Background(), "ghs_installation", "legion", "arch", branch)
			if !errors.Is(err, ErrNoBranch) || !strings.Contains(err.Error(), branch+" on legion/arch") {
				t.Fatalf("Branch = %q, %v; want ErrNoBranch naming %s on legion/arch", sha, err, branch)
			}
			if sha != "" {
				t.Fatalf("Branch = %q, want no SHA", sha)
			}
		})
	}
}

// Any other status is a failed read naming the status and the target, never ErrNoBranch; with no
// App key there is no client, and Branch answers ErrNoAppKey.
func TestBranchOtherFailures(t *testing.T) {
	client := branchServer(t, func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusBadGateway)
	})
	_, err := client.Branch(context.Background(), "ghs_installation", "legion", "arch", "main")
	if err == nil || errors.Is(err, ErrNoBranch) || !strings.Contains(err.Error(), "/repos/legion/arch/git/ref/heads/main: status 502") {
		t.Fatalf("Branch on a 502: err=%v, want a failed read naming the target and status", err)
	}

	var none *Client
	if _, err := none.Branch(context.Background(), "ghs_installation", "legion", "arch", "main"); !errors.Is(err, ErrNoAppKey) {
		t.Fatalf("Branch without a client: err=%v, want ErrNoAppKey", err)
	}
}
