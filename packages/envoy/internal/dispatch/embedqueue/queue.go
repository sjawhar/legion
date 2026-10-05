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
	// disjoint batches without either blocking on the other's embed call. Comfortably longer than
	// one ordinary batch of Bedrock calls should ever take, but not longer than bisecting a
	// batch that fails row by row can take (round 6: Deep measured 2m22.8s to fully bisect a
	// 96-row batch every one of whose rows failed) - claimRenewal is what keeps that case's claim
	// from lapsing mid-bisection; a crashed claimer's rows become eligible again once it passes
	// unrenewed, not stuck forever.
	claimWindow = 2 * time.Minute
	// renewClaimInterval is how often an in-flight bisection re-extends its own claim on every
	// row of the original batch (claimRenewal), comfortably inside claimWindow so a renewal
	// always lands before the claim it is refreshing could lapse.
	renewClaimInterval = claimWindow / 2
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
	// combined (round 6, Rev's reproduction): Bedrock throttles the whole account, not a
	// specific caller, so embed.RateLimitedEmbedder's own in-process, reactive AIMD pacing alone
	// left live search exposed to throttling an unconstrained background process caused by
	// itself, in a different process the reactive limiter never saw. 200,000 is chosen well
	// below the account's measured 300,000 tokens/minute Bedrock quota ("Bedrock quota" in this
	// PR's own body) - 100,000 tokens/minute of headroom, nowhere near what a live query (tens
	// of tokens) could ever need - and against this PR's own measured average of 488
	// tokens/chunk ("Bedrock capacity"), projects to about 410 chunks/minute, backfilling the
	// measured 24,862-chunk production corpus in about an hour.
	tokenRateCeiling = 200_000
)

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
	attempts                    int
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
// ordinary retry rather than dead-lettering any of them (round 6: before this, such an outage
// dead-lettered the whole pending queue in about 13-14 minutes, bringing back for every error
// class outside IsThrottled's allowlist the uniform dead-lettering rounds 1-3 removed). A context
// cancellation - this call's own ctx ending mid-request, including embed.RateLimitedEmbedder's
// own pace() returning early - is never evidence about any row either, whatever error text came
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
	texts := make([]string, len(rows))
	for i, row := range rows {
		texts[i] = row.text
	}
	reserveBackgroundTokens(ctx, deps, texts)
	vectors, embedErr := deps.Embedder.Embed(ctx, texts, embed.InputDocument)
	if embedErr != nil {
		// A single-row batch has no sibling to compare against, so a non-throttled failure here
		// is exactly as ambiguous as it always was - bisection's isolation only has evidentiary
		// value once there is more than one row to split and compare (see bisectBatch's own doc
		// comment): a genuine transient infrastructure failure (not one of IsThrottled's codes,
		// but not evidence about this one row's content either) must keep retrying indefinitely,
		// exactly like a throttle, rather than being dead-lettered the moment attempts run out.
		if embed.IsThrottled(embedErr) || len(rows) == 1 {
			slog.Error("dispatch embedqueue: embed batch", "rows", len(rows), "error", embedErr)
			blocked = retryRows(ctx, deps, rows, false)
			return 0, len(rows), blocked, embed.IsThrottled(embedErr), nil
		}
		slog.Error("dispatch embedqueue: embed batch failed, not throttled - bisecting to isolate the offending row(s)",
			"rows", len(rows), "error", embedErr)
		// rows as a whole is already known to fail non-throttled (embedErr above), so splitting
		// starts directly from its two halves (bisectSplit) rather than calling bisectBatch on
		// the full, already-failed set again, which would re-embed every row at once a second
		// time for no new information before ever splitting. renewal re-extends scanPending's
		// claim on every one of rows (not just whatever subtree bisection is currently in) every
		// renewClaimInterval, so a bisection slower than claimWindow - round 6: Deep measured
		// 2m22.8s to fully bisect a 96-row batch every one of whose rows failed, longer than
		// claimWindow's 2 minutes - never loses its claim to a concurrent scanner mid-bisection.
		renewal := newClaimRenewal(deps, rows)
		result := bisectSplit(ctx, deps, rows, renewal)
		if result.err != nil {
			return 0, 0, false, false, result.err
		}
		if result.succeeded == 0 && len(result.permanent) > 0 {
			// Nothing in this batch ever proved the service works, so every row bisection
			// isolated is systemic, not content-specific - see this function's own doc comment.
			result.retry = append(result.retry, result.permanent...)
			result.permanent = nil
		}
		if len(result.permanent) > 0 {
			blocked = retryRows(ctx, deps, result.permanent, true)
		}
		if len(result.retry) > 0 {
			blocked = retryRows(ctx, deps, result.retry, false) || blocked
		}
		return result.succeeded, len(rows) - result.succeeded, blocked, result.throttled, nil
	}
	committed, permanent, err := commitEmbeddings(ctx, deps, rows, vectors)
	if err != nil {
		return 0, 0, false, false, err
	}
	if len(permanent) > 0 {
		blocked = retryRows(ctx, deps, permanent, true)
	}
	return committed, len(permanent), blocked, false, nil
}

// bisectResult is bisectBatch's own report: every row it was given ends up in exactly one of
// succeeded (committed), permanent (isolated as the specific content a non-throttled failure
// attributes to - tentative until ProcessBatch confirms some other row of the same batch
// succeeded during this bisection; see ProcessBatch's own doc comment), or retry (not yet proven
// guilty - interrupted by a throttle or a cancellation, or still batched with others pending
// further bisection that a database error cut short). err is a genuine infrastructure failure
// (commitEmbeddings' own), never a reason to call any row permanent.
type bisectResult struct {
	succeeded int
	permanent []pendingRow
	retry     []pendingRow
	throttled bool
	err       error
}

// bisectBatch isolates which row(s) in rows actually broke a non-throttled whole-batch embed
// failure, rather than retrying the entire batch - including rows that would embed fine on
// their own - forever. It splits rows in half and re-embeds each half independently, recursively,
// down to one row at a time: a half that embeds cleanly is committed; a half that still fails
// non-throttled and holds more than one row is split again; a single row that still fails
// non-throttled is tentatively isolated as permanent, confirmed only once ProcessBatch sees some
// other row of the same original batch actually succeed (this function alone cannot tell a
// content-specific failure from an outage that fails every row identically). A throttled failure
// at any point aborts bisection for that subtree immediately - throttling is a shared-capacity
// signal, never evidence about specific content, and isolating "which row triggered it" would be
// meaningless (and would cost far more calls against a provider that has already said to slow
// down) - so every row in that subtree is reported for an ordinary retry instead, and throttled
// propagates up so ProcessBatch's caller still backs off its own cadence exactly as an
// outright-throttled whole batch would. A context cancellation reaching this call - its own ctx
// already done before it starts, or ending while its embed call was in flight (embed's own
// RateLimitedEmbedder.pace(), waiting out this package's pacing between calls, returns ctx.Err()
// exactly this way) - is likewise never evidence about content: every row in that subtree goes to
// retry, whatever the error otherwise reads like.
func bisectBatch(ctx context.Context, deps Deps, rows []pendingRow, renewal *claimRenewal) bisectResult {
	if len(rows) == 0 {
		return bisectResult{}
	}
	if ctx.Err() != nil {
		return bisectResult{retry: rows}
	}
	texts := make([]string, len(rows))
	for i, row := range rows {
		texts[i] = row.text
	}
	renewal.renew(ctx)
	reserveBackgroundTokens(ctx, deps, texts)
	vectors, embedErr := deps.Embedder.Embed(ctx, texts, embed.InputDocument)
	if embedErr == nil {
		committed, permanent, err := commitEmbeddings(ctx, deps, rows, vectors)
		if err != nil {
			return bisectResult{err: err}
		}
		return bisectResult{succeeded: committed, permanent: permanent}
	}
	if embed.IsThrottled(embedErr) {
		return bisectResult{retry: rows, throttled: true}
	}
	if ctx.Err() != nil {
		return bisectResult{retry: rows}
	}
	if len(rows) == 1 {
		slog.Error("dispatch embedqueue: bisection isolated the row a non-throttled batch failure attributes to, pending confirmation some other row of the batch embedded",
			"kind", rows[0].kind, "id", rows[0].id, "error", embedErr)
		return bisectResult{permanent: rows}
	}
	return bisectSplit(ctx, deps, rows, renewal)
}

// bisectSplit is bisectBatch's own split-and-merge step: split rows (already known to need
// isolating - every caller reaches it only after an embed of this exact set failed non-throttled)
// into halves, bisect each independently, and merge their reports. ProcessBatch calls it directly
// for the top-level batch rather than calling bisectBatch there, since bisectBatch would embed
// the same unsplit rows a second time - which ProcessBatch has already done and already knows
// fails - before ever splitting; bisectBatch's own recursion calls it once it has made that same
// determination for a half it was actually given to embed. The merge copies left's and right's
// permanent/retry rows into freshly allocated slices rather than appending onto one of them in
// place: rows[:mid] and rows[mid:] share rows' own backing array, so a bisectResult built
// straight from one of those slices (bisectBatch's size-1 and throttled/cancelled returns both
// are) still aliases it, and appending onto one with spare capacity - exactly what rows[:mid] has
// when mid < len(rows) - would silently overwrite the other half's rows.
func bisectSplit(ctx context.Context, deps Deps, rows []pendingRow, renewal *claimRenewal) bisectResult {
	mid := len(rows) / 2
	left := bisectBatch(ctx, deps, rows[:mid], renewal)
	if left.err != nil {
		return left
	}
	right := bisectBatch(ctx, deps, rows[mid:], renewal)
	if right.err != nil {
		return right
	}
	permanent := make([]pendingRow, 0, len(left.permanent)+len(right.permanent))
	permanent = append(permanent, left.permanent...)
	permanent = append(permanent, right.permanent...)
	toRetry := make([]pendingRow, 0, len(left.retry)+len(right.retry))
	toRetry = append(toRetry, left.retry...)
	toRetry = append(toRetry, right.retry...)
	return bisectResult{
		succeeded: left.succeeded + right.succeeded,
		permanent: permanent,
		retry:     toRetry,
		throttled: left.throttled || right.throttled,
	}
}

// claimRenewal re-extends scanPending's claim on every row of the original top-level batch a
// bisection is working through - not just whatever subtree it is currently in, since every row
// not yet committed or confirmed permanent is still this ProcessBatch call's responsibility until
// it returns - once renewClaimInterval has passed since the last renewal, so a bisection slower
// than claimWindow never loses its claim to a concurrent scanner partway through. Best-effort: a
// failed renewal is logged and never aborts bisection, since a row a concurrent scanner reclaims
// is merely processed twice, not corrupted - commitEmbeddings' own content_hash-matched update
// already makes a doubly-claimed row safe, just not free.
type claimRenewal struct {
	deps Deps
	rows []pendingRow
	last time.Time
}

func newClaimRenewal(deps Deps, rows []pendingRow) *claimRenewal {
	return &claimRenewal{deps: deps, rows: rows, last: time.Now()}
}

func (c *claimRenewal) renew(ctx context.Context) {
	if ctx.Err() != nil || time.Since(c.last) < renewClaimInterval {
		return
	}
	c.last = time.Now()
	kinds := make([]string, len(c.rows))
	ids := make([]string, len(c.rows))
	for i, row := range c.rows {
		kinds[i], ids[i] = row.kind, row.id
	}
	if _, err := c.deps.Store.Pool.Exec(ctx, `
		update embeddings e set next_attempt_at = now() + $3
		from unnest($1::text[], $2::text[]) as v(kind, id)
		where e.kind = v.kind and e.id = v.id
	`, kinds, ids, claimWindow); err != nil {
		slog.Error("dispatch embedqueue: bisection claim renewal failed", "error", err)
	}
}

// reserveBackgroundTokens is the one gate every background embed call in this package passes
// through - ProcessBatch's own top-level attempt, and every one of bisectBatch's recursive
// attempts - before it reaches deps.Embedder.Embed: it estimates texts' tokens
// (embed.EstimateTokens) and reserves them against tokenRateCeiling's shared, database-backed
// budget (reserveTokens), waiting out whatever debt that reservation leaves. Best-effort: a
// reservation that fails to read or write (a transient database error) is logged and never
// blocks or fails the batch - embed.RateLimitedEmbedder's own in-process, reactive AIMD backoff
// is the backstop if this proactive ceiling is ever skipped or under-estimates a call.
func reserveBackgroundTokens(ctx context.Context, deps Deps, texts []string) {
	reserveTokens(ctx, deps, embed.EstimateTokens(texts), tokenRateCeiling)
}

// reserveTokens draws tokens from embeddings_rate_limit's one shared row (migration 0077),
// refilling it continuously at ceilingPerMinute/60 tokens per second and capping it at
// ceilingPerMinute (one statement does both: a plain token bucket), then waits out whatever
// negative balance - debt - that reservation leaves before returning, so the caller's real
// Bedrock call never races ahead of the budget it just drew down. ceilingPerMinute is a
// parameter rather than always tokenRateCeiling so a test can use a tiny ceiling and observe a
// real, short wait instead of needing to simulate a whole minute.
func reserveTokens(ctx context.Context, deps Deps, tokens, ceilingPerMinute int) {
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
	pauseFor(ctx, wait)
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
		returning kind, id, text_snapshot, content_hash, attempt_count
	`, batchSize, claimWindow)
	if err != nil {
		return nil, fmt.Errorf("scan pending embeddings: %w", err)
	}
	defer rows.Close()
	var pending []pendingRow
	for rows.Next() {
		var row pendingRow
		if err := rows.Scan(&row.kind, &row.id, &row.text, &row.contentHash, &row.attempts); err != nil {
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
		    embedded_at = now(), attempt_count = 0, next_attempt_at = now()
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
// only when permanent is true - marks a row dead once its own attempts reach
// retry.DeadLetterAttempts. permanent distinguishes a row's own content failing
// (commitEmbeddings' non-finite-vector case) from a whole-batch embed failure (infrastructure,
// never evidence about any specific row) - the latter is retried indefinitely, however many times
// it recurs. A dead row is still visible (`select * from embeddings where dead`) and still
// recoverable: a future write to the same content (embeddings_enqueue clears dead the moment
// content_hash changes) or a manual `update embeddings set dead = false, attempt_count = 0`
// re-admits it to the next scan. It reports whether the batch is blocked, exactly as outbox's own
// scheduleRetry does.
func retryRows(ctx context.Context, deps Deps, rows []pendingRow, permanent bool) bool {
	kinds := make([]string, len(rows))
	ids := make([]string, len(rows))
	hashes := make([]string, len(rows))
	attempts := make([]int32, len(rows))
	nextAttempts := make([]time.Time, len(rows))
	deads := make([]bool, len(rows))
	for i, row := range rows {
		n := row.attempts + 1
		dead := permanent && n >= retry.DeadLetterAttempts
		if dead {
			slog.Error("dispatch embedqueue: row reached the dead-letter threshold, no further automatic retry", "kind", row.kind, "id", row.id, "attempts", n)
		}
		kinds[i], ids[i], hashes[i] = row.kind, row.id, row.contentHash
		attempts[i] = int32(n)
		nextAttempts[i] = time.Now().Add(retry.Delay(n))
		deads[i] = dead
	}
	_, err := deps.Store.Pool.Exec(ctx, `
		update embeddings e
		set attempt_count = v.attempts, next_attempt_at = v.next_attempt_at, dead = v.dead
		from unnest($1::text[], $2::text[], $3::text[], $4::int[], $5::timestamptz[], $6::bool[])
		  as v(kind, id, content_hash, attempts, next_attempt_at, dead)
		where e.kind = v.kind and e.id = v.id and e.content_hash = v.content_hash
	`, kinds, ids, hashes, attempts, nextAttempts, deads)
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
func pendingStatus(ctx context.Context, deps Deps) (count int64, nextDue time.Time, hasPending bool, err error) {
	var nextDuePtr *time.Time
	if err := deps.Store.Pool.QueryRow(ctx, `
		select
		  (select count(*) from embeddings where embedded_hash is distinct from content_hash and not dead),
		  (select min(next_attempt_at) from embeddings where embedded_hash is distinct from content_hash and not dead)
	`).Scan(&count, &nextDuePtr); err != nil {
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
