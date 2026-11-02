package delivery

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/sjawhar/envoy/internal/dispatch/githubapp"
	"github.com/sjawhar/envoy/internal/dispatch/refs"
	"github.com/sjawhar/envoy/internal/dispatch/store"
)

// A pull request's head is what a recorded check result is measured against: a `before_merge`
// result is current while its commit is the head of an open pull request, or the final head of
// a merged one, attributed to the issue the result was recorded on or to the check's issue.
// pull_request_heads holds that head for every pull request Dispatch has seen an event for, in
// any repository and by any author — not the merged population delivery_pull_requests keeps —
// and the issue Dispatch's own attribution picks for it (attribution.go), held to the
// repository the pull request is in (headAttribution). A child's pull request is attributed to
// the child, so it makes the child's own result against its parent's check current — the child
// is what carries that work — and never a result the parent recorded for itself.
//
// Three writers keep it: the `pull_request` envelope (intake.go, every action), the read-through
// a recorded result makes when its commit is no head Dispatch knows (ReadThroughCommitPulls),
// and the reconcile's five-minute pass over stale open rows (reconcile.go). Nothing is written
// when the results are read.

// PullRequestHead is one pull request's current head, as pull_request_heads stores it.
type PullRequestHead struct {
	Repo              string
	Number            int
	HeadSHA           string
	HeadRef           string
	BaseRef           string
	State             string
	IssueKey          *string
	AttributionSource string
	Title             string
	URL               string
	EventAt           *time.Time
	CheckedAt         *time.Time
}

// PullRequestHeadState is the state a `pull_request` event leaves the row in: a pull request
// that merged is merged, whatever the action, one that closed without merging is closed, and
// every other action leaves it open. The merge is tested first: GitHub normalises every action
// of a pull request, so a `labeled` or `edited` event on an already merged one must not store
// its row as open again and put it back in the reconcile's stale list.
func PullRequestHeadState(action string, merged bool) string {
	if merged {
		return "merged"
	}
	if action != "closed" {
		return "open"
	}
	return "closed"
}

// UpsertPullRequestHead writes one pull request's head row. An event older than the one the row
// already holds changes nothing: GitHub redelivers and the reconcile reads concurrently, so the
// row keeps the newest `updated_at` either has seen rather than whichever write landed last. A
// write carrying no event time (the read-through, which reads no updated_at) always applies, as
// does one landing on a row that has none. A write carrying no read time leaves the row's
// checked_at as it was: nulling it would hand a row the reconcile has just read back to the
// next batch (ListStalePullRequestHeads orders the never-read first).
func UpsertPullRequestHead(ctx context.Context, pool *store.Pool, head PullRequestHead) error {
	_, err := pool.Exec(ctx, `
		insert into pull_request_heads (
			repo, number, head_sha, head_ref, base_ref, state, issue_key, attribution_source,
			title, url, event_at, checked_at
		)
		values ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12)
		on conflict (repo, number) do update set
			head_sha = excluded.head_sha,
			head_ref = excluded.head_ref,
			base_ref = excluded.base_ref,
			state = excluded.state,
			issue_key = excluded.issue_key,
			attribution_source = excluded.attribution_source,
			title = excluded.title,
			url = excluded.url,
			event_at = coalesce(excluded.event_at, pull_request_heads.event_at),
			checked_at = coalesce(excluded.checked_at, pull_request_heads.checked_at)
		where excluded.event_at is null
		   or pull_request_heads.event_at is null
		   or excluded.event_at >= pull_request_heads.event_at
	`,
		head.Repo, head.Number, head.HeadSHA, head.HeadRef, head.BaseRef, head.State,
		head.IssueKey, head.AttributionSource, head.Title, head.URL, head.EventAt, head.CheckedAt,
	)
	if err != nil {
		return fmt.Errorf("store the head of %s#%d: %w", head.Repo, head.Number, err)
	}
	return nil
}

// HeadAttributedTo reports whether sha is the head of a pull request attributed to issueKey that
// is open or merged: the currency test a `before_merge` result's commit passes. A closed-unmerged
// pull request's head proves nothing. A pull request attributed to another issue never answers
// for this one; the results read asks this about both the issue a result was recorded on and
// the check's issue, so a child proving its parent's check is answered by the child's own pull
// request and the parent's own result at that commit is not (api/results.go's currencyReader).
func HeadAttributedTo(ctx context.Context, pool *store.Pool, issueKey, sha string) (bool, error) {
	var found bool
	if err := pool.QueryRow(ctx, `
		select exists (
			select 1 from pull_request_heads
			where issue_key = $1 and head_sha = $2 and state in ('open', 'merged')
		)
	`, issueKey, sha).Scan(&found); err != nil {
		return false, fmt.Errorf("read the pull request heads of %s at %s: %w", issueKey, sha, err)
	}
	return found, nil
}

// PullRequestHeadStaleAfter is how long an open pull request's row may go unread before the
// reconcile reads it again, and PullRequestHeadBatch how many it reads in one pass.
const (
	PullRequestHeadStaleAfter = 5 * time.Minute
	PullRequestHeadBatch      = 200
)

// ListStalePullRequestHeads is at most limit open rows whose checked_at is older than
// PullRequestHeadStaleAfter (a row never read first), the longest unread first.
func ListStalePullRequestHeads(ctx context.Context, pool *store.Pool, limit int, now time.Time) ([]PullRequestHead, error) {
	rows, err := pool.Query(ctx, `
		select repo, number from pull_request_heads
		where state = 'open' and (checked_at is null or checked_at < $1)
		order by checked_at nulls first, repo, number
		limit $2
	`, now.Add(-PullRequestHeadStaleAfter), limit)
	if err != nil {
		return nil, fmt.Errorf("list the stale pull request heads: %w", err)
	}
	defer rows.Close()
	var heads []PullRequestHead
	for rows.Next() {
		var head PullRequestHead
		if err := rows.Scan(&head.Repo, &head.Number); err != nil {
			return nil, fmt.Errorf("list the stale pull request heads: %w", err)
		}
		heads = append(heads, head)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("list the stale pull request heads: %w", err)
	}
	return heads, nil
}

// MarkPullRequestHeadChecked stamps checked_at on a row the reconcile read but could not
// complete, so one unreadable pull request does not hold the batch every pass.
func MarkPullRequestHeadChecked(ctx context.Context, pool *store.Pool, repoFull string, number int, now time.Time) error {
	_, err := pool.Exec(ctx, `
		update pull_request_heads set checked_at = $3 where repo = $1 and number = $2
	`, repoFull, number, now)
	if err != nil {
		return fmt.Errorf("stamp the head of %s#%d: %w", repoFull, number, err)
	}
	return nil
}

// headAttribution resolves the issue a pull request belongs to from what GitHub said about it,
// through the same five ranked sources every other attribution uses (resolveIssueKey), and then
// holds that issue to the repository the pull request is in.
//
// The holding is what keeps the currency mark honest. A pull request's title, body and branch
// are text any account that can open one writes, this row is written for any author and any
// repository the App watches, and an attributed row is exactly what makes a `before_merge`
// result read current (HeadAttributedTo), which is the reviewer's approval condition. So an
// issue that resolves to another repository, or to none, attributes nothing: the row is still
// written, since the reconcile and the head itself are wanted either way, but with no issue and
// the source "none".
func headAttribution(ctx context.Context, pool *store.Pool, facts attributionFacts) (*string, string, error) {
	issueKey, source, err := resolveIssueKey(ctx, pool, facts)
	if err != nil {
		return nil, string(sourceNone), err
	}
	if issueKey == nil {
		return nil, string(source), nil
	}
	resolved, err := refs.IssueRepository(ctx, pool, *issueKey)
	if err != nil {
		return nil, string(sourceNone), err
	}
	if resolved == nil || !strings.EqualFold(*resolved, facts.Repo) {
		slog.Info("dispatch delivery: a pull request names an issue of another repository",
			"repo", facts.Repo, "issue", *issueKey, "source", string(source))
		return nil, string(sourceNone), nil
	}
	return issueKey, string(source), nil
}

// RefreshPullRequestHead reads one pull request through the App and writes its head row: the
// reconcile's per-row step, and the read-through's completion for a pull request GitHub named
// for a commit. A pull request GitHub cannot find leaves the row as it is, stamped so the batch
// moves on; a rate limit comes back as the *githubapp.RateLimitError, which stops the batch.
func RefreshPullRequestHead(ctx context.Context, pool *store.Pool, client *githubapp.Client, repoFull string, number int, now time.Time) error {
	owner, repo, err := splitRepo(repoFull)
	if err != nil {
		return err
	}
	token, err := client.RepositoryToken(ctx, owner, repo)
	if err != nil {
		return fmt.Errorf("mint installation token for %s PR #%d: %w", repoFull, number, err)
	}
	payload, err := fetchPullRequestPayload(ctx, client, token, owner, repo, number)
	if err != nil {
		if errors.Is(err, ErrPullRequestNotFound) {
			return MarkPullRequestHeadChecked(ctx, pool, repoFull, number, now)
		}
		return err
	}
	messages, _, err := fetchCommitMessagesWithToken(ctx, client, func() (string, error) { return token, nil }, owner, repo, number)
	if err != nil {
		if limited, ok := githubapp.AsRateLimit(err); ok {
			return limited
		}
		// The title, body and branch still name an issue; the commit messages are the last of
		// the five sources and the read is retried on the next pass.
		slog.Warn("dispatch delivery: read the commit messages of a pull request head", "repo", repoFull, "number", number, "error", err)
	}
	issueKey, source, err := headAttribution(ctx, pool, attributionFacts{
		Repo: repoFull, URL: payload.HTMLURL, Title: payload.Title, Body: payload.Body,
		HeadRef: payload.Head.Ref, CommitMessages: messages,
	})
	if err != nil {
		return err
	}
	return UpsertPullRequestHead(ctx, pool, PullRequestHead{
		Repo: repoFull, Number: payload.Number, HeadSHA: payload.Head.SHA, HeadRef: payload.Head.Ref,
		BaseRef: payload.Base.Ref, State: PullRequestHeadState(payload.State, payload.Merged),
		IssueKey: issueKey, AttributionSource: source, Title: payload.Title, URL: payload.HTMLURL,
		EventAt: payload.UpdatedAt, CheckedAt: &now,
	})
}

// ReadThroughCommitPulls teaches Dispatch the pull requests GitHub names for one commit
// (`GET /repos/{o}/{r}/commits/{sha}/pulls`) and writes each one's head row, so a result
// recorded at the head of a pull request no event has reached Dispatch for is current at once.
// It answers how many rows it wrote. A repository the App cannot read writes nothing and is no
// error: the result is recorded either way, and its currency is unknown.
func ReadThroughCommitPulls(ctx context.Context, pool *store.Pool, client *githubapp.Client, repoFull, sha string, now time.Time) (int, error) {
	owner, repo, err := splitRepo(repoFull)
	if err != nil {
		return 0, err
	}
	token, err := client.RepositoryToken(ctx, owner, repo)
	if err != nil {
		return 0, fmt.Errorf("mint installation token for %s: %w", repoFull, err)
	}
	pulls, err := client.CommitPulls(ctx, token, owner, repo, sha)
	if err != nil {
		return 0, fmt.Errorf("read the pull requests of %s@%s: %w", repoFull, sha, err)
	}
	written := 0
	for _, pull := range pulls {
		issueKey, source, err := headAttribution(ctx, pool, attributionFacts{
			Repo: repoFull, URL: pull.URL, Title: pull.Title, Body: pull.Body, HeadRef: pull.HeadRef,
		})
		if err != nil {
			return written, err
		}
		updated := pull.UpdatedAt
		if err := UpsertPullRequestHead(ctx, pool, PullRequestHead{
			Repo: repoFull, Number: pull.Number, HeadSHA: pull.HeadSHA, HeadRef: pull.HeadRef,
			BaseRef: pull.BaseRef, State: PullRequestHeadState(pull.State, pull.Merged),
			IssueKey: issueKey, AttributionSource: source, Title: pull.Title, URL: pull.URL,
			EventAt: updated, CheckedAt: &now,
		}); err != nil {
			return written, err
		}
		written++
	}
	return written, nil
}

// recordPullRequestHead writes the head row one `pull_request` envelope reports, whatever its
// action, author or repository, and answers the commit messages it read, nil when it read none.
// It runs before the merge-only guard in handlePullRequestEnvelope, so an `opened`,
// `synchronize`, `reopened` or `edited` event — which the delivery timeline ignores — still
// moves the head a check result is measured against.
//
// The envelope carries the title, body, branch and head commit, which are four of the
// attribution's five sources and outrank the fifth. The commit messages, rank 4, can only
// decide a pull request the other four leave unattributed, so they are read — the one GitHub
// call this path can make — only then, and a merge that follows reuses what was read rather
// than paging the same commits again (completePullRequest). A read the App refuses attributes
// the row from the envelope alone; a rate limit is returned, as the reconcile's own refresh
// returns it, so the caller does not persist a weaker attribution and then stamp the row read.
func (in *Intake) recordPullRequestHead(ctx context.Context, payload map[string]string, number int) ([]string, error) {
	repoFull := payload["repo"]
	owner, repo, err := splitRepo(repoFull)
	if err != nil {
		return nil, err
	}
	facts := attributionFacts{
		Repo: repoFull, URL: payload["url"], Title: payload["title"], Body: payload["body"],
		HeadRef: payload["head_ref"],
	}
	issueKey, source, err := headAttribution(ctx, in.pool, facts)
	if err != nil {
		return nil, err
	}
	var messages []string
	complete := true
	if issueKey == nil {
		read, readErr := fetchCommitMessages(ctx, in.github, owner, repo, number)
		if readErr != nil {
			if limited, ok := githubapp.AsRateLimit(readErr); ok {
				return nil, limited
			}
			complete = false
			slog.Debug("dispatch delivery: a pull request head is attributed without its commit messages",
				"repo", repoFull, "number", number, "error", readErr)
		} else {
			messages = read
			facts.CommitMessages = messages
			if issueKey, source, err = headAttribution(ctx, in.pool, facts); err != nil {
				return nil, err
			}
		}
	}
	// The envelope is itself a fresh read of the head, so the row is stamped read and the
	// reconcile does not spend three GitHub calls re-reading what the event just delivered —
	// unless the commit messages were wanted and could not be had, which the next pass retries.
	var checkedAt *time.Time
	if complete {
		now := time.Now()
		checkedAt = &now
	}
	return messages, UpsertPullRequestHead(ctx, in.pool, PullRequestHead{
		Repo: repoFull, Number: number, HeadSHA: payload["head_sha"], HeadRef: payload["head_ref"],
		BaseRef: payload["base_ref"], State: PullRequestHeadState(payload["action"], payload["merged"] == "true"),
		IssueKey: issueKey, AttributionSource: source, Title: payload["title"], URL: payload["url"],
		EventAt: envelopeTime(payload["updated_at"]), CheckedAt: checkedAt,
	})
}

// envelopeTime reads an envelope's RFC3339 timestamp; an absent or malformed one is no time,
// which the upsert treats as "apply this write".
func envelopeTime(value string) *time.Time {
	if value == "" {
		return nil
	}
	at, err := time.Parse(time.RFC3339, value)
	if err != nil {
		return nil
	}
	return &at
}

// DefaultBranchCommit is what Dispatch last learned about one commit of one repository:
// whether it sits on the default branch, and when that was read.
type DefaultBranchCommit struct {
	OnDefault bool
	CheckedAt time.Time
}

// DefaultBranchCommitStaleAfter is how long a "no" is believed. A "yes" is kept for good: a
// commit on the default branch never leaves it.
const DefaultBranchCommitStaleAfter = 5 * time.Minute

// ReadDefaultBranchCommit is what default_branch_commits holds for one commit, and whether it
// holds anything at all.
func ReadDefaultBranchCommit(ctx context.Context, pool *store.Pool, repoFull, sha string) (DefaultBranchCommit, bool, error) {
	var answer DefaultBranchCommit
	err := pool.QueryRow(ctx, `
		select on_default, checked_at from default_branch_commits where repo = $1 and sha = $2
	`, repoFull, sha).Scan(&answer.OnDefault, &answer.CheckedAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return DefaultBranchCommit{}, false, nil
	}
	if err != nil {
		return DefaultBranchCommit{}, false, fmt.Errorf("read whether %s@%s is on the default branch: %w", repoFull, sha, err)
	}
	return answer, true, nil
}

// WriteDefaultBranchCommit records GitHub's answer about one commit.
func WriteDefaultBranchCommit(ctx context.Context, pool *store.Pool, repoFull, sha string, onDefault bool, now time.Time) error {
	_, err := pool.Exec(ctx, `
		insert into default_branch_commits (repo, sha, on_default, checked_at)
		values ($1, $2, $3, $4)
		on conflict (repo, sha) do update set on_default = excluded.on_default, checked_at = excluded.checked_at
	`, repoFull, sha, onDefault, now)
	if err != nil {
		return fmt.Errorf("record whether %s@%s is on the default branch: %w", repoFull, sha, err)
	}
	return nil
}

// RepositoryDefaultBranch is the name of repoFull's default branch, read through the App.
func RepositoryDefaultBranch(ctx context.Context, client *githubapp.Client, repoFull string) (string, error) {
	owner, repo, err := splitRepo(repoFull)
	if err != nil {
		return "", err
	}
	token, err := client.RepositoryToken(ctx, owner, repo)
	if err != nil {
		return "", fmt.Errorf("mint installation token for %s: %w", repoFull, err)
	}
	return client.DefaultBranch(ctx, token, owner, repo)
}

// CommitOnDefaultBranch asks the App whether sha is on repoFull's default branch.
func CommitOnDefaultBranch(ctx context.Context, client *githubapp.Client, repoFull, branch, sha string) (bool, error) {
	owner, repo, err := splitRepo(repoFull)
	if err != nil {
		return false, err
	}
	token, err := client.RepositoryToken(ctx, owner, repo)
	if err != nil {
		return false, fmt.Errorf("mint installation token for %s: %w", repoFull, err)
	}
	return client.CommitOnBranch(ctx, token, owner, repo, branch, sha)
}
