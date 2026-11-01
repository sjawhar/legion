package api

import (
	"cmp"
	"context"
	"errors"
	"log/slog"
	"math"
	"regexp"
	"slices"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"golang.org/x/sync/singleflight"

	"github.com/sjawhar/envoy/internal/dispatch/duty"
	"github.com/sjawhar/envoy/internal/dispatch/store"
)

const (
	// relatedTermLimit is the most lexemes the related-item check's keyword query ORs together
	// as More Like This does: it keeps a text's 25 most distinctive terms, and a check's text can
	// be a 10,000-word spec.
	relatedTermLimit = 25
	// relatedMatchBudget bounds the chosen terms' summed df, and with it how many rows their OR
	// query can match and so its time: measured on production on 27 October 2026, 2,033 matches
	// answered in 92 ms and 4,759 in 170 ms. A word that would pass it is skipped, not the end of
	// the pick: the rarer words scored below it still fit.
	relatedMatchBudget = 3000
	// relatedProbeLimit is the most lexemes absent from the statistics the picker counts live, by
	// their frequency in the text. Each costs a bounded exists per kind; counting every lexeme of a
	// long spec that way would be thousands of probes.
	relatedProbeLimit = 50
	// relatedCommonFloor is the df at or under which a lexeme is never too common to pick, so a
	// small project, where a tenth of its items is a handful, still has terms.
	relatedCommonFloor = 10
)

// SearchTermStatsInterval is how often RunSearchTermStatsRefresh brings every project's term
// statistics up to date: it applies the items 0167's triggers queued since the last pass, and
// counts every item of a project again only where no pass of this build has counted it. A lexeme
// written since the last pass is probed live (relatedProbeLimit), so a stale table costs probes,
// not results.
const SearchTermStatsInterval = 10 * time.Minute

// searchTermStatsLease names the lease one refresh pass runs under, as the suggestion-outcome
// sweep's does: two processes rebuilding the same rows would be the same work twice.
const searchTermStatsLease = "search-term-stats"

// relatedQOpener is the related-item check's q in place of search's (searchQOpener): the chosen
// lexemes as a tsquery already built (relatedTerms.TSQuery), never re-stemmed through
// websearch_to_tsquery, and no own_key, since no issue key is the whole text.
// The check's two queries (relatedKeywordQuery, relatedMeaningQuery) open with it.
const relatedQOpener = `
with q as (select $1::tsquery as tsq, $4::text as term, ''::text as own_key),
`

// searchTermItemsSQL selects the search vector of every searchable item of the project
// {project} stands for, one row per item, each kind scoped exactly as searchKeywordLegsSQL scopes
// it: an issue, each doc artifact's row in document_search (the vector of the version search
// reads), a comment or an ask by its issue's project or else its document's, a message by its
// issue's.
//
// The document arm reads document_search (migration 0162) where it read each artifact's latest
// version through a lateral join over artifact_versions, for two reasons. Search's own document
// arm reads that table, so counting any other vector would give the check a df for words search
// cannot find; and the queue's document rows are written from document_search too (migration
// 0167), so a step pass and a full build hold one definition of a document's words. The two
// readings differ only for a document whose latest version carries no markdown, which
// document_search does not hold and the lateral read as a null vector.
const searchTermItemsSQL = `
select i.search from issues i where i.project_key = {project}
union all
select d.search from document_search d
  join artifacts a on a.id = d.artifact_id
 where a.kind = 'doc' and a.project_key = {project}
union all
select c.search from comments c
  left join issues i on i.key = c.issue_key
  left join artifacts a on a.id = c.artifact_id
 where coalesce(i.project_key, a.project_key) = {project}
union all
select k.search from asks k
  left join issues i on i.key = k.issue_key
  left join artifacts a on a.id = coalesce(k.artifact_id, k.block_artifact_id)
 where coalesce(i.project_key, a.project_key) = {project}
union all
select m.search from messages m join issues i on i.key = m.issue_key where i.project_key = {project}`

// searchTermItemsTSStat is searchTermItemsSQL as the format() string ts_stat's query is built
// from in SQL: %1$L quotes the project key as a literal, so the key never enters SQL text by Go
// concatenation.
var searchTermItemsTSStat = strings.ReplaceAll(searchTermItemsSQL, "{project}", "%1$L")

// searchTermTotalsClaim makes the project's totals row exist, so the select after it can lock it:
// that row lock serialises two refreshes of one project (a check's and the leased pass), each of
// which writes the same keys. Locking the projects row instead would hold every event append of
// the project, which takes that row, for the whole rebuild. The row it writes carries no
// snapshot, which is what has the pass that claimed it count the project in full.
const (
	searchTermTotalsClaim = `insert into search_term_totals (project_key, items, refreshed_at) values ($1, 0, now())
on conflict (project_key) do nothing`
	// searchTermTotalsMode locks the project's totals row and answers its item count and whether
	// a pass of this build wrote that row last, which is when the queue may be applied on top of
	// it. It did when the row carries a snapshot of its own and refreshed_at is still the
	// snapshot_at written beside it. A null snapshot is a project no pass of this build has
	// counted; a refreshed_at that is not that snapshot_at is somebody else's write, which during
	// a roll is the release before this one, whose rebuild updates items and refreshed_at alone
	// and leaves this build's snapshot in place. Either way the project is counted in full, so
	// the queue is never applied on top of counts another writer already holds.
	searchTermTotalsMode = `select items, snapshot is not null and refreshed_at is not distinct from snapshot_at
from search_term_totals where project_key = $1 for update`
	searchTermStatsClear = `delete from search_term_stats where project_key = $1`
)

// searchTermQueueProjectSQL is the project the item of a queue row q belongs to, read exactly as
// searchTermItemsSQL reads an item's: an issue's and a message's from its issue, a comment's and
// an ask's from its issue else its document, a document's from its artifact, which must be a doc
// as that statement's arm requires. Each owner is a scalar subquery on a primary key, so the
// rows the queue holds bound what a pass reads: a pass with an empty queue reads no item at all,
// which is R1.
//
// A row whose owner is gone names no project. No pass can tell whose counts held that item, so
// searchTermQueueSweep drops the row and the project's next full build is what restores its
// counts; no production code path deletes an issue or an artifact.
const searchTermQueueProjectSQL = `coalesce(
           (select i.project_key from issues i where i.key = q.issue_key),
           (select a.project_key from artifacts a where a.id = q.artifact_id and (q.kind <> 'document' or a.kind = 'doc')))`

// searchTermQueueItemKeySQL names the item a queue row is about, which is what a pass groups its
// rows by: a comment, an ask and a message are their own id, an issue is its key, and a document
// is its artifact. The owner columns are no part of it - an ask gains its block's artifact
// between two of its own changes, and grouping by the owner would read those two rows as two
// items and count the ask twice.
const searchTermQueueItemKeySQL = `coalesce(item_id::text, issue_key, artifact_id::text)`

// searchTermQueuedItemSQL selects the vector the item of the queued row `it` holds now, and that
// the project holds it at all, each kind scoped exactly as that kind's arm of searchTermItemsSQL
// scopes it. It answers no row for an item that has been deleted, which is what has a pass
// subtract that item's words and count one item fewer.
//
// Parameters: $1 the project key.
const searchTermQueuedItemSQL = `
    select i.search, true as present from issues i
     where it.kind = 'issue' and i.key = it.issue_key and i.project_key = $1
    union all
    select d.search, true from document_search d
      join artifacts a on a.id = d.artifact_id
     where it.kind = 'document' and d.artifact_id = it.artifact_id and a.kind = 'doc' and a.project_key = $1
    union all
    select c.search, true from comments c
      left join issues i on i.key = c.issue_key
      left join artifacts a on a.id = c.artifact_id
     where it.kind = 'comment' and c.id = it.item_id and coalesce(i.project_key, a.project_key) = $1
    union all
    select k.search, true from asks k
      left join issues i on i.key = k.issue_key
      left join artifacts a on a.id = coalesce(k.artifact_id, k.block_artifact_id)
     where it.kind = 'ask' and k.id = it.item_id and coalesce(i.project_key, a.project_key) = $1
    union all
    select m.search, true from messages m
      join issues i on i.key = m.issue_key
     where it.kind = 'message' and m.id = it.item_id and i.project_key = $1`

// searchTermStatsStep applies the project's queued changes to its stored counts and answers the
// project's item count with how many items it applied. It answers no row, and writes nothing at
// all, when the queue holds nothing of the project that its snapshot sees.
//
// It takes one snapshot and one time and writes both beside the counts: refreshed_at and
// snapshot_at are the statement's statement_timestamp(), and snapshot is the
// pg_current_snapshot() it takes here. It applies and drops exactly the queue rows that snapshot
// sees, so a row whose transaction commits after it stays for the next pass and no row is applied
// twice. It drains every visible row of the project rather than a batch of them: moving
// refreshed_at after a partial drain would leave the counts behind for every item queued at or
// before that moment, which relatedTermsSQL reads as the moment the counts were taken.
//
// Of two queued rows for one item it reads the older one's vector, by id, which is the vector the
// stored counts hold: a writer takes the item's row lock, so one item's rows are written, and
// become visible, in that order. For each item it subtracts the lexemes that vector holds, adds
// the lexemes the item holds now, and moves items by the items the project now holds less those
// it held before - both for an edit, the second alone for a delete, the first alone for an insert.
//
// The queue is read once for each project, which is a scan of a table holding only what has
// changed since the last pass: every pass of the loop drains what it can see, so the rows left
// behind are the writes of the moment the pass ran.
//
// Parameters: $1 the project key.
const searchTermStatsStep = `
with snapshot as materialized (
  select pg_current_snapshot() as taken, statement_timestamp() as at
),
queued as (
  select q.id, q.kind, q.issue_key, q.artifact_id, q.item_id, q.old_search
    from search_term_queue q, snapshot sn
   where pg_visible_in_snapshot(q.xid, sn.taken) and ` + searchTermQueueProjectSQL + ` = $1
),
drained as (
  delete from search_term_queue d where d.id in (select id from queued)
),
changed as (
  select distinct on (kind, ` + searchTermQueueItemKeySQL + `)
         kind, issue_key, artifact_id, item_id, old_search
    from queued
   order by kind, ` + searchTermQueueItemKeySQL + `, id
),
held as (
  select it.old_search, v.search as now_search, v.present
    from changed it
    left join lateral (` + searchTermQueuedItemSQL + `
    ) v on true
),
held_before as (
  select u.lexeme, count(*) as n from held h cross join lateral unnest(h.old_search) u group by u.lexeme
),
held_now as (
  select u.lexeme, count(*) as n from held h cross join lateral unnest(h.now_search) u group by u.lexeme
),
counted as (
  select coalesce(b.lexeme, n.lexeme) as lexeme,
         coalesce(s.ndoc, 0) + coalesce(n.n, 0) - coalesce(b.n, 0) as ndoc
    from held_before b
    full join held_now n on n.lexeme = b.lexeme
    left join search_term_stats s on s.project_key = $1 and s.lexeme = coalesce(b.lexeme, n.lexeme)
   where coalesce(n.n, 0) <> coalesce(b.n, 0)
),
dropped as (
  delete from search_term_stats s
   where s.project_key = $1 and s.lexeme in (select lexeme from counted where ndoc <= 0)
),
written as (
  insert into search_term_stats (project_key, lexeme, ndoc)
  select $1, lexeme, ndoc from counted where ndoc > 0
  on conflict (project_key, lexeme) do update set ndoc = excluded.ndoc
)
update search_term_totals t
   set items = t.items + (select count(*) filter (where h.present)
                               - count(*) filter (where h.old_search is not null) from held h),
       refreshed_at = sn.at,
       snapshot = sn.taken,
       snapshot_at = sn.at
  from snapshot sn
 where t.project_key = $1 and exists (select 1 from queued)
returning t.items, (select count(*) from changed)`

// searchTermStatsRebuild writes the project's lexemes with the number of items holding each
// (ts_stat's ndoc) and its item count, in one statement so both read the same moment, and drops
// the queue rows that statement's snapshot sees, which are the changes it has counted by reading
// the items themselves. refreshed_at is that statement's start, which is what the picker compares
// item creation times with (relatedTermsSQL), and it writes the snapshot and the snapshot_at
// beside it that have the next pass read this row as one of its own.
//
// Parameters: $1 the project key, $2 searchTermItemsTSStat.
var searchTermStatsRebuild = `
with snapshot as materialized (
  select pg_current_snapshot() as taken, statement_timestamp() as at
),
stats as (
  insert into search_term_stats (project_key, lexeme, ndoc)
  select $1, word, ndoc from ts_stat(format($2::text, $1::text))
),
drained as (
  delete from search_term_queue q using snapshot sn
   where pg_visible_in_snapshot(q.xid, sn.taken) and ` + searchTermQueueProjectSQL + ` = $1
)
update search_term_totals t
   set items = (select count(*) from (` + strings.ReplaceAll(searchTermItemsSQL, "{project}", "$1::text") + `
       ) items),
       refreshed_at = sn.at,
       snapshot = sn.taken,
       snapshot_at = sn.at
  from snapshot sn
 where t.project_key = $1
returning t.items`

// searchTermQueueSweep drops the queued changes no pass can apply: a row whose owner names no
// project, which is an item whose issue or artifact has been deleted, and a document row for an
// artifact that is no doc, which no project's counts ever held - migration 0163 puts no kind
// filter on document_search, deliberately, because every reader of it joins artifacts. A row is
// read only once its writer's transaction has committed, and that writer could not have written
// an item whose owner it could not see, so a row naming no project here names none later either.
const searchTermQueueSweep = `delete from search_term_queue q where ` + searchTermQueueProjectSQL + ` is null`

// searchTermRefresh is what one project's refresh did: the project's item count (the picker's N),
// the mode it ran in, and how many items it applied - the queued items for a step, every item of
// the project for a full build, which counts them all again.
type searchTermRefresh struct {
	Items   int
	Mode    string
	Changes int
}

const (
	searchTermModeStep = "step"
	searchTermModeFull = "full"
)

// refreshSearchTermStats brings the project's search_term_stats rows and its search_term_totals
// row up to date in one transaction, and answers what that took. It applies the queued changes
// where a pass of this build wrote the totals row last, and counts every item of the project
// again where one did not: a project with no totals row (one created since the last pass), one no
// pass of this build has counted, and one whose refreshed_at another writer moved.
func refreshSearchTermStats(ctx context.Context, pool *store.Pool, project string) (searchTermRefresh, error) {
	tx, err := pool.Begin(ctx)
	if err != nil {
		return searchTermRefresh{}, err
	}
	defer func() { _ = tx.Rollback(context.WithoutCancel(ctx)) }()
	if _, err := tx.Exec(ctx, searchTermTotalsClaim, project); err != nil {
		return searchTermRefresh{}, err
	}
	var items int
	var step bool
	if err := tx.QueryRow(ctx, searchTermTotalsMode, project).Scan(&items, &step); err != nil {
		return searchTermRefresh{}, err
	}
	if step {
		var changes int
		err := tx.QueryRow(ctx, searchTermStatsStep, project).Scan(&items, &changes)
		// No row means the pass had nothing of the project to apply and so wrote nothing: the
		// totals row keeps the counts, the moment and the snapshot of the pass that last changed it.
		if err != nil && !errors.Is(err, pgx.ErrNoRows) {
			return searchTermRefresh{}, err
		}
		return searchTermRefresh{Items: items, Mode: searchTermModeStep, Changes: changes}, tx.Commit(ctx)
	}
	if _, err := tx.Exec(ctx, searchTermStatsClear, project); err != nil {
		return searchTermRefresh{}, err
	}
	if err := tx.QueryRow(ctx, searchTermStatsRebuild, project, searchTermItemsTSStat).Scan(&items); err != nil {
		return searchTermRefresh{}, err
	}
	return searchTermRefresh{Items: items, Mode: searchTermModeFull, Changes: items}, tx.Commit(ctx)
}

// RefreshEverySearchTermStats is one pass of RunSearchTermStatsRefresh: every project, each in its
// own transaction, so one project's failure leaves the others' rows fresh, and then one sweep of
// the queued changes no project can apply. It is exported for BuildEverySearchTermStats, which
// cmd/dispatch-seed runs at the end of a seed so that `search_term_totals` holds a row for every
// project and no measured refresh is a first-use full build.
//
// Every project it changes is logged on a line of its own, with the mode it ran in and how many
// items it applied, which is what says on a running server that the pass read a queue rather than
// every item of every project.
func RefreshEverySearchTermStats(ctx context.Context, pool *store.Pool) {
	started := time.Now()
	projects, err := searchTermStatsProjects(ctx, pool)
	if err != nil {
		slog.Warn("dispatch: list the projects whose search term statistics to refresh", "error", err)
		return
	}
	refreshed, items, changes := 0, 0, 0
	for _, project := range projects {
		result, err := refreshSearchTermStats(ctx, pool, project)
		if err != nil {
			if ctx.Err() != nil {
				return
			}
			slog.Warn("dispatch: refresh a project's search term statistics", "project", project, "error", err)
			continue
		}
		refreshed++
		items += result.Items
		changes += result.Changes
		if result.Mode == searchTermModeFull || result.Changes > 0 {
			slog.Info("dispatch: search term statistics refreshed for a project",
				"project", project, "mode", result.Mode, "changes", result.Changes)
		}
	}
	swept := sweepSearchTermQueue(ctx, pool)
	slog.Info("dispatch: search term statistics refreshed",
		"projects", refreshed, "failed", len(projects)-refreshed, "items", items, "changes", changes,
		"swept", swept, "duration", time.Since(started).Round(time.Millisecond))
}

// searchTermStatsProjects is every project a whole pass builds, in key order: the pass of this
// release and the pass of the release before its snapshot columns
// (buildEverySearchTermStatsBeforeSnapshots) read the same list in the same order.
func searchTermStatsProjects(ctx context.Context, pool *store.Pool) ([]string, error) {
	rows, err := pool.Query(ctx, `select key from projects order by key`)
	if err != nil {
		return nil, err
	}
	return pgx.CollectRows(rows, pgx.RowTo[string])
}

// sweepSearchTermQueue drops the queued changes no project's pass can apply and answers how many
// it dropped; searchTermQueueSweep says which rows those are. A failure is logged and no more:
// the rows it leaves cost the queue its space, never a wrong count.
func sweepSearchTermQueue(ctx context.Context, pool *store.Pool) int64 {
	tag, err := pool.Exec(ctx, searchTermQueueSweep)
	if err != nil {
		if ctx.Err() == nil {
			slog.Warn("dispatch: drop the queued search term changes that name no project", "error", err)
		}
		return 0
	}
	return tag.RowsAffected()
}

// RunSearchTermStatsRefresh keeps every project's term statistics, the related-item check's df
// and N, up to date at boot and then every interval, until ctx is done. Each pass runs under
// searchTermStatsLease for one interval, so one process refreshes per interval however many serve
// the database; leases is required, as it is of every leased loop. The ticks are
// duty.RunLeased's, a ticker's: the first at the moment the call starts, then one each interval.
func RunSearchTermStatsRefresh(ctx context.Context, pool *store.Pool, interval time.Duration, leases duty.Leases) {
	duty.RunLeased(ctx, leases, searchTermStatsLease, interval, duty.TickAtStart, func(ctx context.Context) {
		RefreshEverySearchTermStats(ctx, pool)
	})
}

// ensureSearchTermStats starts a refresh of project's statistics in the background, unless one
// this process started is still running, and returns at once with a channel that receives once
// that refresh ends. A check that finds a project with no totals row (one created since the last
// pass, or a server whose first pass has not reached it) calls it and picks in probe mode
// meanwhile; many checks of that project then cost one refresh, not one each. The refresh runs on
// the server's lifetime, bounded by one interval, by which the leased pass would have refreshed
// the project anyway.
func (s *server) ensureSearchTermStats(project string) <-chan singleflight.Result {
	return s.termStats.DoChan(project, func() (any, error) {
		ctx, cancel := context.WithTimeout(s.lifetime, SearchTermStatsInterval)
		defer cancel()
		started := time.Now()
		result, err := refreshSearchTermStats(ctx, s.deps.Store.Pool, project)
		if err != nil {
			slog.Warn("dispatch: refresh the search term statistics of a project the related-item check found without them",
				"project", project, "error", err)
			return nil, err
		}
		slog.Info("dispatch: search term statistics refreshed for a project the related-item check found without them",
			"project", project, "mode", result.Mode, "changes", result.Changes, "items", result.Items,
			"duration", time.Since(started).Round(time.Millisecond))
		return result.Items, nil
	})
}

// relatedExclusion names the check's source owner, whose items are left out of df and out of the
// probes: an issue (a new issue, an ask's issue, exclude_issue) or an unlinked project document
// (an ask on it). Both empty excludes nothing.
type relatedExclusion struct {
	issueKey   string
	artifactID string
}

// relatedTerm is one lexeme the picker chose.
type relatedTerm struct {
	Lexeme string
	// Surface is the lexeme as the title spells it (the first whitespace-separated word of the
	// title whose own vector holds the lexeme, a word that is only that lexeme first, its outer
	// punctuation trimmed), or empty where no single word of the title yields it.
	Surface string
	DF      int
	// IDF is the lexeme's idf as the picker weighed it, and its weight in the check's keyword
	// rank (relatedCoordinationRank); Score is the text's tf of it times IDF.
	IDF   float64
	Score float64
}

// relatedTerms is what pickRelatedTerms chose for a text.
type relatedTerms struct {
	Terms []relatedTerm
	// TSQuery is Terms as one tsquery text, each lexeme quoted by quote_literal and joined with
	// " | ", to be cast with ::tsquery (relatedQOpener); empty when Terms is.
	TSQuery string
	// StatsPresent is whether the project had a totals row. Without one every lexeme was probed
	// (probe mode) and a refresh was started.
	StatsPresent bool
	// Items is the project's item count at its last refresh (N); zero in probe mode.
	Items int
	// NoLexemes is a text that holds no lexeme at all: blank, or stopwords alone.
	NoLexemes bool
}

// relatedTermsSQL reads, for every lexeme of the text, its frequency there (tf), its df from the
// project's statistics, and its surface word in the title, in one statement.
//
// The text is tokenized as a document version's vector is built (0087_search_vector_bound):
// search_text, then ask blocks stripped, then search_vector, in that order, so its lexemes are the
// ones stored vectors hold: stripping a block first can join the underscore runs either side of it
// into one search_text breaks, which the stored vector holds unbroken. Each lexeme's tf is its
// position count.
//
// df is null for a lexeme the statistics do not hold, and for every lexeme when the project has
// no totals row; the caller probes those. Otherwise it is ndoc less the excluded owner's items
// the refresh counted: those holding the lexeme that existed at refreshed_at (created at or
// before it, the moment of the last pass that changed the project's counts), and of a document
// the latest version created by then, which is the version document_search held for the refresh
// to count. Issues, comments, asks and messages keep no history, so their current vector stands
// in for the one counted; a word one gained since is subtracted though it was not counted, which
// only lowers df (never below zero). A df lowered to zero is not taken as final
// (pickRelatedTerms probes it): an own item whose transaction began before the pass's snapshot
// and committed after it is subtracted though the stored count never held it - the pass leaves
// that item's queued change for its successor, while refreshed_at has already moved past the
// item's creation - and an item written since may hold a word only the source held then.
//
// A surface costs a to_tsvector per distinct word, so only the title's words are read for one:
// they are what a reader recognises (Suggestions.SearchQuery), and the title is a few words where
// a spec can be thousands.
//
// Parameters: $1 project, $2 text, $3 excluded issue key (empty for none), $4 excluded document
// artifact id (null for none), $5 the title.
const relatedTermsSQL = `
with body as (
  select regexp_replace(search_text($2::text), '(?s):::ask\{.*?:::', '', 'g') as text
),
text_lexemes as (
  select u.lexeme, coalesce(array_length(u.positions, 1), 1) as tf
    from body cross join lateral unnest(search_vector('', body.text)) u
),
totals as (
  select items, refreshed_at from search_term_totals where project_key = $1
),
own_items as (
  select i.search from issues i cross join totals t
   where i.key = $3 and i.project_key = $1 and i.created_at <= t.refreshed_at
  union all
  select v.search from artifacts a cross join totals t
    join lateral (select v.search from artifact_versions v
                  where v.artifact_id = a.id and v.created_at <= t.refreshed_at
                  order by v.number desc limit 1) v on true
   where a.kind = 'doc' and a.project_key = $1 and (a.issue_key = $3 or a.id = $4::uuid)
  union all
  select c.search from comments c cross join totals t
    left join issues i on i.key = c.issue_key
    left join artifacts a on a.id = c.artifact_id
   where (c.issue_key = $3 or c.artifact_id = $4::uuid) and coalesce(i.project_key, a.project_key) = $1
     and c.created_at <= t.refreshed_at
  union all
  select k.search from asks k cross join totals t
    left join issues i on i.key = k.issue_key
    left join artifacts a on a.id = coalesce(k.artifact_id, k.block_artifact_id)
   where (k.issue_key = $3 or coalesce(k.artifact_id, k.block_artifact_id) = $4::uuid)
     and coalesce(i.project_key, a.project_key) = $1 and k.created_at <= t.refreshed_at
  union all
  select m.search from messages m cross join totals t
    join issues i on i.key = m.issue_key
   where m.issue_key = $3 and i.project_key = $1 and m.created_at <= t.refreshed_at
),
own_counts as (
  select u.lexeme, count(*) as n
    from own_items o cross join lateral unnest(o.search) u
   where u.lexeme in (select lexeme from text_lexemes)
   group by u.lexeme
),
words as (
  select w.word, min(w.n) as n
    from regexp_split_to_table(search_text($5::text), '\s+') with ordinality w(word, n)
   where w.word <> ''
   group by w.word
),
surfaces as (
  select distinct on (u.lexeme) u.lexeme, regexp_replace(w.word, '^[^[:alnum:]]+|[^[:alnum:]]+$', '', 'g') as word
    from words w
    cross join lateral (select to_tsvector('english', w.word) as vector) wv
    cross join lateral unnest(wv.vector) u
   order by u.lexeme, length(wv.vector) > 1, w.n
)
select l.lexeme, l.tf, quote_literal(l.lexeme),
       case when s.ndoc is not null then greatest(s.ndoc - coalesce(o.n, 0), 0) end,
       coalesce(sf.word, ''), t.items
  from text_lexemes l
  left join totals t on true
  left join search_term_stats s on t.items is not null and s.project_key = $1 and s.lexeme = l.lexeme
  left join own_counts o on o.lexeme = l.lexeme
  left join surfaces sf on sf.lexeme = l.lexeme`

// relatedProbeSQL answers which of the given lexemes some item of the project holds, leaving out
// the excluded owner's items. Issues, comments, asks and messages each probe their search index
// with an exists; documents probe document_search (migration 0162), the GIN-indexed vector of
// each document's latest version, which they are read through once for all the lexemes: the
// vector is tested against the lexemes ORed, and only a document that matches is unnested. An
// item on no document (an issue's comment or ask) is never the excluded document's, so the
// document test applies only when $4 names one: `is distinct from` a null $4 would leave out
// every such item.
//
// Parameters: $1 project, $2 lexemes, $3 excluded issue key (empty for none), $4 excluded document
// artifact id (null for none).
const relatedProbeSQL = `
with probe as (
  select l.lexeme, quote_literal(l.lexeme)::tsquery as tq from unnest($2::text[]) l(lexeme)
),
any_probe as (
  select string_agg(quote_literal(lexeme), ' | ')::tsquery as tq from probe
),
document_hits as (
  select distinct u.lexeme
    from document_search v
    join artifacts a on a.id = v.artifact_id
    cross join any_probe
    cross join lateral unnest(v.search) u
   where a.kind = 'doc' and a.project_key = $1
     and a.issue_key is distinct from $3 and a.id is distinct from $4::uuid
     and v.search @@ any_probe.tq and u.lexeme = any($2::text[])
)
select p.lexeme from probe p
 where exists (select 1 from issues i where i.search @@ p.tq and i.project_key = $1 and i.key <> $3)
    or exists (select 1 from document_hits d where d.lexeme = p.lexeme)
    or exists (select 1 from comments c
                 left join issues i on i.key = c.issue_key
                 left join artifacts a on a.id = c.artifact_id
                where c.search @@ p.tq and coalesce(i.project_key, a.project_key) = $1
                  and c.issue_key is distinct from $3 and ($4::uuid is null or c.artifact_id is distinct from $4::uuid))
    or exists (select 1 from asks k
                 left join issues i on i.key = k.issue_key
                 left join artifacts a on a.id = coalesce(k.artifact_id, k.block_artifact_id)
                where k.search @@ p.tq and coalesce(i.project_key, a.project_key) = $1
                  and k.issue_key is distinct from $3
                  and ($4::uuid is null or coalesce(k.artifact_id, k.block_artifact_id) is distinct from $4::uuid))
    or exists (select 1 from messages m join issues i on i.key = m.issue_key
                where m.search @@ p.tq and i.project_key = $1 and m.issue_key <> $3)`

var (
	// relatedDigitsLexeme is a lexeme of digits and separators alone: a number, a version, a date
	// or a part of one.
	relatedDigitsLexeme = regexp.MustCompile(`^[0-9[:punct:]]+$`)
	// relatedHexLexeme is a hash, a uuid or a part of one: hex digits and separators with a run
	// of 12 or more hex digits.
	relatedHexLexeme = regexp.MustCompile(`^[0-9a-f[:punct:]]*[0-9a-f]{12,}[0-9a-f[:punct:]]*$`)
)

// relatedNoise reports a lexeme the picker never chooses: a number or a hash matches nothing a
// person would call related. A lexeme holding a backslash is left out too: quote_literal writes
// it as E'...', a form tsquery input does not read.
func relatedNoise(lexeme string) bool {
	return relatedDigitsLexeme.MatchString(lexeme) || relatedHexLexeme.MatchString(lexeme) ||
		strings.ContainsRune(lexeme, '\\')
}

// relatedCandidate is a lexeme of the text while the picker weighs it.
type relatedCandidate struct {
	relatedTerm
	tf     int
	quoted string
	known  bool
}

// pickRelatedTerms chooses the related-item check's keywords from text (as More Like
// This does): each lexeme scored tf × idf, idf = ln(1 + (N − df + 0.5)/(df + 0.5)) (Lucene's BM25
// idf), where df is how many of the project's items hold it and N how many items the project
// has. It keeps at most relatedTermLimit lexemes by score, ties by lower df then lexeme, and drops
// a lexeme no other item holds (df 0), one in more than a tenth of the project (past
// relatedCommonFloor), and numbers and hashes (relatedNoise); a lexeme whose df would take the
// summed df past relatedMatchBudget is skipped, and the next by score is weighed in its place. The
// excluded owner's items count toward no df.
//
// df comes from the project's statistics (refreshSearchTermStats). A lexeme they do not hold, or
// whose df the excluded owner's items bring to zero, is probed live, the relatedProbeLimit most
// frequent in the text, and a hit counts as df 1. A project with no statistics starts their
// refresh (ensureSearchTermStats) and is picked in probe mode, every lexeme probed and N taken as
// 0. Each chosen term's surface is read from title alone (relatedTermsSQL).
func (s *server) pickRelatedTerms(
	ctx context.Context, q queryer, project, text, title string, exclude relatedExclusion,
) (relatedTerms, error) {
	var artifactID *string
	if exclude.artifactID != "" {
		artifactID = &exclude.artifactID
	}
	rows, err := q.Query(ctx, relatedTermsSQL, project, text, exclude.issueKey, artifactID, title)
	if err != nil {
		return relatedTerms{}, err
	}
	var candidates []relatedCandidate
	var items *int
	for rows.Next() {
		var candidate relatedCandidate
		var df *int
		if err := rows.Scan(&candidate.Lexeme, &candidate.tf, &candidate.quoted, &df, &candidate.Surface, &items); err != nil {
			rows.Close()
			return relatedTerms{}, err
		}
		if df != nil && *df > 0 {
			candidate.DF, candidate.known = *df, true
		}
		candidates = append(candidates, candidate)
	}
	if err := rows.Err(); err != nil {
		return relatedTerms{}, err
	}
	if len(candidates) == 0 {
		return relatedTerms{NoLexemes: true}, nil
	}
	picked := relatedTerms{StatsPresent: items != nil}
	if items != nil {
		picked.Items = *items
	} else {
		s.ensureSearchTermStats(project)
	}

	candidates = slices.DeleteFunc(candidates, func(candidate relatedCandidate) bool {
		return relatedNoise(candidate.Lexeme)
	})
	if err := probeRelatedCandidates(ctx, q, project, exclude.issueKey, artifactID, candidates); err != nil {
		return relatedTerms{}, err
	}

	commonAbove := max(float64(relatedCommonFloor), float64(picked.Items)/10)
	candidates = slices.DeleteFunc(candidates, func(candidate relatedCandidate) bool {
		return !candidate.known || candidate.DF == 0 || float64(candidate.DF) > commonAbove
	})
	for index := range candidates {
		candidate := &candidates[index]
		// N is floored at df: in probe mode N is 0, and an idf below zero would rank the words the
		// text repeats lowest.
		n := float64(max(picked.Items, candidate.DF))
		df := float64(candidate.DF)
		candidate.IDF = math.Log(1 + (n-df+0.5)/(df+0.5))
		candidate.Score = float64(candidate.tf) * candidate.IDF
	}
	slices.SortFunc(candidates, func(a, b relatedCandidate) int {
		return cmp.Or(cmp.Compare(b.Score, a.Score), cmp.Compare(a.DF, b.DF), strings.Compare(a.Lexeme, b.Lexeme))
	})

	quoted := make([]string, 0, relatedTermLimit)
	matches := 0
	for _, candidate := range candidates {
		if len(picked.Terms) == relatedTermLimit {
			break
		}
		if matches+candidate.DF > relatedMatchBudget {
			continue
		}
		matches += candidate.DF
		picked.Terms = append(picked.Terms, candidate.relatedTerm)
		quoted = append(quoted, candidate.quoted)
	}
	picked.TSQuery = strings.Join(quoted, " | ")
	return picked, nil
}

// probeRelatedCandidates counts live, as df 1 or 0, the relatedProbeLimit most frequent of the
// candidates the statistics do not hold, marking each probed one known; the rest stay unknown and
// are not picked.
func probeRelatedCandidates(
	ctx context.Context, q queryer, project, excludeIssue string, excludeArtifact *string, candidates []relatedCandidate,
) error {
	var unknown []*relatedCandidate
	for index := range candidates {
		if !candidates[index].known {
			unknown = append(unknown, &candidates[index])
		}
	}
	if len(unknown) == 0 {
		return nil
	}
	slices.SortFunc(unknown, func(a, b *relatedCandidate) int {
		return cmp.Or(cmp.Compare(b.tf, a.tf), strings.Compare(a.Lexeme, b.Lexeme))
	})
	unknown = unknown[:min(len(unknown), relatedProbeLimit)]
	lexemes := make([]string, len(unknown))
	byLexeme := make(map[string]*relatedCandidate, len(unknown))
	for index, candidate := range unknown {
		lexemes[index] = candidate.Lexeme
		byLexeme[candidate.Lexeme] = candidate
		candidate.known = true
	}
	rows, err := q.Query(ctx, relatedProbeSQL, project, lexemes, excludeIssue, excludeArtifact)
	if err != nil {
		return err
	}
	hits, err := pgx.CollectRows(rows, pgx.RowTo[string])
	if err != nil {
		return err
	}
	for _, lexeme := range hits {
		byLexeme[lexeme].DF = 1
	}
	return nil
}
