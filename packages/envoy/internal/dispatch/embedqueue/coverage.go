package embedqueue

import (
	"context"
	"fmt"
	"log/slog"
	"strings"
	"time"

	"github.com/sjawhar/envoy/internal/dispatch/duty"
	"github.com/sjawhar/envoy/internal/dispatch/embed"
	"github.com/sjawhar/envoy/internal/dispatch/model"
	"github.com/sjawhar/envoy/internal/dispatch/store"
)

// coverageStatementRows bounds how many rows one coverage statement changes. Each statement is
// its own short transaction, so a pass over a large database never holds locks a live write
// would wait behind for longer than one small statement takes.
const coverageStatementRows = 500

// identityRecheckInterval is how long, plus up to a tenth again, Run's coverage loop waits
// between runs of the identity steps (runCoverage), and after a failed run before it tries that
// run again. While a rolling deploy that changes the identity still has an old-configuration
// process embedding, that process commits vectors the new identity must drop; ten minutes was
// the whole pass's period before, and nothing faster is owed to a vector search already filters
// out by identity. A var, not a const, so a test can shrink it rather than wait out ten minutes.
var identityRecheckInterval = 10 * time.Minute

// issueKind is the one kind of row meaning search ranks that no indexer owns, and so the one
// kind this pass builds: insertMissingIssues and repairStaleIssues, the two statements that
// build a kind, are the issue's alone and take it as $1::text. A window kind
// (windowCoverageKinds) has no statement of that sort at all.
const issueKind = "issue"

// issueRows is every issue as (id, text, hash, written_at), the shape every statement of this
// pass and the report read an item in; windowRows gives a window kind's rows that same shape.
//
// id and text are the issue's write-time triggers' own expressions, byte for byte: 0073's on
// issues, as 0140 replaced it, and 0140's on a new version of an issue's primary document, which
// enqueue new.key and issue_embedding_text(new.title, issue_embedding_spec(new.key)). So a row
// this pass writes is the row a trigger would have written, and a row a trigger wrote is never
// seen as stale. hash is embeddings_enqueue's own content_hash expression (0072, 0118) over that
// text, so an item's hash equals the content_hash its trigger would store. What it selects from
// is the items search ranks (api/search.go's meaning candidate CTEs), which the trigger can
// exceed.
//
// Issues whose text has nothing to embed are kept in: repairStaleIssues needs them, so a row
// whose issue's text became empty with the trigger off takes that text and the queue settles it,
// dropping the vector of the text the item no longer holds.
const issueRows = `select s.id, s.text, encode(sha256(convert_to(s.text, 'UTF8')), 'hex') as hash, s.written_at
	from (select i.key as id, issue_embedding_text(i.title, issue_embedding_spec(i.key)) as text, i.created_at as written_at from issues i) s`

// issueItems is the issues worth a vector: those whose text has something to embed (not
// nothingToEmbed, which is the empty string or white space alone). An item with nothing to embed
// is not covered, since it matches nothing by meaning, so insertMissingIssues gives it no row
// and the queue settles the row it already holds with no vector.
var issueItems = searchableItems(issueRows)

// windowCoverageKinds is every kind whose rows the indexers own: one row per window of a
// document's, a comment's, a message's blocks or of an ask's parts (INFRA-370). Migration
// 20261102233626 and the three after it dropped the whole-text triggers these replace, so
// every such row is written by the indexer that wrote the blocks it covers, in that write's
// own transaction, or by the block-index tick for an owner written before this build. No
// statement of this pass may invent one: the pass has no way to window an owner, and a row it
// inserted for a window that no longer exists would be embedded for ever at a text no reader
// can reach. So no statement here builds a window row (insertMissingIssues and repairStaleIssues
// are the issue's alone), a window kind takes the settling and the identity steps alone
// (coverPass), and it is counted in the report from its own rows (windowRows).
var windowCoverageKinds = []string{"document_block", "comment_block", "message_block", "ask_part"}

// windowRows is a window kind's items, in the shape issueRows gives the issue's: its own
// rows, since a window exists only while the indexer that wrote it says so, and its text and
// hash are the ones that indexer wrote. Reading them from embeddings itself is what makes the
// report count the windows of the database rather than items the pass has no way to window.
func windowRows(kind string) string {
	return fmt.Sprintf(`select s.id, s.text, s.hash, s.written_at from (
		select e.id, e.text_snapshot as text, e.content_hash as hash, e.pending_since as written_at
		from embeddings e where e.kind = '%s') s`, kind)
}

// searchableItems holds a kind's items (issueRows, windowRows) to those whose text has something
// to embed.
func searchableItems(items string) string {
	return items + " where not " + nothingToEmbed("s.text")
}

// insertMissingIssues adds a pending row for up to $2 issues ($1::text is issueKind) that have
// none, exactly as the trigger's embeddings_enqueue would insert it. ON CONFLICT DO NOTHING,
// never DO UPDATE: a row a trigger inserts between this statement's read and its insert holds
// text at least as fresh as what this statement read, and DO UPDATE would revert it to the older
// text.
var insertMissingIssues = `
	insert into embeddings (kind, id, text_snapshot, content_hash)
	select $1::text, item.id, item.text, item.hash from (` + issueItems + `) item
	where not exists (select 1 from embeddings e where e.kind = $1::text and e.id = item.id)
	limit $2
	on conflict (kind, id) do nothing`

// repairStaleIssues points up to $2 issue rows whose content_hash is not the hash of the issue's
// current text (text changed with the trigger off, as a restore or replication does) at that
// text, starting the row's wait and attempts again as embeddings_enqueue does on a text change;
// a text with nothing to embed is repaired too, and scanPending then settles the row.
// It is a compare-and-swap: the stale CTE captures each row's content_hash as seen, and the
// update writes only where the row still holds seen. Under READ COMMITTED an update that finds
// its row changed since the statement's snapshot re-checks its WHERE against the newest version,
// so a trigger write committing after the read makes the update skip that row, keeping the
// trigger's text, hash, claim and counters. The CTE takes no row lock (FOR UPDATE would re-read
// the newest version into seen and defeat the check).
var repairStaleIssues = `
	with stale as (
		select e.kind, e.id, e.content_hash as seen, item.text, item.hash
		from (` + issueRows + `) item
		join embeddings e on e.kind = $1::text and e.id = item.id
		where e.content_hash <> item.hash
		limit $2
	)
	update embeddings e
	set text_snapshot = stale.text, content_hash = stale.hash, attempt_count = 0,
	    confirmed_failures = 0, dead = false, dead_model = null, next_attempt_at = now(),
	    pending_since = now()
	from stale
	where e.kind = stale.kind and e.id = stale.id and e.content_hash = stale.seen`

// dropForeignVectors takes up to $3 of the kind's rows holding a vector another identity made
// ($2 is the configured one) out of the HNSW index's rows: search compares a stored vector only
// with a query vector of its own identity, and leaving foreign vectors in the index would cost
// filtered search recall. Every row of the kind, not only the ones this pass builds, so no foreign
// vector survives anywhere. A row embedded at its current text (embedded_hash = content_hash) has
// no claim on it, so it becomes pending now with fresh attempts; a row already pending keeps its
// claim, backoff and pending_since. SKIP LOCKED leaves a row a worker is committing to the next
// pass. Once done the row matches no longer, so a second pass changes nothing. Cover runs it only
// once the configured embedder has answered (identityStepsProven): a vector dropped for an
// embedder that cannot embed leaves its item with no vector at all.
const dropForeignVectors = `
	with picked as (
		select kind, id from embeddings
		where kind = $1 and embedding is not null and model is distinct from $2
		limit $3
		for update skip locked
	)
	update embeddings e
	set embedding = null, model = null, embedded_hash = null, embedded_at = null,
	    attempt_count = case when e.embedded_hash = e.content_hash then 0 else e.attempt_count end,
	    confirmed_failures = case when e.embedded_hash = e.content_hash then 0 else e.confirmed_failures end,
	    next_attempt_at = case when e.embedded_hash = e.content_hash then now() else e.next_attempt_at end,
	    pending_since = case when e.embedded_hash = e.content_hash then now() else e.pending_since end
	from picked
	where e.kind = picked.kind and e.id = picked.id`

// readmitForeignDead gives up to $3 of the kind's rows confirmed dead under another identity ($2
// is the configured one; 0118 recorded embed.BedrockModel for every row dead before it, and only a
// row the release before 0118 confirmed dead during the roll records none) this identity's
// attempts, once: the content failed against another model or place, which says nothing about this
// one. A row that dies again records the configured identity (retryRows) and is not re-admitted
// again. Like dropForeignVectors, Cover runs it only once the configured embedder has answered:
// attempts against an embedder that cannot embed would only fail.
const readmitForeignDead = `
	with picked as (
		select kind, id from embeddings
		where kind = $1 and dead and dead_model is distinct from $2
		limit $3
		for update skip locked
	)
	update embeddings e
	set dead = false, confirmed_failures = 0, attempt_count = 0, next_attempt_at = now(),
	    pending_since = now(), dead_model = null
	from picked
	where e.kind = picked.kind and e.id = picked.id`

// settleDeadNothingToEmbed settles up to $3 of the kind's dead rows whose text has nothing to
// embed ($2 is the configured identity) as scanPending settles a pending one, which a dead row
// never reaches: such a row died from a model service refusing its text (Azure AI Foundry answers
// HTTP 500 to an empty input) before the queue settled these, and it needs no vector. SKIP LOCKED
// leaves a row a write is changing to the next pass; a row the write gives text to embed matches
// no longer, its trigger having made it alive and pending.
var settleDeadNothingToEmbed = `
	with picked as (
		select kind, id from embeddings
		where kind = $1 and dead and ` + nothingToEmbed("text_snapshot") + `
		limit $3
		for update skip locked
	)
	update embeddings e
	set ` + settledNothingToEmbed("$2") + `
	from picked
	where e.kind = picked.kind and e.id = picked.id`

// foreignRows tells whether any row of the covered kinds ($1) holds what dropForeignVectors
// or readmitForeignDead would change for the configured identity ($2): a vector another identity
// made, or a row dead under another identity. Each exists stops at the first such row, so a
// database that holds some answers quickly; one that holds none reads every row once.
const foreignRows = `
	select exists (
		select 1 from embeddings where kind = any($1) and embedding is not null and model is distinct from $2
	) or exists (
		select 1 from embeddings where kind = any($1) and dead and dead_model is distinct from $2
	)`

// foreignRowsLeft counts the rows foreignRows looks for, the vectors and the dead rows apart, for
// the warning Cover logs when it leaves them in place.
const foreignRowsLeft = `
	select count(*) filter (where embedding is not null and model is distinct from $2),
	       count(*) filter (where dead and dead_model is distinct from $2)
	from embeddings where kind = any($1)`

// coverageKinds is every kind meaning search covers, the one this pass builds (issueKind)
// and the window kinds the indexers own (windowCoverageKinds), and the argument foreignRows and
// foreignRowsLeft take.
var coverageKinds = append([]string{issueKind}, windowCoverageKinds...)

// coverStep is one statement of the coverage pass, run until it changes nothing.
type coverStep struct {
	changed *int64
	sql     string
	args    []any
}

// run executes step until a statement changes nothing, adding what each changed to its counter
// and waking Run's scan loop after each statement that changed rows (Deps.signalEnqueued): the
// pass writes embeddings directly, so no notification announces what it made pending.
func (step coverStep) run(ctx context.Context, deps Deps, kind string) error {
	for {
		tag, err := deps.Store.Pool.Exec(ctx, step.sql, step.args...)
		if err != nil {
			return fmt.Errorf("coverage pass: %s: %w", kind, err)
		}
		if tag.RowsAffected() == 0 {
			return nil
		}
		*step.changed += tag.RowsAffected()
		deps.signalEnqueued()
	}
}

// decideIdentitySteps decides whether this pass runs its identity steps (dropForeignVectors,
// readmitForeignDead): only once the configured embedder has answered, since a misconfigured one
// (a misnamed Azure deployment answers 404 DeploymentNotFound) would otherwise drop, at its first
// pass, every vector the identity before it made, with nothing able to embed those items again.
// With no row those steps would change (foreignRows) it runs neither and calls the embedder zero
// times; skipped is false, as there is nothing to skip. With such rows it runs them, with no call,
// when an Embed this process's queue made has already succeeded (Deps.proven), and otherwise
// embeds embedCanary (proveEmbedder): a success is proof; a failure, a throttle included, skips
// them until a later run (runCoverage), logged once at WARN with the identity, the canary's
// error, and how many vectors and dead rows stay in place.
func decideIdentitySteps(ctx context.Context, deps Deps, identity string) (run, skipped bool, err error) {
	var foreign bool
	if err := deps.Store.Pool.QueryRow(ctx, foreignRows, coverageKinds, identity).Scan(&foreign); err != nil {
		return false, false, fmt.Errorf("coverage pass: find other identities' rows: %w", err)
	}
	if !foreign {
		return false, false, nil
	}
	if deps.embedderProven() {
		return true, false, nil
	}
	canaryErr := proveEmbedder(ctx, deps)
	if canaryErr == nil {
		return true, false, nil
	}
	if ctx.Err() != nil {
		return false, true, fmt.Errorf("coverage pass: %w", ctx.Err())
	}
	var vectors, dead int64
	if err := deps.Store.Pool.QueryRow(ctx, foreignRowsLeft, coverageKinds, identity).Scan(&vectors, &dead); err != nil {
		return false, true, fmt.Errorf("coverage pass: count other identities' rows: %w", err)
	}
	slog.Warn("dispatch embedqueue: coverage pass left other identities' vectors and dead rows in place: the configured embedder has not embedded anything yet and a canary failed; they are handled once it answers",
		"identity", identity, "canaryError", canaryErr, "foreignVectorsLeft", vectors, "foreignDeadLeft", dead)
	return false, true, nil
}

// identityRun is what one run of the identity steps (runIdentitySteps) found and did.
type identityRun struct {
	// skipped is whether the run left rows of another identity in place because the embedder
	// has not answered yet.
	skipped             bool
	dropped, readmitted int64
}

// runIdentitySteps decides whether to run the identity steps (decideIdentitySteps) and runs them
// over every covered kind when it does - the same kinds decideIdentitySteps looked for rows of
// another identity in (coverageKinds), the windows the indexers own among them, since an
// old-configuration process commits those vectors too.
func runIdentitySteps(ctx context.Context, deps Deps, identity string) (identityRun, error) {
	run, skipped, err := decideIdentitySteps(ctx, deps, identity)
	if err != nil {
		return identityRun{}, err
	}
	result := identityRun{skipped: skipped}
	if !run {
		return result, nil
	}
	for _, kind := range coverageKinds {
		for _, step := range []coverStep{
			{&result.dropped, dropForeignVectors, []any{kind, identity, coverageStatementRows}},
			{&result.readmitted, readmitForeignDead, []any{kind, identity, coverageStatementRows}},
		} {
			if err := step.run(ctx, deps, kind); err != nil {
				return identityRun{}, err
			}
		}
	}
	return result, nil
}

// Cover is the coverage pass: for every covered kind (coverageKinds) it settles each dead row
// whose text has nothing to embed and, for the kind it builds (issueKind), adds a row for
// each item that has none and repairs each row whose text is stale; then, once the configured
// embedder has answered (decideIdentitySteps), it drops each vector another identity made and
// re-admits each other row dead under another identity; and it logs one line with what it
// changed, whether it skipped those two steps, and the coverage that leaves (Coverage). Every
// step is a statement of at most coverageStatementRows rows in its own short transaction,
// repeated until one changes nothing, and each is safe beside a live write and beside another
// process's pass, so it takes no lease: two envoy-dispatch processes may run it at once, as both
// do at boot during a rolling deploy, and a lease would let an old-configuration process's pass
// stand in for the new one's. One deployment configures one identity; two processes configured
// differently would each drop the other's vectors, which is why every replica gets the same
// settings.
//
// A kind the indexers own (windowCoverageKinds) takes the settling and the identity steps
// alone. The pass has no way to window an owner, so a row it added would name a window nothing
// wrote, and every whole-text row the block-index tick retires would come back at the next
// boot, be embedded again, and be retired again, for ever.
func Cover(ctx context.Context, deps Deps) error {
	_, err := coverPass(ctx, deps)
	return err
}

// coverPass is Cover, returning what its identity steps found for runCoverage.
func coverPass(ctx context.Context, deps Deps) (identityRun, error) {
	identity := deps.Embedder.Identity()
	var added, repaired, settled int64
	// Every covered kind settles its dead rows with nothing to embed; only the kind this pass
	// builds (issueKind) takes the other two steps, since no statement here may invent a
	// window.
	for _, kind := range coverageKinds {
		step := coverStep{&settled, settleDeadNothingToEmbed, []any{kind, identity, coverageStatementRows}}
		if err := step.run(ctx, deps, kind); err != nil {
			return identityRun{}, err
		}
	}
	for _, step := range []coverStep{
		{&added, insertMissingIssues, []any{issueKind, coverageStatementRows}},
		{&repaired, repairStaleIssues, []any{issueKind, coverageStatementRows}},
	} {
		if err := step.run(ctx, deps, issueKind); err != nil {
			return identityRun{}, err
		}
	}
	// Settling runs first, so a dead row with nothing to embed is never what makes the pass
	// send a canary.
	steps, err := runIdentitySteps(ctx, deps, identity)
	if err != nil {
		return identityRun{}, err
	}
	report, err := Coverage(ctx, deps.Store, identity)
	if err != nil {
		return identityRun{}, fmt.Errorf("coverage pass: %w", err)
	}
	var oldest any // null in the log when nothing lacks, as in the report
	if report.Total.OldestLackingSeconds != nil {
		oldest = *report.Total.OldestLackingSeconds
	}
	slog.Info("dispatch embedqueue: coverage pass",
		"identity", identity, "added", added, "repaired", repaired,
		"foreignVectorsDropped", steps.dropped, "deadNothingToEmbedSettled", settled, "foreignDeadReadmitted", steps.readmitted,
		"identityStepsSkipped", steps.skipped,
		"items", report.Total.Items, "current", report.Total.Current, "lacking", report.Total.Lacking,
		"failed", report.Total.Failed, "oldestLackingSeconds", oldest)
	return steps, nil
}

// recheckIdentity is the coverage loop's later run: the identity steps alone, logged when they
// changed anything.
func recheckIdentity(ctx context.Context, deps Deps) (identityRun, error) {
	identity := deps.Embedder.Identity()
	steps, err := runIdentitySteps(ctx, deps, identity)
	if err == nil && (steps.dropped > 0 || steps.readmitted > 0) {
		slog.Info("dispatch embedqueue: coverage identity recheck",
			"identity", identity, "foreignVectorsDropped", steps.dropped, "foreignDeadReadmitted", steps.readmitted)
	}
	return steps, err
}

// runCoverage runs Cover once at start, and the identity steps alone every
// identityRecheckInterval (jittered) after it, for as long as the process runs. Writes reach the
// queue through their triggers in their own transactions, so the rest of the pass - what no
// trigger saw - has nothing new to find until the next boot.
//
// The identity steps do have something new to find at any moment: a process of the configuration
// before a rolling deploy keeps embedding until it stops, and every vector it commits is one
// this identity must drop. Nothing else would drop it. The scan loop selects only rows whose
// embedded_hash is distinct from content_hash, which such a row's is not, and search compares a
// stored vector only with a query vector of its own identity, so that item would be matched by
// nothing until some later boot's pass. A run that finds no such row is evidence about the
// moment it looked and about no later one, which is why the loop keeps looking rather than
// stopping at the first clean run.
//
// What that costs, while there is nothing to do, is one statement per interval: foreignRows, the
// two exists over the covered kinds, which stops at the first row of another identity and
// otherwise reads their rows once. A run that finds one pays two statements per kind on top,
// each repeated until it changes nothing, and only while such rows remain. One read of the
// queue's rows every ten minutes is the right price for what it buys: without it a vector
// another identity commits after this process's boot pass is matched by no search and reached
// by no scan for as long as this process lives.
//
// A run that skipped the steps because the embedder had not answered runs them again as soon as
// it does (Deps.proven: the queue's first successful embed), not ten minutes later. A failed run
// is logged and tried again after identityRecheckInterval, a failed boot pass as the whole pass
// since it may have failed before its later steps: every step is idempotent, so nothing is lost
// by waiting. The context ending returns at once.
func runCoverage(ctx context.Context, deps Deps) {
	pass := coverPass
	for {
		steps, err := pass(ctx, deps)
		var answered <-chan struct{}
		switch {
		case ctx.Err() != nil:
			return
		case err != nil:
			slog.Error("dispatch embedqueue: coverage pass failed; it runs again later", "error", err, "retryIn", identityRecheckInterval)
		default:
			pass = recheckIdentity
			if steps.skipped {
				answered = deps.proven.answered()
			}
		}
		timer := time.NewTimer(duty.Jittered(identityRecheckInterval))
		select {
		case <-ctx.Done():
			timer.Stop()
			return
		case <-timer.C:
		case <-answered:
			timer.Stop()
		}
	}
}

// coverageQuery counts, per covered kind, its items left-joined to their embeddings rows, in
// one statement so every count reads one snapshot: the issue's items are issueItems, a window
// kind's its own rows (windowRows). $1 is the configured identity, the empty string when there
// is none, and then no item is current. greatest ignores a null, so an item with no row ages
// from when it was written; the oldest age is null when nothing of the kind lacks.
var coverageQuery = func() string {
	arm := func(kind, items string) string {
		return fmt.Sprintf("select '%s'::text as kind, item.id, item.hash, item.written_at from (%s) item", kind, items)
	}
	arms := make([]string, 0, 1+len(windowCoverageKinds))
	arms = append(arms, arm(issueKind, issueItems))
	for _, kind := range windowCoverageKinds {
		arms = append(arms, arm(kind, searchableItems(windowRows(kind))))
	}
	return `
		with items as (` + strings.Join(arms, "\n union all ") + `),
		judged as (
			select item.kind, item.written_at, e.pending_since, coalesce(e.dead, false) as dead,
			       coalesce($1::text <> '' and e.embedding is not null and e.model = $1::text and e.embedded_hash = item.hash, false) as current
			from items item
			left join embeddings e on e.kind = item.kind and e.id = item.id
		)
		select kind, count(*), count(*) filter (where current), count(*) filter (where dead and not current),
		       floor(extract(epoch from now() - min(greatest(pending_since, written_at)) filter (where not current and not dead)))::bigint
		from judged
		group by kind`
}()

// Coverage reports how much of what meaning search covers (coverageKinds) has a current vector
// from identity, the configured embedder's (empty when none is configured). Its definitions are
// model.SearchCoverageCounts'; Total sums the kinds, and its oldest lacking age is theirs.
func Coverage(ctx context.Context, database *store.Store, identity string) (model.SearchCoverage, error) {
	var report model.SearchCoverage
	if identity != "" {
		place, name := embed.IdentityPlace(identity)
		report.Identity, report.Place, report.Model = &identity, &place, &name
	}
	kinds := map[string]*model.SearchCoverageCounts{
		"issue":          &report.Kinds.Issue,
		"document_block": &report.Kinds.DocumentBlock,
		"comment_block":  &report.Kinds.CommentBlock,
		"message_block":  &report.Kinds.MessageBlock,
		"ask_part":       &report.Kinds.AskPart,
	}
	rows, err := database.Pool.Query(ctx, coverageQuery, identity)
	if err != nil {
		return model.SearchCoverage{}, fmt.Errorf("search coverage: %w", err)
	}
	defer rows.Close()
	for rows.Next() {
		var kind string
		var counts model.SearchCoverageCounts
		if err := rows.Scan(&kind, &counts.Items, &counts.Current, &counts.Failed, &counts.OldestLackingSeconds); err != nil {
			return model.SearchCoverage{}, fmt.Errorf("search coverage: %w", err)
		}
		counts.Lacking = counts.Items - counts.Current - counts.Failed
		// A row that became pending in a transaction that began after this statement's own
		// carries a pending_since a moment past this statement's now(): it has waited nothing yet.
		if counts.OldestLackingSeconds != nil && *counts.OldestLackingSeconds < 0 {
			*counts.OldestLackingSeconds = 0
		}
		into, ok := kinds[kind]
		if !ok {
			return model.SearchCoverage{}, fmt.Errorf("search coverage: unknown kind %q", kind)
		}
		*into = counts
	}
	if err := rows.Err(); err != nil {
		return model.SearchCoverage{}, fmt.Errorf("search coverage: %w", err)
	}
	for _, counts := range kinds {
		report.Total.Items += counts.Items
		report.Total.Current += counts.Current
		report.Total.Lacking += counts.Lacking
		report.Total.Failed += counts.Failed
		if counts.OldestLackingSeconds != nil && (report.Total.OldestLackingSeconds == nil || *counts.OldestLackingSeconds > *report.Total.OldestLackingSeconds) {
			report.Total.OldestLackingSeconds = counts.OldestLackingSeconds
		}
	}
	return report, nil
}
