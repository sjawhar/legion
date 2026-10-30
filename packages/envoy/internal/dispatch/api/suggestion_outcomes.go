package api

import (
	"context"
	"fmt"
	"log/slog"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/sjawhar/envoy/internal/dispatch/duty"
	"github.com/sjawhar/envoy/internal/dispatch/store"
)

// suggestionOutcomePage is the most events, and separately the most newly recorded suggestions,
// one transaction of a pass reads. A pass whose page was full reads the next one at once, in a
// transaction of its own, so a backlog is worked through in bounded steps that each commit their
// progress.
const suggestionOutcomePage = 1000

// suggestionSchedule is the sweep's pace. minGap is the least time from one pass's start to the
// next one's, whatever woke it (duty.WokenLoop.MinGap), and how long a pass that failed waits
// before trying again; settle is how old an event or a newly recorded suggestion must be before a
// pass judges by it (judgeSuggestionPage); page is suggestionOutcomePage. A test runs the loop
// faster, or with a smaller page.
type suggestionSchedule struct {
	minGap time.Duration
	settle time.Duration
	page   int
}

// suggestionOutcomeSchedule is the server's pace: a minute between passes, the period of the timer
// this loop replaced, so under load the loop runs no more often than that timer did; and a minute
// before an event is judged by, the same period, so the writes an agent makes within a minute of
// one another are judged together - both acted_on rules before the overridden one, as each pass
// orders them - as they mostly were when a tick fell anywhere in that minute. Nothing here is in a
// request's latency budget and no reader waits on a fresh outcome, so a minute or two of delay
// costs nothing a caller can see.
var suggestionOutcomeSchedule = suggestionSchedule{minGap: time.Minute, settle: time.Minute, page: suggestionOutcomePage}

// Each rule below is a format string whose one verb is the candidates a pass judges: every
// suggestion (everySuggestion) on the first pass a database sees, or after that the ones a page of
// events or newly recorded suggestions can touch (touchedSuggestions). A rule considers only the
// candidates still pending (outcome = 'ignored') and probes one indexed lookup per pending row
// rather than scanning refs or events in full:
//   - suggestionActedOnByCitation looks up refs by its existing (to_kind, to_id) index
//     (refs_to, migration 0001), one row at a time, and resolves each hit's owning issue by the
//     citing table's own primary key, also indexed.
//   - suggestionActedOnByDirectUpdate and suggestionOverridden each check events filtered by
//     issue_key, which events' own unique (issue_key, seq) constraint already indexes, or by
//     artifact_id, which its unique (artifact_id, seq) one does.
// The first pass is bounded by the pending count, as every pass of the previous release was
// (write_suggestions_pending_source / write_suggestions_pending_target, partial indexes, migration
// 0070). Every later pass is bounded by the candidates alone, so the suggestions still pending on
// issues nothing has written to cost it nothing.

// everySuggestion is the first pass's candidates: the whole table, whose pending rows each rule's
// own filter reaches through the partial indexes above.
const everySuggestion = `select * from write_suggestions`

// touchedSuggestions is a later pass's candidates: the suggestions recorded since the last pass
// ($1, ids as text), and the pending ones at least $4 seconds old (settle) whose source issue or
// suggested issue is an issue of a page's events ($2) or whose source document is a document of a
// page's events ($3, ids as text). Those are the only suggestions a page can resolve: each rule
// needs an event on the source or suggested issue, or on the source document - a citation lands an
// event there too, from the write that indexed the citing body - after the suggestion was
// recorded, and a suggestion recorded since the last pass is judged against all its events, not
// only the page's. The age bound keeps a suggestion younger than settle out of the pass, as
// readSuggestionPage keeps it out of $1: a page's events are all settle old, so none of them is
// newer than such a suggestion and none can resolve it, and judging it then would judge it by its
// younger events alone - the comment on the source without the moments-later update of the
// suggested issue. It is still judged once it is settle old: after the first pass a suggestion is
// swept only once it is settle old, so one this bound passes over is still unswept, the page
// reports when it settles (readSuggestionPage), and the pass then judges it in $1 against all its
// events.
// Each arm reads whole rows through an index of its own (the primary key,
// write_suggestions_pending_source, write_suggestions_pending_target,
// write_suggestions_pending_artifact), which an OR across the columns would not, and the union
// keeps one copy of a row two arms find; no arm reaches a row it does not match, so a pending
// suggestion no arm names is never read.
const touchedSuggestions = `
select * from write_suggestions where id = any($1::text[]::uuid[])
union
select * from write_suggestions
 where outcome = 'ignored' and source_issue_key = any($2::text[])
   and created_at <= now() - $4::float8 * interval '1 second'
union
select * from write_suggestions
 where outcome = 'ignored' and suggested_issue_key is not null and suggested_issue_key = any($2::text[])
   and created_at <= now() - $4::float8 * interval '1 second'
union
select * from write_suggestions
 where outcome = 'ignored' and source_artifact_id is not null and source_artifact_id = any($3::text[]::uuid[])
   and created_at <= now() - $4::float8 * interval '1 second'`

// suggestionActedOnByCitation marks 'acted_on' every pending suggestion whose suggested item was
// cited — a `dispatch://` mention landing a new reference-graph edge (packages/envoy/internal/
// dispatch/refs) — from a document, ask, comment or message owned by the suggestion's own source
// issue, after the suggestion was recorded. This is "closed its own as duplicate of it" and
// "cite decisions where they were made" (skills/dispatch-first): the agent's own closing note or
// reply names the suggested item, and refs already turns that into an edge for every write that
// indexes a body, so no separate instrumentation of those write paths is needed here.
const suggestionActedOnByCitation = `
with candidates as (%s),
pending as (
  select id, source_issue_key, source_artifact_id, suggested_kind, suggested_id, created_at
    from candidates
   where outcome = 'ignored'
)
update write_suggestions ws
   set outcome = 'acted_on', outcome_at = now(),
       outcome_detail = 'cited from a ' || c.from_kind || ' on the source'
  from pending p
  join lateral (
    select r.from_kind
      from refs r
     where r.to_kind = p.suggested_kind and r.to_id = p.suggested_id and r.created_at > p.created_at
       and (
         (r.from_kind = 'artifact' and exists(select 1 from artifacts a where a.id::text = r.from_id
            and (a.issue_key = p.source_issue_key or a.id = p.source_artifact_id)))
         or (r.from_kind = 'ask' and exists(select 1 from asks k where k.id::text = r.from_id
            and (k.issue_key = p.source_issue_key or k.artifact_id = p.source_artifact_id)))
         or (r.from_kind = 'comment' and exists(select 1 from comments c2 where c2.id::text = r.from_id
            and (c2.issue_key = p.source_issue_key or c2.artifact_id = p.source_artifact_id)))
         or (r.from_kind = 'message' and exists(select 1 from messages m where m.id::text = r.from_id and m.issue_key = p.source_issue_key))
       )
     limit 1
  ) c on true
 where ws.id = p.id
`

// suggestionActedOnByDirectUpdate marks 'acted_on' every pending suggestion whose suggested item
// (an issue, or anything else owned by one) received a new event on that issue from the
// suggestion's own actor after the suggestion was recorded — "update the existing issue instead
// of filing another" (skills/dispatch-first), done without necessarily citing anything back.
const suggestionActedOnByDirectUpdate = `
with candidates as (%s),
pending as (
  select id, suggested_issue_key, actor_kind, actor_id, created_at
    from candidates
   where outcome = 'ignored' and suggested_issue_key is not null
)
update write_suggestions ws
   set outcome = 'acted_on', outcome_at = now(),
       outcome_detail = 'the suggested issue was updated directly'
  from pending p
  -- A lateral with LIMIT, not a bare EXISTS: the planner flattens an EXISTS here into a semi
  -- join and hashes the whole events table (measured on a seeded copy: a sequential scan of
  -- 1,000,000 rows, 784 ms, where this shape is single-digit ms), since its estimate follows
  -- events' size rather than the handful of pending rows the join actually probes with.
  join lateral (
    select 1 from events e
     where e.issue_key = p.suggested_issue_key
       and e.created_at > p.created_at
       and e.actor ->> 'kind' = p.actor_kind
       and e.actor ->> 'id' = p.actor_id
     limit 1
  ) touched on true
 where ws.id = p.id
`

// suggestionOverridden marks 'overridden' every suggestion still pending after both acted_on
// passes above, where the source (the issue, or for a project-document ask the document) got
// further activity from the suggestion's own actor: a sign the agent saw the suggestion and kept
// working on what it had filed rather than acting on it. The issue's own creation event is
// always older than the suggestion (suggestions are persisted after that write commits), so it
// never satisfies "created_at > ws.created_at" on its own. A lateral with LIMIT, the same shape
// suggestionActedOnByDirectUpdate above needed: the two legs (issue-sourced, artifact-sourced)
// stay separate queries, each keeping its own column's index condition, so neither leg's plan
// risks being flattened the way a single bare EXISTS was measured to be.
const suggestionOverridden = `
with candidates as (%s),
pending as (
  select id, source_issue_key, source_artifact_id, actor_kind, actor_id, created_at
    from candidates
   where outcome = 'ignored'
)
update write_suggestions ws
   set outcome = 'overridden', outcome_at = now(),
       outcome_detail = 'further activity on the source did not address the suggestion'
  from pending p
  join lateral (
    (select 1 from events e
      where e.issue_key = p.source_issue_key
        and e.created_at > p.created_at
        and e.actor ->> 'kind' = p.actor_kind
        and e.actor ->> 'id' = p.actor_id
      limit 1)
    union all
    (select 1 from events e
      where e.artifact_id = p.source_artifact_id
        and e.created_at > p.created_at
        and e.actor ->> 'kind' = p.actor_kind
        and e.actor ->> 'id' = p.actor_id
      limit 1)
    limit 1
  ) touched on true
 where ws.id = p.id
`

// suggestionRules is the three rules over candidates, in the order that lets an edge created in
// the same write as further activity count as acted_on rather than overridden: both acted_on
// rules run before the overridden one sees what they left pending.
func suggestionRules(candidates string) [3]string {
	return [3]string{
		fmt.Sprintf(suggestionActedOnByCitation, candidates),
		fmt.Sprintf(suggestionActedOnByDirectUpdate, candidates),
		fmt.Sprintf(suggestionOverridden, candidates),
	}
}

var (
	everySuggestionRules   = suggestionRules(everySuggestion)
	touchedSuggestionRules = suggestionRules(touchedSuggestions)
)

// startSuggestionCursor is what the first pass on a database reads before its rules run: the
// highest event id, which the cursor moves to once the rules have judged every pending suggestion
// against every event up to it, and up to $1 of the suggestions not yet swept, oldest first (all
// of them recorded since migration 0154), which the rules judge too and the pass then marks.
const startSuggestionCursor = `
select coalesce((select max(id) from events), 0),
       array(select id::text from write_suggestions where not swept order by created_at limit $1)`

// readSuggestionPage is what a later pass reads before its rules run, from up to $2 events after
// the cursor ($1) and up to $2 suggestions not yet swept, oldest first: of the events, those
// before the first one younger than $3 seconds, as the last id, how many, and their issues and
// documents; of the suggestions, the ids of those at least $3 seconds old; and the seconds until
// the first thing it held back for being younger than that is old enough, null when it held back
// nothing. Events stop at the first young one, rather than skipping it, so the cursor never moves
// past an event no pass has read.
const readSuggestionPage = `
with page as (
  select id, issue_key, artifact_id, created_at from events where id > $1 order by id limit $2
),
young as (
  select id, created_at from page
   where created_at > now() - $3::float8 * interval '1 second'
   order by id limit 1
),
settled as (
  select * from page where not exists (select 1 from young where page.id >= young.id)
),
fresh as (
  select id, created_at from write_suggestions where not swept order by created_at limit $2
)
select (select max(id) from settled),
       (select count(*) from settled),
       array(select distinct issue_key from settled where issue_key is not null),
       array(select distinct artifact_id::text from settled where artifact_id is not null),
       array(select id::text from fresh where created_at <= now() - $3::float8 * interval '1 second'),
       extract(epoch from least(
         (select created_at from young),
         (select min(created_at) from fresh where created_at > now() - $3::float8 * interval '1 second')
       ) + $3::float8 * interval '1 second' - now())::float8`

// advanceSuggestionCursor marks swept the suggestions a pass judged ($1, ids as text) and moves
// the cursor to the last event id it read ($2), leaving it where it was when the pass read none.
const advanceSuggestionCursor = `
with swept as (
  update write_suggestions set swept = true where id = any($1::text[]::uuid[])
)
update suggestion_outcome_cursor set event_id = coalesce($2::bigint, event_id)`

// sweepSuggestionOutcomePages judges page after page, each in a transaction of its own
// (judgeSuggestionPage), until one comes back short, and returns when what it held back for being
// younger than settle is old enough to judge, or the zero time when it held back nothing.
func sweepSuggestionOutcomePages(ctx context.Context, pool *store.Pool, page int, settle time.Duration) (next time.Time, err error) {
	for {
		full, settlesIn, err := judgeSuggestionPageInTransaction(ctx, pool, page, settle)
		switch {
		case err != nil:
			return time.Time{}, err
		case full:
			continue
		case settlesIn > 0:
			return time.Now().Add(settlesIn), nil
		}
		return time.Time{}, nil
	}
}

// judgeSuggestionPageInTransaction is judgeSuggestionPage in a transaction of its own, committed
// when the page is judged.
func judgeSuggestionPageInTransaction(ctx context.Context, pool *store.Pool, page int, settle time.Duration) (full bool, settlesIn time.Duration, err error) {
	tx, err := pool.Begin(ctx)
	if err != nil {
		return false, 0, err
	}
	defer func() { _ = tx.Rollback(context.Background()) }()
	full, settlesIn, err = judgeSuggestionPage(ctx, tx, page, settle)
	if err != nil {
		return false, 0, err
	}
	return full, settlesIn, tx.Commit(ctx)
}

// judgeSuggestionPage runs the three rules over one page, in tx, which holds the cursor row for
// update until it ends. It reports whether the page was full - more events or suggestions may be
// waiting - and, when it held back an event or a suggestion younger than settle, how long until
// that one is old enough to judge.
//
// A cursor still null is the first pass on this database: the rules run over every suggestion,
// as each pass of the previous release did, and the cursor moves to the highest event id read
// before they ran. After that a page is the events after the cursor and the suggestions not yet
// swept, and the rules run over the suggestions those can touch (touchedSuggestions). A
// suggestion is recorded after its write commits, outside the events' commit-order lock
// (persistSuggestions), so it can commit after an event that started later than it did and that
// an earlier page already read; judging each newly recorded suggestion against all its events,
// rather than only this page's, is what still finds that event.
//
// An event or suggestion younger than settle waits for a later page, also when an older event of
// its issues or document is in this one (touchedSuggestions). The rules see every event committed
// when they run, not only the page's, so judging by an event no sooner than settle after it was
// written, and judging a suggestion no sooner than settle after it was recorded, judges with it
// every write its actor made in that time: an update to the suggested issue a few seconds after a
// comment on the source counts as acted_on, both acted_on rules running before the overridden
// one, rather than the comment being judged alone the moment it woke the loop.
//
// Reading `id > cursor` skips no event because events.id is commit order today:
// events.Broker.Append takes the database-wide commit-order lock before its insert and holds it
// until its transaction commits, and nothing else inserts an event
// (TestOnlyTheBrokerInsertsEvents), so no id at or below a committed one can still commit, and a
// page holds only committed events. That rests on the commit-order lock in events.Broker.Append,
// which a later change may replace: one that does must move this sweep, with the event log's
// other readers by id, to reading only below the id under which nothing can still commit.
func judgeSuggestionPage(ctx context.Context, tx pgx.Tx, page int, settle time.Duration) (full bool, settlesIn time.Duration, err error) {
	var cursor *int64
	if err := tx.QueryRow(ctx, `select event_id from suggestion_outcome_cursor for update`).Scan(&cursor); err != nil {
		return false, 0, fmt.Errorf("read the suggestion outcome cursor: %w", err)
	}
	if cursor == nil {
		var high int64
		var fresh []string
		if err := tx.QueryRow(ctx, startSuggestionCursor, page).Scan(&high, &fresh); err != nil {
			return false, 0, fmt.Errorf("read where the first suggestion outcome pass starts: %w", err)
		}
		if err := runSuggestionRules(ctx, tx, everySuggestionRules); err != nil {
			return false, 0, err
		}
		if _, err := tx.Exec(ctx, advanceSuggestionCursor, fresh, high); err != nil {
			return false, 0, fmt.Errorf("advance the suggestion outcome cursor: %w", err)
		}
		return len(fresh) == page, 0, nil
	}
	var (
		last                     *int64
		events                   int
		issues, documents, fresh []string
		settlesInSeconds         *float64
	)
	if err := tx.QueryRow(ctx, readSuggestionPage, *cursor, page, settle.Seconds()).Scan(
		&last, &events, &issues, &documents, &fresh, &settlesInSeconds,
	); err != nil {
		return false, 0, fmt.Errorf("read the events and suggestions after the outcome cursor: %w", err)
	}
	if settlesInSeconds != nil {
		// A deferred row is never due sooner than now, whatever two clocks' readings of it say.
		settlesIn = max(time.Duration(*settlesInSeconds*float64(time.Second)), time.Millisecond)
	}
	if events == 0 && len(fresh) == 0 {
		return false, settlesIn, nil
	}
	if err := runSuggestionRules(ctx, tx, touchedSuggestionRules, fresh, issues, documents, settle.Seconds()); err != nil {
		return false, 0, err
	}
	if _, err := tx.Exec(ctx, advanceSuggestionCursor, fresh, last); err != nil {
		return false, 0, fmt.Errorf("advance the suggestion outcome cursor: %w", err)
	}
	return events == page || len(fresh) == page, settlesIn, nil
}

// runSuggestionRules runs rules in order, in tx, with args.
func runSuggestionRules(ctx context.Context, tx pgx.Tx, rules [3]string, args ...any) error {
	for _, rule := range rules {
		if _, err := tx.Exec(ctx, rule, args...); err != nil {
			return fmt.Errorf("judge suggestion outcomes: %w", err)
		}
	}
	return nil
}

// RunSuggestionOutcomeSweep resolves LEGION-550's write_suggestions off the write path entirely:
// whether the agent acted on a suggestion (cited it, or updated the suggested issue directly) or
// overrode it (kept working on what it filed instead). It stops when ctx is done.
//
// It is a woken loop (duty.RunWoken): one pass at start, then a pass when wake is signalled - in
// the server, the pglisten.Hub wake on EventsChannel, which every event commit announces
// whichever process made it, and which the hub also signals after each successful LISTEN, so what
// was written while the listener had no connection is caught up then - and when a pass asks for
// one: once what it held back for being under a minute old is a minute old
// (judgeSuggestionPage), and a minute after a pass that failed. Passes start at least a minute
// apart, and each reads page after page until it has caught up (sweepSuggestionOutcomePages). A
// nil wake leaves the loop with its start pass and the passes those ask for. With nothing
// written and nothing due it holds no timer and makes no statement.
//
// It takes no lease (duty.WokenLoop.Lease empty), as the embedding queue takes none: every page
// runs in a transaction holding the suggestion_outcome_cursor row `for update`
// (judgeSuggestionPage), which is what serialises two processes woken by one commit. The second
// process's page waits there for the first's transaction and then reads the cursor that
// transaction advanced, so it judges what the first did not rather than the same page twice. That
// holds for the first pass on a database too: the row exists from migration 0153, with event_id
// null, so the pass that finds it null holds the same row and the one behind it reads the id that
// pass recorded. A lease over that would only make the second process retry later for a page the
// cursor already serialises, and the work no wake announces - the Idempotency-Keys past their
// window and the deliveries a dead process owed - is a leased timer of its own
// (RunMaintenanceSweep), which is why this loop holds no lease connection and keeps no tick.
//
// Only an event can resolve a suggestion - every rule needs one written after the suggestion - so
// a suggestion that commits after every pass its own write's event earned stays unswept until the
// next event wakes the loop, which is no later than anything could resolve it, and is judged then
// against all its events.
func RunSuggestionOutcomeSweep(ctx context.Context, pool *store.Pool, wake <-chan struct{}) {
	runSuggestionOutcomeSweep(ctx, pool, wake, suggestionOutcomeSchedule)
}

func runSuggestionOutcomeSweep(ctx context.Context, pool *store.Pool, wake <-chan struct{}, schedule suggestionSchedule) {
	ctx = store.WithTransactionTracking(ctx)
	duty.RunWoken(ctx, duty.WokenLoop{
		Wake:   wake,
		MinGap: schedule.minGap,
		Pass: func(ctx context.Context) time.Time {
			next, err := sweepSuggestionOutcomePages(ctx, pool, schedule.page, schedule.settle)
			if err != nil && ctx.Err() == nil {
				slog.Warn("dispatch: suggestion outcome sweep failed", "error", err)
				next = time.Now().Add(schedule.minGap)
			}
			if ctx.Err() != nil {
				return time.Time{}
			}
			return next
		},
	})
}
