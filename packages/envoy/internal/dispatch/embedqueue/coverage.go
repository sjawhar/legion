package embedqueue

import (
	"context"
	"fmt"
	"log/slog"
	"strings"
	"time"

	"github.com/sjawhar/envoy/internal/dispatch/model"
	"github.com/sjawhar/envoy/internal/dispatch/store"
)

const (
	// coverageInterval is how often Run's coverage pass runs after the one at startup. Writes
	// reach the queue through their triggers within the same transaction, so the pass only has
	// to catch what no trigger saw (a restore, replication, a bulk load with triggers off) and
	// vectors another model or place made during a roll; neither needs a faster cadence.
	coverageInterval = 10 * time.Minute
	// coverageStatementRows bounds how many rows one coverage statement changes. Each statement
	// is its own short transaction, so a pass over a large database never holds locks a live
	// write would wait behind for longer than one small statement takes.
	coverageStatementRows = 500
)

// The places an embedder identity names (identityPlace).
const (
	placeBedrock = "bedrock"
	placeAzure   = "azure"
)

// coverageSource is one kind of item meaning search ranks: its FROM clause (from, plus where when
// the kind needs a filter), and the expressions for its id, its text and when it was written.
// id and text are the kind's write-time trigger's own (0073-0077), byte for byte, so a row the
// coverage pass writes is the row the trigger would have written, and a row the trigger wrote is
// never seen as stale. from and where are the items search ranks (api/search.go's meaning
// candidate CTEs), which the triggers can exceed: see coverageSources.
type coverageSource struct {
	kind, from, where, id, text, writtenAt string
}

// coverageSources is the single list of what meaning search covers, read by the coverage pass
// (Cover) and the coverage report (Coverage) alike.
//
//   - issue: 0073 enqueues new.key, new.title.
//   - document: 0074 enqueues new.artifact_id::text, new.markdown for every artifact_versions
//     insert with markdown, the latest version's being the last; search ranks only doc artifacts,
//     at their latest version (only a doc's versions carry markdown today).
//   - comment: 0075 enqueues new.id::text, new.body.
//   - ask: 0076 enqueues new.id::text and new.question, one space, then the answer's text
//     (coalesce(new.answer->>'text', ...) with the empty string): an unanswered ask's text keeps
//     its trailing space, and options are not part of it.
//   - message: 0077 enqueues new.id::text, new.body for every message, an agent's conversation's
//     too; search ranks only a message on an issue, so that is what this list covers. An agent
//     message's row is still the queue's to embed, and the pass's identity steps still reach it.
var coverageSources = []coverageSource{
	{kind: "issue", from: "issues i", id: "i.key", text: "i.title", writtenAt: "i.created_at"},
	{
		kind: "document",
		from: `artifacts a join lateral (
			select v.markdown, v.created_at from artifact_versions v where v.artifact_id = a.id order by v.number desc limit 1
		) v on true`,
		where:     "a.kind = 'doc' and v.markdown is not null",
		id:        "a.id::text",
		text:      "v.markdown",
		writtenAt: "v.created_at",
	},
	{kind: "comment", from: "comments c", id: "c.id::text", text: "c.body", writtenAt: "c.created_at"},
	{kind: "ask", from: "asks k", id: "k.id::text", text: "k.question || ' ' || coalesce(k.answer->>'text', '')", writtenAt: "k.created_at"},
	{kind: "message", from: "messages m join issues i on i.key = m.issue_key", id: "m.id::text", text: "m.body", writtenAt: "m.created_at"},
}

// items renders the kind's searchable items as a query of (id, text, hash, written_at). hash is
// embeddings_enqueue's own content_hash expression (0072, 0117) over the trigger's text, so an
// item's hash equals the content_hash its trigger would store.
func (s coverageSource) items() string {
	where := ""
	if s.where != "" {
		where = " where " + s.where
	}
	return fmt.Sprintf(`select s.id, s.text, encode(sha256(convert_to(s.text, 'UTF8')), 'hex') as hash, s.written_at
		from (select %s as id, %s as text, %s as written_at from %s%s) s`, s.id, s.text, s.writtenAt, s.from, where)
}

// insertMissing adds a pending row for up to $2 items of the kind that have none, exactly as the
// trigger's embeddings_enqueue would insert it. ON CONFLICT DO NOTHING, never DO UPDATE: a row a
// trigger inserts between this statement's read and its insert holds text at least as fresh as
// what this statement read, and DO UPDATE would revert it to the older text.
func (s coverageSource) insertMissing() string {
	return fmt.Sprintf(`
		insert into embeddings (kind, id, text_snapshot, content_hash)
		select $1::text, item.id, item.text, item.hash from (%s) item
		where not exists (select 1 from embeddings e where e.kind = $1::text and e.id = item.id)
		limit $2
		on conflict (kind, id) do nothing`, s.items())
}

// repairStale points up to $2 of the kind's rows whose content_hash is not the hash of the item's
// current text (text changed with the trigger off, as a restore or replication does) at that
// text, starting the row's wait and attempts again as embeddings_enqueue does on a text change.
// It is a compare-and-swap: the stale CTE captures each row's content_hash as seen, and the
// update writes only where the row still holds seen. Under READ COMMITTED an update that finds
// its row changed since the statement's snapshot re-checks its WHERE against the newest version,
// so a trigger write committing after the read makes the update skip that row, keeping the
// trigger's text, hash, claim and counters. The CTE takes no row lock (FOR UPDATE would re-read
// the newest version into seen and defeat the check).
func (s coverageSource) repairStale() string {
	return fmt.Sprintf(`
		with stale as (
			select e.kind, e.id, e.content_hash as seen, item.text, item.hash
			from (%s) item
			join embeddings e on e.kind = $1::text and e.id = item.id
			where e.content_hash <> item.hash
			limit $2
		)
		update embeddings e
		set text_snapshot = stale.text, content_hash = stale.hash, attempt_count = 0,
		    confirmed_failures = 0, dead = false, dead_model = null, next_attempt_at = now(),
		    pending_since = now()
		from stale
		where e.kind = stale.kind and e.id = stale.id and e.content_hash = stale.seen`, s.items())
}

// dropForeignVectors takes up to $3 of the kind's rows holding a vector another identity made
// ($2 is the configured one) out of the HNSW index's rows: search compares a stored vector only
// with a query vector of its own identity, and leaving foreign vectors in the index would cost
// filtered search recall. Every row of the kind, not only coverageSources' items, so no foreign
// vector survives anywhere. A row embedded at its current text (embedded_hash = content_hash) has
// no claim on it, so it becomes pending now with fresh attempts; a row already pending keeps its
// claim, backoff and pending_since. SKIP LOCKED leaves a row a worker is committing to the next
// pass. Once done the row matches no longer, so a second pass changes nothing.
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
// is the configured one; a row dead before 0117 recorded none) this identity's attempts, once:
// the content failed against another model or place, which says nothing about this one. A row
// that dies again records the configured identity (retryRows) and is not re-admitted again.
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

// coverStep is one statement of the coverage pass, run until it changes nothing.
type coverStep struct {
	changed *int64
	sql     string
	args    []any
}

// Cover is the coverage pass: for every kind in coverageSources it adds a row for each item that
// has none, repairs each row whose text is stale, drops each vector another identity made and
// re-admits each row dead under another identity, then logs one line with what it changed and the
// coverage that leaves (Coverage). Every step is a statement of at most coverageStatementRows rows
// in its own short transaction, repeated until one changes nothing, and each is safe beside a live
// write and beside another process's pass, so it takes no lease: two envoy-dispatch processes may
// run it at once. One deployment configures one identity; two processes configured differently
// would each drop the other's vectors, which is why every replica gets the same settings.
func Cover(ctx context.Context, deps Deps) error {
	identity := deps.Embedder.Identity()
	var added, repaired, dropped, readmitted int64
	for _, source := range coverageSources {
		for _, step := range []coverStep{
			{&added, source.insertMissing(), []any{source.kind, coverageStatementRows}},
			{&repaired, source.repairStale(), []any{source.kind, coverageStatementRows}},
			{&dropped, dropForeignVectors, []any{source.kind, identity, coverageStatementRows}},
			{&readmitted, readmitForeignDead, []any{source.kind, identity, coverageStatementRows}},
		} {
			for {
				tag, err := deps.Store.Pool.Exec(ctx, step.sql, step.args...)
				if err != nil {
					return fmt.Errorf("coverage pass: %s: %w", source.kind, err)
				}
				*step.changed += tag.RowsAffected()
				if tag.RowsAffected() == 0 {
					break
				}
			}
		}
	}
	report, err := Coverage(ctx, deps.Store, identity)
	if err != nil {
		return fmt.Errorf("coverage pass: %w", err)
	}
	var oldest any // null in the log when nothing lacks, as in the report
	if report.Total.OldestLackingSeconds != nil {
		oldest = *report.Total.OldestLackingSeconds
	}
	slog.Info("dispatch embedqueue: coverage pass",
		"identity", identity, "added", added, "repaired", repaired,
		"foreignVectorsDropped", dropped, "foreignDeadReadmitted", readmitted,
		"items", report.Total.Items, "current", report.Total.Current, "lacking", report.Total.Lacking,
		"failed", report.Total.Failed, "oldestLackingSeconds", oldest)
	return nil
}

// runCoverage runs Cover at once and then every coverageInterval until ctx ends. A failed pass is
// logged and the next tick tries again: every step is idempotent, so nothing is lost by waiting.
func runCoverage(ctx context.Context, deps Deps) {
	ticker := time.NewTicker(coverageInterval)
	defer ticker.Stop()
	for {
		if err := Cover(ctx, deps); err != nil && ctx.Err() == nil {
			slog.Error("dispatch embedqueue: coverage pass failed; the next one runs on schedule", "error", err)
		}
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
	}
}

// coverageQuery counts, per kind, coverageSources' items left-joined to their embeddings rows, in
// one statement so every count reads one snapshot. $1 is the configured identity, the empty string
// when there is none, and then no item is current. greatest ignores a null, so an item with no row
// ages from when it was written; the oldest age is null when nothing of the kind lacks.
var coverageQuery = func() string {
	arms := make([]string, len(coverageSources))
	for i, source := range coverageSources {
		arms[i] = fmt.Sprintf("select '%s'::text as kind, item.id, item.hash, item.written_at from (%s) item", source.kind, source.items())
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

// Coverage reports how much of what meaning search covers (coverageSources) has a current vector
// from identity, the configured embedder's (empty when none is configured). Its definitions are
// model.SearchCoverageCounts'; Total sums the kinds, and its oldest lacking age is theirs.
func Coverage(ctx context.Context, database *store.Store, identity string) (model.SearchCoverage, error) {
	var report model.SearchCoverage
	if identity != "" {
		place, name := identityPlace(identity)
		report.Identity, report.Place, report.Model = &identity, &place, &name
	}
	kinds := map[string]*model.SearchCoverageCounts{
		"issue":    &report.Kinds.Issue,
		"document": &report.Kinds.Document,
		"comment":  &report.Kinds.Comment,
		"ask":      &report.Kinds.Ask,
		"message":  &report.Kinds.Message,
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
	for _, counts := range []model.SearchCoverageCounts{report.Kinds.Issue, report.Kinds.Document, report.Kinds.Comment, report.Kinds.Ask, report.Kinds.Message} {
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

// identityPlace names the place and the model an embedder identity records: embed.AzureClient's
// is "azure:<endpoint host>/<deployment>", whose model is the deployment; any other is a Bedrock
// client's, whose identity is the Bedrock model id itself.
func identityPlace(identity string) (place, name string) {
	if rest, ok := strings.CutPrefix(identity, "azure:"); ok {
		return placeAzure, rest[strings.LastIndex(rest, "/")+1:]
	}
	return placeBedrock, identity
}
