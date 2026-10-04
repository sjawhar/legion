package daemon

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/sjawhar/legion/daemon/internal/intake"
	"github.com/sjawhar/legion/daemon/internal/phase"
	"github.com/sjawhar/legion/daemon/internal/record"
)

// A review decides a round only from an account with write access or higher, which the daemon
// reads from GitHub's collaborator permission before the fact is applied. GitHub's legacy
// `permission` answers `admin` for an admin, `write` for a maintainer as well as a writer, and
// `read` (a triage collaborator's too) or `none` below that; an answer naming another account than
// the review's author, matched without case, is no write access either. An account GitHub does not
// know on the repository (404) has none, and nor does one the installation may not read (a 403 that
// is not a rate limit), which is logged at error naming the installation, since it holds for every
// author. A rate limit is returned with the wait GitHub names, so intake waits it out before the
// review is read again; any other failure is returned too, so intake retries the review rather than
// applying it with a permission nobody read. The review App's own login, and a review on a pull
// request the daemon does not record, are answered without a GitHub call.
func TestTheReviewersRepositoryPermissionDecidesWhoMayEndARound(t *testing.T) {
	pool := reviewPermissionPool(t)
	reset := strconv.FormatInt(time.Now().Add(90*time.Second).Unix(), 10)
	for _, tc := range []struct {
		name       string
		login      string
		number     int
		permission string
		// answered is the login GitHub's answer names; empty is the review's author.
		answered  string
		status    int
		header    map[string]string
		want      bool
		wantErr   string
		wantWait  time.Duration
		wantLog   string
		wantCalls int
	}{
		{name: "an admin", login: "an-admin", permission: "admin", want: true, wantCalls: 1},
		{name: "a writer", login: "a-writer", permission: "write", want: true, wantCalls: 1},
		{name: "a writer GitHub names in another case", login: "a-writer", answered: "A-Writer", permission: "write", want: true, wantCalls: 1},
		{name: "a reader", login: "a-reader", permission: "read", wantCalls: 1},
		{name: "an account with no permission", login: "a-stranger", permission: "none", wantCalls: 1},
		{name: "an answer for another account", login: "copilot-pull-request-reviewer[bot]", answered: "Copilot", permission: "write",
			wantLog: "level=WARN msg=\"workflow: GitHub answered the repository permission of another account", wantCalls: 1},
		{name: "an account GitHub does not know", login: "a-ghost", status: http.StatusNotFound, wantCalls: 1},
		{name: "an installation that may not read the permission", login: "a-member", status: http.StatusForbidden, header: map[string]string{"X-RateLimit-Remaining": "4999"},
			wantLog: "level=ERROR msg=\"workflow: GitHub refuses the review App's installation", wantCalls: 1},
		{name: "a 403 out of rate limit", login: "a-writer", status: http.StatusForbidden, header: map[string]string{"X-RateLimit-Remaining": "0", "X-RateLimit-Reset": reset},
			wantErr: "403", wantWait: 90 * time.Second, wantCalls: 1},
		{name: "a 403 with retry-after", login: "a-writer", status: http.StatusForbidden, header: map[string]string{"Retry-After": "120"},
			wantErr: "403", wantWait: 120 * time.Second, wantCalls: 1},
		{name: "a 429 naming no wait", login: "a-writer", status: http.StatusTooManyRequests, wantErr: "429", wantWait: time.Minute, wantCalls: 1},
		{name: "GitHub failing", login: "a-writer", status: http.StatusBadGateway, wantErr: "502", wantCalls: 1},
		{name: "the review App itself", login: "legion-reviewer[bot]", wantCalls: 0},
		{name: "a pull request the daemon does not record", login: "a-writer", number: 43, permission: "write", wantCalls: 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			calls := 0
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				calls++
				if got, want := r.URL.Path, "/repos/acme/widgets/collaborators/"+tc.login+"/permission"; got != want {
					t.Errorf("GitHub was asked %s, want %s", got, want)
				}
				for name, value := range tc.header {
					w.Header().Set(name, value)
				}
				if tc.status != 0 {
					http.Error(w, "refused", tc.status)
					return
				}
				answered := tc.answered
				if answered == "" {
					answered = tc.login
				}
				fmt.Fprintf(w, `{"permission":%q,"user":{"login":%q}}`, tc.permission, answered)
			}))
			defer server.Close()
			var logs bytes.Buffer
			w := reviewPermissionRuntime(pool, server.URL, slog.New(slog.NewTextHandler(&logs, nil)))
			number := tc.number
			if number == 0 {
				number = 42
			}

			got, err := w.reviewerCanWrite(context.Background(), intake.PullRequestReview{Repo: "acme/widgets", Number: number, State: "approved", Author: tc.login})
			switch {
			case tc.wantErr != "" && (err == nil || !strings.Contains(err.Error(), tc.wantErr)):
				t.Fatalf("%s's permission = %v, %v; want an error naming %s", tc.login, got, err, tc.wantErr)
			case tc.wantErr == "" && err != nil:
				t.Fatalf("%s's permission = %v, %v; want no error", tc.login, got, err)
			}
			var later *intake.RetryLater
			switch {
			case tc.wantWait == 0 && errors.As(err, &later):
				t.Fatalf("%s's permission = %v; want no wait named", tc.login, err)
			case tc.wantWait != 0 && !errors.As(err, &later):
				t.Fatalf("%s's permission = %v; want the %s wait GitHub named", tc.login, err, tc.wantWait)
			case tc.wantWait != 0 && (later.After > tc.wantWait || later.After < tc.wantWait-5*time.Second):
				t.Fatalf("%s's permission names a %s wait, want %s", tc.login, later.After, tc.wantWait)
			}
			if got != tc.want {
				t.Errorf("%s can write = %v, want %v", tc.login, got, tc.want)
			}
			if calls != tc.wantCalls {
				t.Errorf("GitHub was asked %d times, want %d", calls, tc.wantCalls)
			}
			if tc.wantLog != "" && !strings.Contains(logs.String(), tc.wantLog) {
				t.Errorf("logs = %s, want a line starting %s", logs.String(), tc.wantLog)
			}
			if strings.HasPrefix(tc.wantLog, "level=ERROR") && !strings.Contains(logs.String(), "installation_owner=acme") {
				t.Errorf("logs = %s, want the line to name the installation's owner", logs.String())
			}
		})
	}
}

// Any account can review a public repository's pull request, so the daemon asks GitHub about an
// account once per reviewPermissionTTL, whatever case its login comes in, however many reviews it
// submits, and asks again once that passes. A failed read stands for nothing: a rate limit is read
// again, so the writer the limit hid still decides once it lifts.
func TestAReviewersPermissionIsReadOncePerWindow(t *testing.T) {
	pool := reviewPermissionPool(t)
	var answers []int
	calls := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		status := http.StatusOK
		if calls < len(answers) {
			status = answers[calls]
		}
		calls++
		if status == http.StatusForbidden {
			w.Header().Set("Retry-After", "60")
			http.Error(w, "rate limited", status)
			return
		}
		fmt.Fprint(w, `{"permission":"write","user":{"login":"a-writer"}}`)
	}))
	defer server.Close()
	review := func(author string) intake.PullRequestReview {
		return intake.PullRequestReview{Repo: "acme/widgets", Number: 42, State: "approved", Author: author}
	}

	t.Run("within the window", func(t *testing.T) {
		answers, calls = nil, 0
		w := reviewPermissionRuntime(pool, server.URL, quietLogger())
		for _, author := range []string{"a-writer", "a-writer", "A-Writer"} {
			if canWrite, err := w.reviewerCanWrite(context.Background(), review(author)); err != nil || !canWrite {
				t.Fatalf("%s's permission = %v, %v; want write access", author, canWrite, err)
			}
		}
		if calls != 1 {
			t.Fatalf("GitHub was asked %d times for three reviews in one window, want once", calls)
		}
	})
	t.Run("after the window", func(t *testing.T) {
		answers, calls = nil, 0
		w := reviewPermissionRuntime(pool, server.URL, quietLogger())
		w.permissionTTL = time.Millisecond
		for range 2 {
			if canWrite, err := w.reviewerCanWrite(context.Background(), review("a-writer")); err != nil || !canWrite {
				t.Fatalf("a-writer's permission = %v, %v; want write access", canWrite, err)
			}
			time.Sleep(2 * w.permissionTTL)
		}
		if calls != 2 {
			t.Fatalf("GitHub was asked %d times for two reviews a window apart, want twice", calls)
		}
	})
	t.Run("after a rate limit", func(t *testing.T) {
		answers, calls = []int{http.StatusForbidden}, 0
		w := reviewPermissionRuntime(pool, server.URL, quietLogger())
		if _, err := w.reviewerCanWrite(context.Background(), review("a-writer")); err == nil {
			t.Fatal("a rate-limited read answered; want its error")
		}
		if canWrite, err := w.reviewerCanWrite(context.Background(), review("a-writer")); err != nil || !canWrite {
			t.Fatalf("a-writer's permission once the limit lifted = %v, %v; want write access", canWrite, err)
		}
		if calls != 2 {
			t.Fatalf("GitHub was asked %d times, want twice: the limit stands for nothing", calls)
		}
	})
}

// reviewPermissionPool is a store recording acme/widgets#42 for LEGION-208, and no other pull
// request.
func reviewPermissionPool(t *testing.T) *pgxpool.Pool {
	t.Helper()
	pool := isolatedOutboxPool(t)
	records := record.NewStore()
	putOutboxIssue(t, pool, records, record.Issue{Key: "LEGION-208", Project: "LEGION", Tree: "LEGION-208", Title: "Workflow", Phase: phase.Reviewing, Generation: 1, Status: "needs_review"})
	if err := pgx.BeginFunc(context.Background(), pool, func(tx pgx.Tx) error {
		return records.PutPullRequest(context.Background(), tx, record.PullRequest{Issue: "LEGION-208", Repo: "acme/widgets", Number: 42, Branch: "legion/LEGION-208", HeadSHA: "head", State: record.PullRequestOpen})
	}); err != nil {
		t.Fatalf("put the pull request: %v", err)
	}
	return pool
}

func reviewPermissionRuntime(pool *pgxpool.Pool, githubAPI string, log *slog.Logger) *workflowRuntime {
	return &workflowRuntime{pool: pool, records: projectRecords{Store: record.NewStore(), project: "LEGION"}, tokens: outboxTokens{},
		githubAPI: githubAPI, log: log, reviewAppLogin: "legion-reviewer[bot]"}
}
