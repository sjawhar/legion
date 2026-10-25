// Package embedqueue durably retries the model service's embedding calls for Dispatch's meaning
// search, on whichever place (Bedrock or Azure AI Foundry) the deployment configures. A write's
// AFTER INSERT/UPDATE trigger (0073-0077) upserts a pending row into embeddings with the latest
// text and a hash of it in the same transaction as the write, and this package's poller embeds
// whatever that enqueue left pending, with the same scan-then-retry shape as the event outbox
// (internal/dispatch/outbox): a write never fails because the embedder did, since the model
// service is never called inside the enqueueing transaction. A text with nothing to embed
// (nothingToEmbed) is settled with no vector and no model call instead. The coverage pass
// (coverage.go) adds what no trigger saw - items written before the triggers existed or with them
// off - and drops vectors another model or place made, so the server keeps meaning search's
// coverage itself.
package embedqueue

import (
	"context"
	"fmt"
	"log/slog"
	"math"
	"math/rand/v2"
	"sync"
	"time"

	"github.com/sjawhar/envoy/internal/dispatch/embed"
	"github.com/sjawhar/envoy/internal/dispatch/retry"
	"github.com/sjawhar/envoy/internal/dispatch/store"
)

const (
	retryInterval = 5 * time.Second
	// claimWindow is how long scanPending's claim keeps a row off every other scanner's list.
	// It is a plain UPDATE ... RETURNING, not a transaction held open across the embed call: this
	// package follows the event outbox's own rule (internal/dispatch/outbox: "an open cursor
	// holds its pooled connection until it closes, so publishing ... inside the loop would hold
	// one of the pool's four connections across all of it") - a lock held across a network call
	// is exactly what that rule exists to avoid. Advancing next_attempt_at instead costs one
	// short statement and lets the pollers of two envoy-dispatch processes (a rolling deploy runs
	// both) claim disjoint batches without either blocking on the other's embed call. Bisecting a
	// batch every one of whose rows fails can itself take minutes, so claimWindow alone is not
	// always enough; claimRenewal is what keeps that case's claim from lapsing mid-bisection; a
	// crashed claimer's rows become eligible again once it passes unrenewed, not stuck forever.
	claimWindow = 2 * time.Minute
	// batchPause separates one embed call from the next when neither is throttled: pending rows
	// arrive in bursts (a bulk import, a coverage pass adding everything no trigger saw), and
	// nothing upstream paces how fast this package reads them back out and calls the embedder -
	// without this, draining a large burst would fire batch after batch back to back, which is
	// how a well-behaved client still trips a provider's rate limit.
	batchPause = 200 * time.Millisecond
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
// notification cannot strand a row, and beside that loop runs the coverage pass (Cover) at
// startup and every coverageInterval, so content no trigger saw is embedded with no person-run
// step. The pass runs on its own goroutine: one over a large database takes a while, and the
// scan loop keeps draining what the pass has already added meanwhile. It returns once ctx is
// cancelled and both loops have stopped. Run does nothing when deps.Embedder is nil: Dispatch
// with no place configured, or one that could not be built at boot, never starts either loop,
// and the next boot with an embedder covers everything left behind.
func Run(ctx context.Context, deps Deps) {
	if deps.Embedder == nil {
		return
	}
	ctx = store.WithTransactionTracking(ctx)
	var wg sync.WaitGroup
	wg.Go(func() { runCoverage(ctx, deps) })
	defer wg.Wait()
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
// just the rows it just saw) while the model service is throttling - the poller is called again
// by Run's own ticker in any case, but a sustained throttle episode should not have it hammering
// the model service every five seconds regardless.
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
		if succeeded+failed < deps.Embedder.Limits().MaxBatchTexts || blocked {
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
// cancelled Run should stop promptly, not finish out its pacing delay.
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
// whole batch failed because the model service is throttling (embed.IsThrottled) rather than
// because of anything about the rows themselves - its caller (scan) uses that to back off its own
// cadence rather than hammering a provider that has already said to slow down. A row whose text
// has nothing to embed, which scanPending settles with no vector and no model call, counts as
// committed: it is done, so a full batch of them keeps scan draining. Every poller drives the
// queue through this one function; scanPending's claim is what lets the pollers of two
// envoy-dispatch processes run safely at once, each claiming a different set of pending rows
// rather than racing to embed the same ones twice.
//
// A throttled whole-batch failure never counts toward any row's dead-letter threshold, however
// many times it recurs: it is infrastructure (the provider is rate-limiting this account), never
// evidence about a specific row's content - every row in the batch is retried as a whole, not
// dead-lettered. A non-throttled whole-batch failure of two or more rows is different: something
// about this batch's own content *might* have broken the request (a text the API refuses
// outright, say), and handleGroupFailure isolates which row(s) via bisectSplit, splitting the
// batch and re-embedding each half, recursively. A failure isolated down to a single row is
// decided by confirmSoloFailure: a fixed, known-good embedCanary probe sent immediately after
// that row's own failed call. Canary success proves the service, credentials and model were all
// working at that exact moment, so the row's own failure is confirmed permanent (dead-lettered
// once confirmed often enough); canary failure - including a throttle - means the condition is
// systemic, not this row's fault, and the row goes back to retry instead. This confirmation is
// local to the one row and its own canary call: it never depends on any other row in the batch,
// so a batch that was already exactly one row with no siblings at all is confirmed and
// dead-lettered exactly the same way as one isolated down to size one by bisection. A context
// cancellation - this call's own ctx ending mid-request, including embed.RateLimitedEmbedder's
// own pacing wait ending early - is never evidence about any row either, whatever error text
// came back: handleGroupFailure checks for it before confirmSoloFailure or bisectSplit ever run,
// and retries the whole group instead.
func ProcessBatch(ctx context.Context, deps Deps) (succeeded, failed int, blocked, throttled bool, err error) {
	rows, settled, err := scanPending(ctx, deps)
	if err != nil {
		return 0, 0, false, false, err
	}
	if len(rows) == 0 {
		return settled, 0, false, false, nil
	}
	if ctx.Err() != nil {
		blocked = retryRows(ctx, deps, rows, false)
		return settled, len(rows), blocked, false, nil
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
	return settled + result.succeeded, len(rows) - result.succeeded, blocked, result.throttled, nil
}

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
// reservation ever exceeds the embedder's BackgroundTokensPerMinute), committing each group's
// vectors to the database as soon as that group's own Embed call succeeds: a later group's
// failure never re-embeds an earlier group's already-committed rows, and a group that itself
// fails non-throttled is handed to handleGroupFailure, which isolates it without ever
// re-embedding that exact group again. The returned bisectResult accumulates every group's own
// outcome across the whole call. A genuine infrastructure error (commitEmbeddings' own) stops the
// loop immediately and propagates as-is, abandoning whatever groups were not yet attempted -
// ProcessBatch and bisectSplit both treat it as a hard failure, never a reason to retry row by
// row.
func embedRows(ctx context.Context, deps Deps, rows []pendingRow, renewal *claimRenewal) bisectResult {
	var result bisectResult
	ceiling := deps.Embedder.Limits().BackgroundTokensPerMinute
	for _, group := range chunkForBudget(rows, ceiling) {
		if ctx.Err() != nil {
			result.retry = append(result.retry, group.rows...)
			continue
		}
		texts := make([]string, len(group.rows))
		for i, row := range group.rows {
			texts[i] = row.text
		}
		renewal.renew(ctx)
		reserveTokens(ctx, deps, group.tokens, ceiling, renewal)
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
// (embed.EstimateTokens) each stay at or under ceiling, the embedder's own
// BackgroundTokensPerMinute, in original order, so reserveTokens never has to reserve more than
// one minute's worth of budget for a single group. A row whose own estimate alone meets or
// exceeds the ceiling - an unusually dense maxInputChars-length document, or any long text under
// a small quota - still gets a group of its own rather than being split further: EstimateTokens
// already truncates at maxInputChars, so even that worst case is a known, bounded size, and one
// such row is the smallest unit this package ever embeds. Each group's tokens field is the sum of
// its own rows' estimates, computed here once and carried forward rather than re-estimated from
// the group's joined texts later.
func chunkForBudget(rows []pendingRow, ceiling int) []budgetedGroup {
	if len(rows) == 0 {
		return nil
	}
	groups := make([]budgetedGroup, 0, 1)
	start, tokens := 0, 0
	for i, row := range rows {
		rowTokens := embed.EstimateTokens([]string{row.text})
		if i > start && tokens+rowTokens > ceiling {
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
// against the embedder's BackgroundTokensPerMinute in the shared, database-backed budget
// (reserveTokens), waiting out whatever debt that reservation leaves - renewing renewal's claim
// during a wait longer than renewClaimInterval, not only before or after it. embedRows' own
// per-group reservations call reserveTokens directly with chunkForBudget's already-computed token
// total instead of this helper, so confirmSoloFailure's one-off embedCanary probe is its only
// caller. Best-effort: a reservation that fails to read or write (a transient database error) is
// logged and never blocks or fails the batch - embed.RateLimitedEmbedder's own in-process,
// reactive AIMD backoff is the backstop if this proactive ceiling is ever skipped or
// under-estimates a call.
func reserveBackgroundTokens(ctx context.Context, deps Deps, texts []string, renewal *claimRenewal) {
	reserveTokens(ctx, deps, embed.EstimateTokens(texts), deps.Embedder.Limits().BackgroundTokensPerMinute, renewal)
}

// reserveTokens draws tokens from embeddings_rate_limit's one shared row (migration 0078),
// refilling it continuously at ceilingPerMinute/60 tokens per second and capping it at
// ceilingPerMinute (one statement does both: a plain token bucket), then waits out whatever
// negative balance - debt - that reservation leaves before returning, so the caller's real
// model-service call never races ahead of the budget it just drew down. The row is in the
// database, not in-process memory, because a place's quota belongs to the whole account or
// deployment, not to one caller: every envoy-dispatch process's background work draws from the
// same bucket, and embed.RateLimitedEmbedder's in-process, reactive pacing alone cannot keep a
// live search request in one process safe from throttling another process's background work
// causes. ceilingPerMinute is the configured place's BackgroundTokensPerMinute (two thirds of its
// quota, the rest left to live queries) in estimated tokens (embed.EstimateTokens counts 4
// characters a token), not what the model service counts. The cap and the refill together can
// release about twice the ceiling in one minute, so a minute can pass the quota even when the
// estimate is exact: through LEGION-549's Bedrock backfill (2026-10-11, 72 minutes, 15,117 rows)
// Bedrock counted about 7% more input tokens than the estimate, about 220,600 a minute against
// a 300,000 quota with 8 minutes above it (peak 431,590), and throttled none of it.
//
// chunkForBudget already keeps one reservation's own tokens at or under ceilingPerMinute, but the
// wait can still run past renewClaimInterval when other debt was already on the row (another
// process's, or this one's own earlier groups) - renewal is woken (renew) at every
// renewClaimInterval-sized slice of the wait, not only before or after it, so a claim never lapses
// mid-wait; renewal.renew is a safe no-op on nil (a caller with no claim to protect, such as a
// test exercising the budget alone). ceilingPerMinute is a parameter rather than read from deps
// here so a test can use a tiny ceiling and observe a real, short wait instead of needing to
// simulate a whole minute.
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

// nothingToEmbed renders, over the text expression expr, the one SQL test for a text with nothing
// to embed: no character but white space, the empty string included. Such a text matches nothing
// by meaning, so it needs no vector, and a model service may refuse it outright (Azure AI
// Foundry's deployment answers HTTP 500 to an empty input, which bisection would confirm as the
// row's own failure and dead-letter). The queue settles its row with no model call
// (scanPending, settleDeadNothingToEmbed), and the coverage pass and report leave the item out
// (coverageSource.items).
func nothingToEmbed(expr string) string {
	return "(" + expr + " !~ '[^[:space:]]')"
}

// settledNothingToEmbed is the SET clause that settles a row whose text has nothing to embed, as
// commitEmbeddings would commit it but with no vector: embedded at its current text
// (embedded_hash = content_hash, so no scan selects it again) by the configured identity (the
// parameter identity names), with a committed row's retry state, and alive. A later text with
// something to embed changes content_hash, and the row is pending again like any other.
func settledNothingToEmbed(identity string) string {
	return `embedding = null, embedded_hash = e.content_hash, model = ` + identity + `::text, embedded_at = now(),
		attempt_count = 0, confirmed_failures = 0, dead = false, dead_model = null, next_attempt_at = now()`
}

// scanPending claims at most one embedder batch (Limits().MaxBatchTexts) of pending rows,
// oldest-eligible first, atomically advancing their next_attempt_at by claimWindow so a
// concurrent scanner (another envoy-dispatch process's poller) does not also claim them - without
// holding a lock across the embed call that follows; see claimWindow's own comment for why not.
// In the same statement it settles each of those rows whose text has nothing to embed
// (nothingToEmbed, settledNothingToEmbed) instead of claiming it, and reports how many it settled:
// no such row ever reaches the embedder, and the batch it returns holds only the rest. head locks
// every row it picks, and FOR UPDATE re-reads a row a write changed since the statement began, so
// whether a row has nothing to embed and the content_hash settling records are read from the
// same text: a write landing after head picked the row waits for this statement, and its own
// trigger makes the row pending again with the new text.
func scanPending(ctx context.Context, deps Deps) (pending []pendingRow, settled int, err error) {
	rows, err := deps.Store.Pool.Query(ctx, `
		with head as (
			select kind, id, `+nothingToEmbed("text_snapshot")+` as nothing from embeddings
			where embedded_hash is distinct from content_hash
			  and next_attempt_at <= now()
			  and not dead
			order by next_attempt_at, kind, id
			limit $1
			for update skip locked
		),
		settled as (
			update embeddings e set `+settledNothingToEmbed("$3")+`
			from head where head.nothing and e.kind = head.kind and e.id = head.id
			returning e.kind, e.id
		),
		claimed as (
			update embeddings e set next_attempt_at = now() + $2
			from head where not head.nothing and e.kind = head.kind and e.id = head.id
			returning e.kind, e.id, e.text_snapshot, e.content_hash, e.attempt_count, e.confirmed_failures
		)
		select false, kind, id, text_snapshot, content_hash, attempt_count, confirmed_failures from claimed
		union all
		select true, kind, id, '', '', 0, 0 from settled
	`, deps.Embedder.Limits().MaxBatchTexts, claimWindow, deps.Embedder.Identity())
	if err != nil {
		return nil, 0, fmt.Errorf("scan pending embeddings: %w", err)
	}
	defer rows.Close()
	for rows.Next() {
		var row pendingRow
		var wasSettled bool
		if err := rows.Scan(&wasSettled, &row.kind, &row.id, &row.text, &row.contentHash, &row.attempts, &row.confirmedFailures); err != nil {
			return nil, 0, fmt.Errorf("scan pending embeddings: %w", err)
		}
		if wasSettled {
			settled++
			continue
		}
		pending = append(pending, row)
	}
	if err := rows.Err(); err != nil {
		return nil, 0, fmt.Errorf("scan pending embeddings: %w", err)
	}
	return pending, settled, nil
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
	`, kinds, ids, hashes, literals, deps.Embedder.Identity())
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
// row's own content is at fault ever can. A row that dies records the embedder's Identity as
// dead_model: the failure was confirmed against that model and place, so the coverage pass
// (Cover) leaves it dead while that identity is configured and re-admits it once when another is.
// A dead row is still visible (`select * from embeddings where dead`) and still recoverable: a
// future write to the same content (embeddings_enqueue clears both counters, dead and dead_model
// the moment content_hash changes) or a manual `update embeddings set dead = false,
// confirmed_failures = 0` re-admits it to the next scan. It reports whether the batch is blocked,
// exactly as outbox's own scheduleRetry does.
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
		    next_attempt_at = v.next_attempt_at, dead = v.dead,
		    dead_model = case when v.dead then $8::text end
		from unnest($1::text[], $2::text[], $3::text[], $4::int[], $5::int[], $6::timestamptz[], $7::bool[])
		  as v(kind, id, content_hash, attempts, confirmed_failures, next_attempt_at, dead)
		where e.kind = v.kind and e.id = v.id and e.content_hash = v.content_hash
	`, kinds, ids, hashes, attempts, confirmedFailures, nextAttempts, deads, deps.Embedder.Identity())
	if err != nil {
		slog.Error("dispatch embedqueue: schedule retry", "rows", len(rows), "error", err)
		return true
	}
	return false
}
