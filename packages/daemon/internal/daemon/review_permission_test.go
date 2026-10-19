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

	"github.com/sjawhar/legion/daemon/internal/appauth"
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
		answered string
		status   int
		header   map[string]string
		// body is GitHub's refusal; empty is "refused".
		body      string
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
		{name: "a 401 for the review App's token, retried with a fresh one", login: "a-writer", status: http.StatusUnauthorized,
			wantErr: "401", wantWait: tokenRefusedWait, wantCalls: 1},
		{name: "a 403 out of rate limit", login: "a-writer", status: http.StatusForbidden, header: map[string]string{"X-RateLimit-Remaining": "0", "X-RateLimit-Reset": reset},
			wantErr: "403", wantWait: 90 * time.Second, wantCalls: 1},
		{name: "a 403 with retry-after", login: "a-writer", status: http.StatusForbidden, header: map[string]string{"Retry-After": "120"},
			wantErr: "403", wantWait: 120 * time.Second, wantCalls: 1},
		{name: "a 403 for a secondary rate limit named only by its message", login: "a-writer", status: http.StatusForbidden,
			header: map[string]string{"X-RateLimit-Remaining": "4321"}, body: `{"message":"You have exceeded a secondary rate limit. Please wait a few minutes before you try again."}`,
			wantErr: "403", wantWait: time.Minute, wantCalls: 1},
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
					body := tc.body
					if body == "" {
						body = "refused"
					}
					http.Error(w, body, tc.status)
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

// GitHub answers 401 for a review App token it has not yet accepted (a token minted moments
// before) as for one whose installation was removed since. Neither says anything about the
// author, so a 401 decides nothing and keeps nothing, and GitHub's rate limit is not held for it.
// A token older than the acceptance window is dropped and the delivery retried with a fresh mint,
// which either reads the permission or, for a removed App, is refused as not installed; a younger
// one is kept and tried again once it is that old.
func TestA401ForTheReviewAppsTokenIsRetriedWithAFreshOne(t *testing.T) {
	for _, tc := range []struct {
		name     string
		minted   time.Duration
		wantWait time.Duration
		forgot   string
	}{
		{"a token older than the window is minted again", -time.Hour, tokenRefusedWait, "review acme"},
		{"a token moments old is tried again as it is", -time.Second, tokenAcceptWindow - time.Second, ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			pool := reviewPermissionPool(t)
			calls := 0
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				calls++
				if calls == 1 {
					http.Error(w, `{"message":"Bad credentials"}`, http.StatusUnauthorized)
					return
				}
				fmt.Fprint(w, `{"permission":"write","user":{"login":"a-writer"}}`)
			}))
			defer server.Close()
			w := reviewPermissionRuntime(pool, server.URL, quietLogger())
			tokens := &forgettingTokens{minted: time.Now().Add(tc.minted)}
			w.tokens, w.forget = tokens, tokens
			review := intake.PullRequestReview{Repo: "acme/widgets", Number: 42, State: "approved", Author: "a-writer"}
			_, err := w.reviewerCanWrite(context.Background(), review)
			var later *intake.RetryLater
			if !errors.As(err, &later) || later.After > tc.wantWait || later.After < tc.wantWait-time.Second {
				t.Fatalf("a 401 = %v, want a retry after about %s", err, tc.wantWait)
			}
			if tokens.forgot != tc.forgot {
				t.Fatalf("forgot %q, want %q", tokens.forgot, tc.forgot)
			}
			if left := w.rateLimitLeft(); left > 0 {
				t.Fatalf("a 401 held GitHub's rate limit for %s, want no hold", left)
			}
			if canWrite, err := w.reviewerCanWrite(context.Background(), review); err != nil || !canWrite {
				t.Fatalf("the retried review = %v, %v; want the writer's write access read again", canWrite, err)
			}
			if calls != 2 {
				t.Fatalf("GitHub was asked %d times, want twice: the 401 kept no answer", calls)
			}
		})
	}
}

// forgettingTokens are outboxTokens whose leases were minted at minted, recording the cached token
// Forget drops.
type forgettingTokens struct {
	outboxTokens
	minted time.Time
	forgot string
}

func (t *forgettingTokens) Token(ctx context.Context, role appauth.AppRole, owner string) (appauth.Lease, error) {
	lease, err := t.outboxTokens.Token(ctx, role, owner)
	lease.Minted = t.minted
	return lease, err
}

func (t *forgettingTokens) Forget(role appauth.AppRole, owner string) {
	t.forgot = string(role) + " " + owner
}

// Any account can review a public repository's pull request, so an account GitHub gives no write
// access is asked about once per reviewPermissionTTL, whatever case its login comes in, however
// many reviews it submits, and asked about again once that passes. Write access is never kept: a
// writer reviews rarely, and a kept answer would let one whose access was just removed decide, and
// would skip the check that GitHub's answer names the review's author. While GitHub's rate limit
// stands, an account with no answer kept is answered with the wait left, with no call made against
// a limited API; once the wait has passed, reads resume, so the writer the limit hid still decides.
func TestAReviewersPermissionIsReadOncePerWindow(t *testing.T) {
	pool := reviewPermissionPool(t)
	var limitFirst bool
	calls := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		if limitFirst && calls == 1 {
			w.Header().Set("Retry-After", "1")
			http.Error(w, "rate limited", http.StatusForbidden)
			return
		}
		login := strings.TrimSuffix(strings.TrimPrefix(r.URL.Path, "/repos/acme/widgets/collaborators/"), "/permission")
		permission := "write"
		if strings.Contains(login, "reader") {
			permission = "read"
		}
		fmt.Fprintf(w, `{"permission":%q,"user":{"login":%q}}`, permission, login)
	}))
	defer server.Close()
	review := func(author string) intake.PullRequestReview {
		return intake.PullRequestReview{Repo: "acme/widgets", Number: 42, State: "approved", Author: author}
	}
	read := func(t *testing.T, w *workflowRuntime, author string, want bool) {
		t.Helper()
		canWrite, err := w.reviewerCanWrite(context.Background(), review(author))
		if err != nil || canWrite != want {
			t.Fatalf("%s's permission = %v, %v; want %v and no error", author, canWrite, err, want)
		}
	}

	t.Run("no write access, within the window", func(t *testing.T) {
		limitFirst, calls = false, 0
		w := reviewPermissionRuntime(pool, server.URL, quietLogger())
		for _, author := range []string{"a-reader", "a-reader", "A-Reader"} {
			read(t, w, author, false)
		}
		if calls != 1 {
			t.Fatalf("GitHub was asked %d times for three reviews in one window, want once", calls)
		}
	})
	t.Run("no write access, after the window", func(t *testing.T) {
		limitFirst, calls = false, 0
		w := reviewPermissionRuntime(pool, server.URL, quietLogger())
		w.permissionTTL = time.Millisecond
		for range 2 {
			read(t, w, "a-reader", false)
			time.Sleep(2 * w.permissionTTL)
		}
		if calls != 2 {
			t.Fatalf("GitHub was asked %d times for two reviews a window apart, want twice", calls)
		}
	})
	t.Run("write access is read every time", func(t *testing.T) {
		limitFirst, calls = false, 0
		w := reviewPermissionRuntime(pool, server.URL, quietLogger())
		for range 3 {
			read(t, w, "a-writer", true)
		}
		if calls != 3 {
			t.Fatalf("GitHub was asked %d times for three of a writer's reviews, want three: write access is never kept", calls)
		}
	})
	t.Run("while the rate limit stands", func(t *testing.T) {
		limitFirst, calls = true, 0
		w := reviewPermissionRuntime(pool, server.URL, quietLogger())
		if _, err := w.reviewerCanWrite(context.Background(), review("a-writer")); err == nil {
			t.Fatal("a rate-limited read answered; want its error")
		}
		_, err := w.reviewerCanWrite(context.Background(), review("another-writer"))
		var later *intake.RetryLater
		if !errors.As(err, &later) || later.After <= 0 {
			t.Fatalf("a read while the limit stands = %v; want the wait left", err)
		}
		if calls != 1 {
			t.Fatalf("GitHub was asked %d times while its limit stood, want once: the limit itself", calls)
		}
		time.Sleep(1100 * time.Millisecond)
		read(t, w, "a-writer", true)
		if calls != 2 {
			t.Fatalf("GitHub was asked %d times once the wait had passed, want twice", calls)
		}
	})
}

// Minting the review App's token can itself meet GitHub's rate limit, answered as GitHub's own
// trouble rather than a permission (appauth.TransientError) rather than an answer about the
// account. That is retried in a minute, same as a rate-limited permission read, and holds the
// limit the same way, so the next uncached review inside the hold is answered without minting
// again: minting on through the App's own limit risks the ban GitHub warns a repeated call does.
func TestAReviewMintThatFailsTransientlyHoldsTheLimitWithoutMintingAgain(t *testing.T) {
	pool := reviewPermissionPool(t)
	tokens := &transientOnceTokens{}
	w := reviewPermissionRuntime(pool, "http://unused.invalid", quietLogger())
	w.tokens = tokens

	review := func(author string) intake.PullRequestReview {
		return intake.PullRequestReview{Repo: "acme/widgets", Number: 42, State: "approved", Author: author}
	}

	_, err := w.reviewerCanWrite(context.Background(), review("a-writer"))
	var later *intake.RetryLater
	if !errors.As(err, &later) || later.After != time.Minute {
		t.Fatalf("a transient mint failure = %v; want a one-minute RetryLater", err)
	}
	if left := w.rateLimitLeft(); left <= 0 {
		t.Fatalf("rate limit left = %v after a transient mint failure; want the hold set", left)
	}

	_, err = w.reviewerCanWrite(context.Background(), review("another-writer"))
	if !errors.As(err, &later) || later.After <= 0 {
		t.Fatalf("a review inside the hold = %v; want a RetryLater naming the wait left", err)
	}
	if tokens.calls != 1 {
		t.Fatalf("the review App was minted %d times while the hold stood, want once", tokens.calls)
	}
}

// A review on a repository whose owner the review App is not installed on (installed once, then
// removed, while a record still names the repository, which GitHub intake keeps hearing) decides
// nothing: the mint's refusal is answered at once as no write access, logged at error naming the
// owner, and held for the permission window, rather than returned for a delivery that would mint
// again on every redelivery. The review App's own review is restored from what was delivered.
func TestAReviewOnAnOwnerTheReviewAppIsNotInstalledOnDecidesNothing(t *testing.T) {
	pool := reviewPermissionPool(t)
	var logged syncBuffer
	w := reviewPermissionRuntime(pool, "http://unused.invalid", slog.New(slog.NewJSONHandler(&logged, nil)))
	tokens := &notInstalledTokens{}
	w.tokens = tokens
	review := func(author string) intake.PullRequestReview {
		return intake.PullRequestReview{Repo: "acme/widgets", Number: 42, State: "approved", Author: author, Body: "delivered body"}
	}
	for range 2 {
		canWrite, err := w.reviewerCanWrite(context.Background(), review("a-writer"))
		if err != nil || canWrite {
			t.Fatalf("a review on an owner with no review App = %v, %v; want no write access and no error", canWrite, err)
		}
	}
	if tokens.calls != 1 {
		t.Fatalf("the review App was minted %d times for one author inside the permission window, want once", tokens.calls)
	}
	if !strings.Contains(logged.String(), `"installation_owner":"acme"`) || !strings.Contains(logged.String(), `"level":"ERROR"`) {
		t.Fatalf("log = %s, want an error naming the owner the review App is not installed on", logged.String())
	}
	w.reviewAppLogin = "legion-review[bot]"
	if body, err := w.reviewBody(context.Background(), review("legion-review[bot]")); err != nil || body != "delivered body" {
		t.Fatalf("the review App's own review body = %q, %v; want the delivered body and no error", body, err)
	}
}

// notInstalledTokens is a review App not installed on any owner: every mint is refused as
// appauth refuses one (appauth.ErrNotInstalled), counted.
type notInstalledTokens struct{ calls int }

func (t *notInstalledTokens) Token(_ context.Context, role appauth.AppRole, owner string) (appauth.Lease, error) {
	t.calls++
	return appauth.Lease{}, fmt.Errorf("%w: %s not installed on %s", appauth.ErrNotInstalled, role, owner)
}

func (t *notInstalledTokens) ReadToken(ctx context.Context, role appauth.AppRole, owner string) (appauth.Lease, error) {
	return t.Token(ctx, role, owner)
}

// transientOnceTokens answers its first mint with appauth.TransientError and every one after with
// a lease, so a test can tell a retried mint from one the hold should have skipped.
type transientOnceTokens struct{ calls int }

func (t *transientOnceTokens) Token(context.Context, appauth.AppRole, string) (appauth.Lease, error) {
	t.calls++
	if t.calls == 1 {
		return appauth.Lease{}, &appauth.TransientError{Err: errors.New("mint refused: GitHub's rate limit")}
	}
	return appauth.Lease{Token: "token", ExpiresAt: time.Now().Add(time.Hour)}, nil
}

func (t *transientOnceTokens) ReadToken(ctx context.Context, role appauth.AppRole, owner string) (appauth.Lease, error) {
	lease, err := t.Token(ctx, role, owner)
	lease.Token += "-read"
	return lease, err
}

// reviewPermissionPool is a store recording acme/widgets#42 for LEGION-208, and no other pull
// request.
func reviewPermissionPool(t *testing.T) *pgxpool.Pool {
	t.Helper()
	pool := isolatedOutboxPool(t)
	records := record.NewStore()
	putOutboxIssue(t, pool, records, record.Issue{Key: "LEGION-208", Repo: "acme/widgets", DefaultBranch: "main", GitHubRepo: "acme/widgets", Project: "LEGION", Tree: "LEGION-208", Title: "Workflow", Phase: phase.Reviewing, Generation: 1, Status: "needs_review"})
	if err := pgx.BeginFunc(context.Background(), pool, func(tx pgx.Tx) error {
		return records.PutPullRequest(context.Background(), tx, record.PullRequest{Issue: "LEGION-208", Repo: "acme/widgets", Number: 42, Branch: "legion/LEGION-208", HeadSHA: "head", State: record.PullRequestOpen})
	}); err != nil {
		t.Fatalf("put the pull request: %v", err)
	}
	return pool
}

func reviewPermissionRuntime(pool *pgxpool.Pool, githubAPI string, log *slog.Logger) *workflowRuntime {
	tokens := &forgettingTokens{}
	return &workflowRuntime{pool: pool, records: projectRecords{Store: record.NewStore(), project: "LEGION"}, tokens: tokens, forget: tokens,
		githubAPI: githubAPI, log: log, reviewAppLogin: "legion-reviewer[bot]"}
}
