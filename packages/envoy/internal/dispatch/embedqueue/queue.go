// Package embedqueue durably retries Bedrock embedding calls for Dispatch's meaning search. A
// write's AFTER INSERT/UPDATE trigger (0072-0076) upserts a pending row into embeddings with the
// latest text and a hash of it in the same transaction as the write, and this package's poller
// embeds whatever that enqueue left pending, with the same scan-then-retry shape as the event
// outbox (internal/dispatch/outbox): a write never fails because the embedder did, since Bedrock
// is never called inside the enqueueing transaction.
package embedqueue

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"math"
	"math/rand/v2"
	"sync"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/sjawhar/envoy/internal/dispatch/embed"
	"github.com/sjawhar/envoy/internal/dispatch/retry"
	"github.com/sjawhar/envoy/internal/dispatch/store"
)

const (
	batchSize        = embed.MaxBatchTexts
	retryInterval    = 5 * time.Second
	backfillPageSize = 500
	// claimWindow is how long scanPending's claim keeps a row off every other scanner's list.
	// It is a plain UPDATE ... RETURNING, not a transaction held open across the embed call: this
	// package follows the event outbox's own rule (internal/dispatch/outbox: "an open cursor
	// holds its pooled connection until it closes, so publishing ... inside the loop would hold
	// one of the pool's four connections across all of it") - a lock held across a network call
	// is exactly what that rule exists to avoid. Advancing next_attempt_at instead costs one
	// short statement and lets the poller and a concurrent `backfill-embeddings` run claim
	// disjoint batches without either blocking on the other's embed call. Bisecting a batch every
	// one of whose rows fails can itself take minutes, so claimWindow alone is not always enough;
	// claimRenewal is what keeps that case's claim from lapsing mid-bisection; a crashed claimer's
	// rows become eligible again once it passes unrenewed, not stuck forever.
	claimWindow = 2 * time.Minute
	// batchPause separates one embed call from the next when neither is throttled, in both Run's
	// poller and Backfill's drain loop: pending rows arrive in bursts (a bulk import, a backfill's
	// own enqueue pass), and nothing upstream paces how fast this package reads them back out and
	// calls the embedder - without this, draining a large burst would fire batch after batch back
	// to back, which is how a well-behaved client still trips a provider's rate limit.
	batchPause = 200 * time.Millisecond
	// tokenRateCeiling is the most estimated tokens per minute reserveTokens lets this process
	// (or any other process sharing the same embeddings_rate_limit row - the server's poller and
	// a separately run `backfill-embeddings` are two independent OS processes in production,
	// each otherwise unaware of the other's traffic) send to Bedrock for background work
	// combined: Bedrock throttles the whole account, not a specific caller, so
	// embed.RateLimitedEmbedder's own in-process, reactive AIMD pacing alone cannot keep a live
	// search request safe from throttling an unconstrained background process causes in a
	// different process the reactive limiter never sees. 200,000 sits well below the account's
	// own Cohere Embed v4 Bedrock quota (300,000 tokens/minute) - 100,000 tokens/minute of
	// headroom, nowhere near what a live query (tens of tokens) could ever need - and against
	// this package's own average chunk size projects to about 410 chunks/minute, backfilling the
	// production corpus (tens of thousands of chunks) in about an hour.
	tokenRateCeiling = 200_000
)

// renewClaimInterval is how often a claimRenewal re-extends its claim on every row it still
// protects - between bisection's own embed attempts, and during a single reservation's wait
// longer than this - comfortably inside claimWindow so a renewal always lands before the claim
// it is refreshing could lapse. A var, not a const alongside claimWindow above, so a test can
// shrink it well below claimWindow's own 2 minutes and observe a renewal firing in milliseconds
// rather than needing to wait out the real interval.
var renewClaimInterval = claimWindow / 2

// Deps are the durable state and embedding dependencies for the queue.
type Deps struct {
	Store    *store.Store
	Embedder embed.Embedder
}

// Run drains pending embeddings at startup and periodically, so a dropped in-process
// notification cannot strand a row. It returns when ctx is cancelled. Run does nothing when
// deps.Embedder is nil: Dispatch with no Bedrock credentials at boot never starts this poller,
// and every row stays pending until credentials are available and a write or a backfill
// re-enqueues it.
func Run(ctx context.Context, deps Deps) {
	if deps.Embedder == nil {
		return
	}
	ctx = store.WithTransactionTracking(ctx)
	scan(ctx, deps)
	ticker := time.NewTicker(retryInterval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			scan(ctx, deps)
		}
	}
}

// scan drains what is immediately eligible right now, backing off the whole batch cadence (not
// just the rows it just saw) while Bedrock is throttling, exactly as Backfill's own drain loop
// does - the live poller is called again by Run's own ticker in any case, but a sustained
// throttle episode should not have it hammering Bedrock every five seconds regardless.
func scan(ctx context.Context, deps Deps) {
	consecutiveThrottles := 0
	for {
		succeeded, failed, blocked, throttled, err := ProcessBatch(ctx, deps)
		if err != nil {
			slog.Error("dispatch embedqueue: scan", "error", err)
			return
		}
		if throttled {
			consecutiveThrottles++
			if !pauseFor(ctx, batchBackoff(consecutiveThrottles)) {
				return
			}
			continue
		}
		consecutiveThrottles = 0
		if succeeded+failed < batchSize || blocked {
			return
		}
		if !pauseFor(ctx, batchPause) {
			return
		}
	}
}

// batchBackoff is the whole-batch-cadence pause after consecutive throttled batches: the same
// doubling schedule retry.Delay already gives individual rows, plus up to 50% jitter so many
// callers backing off from the same throttle event do not all retry in lockstep.
func batchBackoff(consecutiveThrottles int) time.Duration {
	base := retry.Delay(consecutiveThrottles)
	return base + time.Duration(rand.Int64N(int64(base)/2+1))
}

// pauseFor waits d, or returns false without waiting out the rest of it if ctx ends first - a
// cancelled Run or Backfill should stop promptly, not finish out its pacing delay.
func pauseFor(ctx context.Context, d time.Duration) bool {
	timer := time.NewTimer(d)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return false
	case <-timer.C:
		return true
	}
}

type pendingRow struct {
	kind, id, text, contentHash string
	attempts, confirmedFailures int
}

// ProcessBatch embeds up to one embedder batch of pending rows and reports how many of them it
// actually committed (succeeded) versus sent back for retry (failed) - not the same as how many
// it read, which outbox-style callers used to conflate with "done" - whether the batch is
// blocked (a row whose retry could not be scheduled stays eligible immediately, so a caller that
// looped on a blocked batch would otherwise select the same stuck row forever), and whether the
// whole batch failed because Bedrock is throttling (embed.IsThrottled) rather than because of
// anything about the rows themselves - a caller (scan, Backfill) uses that to back off its own
// cadence rather than hammering a provider that has already said to slow down. Both the poller
// and Backfill drive the queue through this one function; scanPending's claim is what lets the
// poller and a concurrent `backfill-embeddings` run safely at once, each claiming a different set
// of pending rows rather than racing to embed the same ones twice.
//
// A throttled whole-batch failure never counts toward any row's dead-letter threshold, however
// many times it recurs: it is infrastructure (the provider is rate-limiting this account), never
// evidence about a specific row's content - every row in the batch is retried as a whole, not
// dead-lettered. A non-throttled whole-batch failure of two or more rows is different: something
// about this batch's own content *might* have broken the request (a text the API refuses
// outright, say), and bisectBatch isolates which row(s) by splitting the batch and re-embedding
// each half, recursively. But a row isolated down to one that still fails non-throttled is only
// evidence about that row's own content once some *other* row of the same original batch has
// actually embedded during this same bisection - proof the service itself works, not just that
// this one request failed. When nothing in the whole batch ever succeeds, every non-throttled
// failure, however many rows deep, is systemic (an outage embed.IsThrottled does not recognize -
// expired credentials, a retired model id, an uncoded 5xx) rather than evidence against any
// specific row, and ProcessBatch demotes every row bisection tentatively isolated back to an
// ordinary retry rather than dead-lettering any of them: an outage outside IsThrottled's
// allowlist is never evidence against any one row's content just because every row happened to
// fail the same way. A context
// cancellation - this call's own ctx ending mid-request, including embed.RateLimitedEmbedder's
// own pacing wait ending early - is never evidence about any row either, whatever error text came
// back, regardless of whether some other row already succeeded: bisectBatch checks for it
// explicitly and never marks a row permanent because of it. A batch that was already exactly one
// row has no sibling to compare against, so bisection has nothing to isolate: it is retried like
// a throttled batch, never dead-lettered, exactly as before.
func ProcessBatch(ctx context.Context, deps Deps) (succeeded, failed int, blocked, throttled bool, err error) {
	rows, err := scanPending(ctx, deps)
	if err != nil {
		return 0, 0, false, false, err
	}
	if len(rows) == 0 {
		return 0, 0, false, false, nil
	}
	if ctx.Err() != nil {
		blocked = retryRows(ctx, deps, rows, false)
		return 0, len(rows), blocked, false, nil
	}
	// renewal is created before this call's very first reservation, not only once bisection is
	// known to be needed: a single reservation for a full batch of maximum-length documents can
	// itself wait close to claimWindow (chunkForBudget's and reserveTokens' own doc comments), so
	// a renewer must be in scope for every embed attempt this call makes.
	renewal := newClaimRenewal(deps, rows)
	result := embedRows(ctx, deps, rows, renewal)
	if result.err != nil {
		return 0, 0, false, false, result.err
	}
	if len(result.permanent) > 0 {
		blocked = retryRows(ctx, deps, result.permanent, true)
	}
	if len(result.retry) > 0 {
		blocked = retryRows(ctx, deps, result.retry, false) || blocked
	}
	return result.succeeded, len(rows) - result.succeeded, blocked, result.throttled, nil
}

// embedCanary is a fixed, short, known-good text, never stored: the one probe
// confirmSoloFailure sends right after a solo row's own embed call fails, to find out whether
// the embedder itself - the service, the credentials, the model - was working at that exact
// moment. It is safe to send on every solo failure because it names nothing about any row's own
// content, costs a negligible, constant number of tokens (reserved from the same shared budget
// as any other background call), and is never written to embeddings or returned to a caller -
// its only use is the one Embed call's own success or failure.
const embedCanary = "dispatch embedqueue confirmation canary"

// bisectResult is embedRows' own report (and handleGroupFailure's, and bisectSplit's): every row
// given to one of them ends up in exactly one of succeeded (committed), permanent (a solo row's
// own non-throttled failure immediately confirmed by a successful embedCanary probe right after
// it - proof the service was working at that exact moment, so the failure is this row's own
// content), or retry (not confirmed - a throttle, a cancellation, a multi-row group not yet split
// down to one, or a solo failure whose own canary probe also failed, which is never evidence
// about this row and means the whole subtree backs off instead). err is a genuine infrastructure
// failure (commitEmbeddings' own), never a reason to call any row permanent.
type bisectResult struct {
	succeeded int
	permanent []pendingRow
	retry     []pendingRow
	throttled bool
	err       error
}

// embedRows embeds and commits rows group by group (chunkForBudget splits rows so no single
// reservation ever exceeds tokenRateCeiling), committing each group's vectors to the database as
// soon as that group's own Embed call succeeds: a later group's failure never re-embeds an
// earlier group's already-committed rows, and a group that itself fails non-throttled is handed
// to handleGroupFailure, which isolates it without ever re-embedding that exact group again. The
// returned bisectResult accumulates every group's own outcome across the whole call. A genuine
// infrastructure error (commitEmbeddings' own) stops the loop immediately and propagates as-is,
// abandoning whatever groups were not yet attempted - ProcessBatch and bisectSplit both treat it
// as a hard failure, never a reason to retry row by row.
func embedRows(ctx context.Context, deps Deps, rows []pendingRow, renewal *claimRenewal) bisectResult {
	var result bisectResult
	for _, group := range chunkForBudget(rows) {
		if ctx.Err() != nil {
			result.retry = append(result.retry, group.rows...)
			continue
		}
		texts := make([]string, len(group.rows))
		for i, row := range group.rows {
			texts[i] = row.text
		}
		renewal.renew(ctx)
		reserveTokens(ctx, deps, group.tokens, tokenRateCeiling, renewal)
		vectors, err := deps.Embedder.Embed(ctx, texts, embed.InputDocument)
		if err != nil {
			groupResult := handleGroupFailure(ctx, deps, group.rows, err, renewal)
			if groupResult.err != nil {
				return groupResult
			}
			result.succeeded += groupResult.succeeded
			result.permanent = append(result.permanent, groupResult.permanent...)
			result.retry = append(result.retry, groupResult.retry...)
			result.throttled = result.throttled || groupResult.throttled
			continue
		}
		committed, permanent, cErr := commitEmbeddings(ctx, deps, group.rows, vectors)
		if cErr != nil {
			return bisectResult{err: cErr}
		}
		result.succeeded += committed
		result.permanent = append(result.permanent, permanent...)
		renewal.resolve(excludingRows(group.rows, permanent))
	}
	return result
}

// handleGroupFailure decides what a group whose own embed call already failed with err needs,
// without ever re-embedding that exact group: a throttled failure, or a context cancelled while
// the call was in flight (embed.RateLimitedEmbedder's own pacing wait, between this package's
// calls, returns ctx.Err() exactly this way), retries the whole group - neither is
// evidence about any row's content, and isolating "which row triggered it" would be meaningless
// (and would cost far more calls against a provider that has already said to slow down). A single
// row goes to confirmSoloFailure. More than one row splits via bisectSplit, which re-embeds only
// the two halves, never the group that already failed.
func handleGroupFailure(ctx context.Context, deps Deps, group []pendingRow, err error, renewal *claimRenewal) bisectResult {
	if embed.IsThrottled(err) {
		return bisectResult{retry: group, throttled: true}
	}
	if ctx.Err() != nil {
		return bisectResult{retry: group}
	}
	if len(group) == 1 {
		return confirmSoloFailure(ctx, deps, group, err, renewal)
	}
	return bisectSplit(ctx, deps, group, renewal)
}

// confirmSoloFailure decides whether a solo row's own non-throttled failure is this row's own
// fault or a systemic condition, by immediately probing embedCanary - right after the row's own
// failed call, in the same call context, drawing its own token reservation like any other
// background call since it is a real (if tiny) Bedrock request. A canary success proves the
// service, credentials and model were all working at that exact moment, so the row's own failure
// is confirmed permanent. A canary failure - including a throttle - means the condition is
// systemic, not this row's fault: nothing is confirmed, the row goes back to retry, and if the
// canary itself was throttled, throttled is reported too, so the caller backs off exactly as an
// outright-throttled call would. This replaces every order-based inference (round 8's provenUp,
// and before it, the whole-batch succeeded-count check) with a direct, local test run at the
// moment it matters, rather than reasoning about which other row happened to succeed earlier or
// later in the same bisection.
func confirmSoloFailure(ctx context.Context, deps Deps, group []pendingRow, rowErr error, renewal *claimRenewal) bisectResult {
	renewal.renew(ctx)
	reserveBackgroundTokens(ctx, deps, []string{embedCanary}, renewal)
	_, canaryErr := deps.Embedder.Embed(ctx, []string{embedCanary}, embed.InputDocument)
	if canaryErr != nil {
		slog.Error("dispatch embedqueue: a known-good canary also failed right after this row's own failure - systemic, not this row's fault",
			"kind", group[0].kind, "id", group[0].id, "rowError", rowErr, "canaryError", canaryErr)
		return bisectResult{retry: group, throttled: embed.IsThrottled(canaryErr)}
	}
	slog.Error("dispatch embedqueue: a known-good canary embedded successfully right after this row's own failure - confirmed",
		"kind", group[0].kind, "id", group[0].id, "error", rowErr)
	return bisectResult{permanent: group}
}

// bisectSplit isolates which row(s) in rows actually broke a non-throttled embed failure
// (handleGroupFailure calls it once a group fails and holds more than one row): split rows in
// half and embed each half independently via embedRows, recursively - a half that embeds cleanly
// commits; a half that still fails non-throttled and holds more than one row is split again by
// embedRows calling handleGroupFailure calling this function again; a half that fails down to one
// row is decided by confirmSoloFailure's own canary probe, run fresh for that one row - never by
// reasoning about any other row's own result.
func bisectSplit(ctx context.Context, deps Deps, rows []pendingRow, renewal *claimRenewal) bisectResult {
	mid := len(rows) / 2
	left := embedRows(ctx, deps, rows[:mid], renewal)
	if left.err != nil {
		return left
	}
	right := embedRows(ctx, deps, rows[mid:], renewal)
	if right.err != nil {
		return right
	}
	return mergeBisectResults(left, right)
}

// mergeBisectResults combines left's and right's reports into one. mergeRows copies each side
// into a freshly allocated slice rather than appending onto one of them in place: rows[:mid] and
// rows[mid:] share rows' own backing array, so a bisectResult built straight from one of those
// slices (handleGroupFailure's throttled/cancelled returns, and confirmSoloFailure's own) still
// aliases it, and appending onto one with spare capacity - exactly what rows[:mid] has when mid
// < len(rows) - would silently overwrite whatever row happens to sit at the index append writes
// to next, which is never guaranteed to be one of the rows actually being merged. A side with
// nothing to contribute is returned as-is instead, which copies nothing and is still safe: only
// appending onto a slice risks writing past what it reports, and an empty side is never appended
// to.
func mergeRows(a, b []pendingRow) []pendingRow {
	if len(a) == 0 {
		return b
	}
	if len(b) == 0 {
		return a
	}
	merged := make([]pendingRow, 0, len(a)+len(b))
	merged = append(merged, a...)
	merged = append(merged, b...)
	return merged
}

func mergeBisectResults(left, right bisectResult) bisectResult {
	return bisectResult{
		succeeded: left.succeeded + right.succeeded,
		permanent: mergeRows(left.permanent, right.permanent),
		retry:     mergeRows(left.retry, right.retry),
		throttled: left.throttled || right.throttled,
	}
}

// budgetedGroup is one chunkForBudget group paired with the token total already computed to
// decide its boundary, so embedRows' own reservation reuses it instead of calling
// embed.EstimateTokens a second time over the same rows.
type budgetedGroup struct {
	rows   []pendingRow
	tokens int
}

// chunkForBudget splits rows into the fewest groups whose own estimated tokens
// (embed.EstimateTokens) each stay at or under tokenRateCeiling, in original order, so
// reserveTokens never has to reserve more than one minute's worth of budget for a single group.
// A row whose own estimate alone meets or exceeds the ceiling - an unusually dense
// maxInputChars-length document - still gets a group of its own rather than being split
// further: EstimateTokens already truncates at maxInputChars, so even that worst case is a
// known, bounded size, and one such row is the smallest unit this package ever embeds. Each
// group's tokens field is the sum of its own rows' estimates, computed here once and carried
// forward rather than re-estimated from the group's joined texts later.
func chunkForBudget(rows []pendingRow) []budgetedGroup {
	if len(rows) == 0 {
		return nil
	}
	groups := make([]budgetedGroup, 0, 1)
	start, tokens := 0, 0
	for i, row := range rows {
		rowTokens := embed.EstimateTokens([]string{row.text})
		if i > start && tokens+rowTokens > tokenRateCeiling {
			groups = append(groups, budgetedGroup{rows: rows[start:i], tokens: tokens})
			start, tokens = i, 0
		}
		tokens += rowTokens
	}
	return append(groups, budgetedGroup{rows: rows[start:], tokens: tokens})
}

// claimRenewal re-extends scanPending's claim on every row of the original top-level batch a
// ProcessBatch call is working through - not just whatever subtree bisection is currently in,
// since every row not yet committed or confirmed permanent is still this call's responsibility
// until it returns - once renewClaimInterval has passed since the last renewal, so work slower
// than claimWindow (a long bisection, or a single reservation's own wait; see renewClaimInterval
// and reserveTokens' own doc comments) never loses its claim to a concurrent scanner partway
// through. Best-effort: a failed renewal is logged and never aborts anything, since a row a
// concurrent scanner reclaims is merely processed twice, not corrupted - commitEmbeddings' own
// content_hash-matched update already makes a doubly-claimed row safe, just not free. A nil
// *claimRenewal (a caller with no claim to protect, such as a test exercising reserveTokens
// alone) is a safe no-op.
type claimRenewal struct {
	deps Deps
	rows []pendingRow
	last time.Time
}

func newClaimRenewal(deps Deps, rows []pendingRow) *claimRenewal {
	return &claimRenewal{deps: deps, rows: rows, last: time.Now()}
}

func (c *claimRenewal) renew(ctx context.Context) {
	if c == nil || ctx.Err() != nil || time.Since(c.last) < renewClaimInterval {
		return
	}
	c.last = time.Now()
	kinds := make([]string, len(c.rows))
	ids := make([]string, len(c.rows))
	hashes := make([]string, len(c.rows))
	for i, row := range c.rows {
		kinds[i], ids[i], hashes[i] = row.kind, row.id, row.contentHash
	}
	// content_hash-matched, exactly as commitEmbeddings and retryRows both are: a row a
	// concurrent write already changed is no longer this claim's to protect, and touching it
	// anyway would delay that write's own, fresher embeddings_enqueue up to claimWindow for no
	// reason - the row's new content_hash makes it eligible again immediately otherwise.
	if _, err := c.deps.Store.Pool.Exec(ctx, `
		update embeddings e set next_attempt_at = now() + $4
		from unnest($1::text[], $2::text[], $3::text[]) as v(kind, id, content_hash)
		where e.kind = v.kind and e.id = v.id and e.content_hash = v.content_hash
	`, kinds, ids, hashes, claimWindow); err != nil {
		slog.Error("dispatch embedqueue: bisection claim renewal failed", "error", err)
	}
}

// resolve removes committed rows from the set a claimRenewal still protects: once a row commits
// (embedded_hash = content_hash), scanPending's own predicate no longer selects it, so renewing
// its claim further touches nothing a concurrent scanner could ever reclaim.
func (c *claimRenewal) resolve(committed []pendingRow) {
	if c == nil || len(committed) == 0 {
		return
	}
	c.rows = excludingRows(c.rows, committed)
}

// excludingRows returns rows with every row also present in exclude removed, matched by (kind,
// id) - embedRows' own way of turning commitEmbeddings' rows/permanent pair into the set that
// actually committed, for claimRenewal.resolve. A fresh slice, never c.rows[:0] or rows itself
// filtered in place: c.rows is the original top-level batch, which bisectSplit's own recursion is
// still actively slicing into (rows[:mid], rows[mid:]) elsewhere in the same call tree -
// overwriting its backing array in place would corrupt whatever sibling subtree is reading from
// it concurrently with this call.
func excludingRows(rows, exclude []pendingRow) []pendingRow {
	if len(exclude) == 0 {
		return rows
	}
	skip := make(map[string]struct{}, len(exclude))
	for _, row := range exclude {
		skip[row.kind+"\x00"+row.id] = struct{}{}
	}
	kept := make([]pendingRow, 0, len(rows))
	for _, row := range rows {
		if _, ok := skip[row.kind+"\x00"+row.id]; !ok {
			kept = append(kept, row)
		}
	}
	return kept
}

// reserveBackgroundTokens estimates texts' tokens (embed.EstimateTokens) and reserves them
// against tokenRateCeiling's shared, database-backed budget (reserveTokens), waiting out whatever
// debt that reservation leaves - renewing renewal's claim during a wait longer than
// renewClaimInterval, not only before or after it. embedRows' own per-group reservations call
// reserveTokens directly with chunkForBudget's already-computed token total instead of this
// helper, so confirmSoloFailure's one-off embedCanary probe is its only caller. Best-effort: a
// reservation that fails to read or write (a transient database error) is logged and never
// blocks or fails the batch - embed.RateLimitedEmbedder's own in-process, reactive AIMD backoff
// is the backstop if this proactive ceiling is ever skipped or under-estimates a call.
func reserveBackgroundTokens(ctx context.Context, deps Deps, texts []string, renewal *claimRenewal) {
	reserveTokens(ctx, deps, embed.EstimateTokens(texts), tokenRateCeiling, renewal)
}

// reserveTokens draws tokens from embeddings_rate_limit's one shared row (migration 0077),
// refilling it continuously at ceilingPerMinute/60 tokens per second and capping it at
// ceilingPerMinute (one statement does both: a plain token bucket), then waits out whatever
// negative balance - debt - that reservation leaves before returning, so the caller's real
// Bedrock call never races ahead of the budget it just drew down. chunkForBudget already keeps
// one reservation's own tokens at or under ceilingPerMinute, but the wait can still run past
// renewClaimInterval when other debt was already on the row (another process's, or this one's
// own earlier groups) - renewal is woken (renew) at every renewClaimInterval-sized slice of the
// wait, not only before or after it, so a claim never lapses mid-wait; renewal.renew is a safe
// no-op on nil (a caller with no claim to protect, such as a test exercising the budget alone).
// ceilingPerMinute is a parameter rather than always tokenRateCeiling so a test can use a tiny
// ceiling and observe a real, short wait instead of needing to simulate a whole minute.
func reserveTokens(ctx context.Context, deps Deps, tokens, ceilingPerMinute int, renewal *claimRenewal) {
	var available float64
	if err := deps.Store.Pool.QueryRow(ctx, `
		update embeddings_rate_limit
		set tokens_available = least($2::double precision, tokens_available + extract(epoch from now() - last_refill_at) * ($2::double precision / 60.0)) - $1::double precision,
		    last_refill_at = now()
		returning tokens_available
	`, float64(tokens), float64(ceilingPerMinute)).Scan(&available); err != nil {
		slog.Error("dispatch embedqueue: background token-budget reservation failed", "error", err)
		return
	}
	if available >= 0 {
		return
	}
	wait := time.Duration(-available / (float64(ceilingPerMinute) / 60.0) * float64(time.Second))
	waitOutDebt(ctx, wait, renewal, renewClaimInterval)
}

// waitOutDebt is reserveTokens' own wait loop, pulled out so a test can drive it with a tiny
// segment instead of renewClaimInterval's real one minute: it sleeps out wait in slices of at
// most segment, calling renewal.renew between every slice (not only before or after the whole
// wait), so a wait longer than segment still renews partway through rather than only at the end.
func waitOutDebt(ctx context.Context, wait time.Duration, renewal *claimRenewal, segment time.Duration) {
	for wait > 0 {
		s := wait
		if s > segment {
			s = segment
		}
		if !pauseFor(ctx, s) {
			return
		}
		wait -= s
		renewal.renew(ctx)
	}
}

// scanPending claims at most batchSize pending rows, oldest-eligible first, atomically advancing
// their next_attempt_at by claimWindow so a concurrent scanner (the poller and a manually run
// `backfill-embeddings` can run at once) does not also claim them - without holding a lock across
// the embed call that follows; see claimWindow's own comment for why not.
func scanPending(ctx context.Context, deps Deps) ([]pendingRow, error) {
	rows, err := deps.Store.Pool.Query(ctx, `
		update embeddings set next_attempt_at = now() + $2
		where (kind, id) in (
			select kind, id from embeddings
			where embedded_hash is distinct from content_hash
			  and next_attempt_at <= now()
			  and not dead
			order by next_attempt_at, kind, id
			limit $1
			for update skip locked
		)
		returning kind, id, text_snapshot, content_hash, attempt_count, confirmed_failures
	`, batchSize, claimWindow)
	if err != nil {
		return nil, fmt.Errorf("scan pending embeddings: %w", err)
	}
	defer rows.Close()
	var pending []pendingRow
	for rows.Next() {
		var row pendingRow
		if err := rows.Scan(&row.kind, &row.id, &row.text, &row.contentHash, &row.attempts, &row.confirmedFailures); err != nil {
			return nil, fmt.Errorf("scan pending embeddings: %w", err)
		}
		pending = append(pending, row)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("scan pending embeddings: %w", err)
	}
	return pending, nil
}

// commitEmbeddings bulk-writes every row whose vector is finite in one statement (unnest instead
// of one UPDATE per row), then reports which of them the database actually recorded: a row whose
// content_hash no longer matches (a write changed its text between the scan and this call) is
// silently skipped, not retried - the write's own trigger already reset its retry state with the
// new text, and nothing this call could do would be better than what it already did. A row whose
// vector came back non-finite (NaN or +/-Inf, which a model should never answer but pgvector's own
// input parser would otherwise accept or reject with a much less specific error) is reported as
// permanent: it is the one failure this package can attribute to the row's own content.
func commitEmbeddings(ctx context.Context, deps Deps, rows []pendingRow, vectors [][]float32) (succeeded int, permanent []pendingRow, err error) {
	kinds := make([]string, 0, len(rows))
	ids := make([]string, 0, len(rows))
	hashes := make([]string, 0, len(rows))
	literals := make([]string, 0, len(rows))
	for i, row := range rows {
		finite := true
		for _, f := range vectors[i] {
			if math.IsNaN(float64(f)) || math.IsInf(float64(f), 0) {
				finite = false
				break
			}
		}
		if !finite {
			permanent = append(permanent, row)
			continue
		}
		kinds = append(kinds, row.kind)
		ids = append(ids, row.id)
		hashes = append(hashes, row.contentHash)
		literals = append(literals, embed.Literal(vectors[i]))
	}
	if len(kinds) == 0 {
		return 0, permanent, nil
	}
	queryRows, err := deps.Store.Pool.Query(ctx, `
		update embeddings e
		set embedding = v.embedding::vector, embedded_hash = e.content_hash, model = $5,
		    embedded_at = now(), attempt_count = 0, confirmed_failures = 0, next_attempt_at = now()
		from unnest($1::text[], $2::text[], $3::text[], $4::text[]) as v(kind, id, content_hash, embedding)
		where e.kind = v.kind and e.id = v.id and e.content_hash = v.content_hash
		returning e.kind
	`, kinds, ids, hashes, literals, embed.Model)
	if err != nil {
		return 0, permanent, fmt.Errorf("commit embeddings: %w", err)
	}
	defer queryRows.Close()
	for queryRows.Next() {
		var discard string
		if err := queryRows.Scan(&discard); err != nil {
			return 0, permanent, fmt.Errorf("commit embeddings: %w", err)
		}
		succeeded++
	}
	if err := queryRows.Err(); err != nil {
		return 0, permanent, fmt.Errorf("commit embeddings: %w", err)
	}
	if raced := len(kinds) - succeeded; raced > 0 {
		slog.Info("dispatch embedqueue: skipped committing a stale embedding (a write changed the row's text first)", "count", raced)
	}
	return succeeded, permanent, nil
}

// retryRows schedules every row in rows for another attempt in one bulk statement (unnest), or -
// only when permanent is true - marks a row dead once its own confirmed failures reach
// retry.DeadLetterAttempts. permanent distinguishes a row's own content failing (confirmed: a
// commitEmbeddings non-finite-vector row, or a bisection row confirmSoloFailure confirmed with a
// successful canary probe) from everything else a batch failure can mean - a whole-batch
// infrastructure failure, a throttle, a cancellation, or a solo row whose own canary probe also
// failed - none of which is ever evidence about a specific row's content.
// attempt_count advances on every call regardless, exactly as it always has (it paces
// next_attempt_at's own backoff and is the general "how many times has this been tried" count);
// confirmed_failures - a separate column, never conflated with attempt_count - advances only
// when permanent is true, so however many outage-era or throttled cycles a row passes through,
// none of them can push it toward dead on their own; only a cycle that genuinely confirms this
// row's own content is at fault ever can. A dead row is still visible
// (`select * from embeddings where dead`) and still recoverable: a future write to the same
// content (embeddings_enqueue clears both counters and dead the moment content_hash changes) or a
// manual `update embeddings set dead = false, confirmed_failures = 0` re-admits it to the next
// scan. It reports whether the batch is blocked, exactly as outbox's own scheduleRetry does.
func retryRows(ctx context.Context, deps Deps, rows []pendingRow, permanent bool) bool {
	kinds := make([]string, len(rows))
	ids := make([]string, len(rows))
	hashes := make([]string, len(rows))
	attempts := make([]int32, len(rows))
	confirmedFailures := make([]int32, len(rows))
	nextAttempts := make([]time.Time, len(rows))
	deads := make([]bool, len(rows))
	for i, row := range rows {
		n := row.attempts + 1
		confirmed := row.confirmedFailures
		if permanent {
			confirmed++
		}
		dead := permanent && confirmed >= retry.DeadLetterAttempts
		if dead {
			slog.Error("dispatch embedqueue: row reached the dead-letter threshold, no further automatic retry", "kind", row.kind, "id", row.id, "confirmedFailures", confirmed)
		}
		kinds[i], ids[i], hashes[i] = row.kind, row.id, row.contentHash
		attempts[i] = int32(n)
		confirmedFailures[i] = int32(confirmed)
		nextAttempts[i] = time.Now().Add(retry.Delay(n))
		deads[i] = dead
	}
	_, err := deps.Store.Pool.Exec(ctx, `
		update embeddings e
		set attempt_count = v.attempts, confirmed_failures = v.confirmed_failures,
		    next_attempt_at = v.next_attempt_at, dead = v.dead
		from unnest($1::text[], $2::text[], $3::text[], $4::int[], $5::int[], $6::timestamptz[], $7::bool[])
		  as v(kind, id, content_hash, attempts, confirmed_failures, next_attempt_at, dead)
		where e.kind = v.kind and e.id = v.id and e.content_hash = v.content_hash
	`, kinds, ids, hashes, attempts, confirmedFailures, nextAttempts, deads)
	if err != nil {
		slog.Error("dispatch embedqueue: schedule retry", "rows", len(rows), "error", err)
		return true
	}
	return false
}

// BackfillReport is what one Backfill call enqueued and embedded, per kind and in total. Embedded
// and Failed count only rows ProcessBatch actually tried to commit, never rows it merely read.
// Pending is what is left still eligible for automatic retry once the drain loop stops - it is 0
// at a normal completion (the loop waits out backoff rather than exiting early) and only nonzero
// if ctx was cancelled mid-run. Dead is separate: rows that stopped retrying automatically and
// need a future write or a manual reset, never conflated into Pending the way an earlier
// round's report (that counted both under one undifferentiated number) was.
type BackfillReport struct {
	Enqueued map[string]int64
	Embedded int64
	Failed   int64
	Pending  int64
	Dead     int64
}

// backfillSource is one kind's resumable enqueue: which table(s) to read (tableExpr, a FROM
// clause - a plain table name, or a join chain for a kind whose text lives in a related table),
// which expression is its id and its text, an optional extra WHERE predicate beyond the
// checkpoint comparison (document's "only a doc artifact with markdown" filter), and the
// expression paginated on (ordExpr; almost always the same as idExpr, written out separately only
// because document's id, a.id::text, is not what page's own ORDER BY needs to reference once the
// lateral join is in scope).
type backfillSource struct {
	kind, tableExpr, idExpr, textExpr, ordExpr, extraWhere string
}

var backfillSources = []backfillSource{
	{kind: "issue", tableExpr: "issues", idExpr: "key", textExpr: "title", ordExpr: "key"},
	{
		kind: "document",
		tableExpr: `artifacts a join lateral (
			select markdown from artifact_versions where artifact_id = a.id order by number desc limit 1
		) v on true`,
		idExpr:     "a.id::text",
		textExpr:   "v.markdown",
		ordExpr:    "a.id::text",
		extraWhere: "a.kind = 'doc' and v.markdown is not null",
	},
	{kind: "comment", tableExpr: "comments", idExpr: "id::text", textExpr: "body", ordExpr: "id::text"},
	{
		kind:      "ask",
		tableExpr: "asks",
		idExpr:    "id::text",
		textExpr:  "question || ' ' || coalesce(answer->>'text', '')",
		ordExpr:   "id::text",
	},
	{kind: "message", tableExpr: "messages", idExpr: "id::text", textExpr: "body", ordExpr: "id::text"},
}

// sql renders this source's resumable enqueue statement: a "page" CTE selecting at most
// backfillPageSize rows past the kind's checkpoint ($1, ” before any row has run), ordered by
// ordExpr, and a sibling writable CTE inserting each into embeddings exactly as the kind's
// write-time trigger's own enqueue would, but ON CONFLICT DO NOTHING rather than DO UPDATE: an
// embeddings row already existing - whether a trigger wrote it after this migration shipped, or
// an earlier Backfill page already did - means Backfill has nothing to add, and must never
// overwrite it. A DO UPDATE here could race a concurrent write: if a trigger's own enqueue
// (fresher text, reset attempt_count) committed between this page's read and this statement's own
// commit, DO UPDATE would clobber it with this page's stale snapshot, silently reverting a live
// write back to the text Backfill saw moments earlier. DO NOTHING cannot do that - whichever
// inserts first wins, and Backfill is content to have lost that race, since the trigger's version
// is always at least as fresh. Postgres always executes every data-modifying CTE in a WITH list to
// completion, whether or not the main query reads its output (the documented writable-CTE
// behavior), so the insert runs in full even though the final SELECT reads only the plain "page"
// rows - which it must: a conflicting row is absent from a RETURNING list by construction (ON
// CONFLICT DO NOTHING returns nothing for it), but the checkpoint still needs to advance past it.
func (s backfillSource) sql() string {
	where := s.ordExpr + " > $1"
	if s.extraWhere != "" {
		where = s.extraWhere + " and " + where
	}
	return fmt.Sprintf(`
		with page as (
			select %s as id, %s as text
			from %s
			where %s
			order by %s limit $2
		), upsert as (
			insert into embeddings (kind, id, text_snapshot, content_hash)
			select '%s', id, text, encode(sha256(convert_to(text, 'UTF8')), 'hex') from page
			on conflict (kind, id) do nothing
		)
		select id from page order by id`, s.idExpr, s.textExpr, s.tableExpr, where, s.ordExpr, s.kind)
}

// Backfill enqueues every existing row of every searchable kind - content this Dispatch was
// already carrying before meaning search existed, which no write-time trigger ever saw - then
// drains the queue to completion, reporting progress to out as it goes.
//
// It is resumable across restarts: each kind's checkpoint (embeddings_backfill_progress) commits
// after every page, so an interrupted run picks up at the next key rather than rescanning rows it
// already enqueued; enqueueing itself is idempotent (ON CONFLICT DO NOTHING), so running a
// finished kind again does no further work beyond one cheap scan that finds nothing past its
// checkpoint. It runs in small, short transactions (backfillPageSize rows at a time, one
// transaction per page) rather than one long scan, so it does not hold locks a concurrent write
// would wait behind. The five kinds' own tables are independent, so they enqueue concurrently.
//
// The drain loop that follows waits out backoff rather than stopping the moment one ProcessBatch
// call finds nothing immediately eligible: a row mid-backoff (still retrying on schedule) is not
// the same as the queue being empty, and a sustained Bedrock throttle backs off the whole batch
// cadence (see ProcessBatch, scan) rather than dead-lettering the rows it hit. Backfill only stops
// early if ctx is cancelled or a database error makes scheduling a retry itself fail (blocked).
func Backfill(ctx context.Context, deps Deps, out io.Writer) (BackfillReport, error) {
	report := BackfillReport{Enqueued: map[string]int64{}}
	if err := enqueueAllKinds(ctx, deps, out, &report); err != nil {
		return report, err
	}

	consecutiveThrottles := 0
	for {
		succeeded, failed, blocked, throttled, err := ProcessBatch(ctx, deps)
		if err != nil {
			return report, fmt.Errorf("backfill: embed queue: %w", err)
		}
		report.Embedded += int64(succeeded)
		report.Failed += int64(failed)
		if succeeded+failed > 0 {
			fmt.Fprintf(out, "backfill-embeddings: embedded %d, failed %d so far\n", report.Embedded, report.Failed)
		}
		if throttled {
			consecutiveThrottles++
			wait := batchBackoff(consecutiveThrottles)
			fmt.Fprintf(out, "backfill-embeddings: Bedrock is throttling; pausing %s before the next batch\n", wait.Round(time.Second))
			if !pauseFor(ctx, wait) {
				break
			}
			continue
		}
		consecutiveThrottles = 0
		if blocked {
			break
		}
		if succeeded+failed == 0 {
			count, nextDue, hasPending, err := pendingStatus(ctx, deps)
			if err != nil {
				return report, fmt.Errorf("backfill: check pending: %w", err)
			}
			if !hasPending {
				break
			}
			wait := time.Until(nextDue) + 100*time.Millisecond
			if wait < 0 {
				wait = 100 * time.Millisecond
			}
			fmt.Fprintf(out, "backfill-embeddings: %d row(s) still retrying on schedule; waiting %s\n", count, wait.Round(time.Second))
			if !pauseFor(ctx, wait) {
				break
			}
			continue
		}
		if !pauseFor(ctx, batchPause) {
			break
		}
	}

	pending, dead, err := finalCounts(ctx, deps)
	if err != nil {
		return report, fmt.Errorf("backfill: count pending: %w", err)
	}
	report.Pending, report.Dead = pending, dead
	return report, nil
}

// pendingStatus reports how many rows still need an embedding and are still eligible for
// automatic retry (excludes dead), and the earliest of their next_attempt_at - what Backfill's
// drain loop waits until before trying again, rather than treating "nothing eligible this
// instant" as "nothing left to do." One round trip, but two independent scalar subqueries rather
// than one combined `select count(*), min(...)`: each scalar subquery is its own subplan, planned
// separately, so the min subquery alone still gets Postgres's min/max-via-index-scan rewrite
// (preprocess_minmax_aggregates) over the embeddings_pending index - which a single statement
// naming both aggregates together loses, since that rewrite only fires when every aggregate in
// one query's target list is a bare MIN/MAX.
// pendingStatusQuery is pendingStatus's own SQL, a package-level const so
// TestPendingStatusMinQueryUsesTheIndexOnlyMinMaxRewrite can EXPLAIN the exact statement this
// function runs, not a hand-copied stand-in that could drift from it.
const pendingStatusQuery = `
	select
	  (select count(*) from embeddings where embedded_hash is distinct from content_hash and not dead),
	  (select min(next_attempt_at) from embeddings where embedded_hash is distinct from content_hash and not dead)
`

func pendingStatus(ctx context.Context, deps Deps) (count int64, nextDue time.Time, hasPending bool, err error) {
	var nextDuePtr *time.Time
	if err := deps.Store.Pool.QueryRow(ctx, pendingStatusQuery).Scan(&count, &nextDuePtr); err != nil {
		return 0, time.Time{}, false, err
	}
	if count == 0 {
		return 0, time.Time{}, false, nil
	}
	return count, *nextDuePtr, true, nil
}

// finalCounts is Backfill's own closing tally: pending (still retrying automatically, excludes
// dead) and dead (stopped retrying, needs a future write or a manual reset), reported separately
// so backfill-embeddings never conflates "still working on it" with "gave up."
func finalCounts(ctx context.Context, deps Deps) (pending, dead int64, err error) {
	err = deps.Store.Pool.QueryRow(ctx, `
		select count(*) filter (where embedded_hash is distinct from content_hash and not dead),
		       count(*) filter (where dead)
		from embeddings
	`).Scan(&pending, &dead)
	return pending, dead, err
}

// enqueueAllKinds runs every kind's resumable enqueue pass concurrently - each reads its own
// table(s) and writes its own embeddings_backfill_progress row, so the five have nothing to
// contend over beyond the shared connection pool.
func enqueueAllKinds(ctx context.Context, deps Deps, out io.Writer, report *BackfillReport) error {
	var mu sync.Mutex
	var wg sync.WaitGroup
	errs := make([]error, len(backfillSources))
	for i, source := range backfillSources {
		wg.Add(1)
		go func(i int, source backfillSource) {
			defer wg.Done()
			count, err := backfillKind(ctx, deps, source, out, &mu)
			if err != nil {
				errs[i] = fmt.Errorf("backfill %s: %w", source.kind, err)
				return
			}
			mu.Lock()
			report.Enqueued[source.kind] = count
			mu.Unlock()
		}(i, source)
	}
	wg.Wait()
	return errors.Join(errs...)
}

// backfillKind drives one source's resumable enqueue to completion. mu serializes this
// goroutine's writes to out (the only state the concurrent kinds in enqueueAllKinds share).
func backfillKind(ctx context.Context, deps Deps, source backfillSource, out io.Writer, mu *sync.Mutex) (int64, error) {
	var checkpoint string
	var done bool
	if err := deps.Store.Pool.QueryRow(ctx, `
		select coalesce(last_id, ''), done from embeddings_backfill_progress where kind = $1
	`, source.kind).Scan(&checkpoint, &done); err != nil {
		if !isNoRows(err) {
			return 0, fmt.Errorf("read checkpoint: %w", err)
		}
	}
	report := func(format string, args ...any) {
		mu.Lock()
		defer mu.Unlock()
		fmt.Fprintf(out, format, args...)
	}
	if done {
		report("backfill-embeddings: %s already complete (resume from %q)\n", source.kind, checkpoint)
		return 0, nil
	}
	sql := source.sql()
	var total int64
	for {
		rows, err := deps.Store.Pool.Query(ctx, sql, checkpoint, backfillPageSize)
		if err != nil {
			return total, fmt.Errorf("enqueue page: %w", err)
		}
		var lastID string
		var pageCount int64
		for rows.Next() {
			var id string
			if err := rows.Scan(&id); err != nil {
				rows.Close()
				return total, fmt.Errorf("enqueue page: %w", err)
			}
			lastID = id
			pageCount++
		}
		if err := rows.Err(); err != nil {
			rows.Close()
			return total, fmt.Errorf("enqueue page: %w", err)
		}
		rows.Close()
		total += pageCount
		pageDone := pageCount < backfillPageSize
		if pageCount > 0 {
			checkpoint = lastID
		}
		if _, err := deps.Store.Pool.Exec(ctx, `
			insert into embeddings_backfill_progress (kind, last_id, done, rows_enqueued, updated_at)
			values ($1, $2, $3, $4, now())
			on conflict (kind) do update
				set last_id = excluded.last_id, done = excluded.done,
				    rows_enqueued = embeddings_backfill_progress.rows_enqueued + $4, updated_at = now()
		`, source.kind, checkpoint, pageDone, pageCount); err != nil {
			return total, fmt.Errorf("write checkpoint: %w", err)
		}
		report("backfill-embeddings: %s enqueued %d (checkpoint %q)\n", source.kind, total, checkpoint)
		if pageDone {
			break
		}
	}
	return total, nil
}

func isNoRows(err error) bool {
	return errors.Is(err, pgx.ErrNoRows)
}
