// Package cireconcile fills Envoy's CI records in from GitHub's own answers.
//
// The listener records a check run or a check suite from the webhook GitHub sends it, and the
// Legion daemon reads the settlement those records produce (`ci-ok`, `pr-title`). A webhook that
// reaches no record leaves that head short of a check, and a head short of a check never settles:
// the daemon waits for an answer that never comes. GitHub's REST API holds the same objects the
// webhook carries, so every Interval a pass reads each open pull request's head and records every
// completed check run and completed check suite the listener's record of that head lacks.
//
// An object read here maps through contracts.GithubCIObservation, the mapping the webhook's own
// body goes through, so a pass over a run the listener already recorded changes nothing: the
// observation is the one the webhook made, and cistore's write applies an unchanged observation as
// no change. The pass writes with a cistore writer, which binds the listener's bucket and creates
// none (cistore.OpenWriter), so one record per head is shared by both processes.
//
// GitHub budgets the App, not a process, so the pass reads as GitHub asks a poller to. Every read
// is conditional on the ETag the pass kept from its last read of that same page, which costs the
// budget nothing when GitHub has not changed the page; requests are paced (Reconciler.Pace); and
// the shared rate limits every Dispatch process obeys (githubapp/ratelimit) are read before the
// pass starts, before each repository and before each head, with a limit in force ending the pass
// where it stands.
//
// The pass is a read of GitHub and a write of NATS; it holds no lease of its own. Lease names the
// one a caller's loop runs it under, so one process's pass is what the App's budget pays for in a
// period however many processes run.
package cireconcile

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strconv"
	"strings"
	"time"

	"github.com/sjawhar/envoy/internal/contracts"
	"github.com/sjawhar/envoy/internal/dispatch/githubapp"
	"github.com/sjawhar/envoy/internal/dispatch/githubapp/ratelimit"
	"github.com/sjawhar/envoy/internal/dispatch/store"
)

const (
	// Interval is how often the running pass reads GitHub.
	Interval = 5 * time.Minute

	// Lease names the lease one pass runs under. It is not what makes the pass safe to run twice
	// -- every write it makes is a compare-and-swap that applies an observation GitHub already
	// holds, so two passes at once write one record -- it is the GitHub budget: each pass reads
	// every open pull request of every mapped repository, and N processes passing is N times those
	// reads for one result.
	Lease = "ci-reconcile"

	// RequestPace is the pause a running pass leaves between the requests it sends GitHub: two a
	// second, the pace the webhook redelivery sweep's own requests keep. A caller sets it on Pace;
	// a test sets Pace to zero and waits for nothing.
	RequestPace = 500 * time.Millisecond

	// maxPages bounds one listing's pages, so a pass can never read on for ever: an answer GitHub
	// keeps reporting full past this many pages is read as a failed listing, which the next pass
	// reads again from its first page. A hundred pages is ten thousand objects of one listing,
	// far past any head or any repository's open pull requests.
	maxPages = 100
)

// Writer is the CI record store a pass writes through: the listener's bucket bound by
// cistore.OpenWriter, which creates nothing. It is an interface rather than the store itself so
// that a caller may put its own writer in the pass's way, as every other collaborator of a
// Reconciler is its caller's to choose; *cistore.Store satisfies it.
type Writer interface {
	Record(context.Context, contracts.CIObservation) error
	RecordSuite(context.Context, contracts.CIObservation) error
	HasRecord(owner, repo, number, sha string) (bool, error)
}

// Reconciler reads GitHub for the CI observations the listener's records lack and writes them.
// Its collaborators are fields its caller sets, as redeliver.Sweeper's are, so a test passes its
// own. One Reconciler runs one pass at a time: it remembers between passes the ETags GitHub
// answered with and the pages they belong to, and nothing guards that memory.
type Reconciler struct {
	// Pool reads repo_projects, the repositories Dispatch is mapped to.
	Pool *store.Pool
	// GitHub reads the repositories, their open pull requests and each head's check objects, as
	// the App the listener's webhook is of (R5: no credential of its own).
	GitHub *githubapp.Client
	// Limits is the GitHub rate limits every Dispatch process shares (github_rate_limits). Nil
	// shares none: the pass then stops only at GitHub's own answers, which is what a test that is
	// not about limits wants.
	Limits *ratelimit.Store
	// Open binds the CI record writer (cistore.OpenWriter), as redeliver.Sweeper.Open binds its
	// bucket: a pass calls it until it has once succeeded, and every later pass writes through
	// that writer. It fails while NATS is not connected, and while the listener has not made the
	// bucket; a pass it fails for reads GitHub nothing and leaves the binding to a later one.
	Open func() (Writer, error)
	// ReviewerAppID is the review App's id, Dispatch's DISPATCH_REVIEWER_APP_ID. A verdict check
	// (`tester`, `architect`) is recorded only when that App created it
	// (contracts.ReviewerVerdictDecision); unset, no verdict check is recorded at all.
	ReviewerAppID string
	// Logger takes the one INFO line each pass writes and the WARN lines of what it skipped. It is
	// required.
	Logger *slog.Logger
	// Pace is the pause left between requests to GitHub. Zero waits for nothing.
	Pace time.Duration

	// writer is what Open bound, nil until a pass has bound it.
	writer Writer
	// rows is what the last pass learned of each repo_projects row, keyed by the row's own text:
	// the ETag of GitHub's answer about it, and the name and id that answer gave.
	rows map[string]rowState
	// pulls is each repository's open pull request pages as the last pass read them, keyed by the
	// id GitHub gave the repository, so two rows naming one repository share one listing.
	pulls map[int64][]pageOf[githubapp.PullRequestHead]
	// heads is what the last pass kept of each head it read (headKey).
	heads map[string]*headState
}

// rowState is what a pass learned of one repo_projects row from GitHub.
type rowState struct {
	// etag is the ETag of GET /repos/{row}, which the next pass reads conditionally on.
	etag string
	// fullName and id are what that answer gave: the name GitHub itself spells the repository
	// with, and the id two names of one repository share.
	fullName string
	id       int64
}

// headState is what a pass kept of one head: the pages of its two listings, each with the ETag
// the next pass reads it conditionally on.
type headState struct {
	runs   []pageOf[object]
	suites []pageOf[object]
	// observed says the last full answer for this head held at least one observation the rules say
	// to record. Such a head is worth reading again in full when its record has gone (the bucket's
	// TTL, or a delete), so the next pass asks HasRecord before it trusts these ETags. A head whose
	// answer held nothing to record has no record to lose, and keeps its ETags whatever the bucket
	// holds.
	observed bool
}

// object is one GitHub check run or check suite as its listing answers it, the shape
// contracts.GithubCIObservation maps.
type object = map[string]any

// stop is the rate limit that ended a pass: the scope it was found or answered under, and when
// GitHub will take a request under it again.
type stop struct {
	scope string
	until time.Time
}

// counts is what one pass did, as its one log line reports it.
type counts struct {
	// requests counts every request the pass sent GitHub, which is what the pace is left between.
	requests int
	// listPages counts the open pull request pages read, heads the heads the pass read, and
	// headRequests the requests those heads' two listings sent.
	listPages    int
	heads        int
	headRequests int
	// notModified counts the heads every page of whose two listings answered 304.
	notModified int
	// records counts the observations written to the CI store. The store writes nothing for an
	// observation its record already holds, which is what a pass over an unchanged head does.
	records int
	// mappings counts the repo_projects rows the pass skipped, and tooLarge the heads whose answer
	// was past what githubapp reads.
	mappings int
	tooLarge int
}

// Pass runs one reconcile pass within ctx and logs exactly one INFO line for it: the pass line
// below, or, for a pass that could not bind the CI record writer, the line saying so.
//
// The pass's loop belongs to its caller, which runs it under Lease every Interval.
func (r *Reconciler) Pass(ctx context.Context) {
	started := time.Now()
	var c counts
	// A limit in force ends the pass before it binds anything or reads GitHub: the App's budget is
	// shared, and a process that waits out a limit another met is why it is recorded at all.
	if at := r.limited(ctx, githubapp.AppScope); at != nil {
		r.report(&c, at, time.Since(started))
		return
	}
	writer, err := r.bind()
	if err != nil {
		// The listener makes the CI bucket and Dispatch's NATS connects in the background, so a
		// pass before either is an ordinary moment of a boot, not a fault: it reads GitHub nothing
		// and a later pass binds the writer.
		r.Logger.Info("ci reconcile pass bound no CI record writer", "error", err)
		return
	}
	at := r.reconcile(ctx, writer, &c)
	r.report(&c, at, time.Since(started))
}

// bind returns the CI record writer, binding it on the first pass that can.
func (r *Reconciler) bind() (Writer, error) {
	if r.writer != nil {
		return r.writer, nil
	}
	writer, err := r.Open()
	if err != nil {
		return nil, err
	}
	r.writer = writer
	return writer, nil
}

// reconcile is one pass over every mapped repository, and the stop that ended it early.
func (r *Reconciler) reconcile(ctx context.Context, writer Writer, c *counts) *stop {
	rows, err := r.mapped(ctx)
	if err != nil {
		r.Logger.Warn("ci reconcile read no repository mappings", "error", err)
		return nil
	}
	seenRows := make(map[string]struct{}, len(rows))
	seenRepositories := make(map[int64]struct{}, len(rows))
	seenHeads := map[string]struct{}{}
	for _, row := range rows {
		// Another process may have met a limit since this pass started, so the shared limits are
		// read again before each repository.
		if at := r.limited(ctx, githubapp.AppScope); at != nil {
			return at
		}
		owner, name, found := strings.Cut(row, "/")
		if !found || owner == "" || name == "" {
			c.mappings++
			r.Logger.Warn("ci reconcile skipped a repository mapping that is not owner/name", "repository", row)
			continue
		}
		seenRows[row] = struct{}{}
		token, err := r.GitHub.RepositoryToken(ctx, owner, name)
		if at := stopAt(err, githubapp.AppScope); at != nil {
			return at
		}
		if err != nil {
			// The App is not installed on it, or its token is refused the repository: neither
			// mends itself within a pass, so the repository is named once and the pass goes on.
			c.mappings++
			r.Logger.Warn("ci reconcile skipped a repository it holds no token for", "repository", row, "error", err)
			continue
		}
		// Every read made with this token is budgeted under its installation, so that scope is
		// read beside the App's before each of the head reads below.
		scope := r.GitHub.TokenScope(token)
		state, at, err := r.identify(ctx, row, owner, name, token, c)
		if at != nil {
			return at
		}
		if err != nil {
			c.mappings++
			r.Logger.Warn("ci reconcile skipped a repository GitHub did not answer for", "repository", row, "error", err)
			delete(r.rows, row)
			continue
		}
		remember(&r.rows, row, state)
		if _, done := seenRepositories[state.id]; done {
			// Two rows name one repository -- an old name and the one it was renamed to -- and
			// GitHub answers both with one id, so its heads are read once.
			continue
		}
		seenRepositories[state.id] = struct{}{}
		// Every record is keyed by the name GitHub itself gives the repository, never by the row:
		// repo_projects.repo is stored lower-cased, while the webhook's key comes from GitHub's own
		// casing and cistore.Key does not lower-case, so a pass keyed from the row would write a
		// second record of the same head and settle it on a second subject.
		ownerOf, nameOf, found := strings.Cut(state.fullName, "/")
		if !found || ownerOf == "" || nameOf == "" {
			c.mappings++
			r.Logger.Warn("ci reconcile skipped a repository GitHub named oddly", "repository", row, "full_name", state.fullName)
			continue
		}
		pulls, at, err := r.openPullRequests(ctx, state.id, ownerOf, nameOf, token, c)
		if at != nil {
			return at
		}
		if err != nil {
			c.mappings++
			r.Logger.Warn("ci reconcile read no open pull requests of a repository", "repository", state.fullName, "error", err)
			continue
		}
		for _, pull := range pulls {
			if at := r.limited(ctx, scopes(scope)...); at != nil {
				return at
			}
			head := pullHead{
				token:  token,
				scope:  scope,
				owner:  ownerOf,
				repo:   nameOf,
				number: strconv.FormatInt(pull.Number, 10),
				sha:    pull.HeadSHA,
			}
			if head.sha == "" {
				continue
			}
			seenHeads[head.key()] = struct{}{}
			if at := r.readHead(ctx, writer, head, c); at != nil {
				return at
			}
		}
	}
	// A pass that reached every repository knows which heads are still open, so what it kept of
	// the rest is forgotten rather than held for the process's life. A pass that stopped early
	// keeps everything: it reached no answer about the repositories it never read.
	forget(r.rows, seenRows)
	forget(r.pulls, seenRepositories)
	forget(r.heads, seenHeads)
	return nil
}

// mapped is every repository repo_projects maps to a project, as the rows spell it: `owner/name`,
// lower-cased with any `.git` stripped. They are read in one order so two passes read GitHub in
// the same order.
func (r *Reconciler) mapped(ctx context.Context) ([]string, error) {
	rows, err := r.Pool.Query(ctx, `select repo from repo_projects order by repo`)
	if err != nil {
		return nil, fmt.Errorf("read the repositories mapped to projects: %w", err)
	}
	defer rows.Close()
	var repositories []string
	for rows.Next() {
		var repository string
		if err := rows.Scan(&repository); err != nil {
			return nil, fmt.Errorf("read a mapped repository: %w", err)
		}
		repositories = append(repositories, repository)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("read the repositories mapped to projects: %w", err)
	}
	return repositories, nil
}

// identify resolves one row against GitHub, conditionally: the name GitHub gives the repository
// and its id. A 304 says the answer the last pass kept still stands.
func (r *Reconciler) identify(ctx context.Context, row, owner, name, token string, c *counts) (rowState, *stop, error) {
	kept := r.rows[row]
	r.pace(ctx, c)
	c.requests++
	answer, err := r.GitHub.RepositoryIdentity(ctx, token, owner, name, kept.etag)
	if at := stopAt(err, githubapp.AppScope); at != nil {
		return rowState{}, at, nil
	}
	if err != nil {
		return rowState{}, nil, err
	}
	if answer.NotModified {
		if kept.id == 0 {
			// Nothing is remembered to stand for the 304, which only a kept ETag can draw, so
			// this cannot happen; reading the row in full next pass is the honest answer to it.
			return rowState{}, nil, errors.New("GitHub answered 304 for a repository this pass remembers nothing of")
		}
		kept.etag = etagOr(answer.ETag, kept.etag)
		return kept, nil, nil
	}
	if answer.FullName == "" || answer.ID == 0 {
		return rowState{}, nil, fmt.Errorf("GitHub named the repository %q with id %d", answer.FullName, answer.ID)
	}
	return rowState{etag: answer.ETag, fullName: answer.FullName, id: answer.ID}, nil, nil
}

// openPullRequests is every open pull request of one repository, read page by page and each page
// conditional on the ETag the last pass kept for it.
func (r *Reconciler) openPullRequests(ctx context.Context, id int64, owner, name, token string, c *counts) ([]githubapp.PullRequestHead, *stop, error) {
	pages, err := readPages(ctx, r, c, &c.listPages, r.pulls[id], func(ctx context.Context, page int, etag string) (conditionalPage[githubapp.PullRequestHead], error) {
		answer, err := r.GitHub.OpenPullRequests(ctx, token, owner, name, page, etag)
		return conditionalPage[githubapp.PullRequestHead]{
			items:       answer.PullRequests,
			etag:        answer.ETag,
			notModified: answer.NotModified,
			full:        answer.Full,
		}, err
	})
	if at := stopAt(err, githubapp.AppScope); at != nil {
		return nil, at, nil
	}
	if err != nil {
		// The listing is read again from its first page next pass.
		delete(r.pulls, id)
		return nil, nil, err
	}
	remember(&r.pulls, id, pages)
	var pulls []githubapp.PullRequestHead
	for _, page := range pages {
		pulls = append(pulls, page.items...)
	}
	return pulls, nil, nil
}

// limited reads the shared GitHub rate limits and reports the limit in force on scopes, nil when
// none is. A read that fails leaves the pass to GitHub's own answers, as githubapp.Client's own
// read before every request does: the limits are the database's, and GitHub still answers each
// process's requests with the limit.
func (r *Reconciler) limited(ctx context.Context, scopes ...string) *stop {
	if r.Limits == nil || len(scopes) == 0 {
		return nil
	}
	limit, found, err := r.Limits.InForce(ctx, scopes...)
	if err != nil {
		r.Logger.Warn("ci reconcile did not read the shared GitHub rate limits", "scopes", scopes, "error", err)
		return nil
	}
	if !found {
		return nil
	}
	return &stop{scope: limit.Scope, until: limit.Until}
}

// stopAt is the stop a GitHub error ends the pass at, and nil for every other error.
// githubapp.Client.request records GitHub's own limit in the shared ledger itself, so the pass
// records nothing here; scope is what the request was budgeted under, which a limit GitHub
// answered with rather than one already held does not carry.
func stopAt(err error, scope string) *stop {
	limited, ok := githubapp.AsRateLimit(err)
	if !ok {
		return nil
	}
	at := &stop{scope: limited.Scope, until: time.Now().Add(limited.Wait)}
	if at.scope == "" {
		at.scope = scope
	}
	return at
}

// scopes is the shared limits a read with an installation token is held by: the App's, and the
// installation's when this process minted the token (githubapp.Client.TokenScope answers nothing
// for a token it did not mint).
func scopes(tokenScope string) []string {
	if tokenScope == "" {
		return []string{githubapp.AppScope}
	}
	return []string{githubapp.AppScope, tokenScope}
}

// pace leaves the pause between requests GitHub asks a poller for. It waits before each request
// but the pass's first, so a pass of one request takes no longer than that request.
func (r *Reconciler) pace(ctx context.Context, c *counts) {
	if r.Pace <= 0 || c.requests == 0 {
		return
	}
	timer := time.NewTimer(r.Pace)
	defer timer.Stop()
	select {
	case <-ctx.Done():
	case <-timer.C:
	}
}

// remember writes one entry of a map the Reconciler keeps between passes, making the map on the
// first entry there is.
func remember[K comparable, V any](held *map[K]V, key K, value V) {
	if *held == nil {
		*held = make(map[K]V, 1)
	}
	(*held)[key] = value
}

// forget drops every entry of held the pass did not see, so what the pass keeps follows the open
// pull requests there are rather than every one the process has ever read.
func forget[K comparable, V any](held map[K]V, seen map[K]struct{}) {
	for key := range held {
		if _, ok := seen[key]; !ok {
			delete(held, key)
		}
	}
}

// etagOr is answered, or kept when GitHub's answer carried none: a 304 should repeat the ETag it
// matched, and a kept ETag is better than no ETag if it does not.
func etagOr(answered, kept string) string {
	if answered != "" {
		return answered
	}
	return kept
}

// report writes the one INFO line a pass logs. Its field names are fixed: an alarm counts a
// pass's records and its rate limits by them.
func (r *Reconciler) report(c *counts, at *stop, took time.Duration) {
	attrs := []any{
		"list_pages", c.listPages,
		"heads", c.heads,
		"head_requests", c.headRequests,
		"heads_not_modified", c.notModified,
		"records", c.records,
		"mappings_skipped", c.mappings,
		"heads_too_large", c.tooLarge,
		"rate_limited", at != nil,
	}
	if at != nil {
		attrs = append(attrs, "rate_limit_scope", at.scope, "rate_limit_until", at.until.UTC().Format(time.RFC3339))
	}
	attrs = append(attrs, "duration", took.String())
	r.Logger.Info("ci reconcile pass", attrs...)
}
