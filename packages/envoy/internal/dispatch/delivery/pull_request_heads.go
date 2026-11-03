package delivery

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"time"

	"github.com/sjawhar/envoy/internal/dispatch/githubapp"
	"github.com/sjawhar/envoy/internal/dispatch/refs"
	"github.com/sjawhar/envoy/internal/dispatch/store"
)

// A pull request's head is what a recorded check result is measured against: a `before_merge`
// result is current while its commit is the head of an open pull request, or the final head of
// a merged one, attributed to the check's own issue. pull_request_heads holds that head for
// every pull request Dispatch has seen an event for, in any repository and by any author — not
// the merged population delivery_pull_requests keeps — and the issue Dispatch's own attribution
// picks for it (attribution.go), held to the repository the pull request is in and to a head
// branch in that same repository, never a fork's (headAttribution). A child's pull request is
// attributed to the child, so it answers for the child's own checks alone and never for a check
// of an issue above it.
//
// Three writers keep it: the `pull_request` envelope (intake.go, every action), the read-through
// a recorded result makes when its commit is no head Dispatch knows (ReadThroughCommitPulls),
// and the reconcile's five-minute pass (reconcile.go), which takes every open row read longer
// than five minutes ago and, once each, the merged rows that no read of GitHub ever reached and
// that are attributed to nobody (ListStalePullRequestHeads). Nothing is written when the results
// are read.

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
// row keeps the newest `updated_at` either has seen rather than whichever write landed last.
// Every writer carries one — the envelope its own, the reconcile and the read-through the
// pull request's `updated_at` as GitHub answered it — so a write with no event time is only one
// whose envelope carried none; it always applies, as does one landing on a row that has none.
// Each writer attributes the row through all five sources (attributeHead), so an attribution a
// later write cannot make is not a weaker one it may overwrite. A write carrying no read time
// leaves the row's checked_at as it was: nulling it would hand a row the reconcile has just
// read back to the next batch (ListStalePullRequestHeads orders the never-read first).
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
// for this one; the results read asks this about the check's own issue, so a child's pull
// request never makes a result against a check above it current (api/results.go's
// currencyReader).
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

// ListStalePullRequestHeads is at most limit rows for the reconcile to read again, the longest
// unread first: every open row whose checked_at is older than PullRequestHeadStaleAfter (a row
// never read first), and every merged row that carries no attribution and has never been read.
//
// The merged arm is what keeps a merged head's currency honest. A `pull_request` envelope that
// names no head repository attributes nobody (recordPullRequestHead), and GitHub's own answer
// always names head.repo, so one read settles the row; without this arm a merge delivered by
// such an envelope would stand unattributed for good, since no other writer looks at a merged
// row again and a result at that head would read `older` for ever.
//
// checked_at is null is the bound, and it holds because only a reader of GitHub stamps it: the
// reconcile's own refresh (RefreshPullRequestHead, which stamps whether or not the read could
// attribute), the read-through, and an envelope only when its payload was a complete read, which
// an envelope naming no head repository is not. So a merged row of this arm is taken exactly
// once — the refresh stamps it, and it never comes back to the list, even when GitHub's answer
// still attributes it to nobody. A merged row that was read while it was open and left
// unattributed is not taken at all: it is stamped already, and GitHub's read of it answers the
// same after the merge as it did before, so reading it again would buy nothing.
func ListStalePullRequestHeads(ctx context.Context, pool *store.Pool, limit int, now time.Time) ([]PullRequestHead, error) {
	rows, err := pool.Query(ctx, `
		select repo, number from pull_request_heads
		where (state = 'open' and (checked_at is null or checked_at < $1))
		   or (state = 'merged' and issue_key is null and checked_at is null)
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

// headInOwnRepository reports whether a pull request's head branch lives in the repository the
// pull request is in, which is what GitHub's `head.repo.full_name` names. A head outside it is a
// fork's, and an answer that names no head repository at all — GitHub's null for a fork whose
// repository was deleted, or a `pull_request` envelope published before Dispatch carried the
// field — does not say that it is this repository, so neither one is.
func headInOwnRepository(repoFull, headRepo string) bool {
	return headRepo != "" && strings.EqualFold(headRepo, repoFull)
}

// headAttribution resolves the issue a pull request belongs to from what GitHub said about it,
// through the same five ranked sources every other attribution uses (resolveIssueKey), and then
// holds that issue to the repository the pull request is in and its head to that repository too.
//
// Both holdings are what keep the currency mark honest. A pull request's title, body and branch
// are text any account that can open one writes, this row is written for any author and any
// repository the App watches, and an attributed row is exactly what makes a `before_merge`
// result read current (HeadAttributedTo), which is the reviewer's approval condition. So an
// issue that resolves to another repository, or to none, attributes nothing; and neither does a
// pull request whose head is not in this repository, since on a public repository anyone may
// open one from a fork at any commit of their choosing — including an earlier one that already
// carries a recorded pass — and title it after an issue, which would make that older result
// current again. Either way the row is still written, since the reconcile and the head itself
// are wanted either way, but with no issue and the source "none".
func headAttribution(ctx context.Context, pool *store.Pool, facts attributionFacts, headRepo string) (*string, string, error) {
	issueKey, source, err := resolveIssueKey(ctx, pool, facts)
	if err != nil {
		return nil, string(sourceNone), err
	}
	if issueKey == nil {
		return nil, string(source), nil
	}
	if !headInOwnRepository(facts.Repo, headRepo) {
		slog.Info("dispatch delivery: a pull request whose head is in another repository names an issue",
			"repo", facts.Repo, "head_repo", headRepo, "issue", *issueKey, "source", string(source))
		return nil, string(sourceNone), nil
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

// attributeHead is headAttribution plus the one source the caller's facts cannot carry: the
// commit messages, rank 5, which are read — one GitHub call, or one per page of 100 commits —
// only when the title, body, branch and links leave the pull request unattributed, as every
// source after the first is read only when the earlier ones name nobody (attribution.go). A
// fork's pull request is not read at all: it attributes nobody whatever its commits say.
//
// It answers the issue, its source, the messages it read (nil when it read none) and whether
// every source was consulted. A read the App refuses leaves the attribution weaker than it could
// be, which a caller records by leaving the row unstamped for the reconcile to read again; a
// rate limit is returned rather than swallowed, so no caller persists a weaker attribution and
// then stamps the row read.
func attributeHead(ctx context.Context, pool *store.Pool, client *githubapp.Client, token func() (string, error), facts attributionFacts, headRepo string, number int) (issueKey *string, source string, messages []string, complete bool, err error) {
	issueKey, source, err = headAttribution(ctx, pool, facts, headRepo)
	if err != nil || issueKey != nil || !headInOwnRepository(facts.Repo, headRepo) {
		return issueKey, source, nil, true, err
	}
	owner, repo, err := splitRepo(facts.Repo)
	if err != nil {
		return nil, string(sourceNone), nil, false, err
	}
	read, _, readErr := fetchCommitMessagesWithToken(ctx, client, token, owner, repo, number)
	if readErr != nil {
		if limited, ok := githubapp.AsRateLimit(readErr); ok {
			return nil, string(sourceNone), nil, false, limited
		}
		slog.Debug("dispatch delivery: a pull request head is attributed without its commit messages",
			"repo", facts.Repo, "number", number, "error", readErr)
		return issueKey, source, nil, false, nil
	}
	facts.CommitMessages = read
	issueKey, source, err = headAttribution(ctx, pool, facts, headRepo)
	return issueKey, source, read, true, err
}

// headRow is the row one pull request GitHub answered makes, with the issue the attribution
// picked: the one builder both GitHub readers below write through, so the two cannot drift. The
// list form of a pull request carries merged_at but not merged, so a merge is read from
// whichever of the two GitHub sent.
func headRow(repoFull string, payload pullRequestPayload, issueKey *string, source string, checkedAt *time.Time) PullRequestHead {
	return PullRequestHead{
		Repo: repoFull, Number: payload.Number, HeadSHA: payload.Head.SHA, HeadRef: payload.Head.Ref,
		BaseRef: payload.Base.Ref, State: PullRequestHeadState(payload.State, payload.Merged || payload.MergedAt != nil),
		IssueKey: issueKey, AttributionSource: source, Title: payload.Title, URL: payload.HTMLURL,
		EventAt: payload.UpdatedAt, CheckedAt: checkedAt,
	}
}

// headRepositoryOf is the repository a pull request's head branch lives in, as GitHub's answer
// names it, and "" when it names none (a deleted fork).
func headRepositoryOf(payload pullRequestPayload) string {
	if payload.Head.Repo == nil {
		return ""
	}
	return payload.Head.Repo.FullName
}

// RefreshPullRequestHead reads one pull request through the App and writes its head row: the
// reconcile's per-row step. A pull request GitHub cannot find, and a repository the App cannot
// read (no installation, or a repository or resource its token is refused), leave the row as it
// is, stamped so the batch moves on and does not warn about the same row every pass; a rate
// limit comes back as the *githubapp.RateLimitError, which stops the batch. The row is stamped
// even when the commit messages could not be read: this pass is itself the retry, so the next
// one reads it again in PullRequestHeadStaleAfter rather than holding the front of every batch.
func RefreshPullRequestHead(ctx context.Context, pool *store.Pool, client *githubapp.Client, repoFull string, number int, now time.Time) error {
	owner, repo, err := splitRepo(repoFull)
	if err != nil {
		return err
	}
	token, err := client.RepositoryToken(ctx, owner, repo)
	if err != nil {
		if repositoryUnreadable(err) {
			return MarkPullRequestHeadChecked(ctx, pool, repoFull, number, now)
		}
		return fmt.Errorf("mint installation token for %s PR #%d: %w", repoFull, number, err)
	}
	payload, err := fetchPullRequestPayload(ctx, client, token, owner, repo, number)
	if err != nil {
		if errors.Is(err, ErrPullRequestNotFound) || repositoryUnreadable(err) {
			return MarkPullRequestHeadChecked(ctx, pool, repoFull, number, now)
		}
		return err
	}
	issueKey, source, _, _, err := attributeHead(ctx, pool, client,
		func() (string, error) { return token, nil },
		attributionFacts{
			Repo: repoFull, URL: payload.HTMLURL, Title: payload.Title, Body: payload.Body,
			HeadRef: payload.Head.Ref,
		}, headRepositoryOf(payload), number)
	if err != nil {
		return err
	}
	return UpsertPullRequestHead(ctx, pool, headRow(repoFull, payload, issueKey, source, &now))
}

// repositoryUnreadable reports whether err is the App saying it cannot read a repository: it is
// installed on none that covers it (githubapp.ErrNoInstallation), or its token is refused the
// repository or the resource (githubapp.ErrRepositoryUnreadable). Neither is this row's own
// problem and neither mends itself within a pass, so the row is stamped read rather than warned
// about and left at the front of every batch.
func repositoryUnreadable(err error) bool {
	return errors.Is(err, githubapp.ErrNoInstallation) || errors.Is(err, githubapp.ErrRepositoryUnreadable)
}

// ReadThroughCommitPulls teaches Dispatch the pull requests GitHub names for one commit
// (`GET /repos/{o}/{r}/commits/{sha}/pulls`) and writes each one's head row, so a result
// recorded at the head of a pull request no event has reached Dispatch for is current at once.
// It answers how many rows it wrote. A repository the App cannot read writes nothing and is no
// error: the result is recorded either way, and its currency is unknown.
//
// Each row is attributed through all five sources, the commit messages included (attributeHead),
// exactly as the envelope writer attributes one. Attributing from fewer would delete an
// attribution this read-through cannot make: the write carries the pull request's own
// updated_at, so it lands on a row a `pull_request` event or the reconcile attributed by its
// commit messages and overwrites that issue with none — for a merged row, permanently: the
// reconcile's merged arm takes only rows no read of GitHub has reached, and this write stamps
// the row read, so that issue's results at the merged head would read `older` for
// good. A row whose commit messages could not be read is written unstamped instead, so the
// reconcile reads it again rather than it standing as this read-through's last word.
func ReadThroughCommitPulls(ctx context.Context, pool *store.Pool, client *githubapp.Client, repoFull, sha string, now time.Time) (int, error) {
	owner, repo, err := splitRepo(repoFull)
	if err != nil {
		return 0, err
	}
	token, err := client.RepositoryToken(ctx, owner, repo)
	if err != nil {
		return 0, fmt.Errorf("mint installation token for %s: %w", repoFull, err)
	}
	pulls, err := fetchCommitPullPayloads(ctx, client, token, owner, repo, sha)
	if err != nil {
		return 0, err
	}
	written := 0
	for _, pull := range pulls {
		issueKey, source, _, complete, err := attributeHead(ctx, pool, client,
			func() (string, error) { return token, nil },
			attributionFacts{
				Repo: repoFull, URL: pull.HTMLURL, Title: pull.Title, Body: pull.Body,
				HeadRef: pull.Head.Ref,
			}, headRepositoryOf(pull), pull.Number)
		if err != nil {
			return written, err
		}
		var checkedAt *time.Time
		if complete {
			checkedAt = &now
		}
		if err := UpsertPullRequestHead(ctx, pool, headRow(repoFull, pull, issueKey, source, checkedAt)); err != nil {
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
// attribution's five sources and outrank the fifth; the fifth, the commit messages, is read only
// when those four name nobody (attributeHead), and a merge that follows reuses what was read
// rather than paging the same commits again (completePullRequest). A read the App refuses
// attributes the row from the envelope alone; a rate limit is returned, as the reconcile's own
// refresh returns it, so the caller does not persist a weaker attribution and then stamp the row
// read.
//
// The envelope also carries the head's own repository (`head_repo`), which the attribution holds
// the currency to. An envelope that does not name it — one a publisher older than this field
// made — attributes nobody and leaves the row unstamped, so the reconcile's own read of GitHub,
// which always names it, attributes the row on its next pass: a head that cannot be shown to be
// this repository's is not taken for it, since that is what a fork's pull request would be.
func (in *Intake) recordPullRequestHead(ctx context.Context, payload map[string]string, number int) ([]string, error) {
	repoFull := payload["repo"]
	owner, repo, err := splitRepo(repoFull)
	if err != nil {
		return nil, err
	}
	headRepo := payload["head_repo"]
	issueKey, source, messages, complete, err := attributeHead(ctx, in.pool, in.github,
		func() (string, error) { return in.github.RepositoryToken(ctx, owner, repo) },
		attributionFacts{
			Repo: repoFull, URL: payload["url"], Title: payload["title"], Body: payload["body"],
			HeadRef: payload["head_ref"],
		}, headRepo, number)
	if err != nil {
		return nil, err
	}
	// The envelope is itself a fresh read of the head, so the row is stamped read and the
	// reconcile does not spend three GitHub calls re-reading what the event just delivered —
	// unless the commit messages were wanted and could not be had, or the envelope named no head
	// repository, which the next pass settles either way.
	var checkedAt *time.Time
	if complete && headRepo != "" {
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
