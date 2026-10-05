package api

import (
	"context"
	"log/slog"
	"time"

	"github.com/sjawhar/envoy/internal/dispatch/store"
)

// SuggestionSweepInterval is how often RunSuggestionOutcomeSweep looks for a pending
// write_suggestions row it can now resolve. Nothing here is in a request's latency budget, and
// no reader waits on a fresh outcome, so a minute of delay before one is recorded costs nothing a
// write's caller can see; a minutes-scale interval also keeps the sweep's three passes, cheap as
// they are (see below), off any tight loop in production.
const SuggestionSweepInterval = time.Minute

// Each pass below starts from write_suggestions filtered to outcome = 'ignored'
// (write_suggestions_pending_source / write_suggestions_pending_target, both partial indexes:
// migration 0070), a set bounded by how many suggestions are currently unresolved — recently
// filed issues and asks only, never the whole table — and probes one indexed lookup per pending
// row rather than scanning refs or events in full:
//   - suggestionActedOnByCitation looks up refs by its existing (to_kind, to_id) index
//     (refs_to, migration 0001), one row at a time, and resolves each hit's owning issue by the
//     citing table's own primary key, also indexed.
//   - suggestionActedOnByDirectUpdate and suggestionOverridden each check events filtered by
//     issue_key, which events' own unique (issue_key, seq) constraint already indexes.
// All three are therefore bounded by the pending count, not by the size of refs or events.

// suggestionActedOnByCitation marks 'acted_on' every pending suggestion whose suggested item was
// cited — a `dispatch://` mention landing a new reference-graph edge (packages/envoy/internal/
// dispatch/refs) — from a document, ask, comment or message owned by the suggestion's own source
// issue, after the suggestion was recorded. This is "closed its own as duplicate of it" and
// "cite decisions where they were made" (skills/dispatch-first): the agent's own closing note or
// reply names the suggested item, and refs already turns that into an edge for every write that
// indexes a body, so no separate instrumentation of those write paths is needed here.
const suggestionActedOnByCitation = `
with pending as (
  select id, source_issue_key, source_artifact_id, suggested_kind, suggested_id, created_at
    from write_suggestions
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
with pending as (
  select id, suggested_issue_key, actor_kind, actor_id, created_at
    from write_suggestions
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
with pending as (
  select id, source_issue_key, source_artifact_id, actor_kind, actor_id, created_at
    from write_suggestions
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

// sweepSuggestionOutcomes runs one pass of all three resolution rules, in the order that lets
// an edge created in the same write as further activity count as acted_on rather than
// overridden: both acted_on passes run before the overridden pass sees what they left pending.
func sweepSuggestionOutcomes(ctx context.Context, pool *store.Pool) error {
	if _, err := pool.Exec(ctx, suggestionActedOnByCitation); err != nil {
		return err
	}
	if _, err := pool.Exec(ctx, suggestionActedOnByDirectUpdate); err != nil {
		return err
	}
	if _, err := pool.Exec(ctx, suggestionOverridden); err != nil {
		return err
	}
	return nil
}

// RunSuggestionOutcomeSweep resolves LEGION-550's write_suggestions on its own schedule, off the
// write path entirely: whether the agent acted on a suggestion (cited it, or updated the
// suggested issue directly) or overrode it (kept working on what it filed instead). It stops
// when ctx is done.
func RunSuggestionOutcomeSweep(ctx context.Context, pool *store.Pool, interval time.Duration) {
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			if err := sweepSuggestionOutcomes(ctx, pool); err != nil {
				slog.Warn("dispatch: suggestion outcome sweep failed", "error", err)
			}
		}
	}
}
