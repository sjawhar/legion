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
// picks for it (attribution.go): the first issue its ranked sources name that resolves to the
// repository the pull request is in, and only where the head branch is in that same repository,
// never a fork's (headAttribution). A child's pull request is attributed to the child, so it
// answers for the child's own checks alone and never for a check of an issue above it.
//
// Three writers keep it: the `pull_request` envelope (intake.go, every action), the read-through
// made for a commit that is no head Dispatch knows (ReadThroughCommitPulls) — by a recorded
// result, and by the read of an issue's results for a `before_merge` result's commit, at most
// once every CommitPullReadStaleAfter — and the reconcile's five-minute pass (reconcile.go),
// which takes every open row read longer than five minutes ago and every merged row credited to
// nobody that the rule this build judges by has not judged yet — one offer per rule version
// (AttributionRuleVersion, ListStalePullRequestHeads).

// PullRequestHead is one pull request's current head, as pull_request_heads stores it, and the
// one shape every writer of that row fills in (UpsertPullRequestHead).
type PullRequestHead struct {
	Repo              string
	Number            int
	HeadSHA           string
	HeadRef           string
	BaseRef           string
	State             string
	IssueKey          *string
	AttributionSource string
	// AttributionComplete is whether IssueKey and AttributionSource come from a read of every
	// source this pull request has, which is what decides whether the write may replace the
	// attribution the row already holds. Only attributeHead can answer it, so the zero value is
	// the careful one: a writer that does not set it deletes nobody's issue.
	AttributionComplete bool
	Title               string
	URL                 string
	EventAt             *time.Time
	CheckedAt           *time.Time
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
//
// Only a write that read every attribution source may replace the row's issue
// (AttributionComplete); an incomplete one leaves issue_key and attribution_source exactly as
// they stand. Two writes are incomplete: one whose payload named no head repository, which
// cannot tell a branch of this repository from a fork's and so attributes nobody, and one whose
// commit-message read failed with anything but a rate limit (attributeHead). Either would
// otherwise delete an attribution an earlier, fuller read made — on a merged row for good,
// since the reconcile's merged arm takes a merged row only until the current rule has judged it
// (ListStalePullRequestHeads) and a complete write judges it, so that issue's results at its
// own merged head would read `older` for ever. An incomplete write never carries an issue of
// its own, so keeping the stored pair costs it nothing. A write that did read every source and
// found nobody is complete, and clears: GitHub's answer is the whole truth about the row.
//
// A write carrying no read time leaves the row's checked_at as it was: nulling it would hand a
// row the reconcile has just read back to the next batch (ListStalePullRequestHeads orders the
// never-read first), which is why an incomplete write is held off the attribution rather than
// sent round again. attribution_rule is kept by the same test, and it is the other half of the
// merged arm's bound: a write that did not read GitHub leaves the stored rule alone, so a
// merged row an incomplete envelope wrote is still offered to the current rule, while a refresh
// that did read GitHub ends that offer whether or not it could attribute the row. The rule
// written is AttributionRuleVersion, bound here and carried in no field of PullRequestHead, so
// every write stamps the rule this build judges by and no writer can leave a row claiming a
// rule that never judged it. An insert writes it outright: a row this build has just made was
// judged by this build's rule. A test that needs a row of an older rule sets attribution_rule
// itself, with one update.
func UpsertPullRequestHead(ctx context.Context, pool *store.Pool, head PullRequestHead) error {
	_, err := pool.Exec(ctx, `
		insert into pull_request_heads (
			repo, number, head_sha, head_ref, base_ref, state, issue_key, attribution_source,
			title, url, event_at, checked_at, attribution_rule
		)
		values ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12, $14)
		on conflict (repo, number) do update set
			head_sha = excluded.head_sha,
			head_ref = excluded.head_ref,
			base_ref = excluded.base_ref,
			state = excluded.state,
			issue_key = case when $13::boolean
				then excluded.issue_key else pull_request_heads.issue_key end,
			attribution_source = case when $13::boolean
				then excluded.attribution_source else pull_request_heads.attribution_source end,
			title = excluded.title,
			url = excluded.url,
			event_at = coalesce(excluded.event_at, pull_request_heads.event_at),
			checked_at = coalesce(excluded.checked_at, pull_request_heads.checked_at),
			attribution_rule = case when excluded.checked_at is null
				then pull_request_heads.attribution_rule else excluded.attribution_rule end
		where excluded.event_at is null
		   or pull_request_heads.event_at is null
		   or excluded.event_at >= pull_request_heads.event_at
	`,
		head.Repo, head.Number, head.HeadSHA, head.HeadRef, head.BaseRef, head.State,
		head.IssueKey, head.AttributionSource, head.Title, head.URL, head.EventAt, head.CheckedAt,
		head.AttributionComplete, AttributionRuleVersion,
	)
	if err != nil {
		return fmt.Errorf("store the head of %s#%d: %w", head.Repo, head.Number, err)
	}
	return nil
}

// upsertHeadUnderCanonicalName writes head, which every caller has already keyed by the
// canonical name of its repository (refs.CanonicalRepo), and drops the row the same pull
// request is stored under as storedUnder when that is another name. A row keyed under a
// repository's old name can be read at all only under the canonical one — the App answers 404
// for the old name's installation, so nothing can refresh it and no issue is ever held to it
// (attributionHeld) — so one pull request keeps one row and one attribution.
//
// Where both rows exist, because the pull request arrived under both names, the canonical row
// wins and the old row goes: the write above lands on the canonical row and its own event-time
// rule decides which facts stand there (UpsertPullRequestHead keeps the newest event either has
// seen), and the old row's facts are dropped with it rather than merged, since they are the same
// pull request read at some earlier moment.
//
// The write comes before the delete, so a failure between them leaves two rows rather than
// none: the next pass is handed the old row's key again and re-keys it again, while losing the
// row would leave the pull request unknown until another event arrived.
func upsertHeadUnderCanonicalName(ctx context.Context, pool *store.Pool, head PullRequestHead, storedUnder string) error {
	if err := UpsertPullRequestHead(ctx, pool, head); err != nil {
		return err
	}
	if strings.EqualFold(storedUnder, head.Repo) {
		return nil
	}
	if _, err := pool.Exec(ctx, `
		delete from pull_request_heads where repo = $1 and number = $2
	`, storedUnder, head.Number); err != nil {
		return fmt.Errorf("drop the head of %s#%d, now %s#%d: %w", storedUnder, head.Number, head.Repo, head.Number, err)
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

// HeadStoredAt reports whether sha is the head of an open or merged pull request stored for
// repoFull, whatever issue it is attributed to: whether Dispatch holds a pull request at that
// commit that its own passes keep reading. It is a weaker question than HeadAttributedTo's and
// it is asked for another purpose — the results read asks it before it reads GitHub for a commit,
// since a commit such a row already heads is one the reconcile re-reads and a read from there
// repairs nothing (api/results_currency.go, currencyReader.readThroughHead).
//
// A closed-unmerged row is counted for nothing, as HeadAttributedTo counts it for nothing: it
// makes no result current, the reconcile keeps no such row in its passes
// (ListStalePullRequestHeads takes the open and the merged), and a pull request closed at a
// commit does not stop another being opened from the same head. Counting one would leave every
// result at that commit `older` for good.
//
// repoFull is the canonical name of the repository (refs.CanonicalRepo), compared without case,
// as every other comparison of a repository here is (refs.CanonicalRepoSQL): a row written from
// a `pull_request` envelope carries GitHub's own spelling of the repository (`Acme/Widgets`),
// while the results read is passed the lowercased spelling the repository-to-project mapping
// stores (api's canonicalRepo), so an exact comparison would answer "no row" for a repository
// whose owner writes their name with capitals. The read is one index lookup on
// pull_request_heads_repo_head (migration 20261105112616), which is built on
// `(lower(repo), head_sha)` for that reason: pull_request_heads_issue_head is partial on
// `issue_key is not null` and keyed on the issue, so it serves no lookup by repository and head
// commit.
func HeadStoredAt(ctx context.Context, pool *store.Pool, repoFull, sha string) (bool, error) {
	var found bool
	if err := pool.QueryRow(ctx, `
		select exists (
			select 1 from pull_request_heads
			where lower(repo) = lower($1) and head_sha = $2 and state in ('open', 'merged')
		)
	`, repoFull, sha).Scan(&found); err != nil {
		return false, fmt.Errorf("read the stored pull request heads of %s at %s: %w", repoFull, sha, err)
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
// never read first), and every merged row credited to nobody that the rule this build judges by
// has not judged yet (AttributionRuleVersion).
//
// A merged row is revisited at all because nothing else ever revisits one. An open row comes
// round every five minutes; a merged row is written once more at most, by an envelope or a
// read-through, and a recorded result reads through only for a commit no head Dispatch knows.
// So a merged row left credited to nobody stays that way, and every `before_merge` result at
// that head reads `older` for ever: a merge delivered by a `pull_request` envelope naming no
// head repository is one such row (recordPullRequestHead attributes nobody, since it cannot
// tell this repository's branch from a fork's), and so is every pull request merged under an
// older attribution rule that credited nobody — a title naming another repository's issue first
// was the whole of INFRA-1240.
//
// Only a row credited to nobody, because a row an older rule did credit keeps that issue under
// this one: a candidate is passed over only where it fails the repository test
// (attributionHeld), so an issue held to the pull request's own repository still wins, and
// re-reading the row would buy nothing for a GitHub call.
//
// The bound is one offer per rule version, and the read ends the offer whatever it concluded.
// Only a reader of GitHub stamps attribution_rule and checked_at — the reconcile's own refresh
// (RefreshPullRequestHead), MarkPullRequestHeadChecked for a row it could not read at all, the
// read-through, and an envelope only when its payload was a complete read (UpsertPullRequestHead
// keeps both where the write carries no read time). So a merged row is taken once per rule: the
// refresh stamps it at AttributionRuleVersion whether or not GitHub's answer attributed it, and
// it comes back only when a later build raises that constant, which is how a change to the
// attribution reaches the merged rows already stored.
func ListStalePullRequestHeads(ctx context.Context, pool *store.Pool, limit int, now time.Time) ([]PullRequestHead, error) {
	rows, err := pool.Query(ctx, `
		select repo, number from pull_request_heads
		where (state = 'open' and (checked_at is null or checked_at < $1))
		   or (state = 'merged' and issue_key is null
		       and (checked_at is null or attribution_rule < $3))
		order by checked_at nulls first, repo, number
		limit $2
	`, now.Add(-PullRequestHeadStaleAfter), limit, AttributionRuleVersion)
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
// complete — the repository unreadable, the pull request gone — so one unreadable pull request
// does not hold the batch every pass. It stamps attribution_rule with it, from the constant and
// not from a parameter, since the row the caller could not read is one the current rule has now
// been offered: that is the same bound the refresh carries, and without it a merged row credited
// to nobody whose pull request is gone would come back to the merged arm every pass for ever.
func MarkPullRequestHeadChecked(ctx context.Context, pool *store.Pool, repoFull string, number int, now time.Time) error {
	_, err := pool.Exec(ctx, `
		update pull_request_heads set checked_at = $3, attribution_rule = $4
		where repo = $1 and number = $2
	`, repoFull, number, now, AttributionRuleVersion)
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

// headAttributionResult is one attribution attempt's answer: the issue it picked and the source
// that named it ("none" when none did), the commit messages it read (nil when it read none,
// which a merge that follows reuses rather than paging them again), whether the head is a
// branch of the pull request's own repository, and whether the attempt read every source there
// was. complete is the write's leave to replace a stored attribution: an attempt that could not
// read a source it wanted — a payload that named no head repository, a commit read GitHub
// refused — may add an attribution but never deletes one (UpsertPullRequestHead).
type headAttributionResult struct {
	issueKey *string
	source   string
	messages []string
	ownHead  bool
	complete bool
}

// headAttribution resolves the issue a pull request belongs to from what GitHub said about it,
// through the same ranked candidates every other attribution reads
// (resolveAttributionCandidates): the first of them whose issue resolves to the repository the
// pull request is in. It holds the head to that repository too.
//
// Both holdings are what keep the currency mark honest. A pull request's title, body and branch
// are text any account that can open one writes, this row is written for any author and any
// repository the App watches, and an attributed row is exactly what makes a `before_merge`
// result read current (HeadAttributedTo), which is the reviewer's approval condition. So a
// candidate whose issue resolves to another repository, or to none, is passed over for the next
// one and a pull request no candidate of which resolves here attributes nobody; and neither
// does a pull request whose head is not in this repository, since on a public repository anyone
// may open one from a fork at any commit of their choosing — including an earlier one that
// already carries a recorded pass — and title it after an issue, which would make that older
// result current again. Either way the row is still written, since the reconcile and the head
// itself are wanted either way, but with no issue and the source "none".
//
// The head is tested before any candidate is taken, since a fork's head attributes nobody
// whatever the title names. This is the one place it is tested (headInOwnRepository), and the
// answer rides back as ownHead, which is what tells attributeHead whether reading the commit
// messages could change anything, and as complete, since an answer naming no head repository at
// all left a source unread and so may attribute nobody but delete nobody either.
//
// A log line is written per call, not per attribution: the fork's head names the first
// candidate it refused, and a pull request that passed candidates over names them and the issue
// it settled on, if any. attributeHead calls this a second time, after reading the commit
// messages, whenever the first call held no candidate, so on that path both calls log and the
// second line names the candidates the commit messages added.
func headAttribution(ctx context.Context, pool *store.Pool, facts attributionFacts, headRepo string) (headAttributionResult, error) {
	attributed := headAttributionResult{
		source:   string(sourceNone),
		ownHead:  headInOwnRepository(facts.Repo, headRepo),
		complete: headRepo != "",
	}
	candidates, err := resolveAttributionCandidates(ctx, pool, facts.Repo, facts.URL, attributionInputsFrom(facts))
	if err != nil || len(candidates) == 0 {
		return attributed, err
	}
	if !attributed.ownHead {
		slog.Info("dispatch delivery: a pull request whose head is in another repository names an issue",
			"repo", facts.Repo, "head_repo", headRepo, "issue", candidates[0].key, "source", string(candidates[0].source))
		return attributed, nil
	}
	// passedOver is every candidate ranked above the one taken, and every candidate when none
	// was held: the keys the log line below names.
	passedOver := candidates
	if i := firstHeldCandidate(candidates); i >= 0 {
		attributed.issueKey, attributed.source = &candidates[i].key, string(candidates[i].source)
		passedOver = candidates[:i]
	}
	if len(passedOver) > 0 {
		keys := make([]string, 0, len(passedOver))
		for _, candidate := range passedOver {
			keys = append(keys, candidate.key)
		}
		settled := ""
		if attributed.issueKey != nil {
			settled = *attributed.issueKey
		}
		slog.Info("dispatch delivery: a pull request names an issue of another repository",
			"repo", facts.Repo, "passed_over", strings.Join(keys, " "), "issue", settled, "source", attributed.source)
	}
	return attributed, nil
}

// attributeHead is headAttribution plus the one source the caller's facts cannot carry: the
// commit messages, rank 5, which are read — one GitHub call, or one per page of 100 commits —
// only when no candidate the title, body, branch and links name is held to this repository, as
// every source after the first is read only when the earlier ones name nobody the pull request
// may keep (attribution.go). A pull request whose head is not this repository's is not read at
// all: it attributes nobody whatever its commits say, which is headAttribution's own ownHead
// and not a second test here.
//
// A read the App refuses leaves the attribution weaker than it could be, so the answer is not
// complete: the caller's write adds no issue, deletes none (UpsertPullRequestHead), and leaves
// the row unstamped where the caller can, for the reconcile to read again. A rate limit is
// returned rather than swallowed, so no caller persists a weaker attribution and then stamps
// the row read.
func attributeHead(ctx context.Context, pool *store.Pool, client *githubapp.Client, token func() (string, error), facts attributionFacts, headRepo string, number int) (headAttributionResult, error) {
	attributed, err := headAttribution(ctx, pool, facts, headRepo)
	if err != nil || attributed.issueKey != nil || !attributed.ownHead {
		return attributed, err
	}
	owner, repo, err := splitRepo(facts.Repo)
	if err != nil {
		return headAttributionResult{source: string(sourceNone)}, err
	}
	read, _, readErr := fetchCommitMessagesWithToken(ctx, client, token, owner, repo, number)
	if readErr != nil {
		if limited, ok := githubapp.AsRateLimit(readErr); ok {
			return headAttributionResult{source: string(sourceNone)}, limited
		}
		slog.Debug("dispatch delivery: a pull request head is attributed without its commit messages",
			"repo", facts.Repo, "number", number, "error", readErr)
		attributed.complete = false
		return attributed, nil
	}
	facts.CommitMessages = read
	attributed, err = headAttribution(ctx, pool, facts, headRepo)
	attributed.messages = read
	return attributed, err
}

// headRow is the row one pull request GitHub answered makes, with the attribution the read
// reached: the one builder both GitHub readers below write through, so the two cannot drift.
// The rule that judged it is the write's own (UpsertPullRequestHead binds
// AttributionRuleVersion). The list form of a pull request carries merged_at but not merged, so
// a merge is read from whichever of the two GitHub sent.
func headRow(repoFull string, payload pullRequestPayload, attributed headAttributionResult, checkedAt *time.Time) PullRequestHead {
	return PullRequestHead{
		Repo: repoFull, Number: payload.Number, HeadSHA: payload.Head.SHA, HeadRef: payload.Head.Ref,
		BaseRef: payload.Base.Ref, State: PullRequestHeadState(payload.State, payload.Merged || payload.MergedAt != nil),
		IssueKey: attributed.issueKey, AttributionSource: attributed.source,
		AttributionComplete: attributed.complete, Title: payload.Title, URL: payload.HTMLURL,
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

// attributePayload attributes one pull request GitHub itself answered: the four facts its
// payload carries, the head repository it names (headRepositoryOf) and, when no candidate those
// four name is held to the repository the pull request is in, its commit messages, read with the
// caller's installation token. The one place the two GitHub readers below build an attribution
// from a payload, so a sixth source, or a sixth fact a payload carries, is added in one.
func attributePayload(ctx context.Context, pool *store.Pool, client *githubapp.Client, token, repoFull string, payload pullRequestPayload) (headAttributionResult, error) {
	return attributeHead(ctx, pool, client, func() (string, error) { return token, nil },
		attributionFacts{
			Repo: repoFull, URL: payload.HTMLURL, Title: payload.Title, Body: payload.Body,
			HeadRef: payload.Head.Ref,
		}, headRepositoryOf(payload), payload.Number)
}

// RefreshPullRequestHead reads one pull request through the App and writes its head row: the
// reconcile's per-row step. A pull request GitHub cannot find, and a repository the App cannot
// read (no installation, or a repository or resource its token is refused), leave the row as it
// is, stamped so the batch moves on and does not warn about the same row every pass; a rate
// limit comes back as the *githubapp.RateLimitError, which stops the batch. The row is stamped
// even when the commit messages could not be read: this pass is itself the retry, so the next
// one reads it again in PullRequestHeadStaleAfter rather than holding the front of every batch,
// and that write, being incomplete, leaves any attribution the row already carries standing.
//
// repoFull is the key ListStalePullRequestHeads read off the row, which may be a name the
// repository has been renamed away from, and every step here but the stamp works on the
// canonical name instead (refs.CanonicalRepo): the token is minted for it, since GitHub answers
// 404 for the installation of a name no longer in use; the attribution holds the pull request's
// issue to it; and the row is written under it, with the row under the old key dropped in the
// same step (upsertHeadUnderCanonicalName). The stamp stays on the key the batch handed over,
// since that is the row the batch must stop offering: a canonical repository the App cannot
// read teaches this row nothing under either name.
func RefreshPullRequestHead(ctx context.Context, pool *store.Pool, client *githubapp.Client, repoFull string, number int, now time.Time) error {
	canonical, err := refs.CanonicalRepo(ctx, pool, repoFull)
	if err != nil {
		return err
	}
	owner, repo, err := splitRepo(canonical)
	if err != nil {
		return err
	}
	token, err := client.RepositoryToken(ctx, owner, repo)
	if err != nil {
		if repositoryUnreadable(err) {
			return MarkPullRequestHeadChecked(ctx, pool, repoFull, number, now)
		}
		return fmt.Errorf("mint installation token for %s PR #%d: %w", canonical, number, err)
	}
	payload, err := fetchPullRequestPayload(ctx, client, token, owner, repo, number)
	if err != nil {
		if errors.Is(err, ErrPullRequestNotFound) || repositoryUnreadable(err) {
			return MarkPullRequestHeadChecked(ctx, pool, repoFull, number, now)
		}
		return err
	}
	attributed, err := attributePayload(ctx, pool, client, token, canonical, payload)
	if err != nil {
		return err
	}
	return upsertHeadUnderCanonicalName(ctx, pool, headRow(canonical, payload, attributed, &now), repoFull)
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
// It answers how many rows it wrote and whether any pull request it saw has sha as its head
// (atHead). A repository the App cannot read writes nothing and comes back as the App's own
// error, which each caller logs at Info and carries on from (api/results.go's
// readThroughPullRequests, api/results_currency.go's readThroughHead): the currency of that
// result stands as its stored rows leave it.
//
// atHead is the difference between the two answers GitHub gives about a commit no pull request
// of Dispatch's heads. GitHub naming nothing leaves the question wide open — the pull request
// may open at that commit a moment later, which is the whole of what this read repairs — while
// GitHub naming a pull request whose head is another commit has answered it for now: the commit
// is behind that pull request, and only a pull request opened later can put a head back on it.
// So the read-time caller holds the pair off for CommitPullSettledFor on that answer rather
// than asking again every CommitPullReadStaleAfter (delivery.CommitPullReads.Settle). The rows
// written say nothing of it on their own: the row of a pull request whose head has moved on is
// written at the head it has now, not at the commit that was asked about.
//
// Each row is attributed through all five sources, the commit messages included (attributeHead),
// exactly as the envelope writer attributes one. Attributing from fewer and calling it complete
// would delete an attribution this read-through cannot make: the write carries the pull
// request's own updated_at, so it lands on a row a `pull_request` event or the reconcile
// attributed by its commit messages and would overwrite that issue with none — for a merged row
// permanently, since the reconcile's merged arm takes a merged row only until the current rule
// has judged it, and a complete write judges it.
// A row whose commit messages could not be read is written incomplete and unstamped instead: it
// keeps the attribution the row holds (UpsertPullRequestHead), and the reconcile reads it again
// rather than this read-through standing as the last word.
//
// repoFull comes from outside — the repository the check's issue resolves to — so it is
// canonicalised before anything is asked of GitHub or written (refs.CanonicalRepo), and no row
// is ever written under a name a repository was renamed away from. The read-time caller has
// canonicalised that name already (api/results_currency.go, currencyReader.repositoryOf) and the
// rule reaches the canonical name in one step, so this read changes nothing there; it is here
// because this function's key is its argument's, not its caller's, and the record-time caller
// hands over the name refs.IssueRepository answered.
func ReadThroughCommitPulls(
	ctx context.Context, pool *store.Pool, client *githubapp.Client, repoFull, sha string, now time.Time,
) (written int, atHead bool, err error) {
	canonical, err := refs.CanonicalRepo(ctx, pool, repoFull)
	if err != nil {
		return 0, false, err
	}
	owner, repo, err := splitRepo(canonical)
	if err != nil {
		return 0, false, err
	}
	token, err := client.RepositoryToken(ctx, owner, repo)
	if err != nil {
		return 0, false, fmt.Errorf("mint installation token for %s: %w", canonical, err)
	}
	pulls, err := fetchCommitPullPayloads(ctx, client, token, owner, repo, sha)
	if err != nil {
		return 0, false, err
	}
	for _, pull := range pulls {
		if pull.Head.SHA == sha {
			atHead = true
		}
		attributed, err := attributePayload(ctx, pool, client, token, canonical, pull)
		if err != nil {
			return written, atHead, err
		}
		var checkedAt *time.Time
		if attributed.complete {
			checkedAt = &now
		}
		if err := upsertHeadUnderCanonicalName(ctx, pool, headRow(canonical, pull, attributed, checkedAt), repoFull); err != nil {
			return written, atHead, err
		}
		written++
	}
	return written, atHead, nil
}

// recordPullRequestHead writes the head row one `pull_request` envelope reports, whatever its
// action, author or repository, and answers the commit messages it read, nil when it read none.
// It runs before the merge-only guard in handlePullRequestEnvelope, so an `opened`,
// `synchronize`, `reopened` or `edited` event — which the delivery timeline ignores — still
// moves the head a check result is measured against.
//
// The envelope carries the title, body, branch and head commit, which are four of the
// attribution's five sources and outrank the fifth; the fifth, the commit messages, is read only
// when no candidate those four name is held to the repository the pull request is in
// (attributeHead), and a merge that follows reuses what was read rather than paging the same
// commits again (completePullRequest). A read the App refuses attributes the row from the
// envelope alone and makes the write an incomplete one, which deletes no attribution the row
// already carries and leaves it unstamped; a rate limit is returned, as the reconcile's own
// refresh returns it, so the caller does not persist a weaker attribution and then stamp the
// row read.
//
// The envelope also carries the head's own repository (`head_repo`), which the attribution holds
// the currency to. An envelope that does not name it — one a publisher older than this field
// made — attributes nobody, since a head that cannot be shown to be this repository's is a
// fork's as far as Dispatch knows. That write is incomplete too: it leaves the row unstamped, so
// the reconcile's own read of GitHub, which always names the head repository, attributes the row
// on its next pass, and it leaves any attribution an earlier, fuller read stored exactly where
// it is rather than clearing it on the way through — on a merge, which nothing but that arm of
// the reconcile ever revisits, clearing it would be for good.
//
// The envelope's own `repo` comes from outside — the topic a publisher built — so it is
// canonicalised before anything is read or written (refs.CanonicalRepo), and no row is ever
// written under a name a repository was renamed away from. It is also what the head is held to:
// an envelope keyed by an old name carries a `head_repo` GitHub spells under the new one, and
// comparing those two would make every such pull request a fork's (headInOwnRepository).
func (in *Intake) recordPullRequestHead(ctx context.Context, payload map[string]string, number int) ([]string, error) {
	given := payload["repo"]
	repoFull, err := refs.CanonicalRepo(ctx, in.pool, given)
	if err != nil {
		return nil, err
	}
	owner, repo, err := splitRepo(repoFull)
	if err != nil {
		return nil, err
	}
	attributed, err := attributeHead(ctx, in.pool, in.github,
		func() (string, error) { return in.github.RepositoryToken(ctx, owner, repo) },
		attributionFacts{
			Repo: repoFull, URL: payload["url"], Title: payload["title"], Body: payload["body"],
			HeadRef: payload["head_ref"],
		}, payload["head_repo"], number)
	if err != nil {
		return nil, err
	}
	// The envelope is itself a fresh read of the head, so the row is stamped read and the
	// reconcile does not spend three GitHub calls re-reading what the event just delivered —
	// unless the read was incomplete, because the commit messages were wanted and could not be
	// had or the envelope named no head repository, which the next pass settles either way.
	var checkedAt *time.Time
	if attributed.complete {
		now := time.Now()
		checkedAt = &now
	}
	return attributed.messages, upsertHeadUnderCanonicalName(ctx, in.pool, PullRequestHead{
		Repo: repoFull, Number: number, HeadSHA: payload["head_sha"], HeadRef: payload["head_ref"],
		BaseRef: payload["base_ref"], State: PullRequestHeadState(payload["action"], payload["merged"] == "true"),
		IssueKey: attributed.issueKey, AttributionSource: attributed.source,
		AttributionComplete: attributed.complete, Title: payload["title"], URL: payload["url"],
		EventAt: envelopeTime(payload["updated_at"]), CheckedAt: checkedAt,
	}, given)
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
