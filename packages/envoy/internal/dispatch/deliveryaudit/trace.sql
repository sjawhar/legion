-- Everything Dispatch recorded about one comment's delivery, for the comment the comment variable
-- names (a uuid). Read-only: five selects, each result's first column, section, labels it.
-- README.md says how to run it (psql --csv -v comment=<uuid>).

-- transaction_events: every event the comment's write appended. Events of one transaction share
-- their owner and their created_at (now(), the transaction's start: events.Broker.Append).
with created as (
    select e.*
    from events e
    where e.type = 'comment.created' and e.payload ->> 'id' = :'comment'::uuid::text
    order by e.id
    limit 1
)
select 'transaction_events' as section,
       e.id::text as event_id,
       e.seq::text as seq,
       e.type,
       coalesce(e.actor ->> 'kind', '') || ':' || coalesce(e.actor ->> 'id', '') as actor,
       case when e.notify then 'true' else 'false' end as notify,
       to_char(e.created_at at time zone 'UTC', 'YYYY-MM-DD"T"HH24:MI:SS.US"Z"') as created_at,
       coalesce(to_char(e.published_at at time zone 'UTC', 'YYYY-MM-DD"T"HH24:MI:SS.US"Z"'), '') as published_at,
       array_to_string(e.published_destinations, ' ') as published_destinations
from created c
join events e
  on e.created_at = c.created_at
 and (e.issue_key = c.issue_key or e.artifact_id = c.artifact_id or e.project_key = c.project_key)
order by e.seq;

-- comment_mentions: whom the comment mentioned, and the session each mention resolved to.
select 'comment_mentions' as section,
       m.target,
       m.delivery,
       coalesce(m.resolved_session_id, '') as resolved_session_id
from comment_mentions m
where m.comment_id = :'comment'::uuid
order by m.target collate "C";

-- comment_deliveries: every delivery attempt of those mentions.
select 'comment_deliveries' as section,
       d.target,
       d.attempt::text as attempt,
       d.delivery,
       coalesce(d.session_id, '') as session_id,
       d.state,
       coalesce(d.error, '') as error,
       coalesce(d.resolve_error, '') as resolve_error,
       coalesce(d.envelope_id, '') as envelope_id,
       case when d.duplicate then 'true' else 'false' end as duplicate,
       to_char(d.created_at at time zone 'UTC', 'YYYY-MM-DD"T"HH24:MI:SS.US"Z"') as created_at
from comment_deliveries d
where d.comment_id = :'comment'::uuid
order by d.target collate "C", d.attempt;

-- issue_state: the issue's route and claim as they stood when the comment.created event was
-- published (its created_at while unpublished; the comment's own when it has no event), each from
-- the latest issue event at or before then whose payload carries it, else from the issues row; and
-- the row as it stands now.
with target as (
    select c.id,
           coalesce(c.issue_key, ar.issue_key) as issue_key,
           coalesce(e.published_at, e.created_at, c.created_at) as as_of
    from comments c
    left join artifacts ar on ar.id = c.artifact_id
    left join lateral (
        select ev.published_at, ev.created_at
        from events ev
        where ev.type = 'comment.created' and ev.payload ->> 'id' = c.id::text
        order by ev.id
        limit 1
    ) e on true
    where c.id = :'comment'::uuid
)
select 'issue_state' as section,
       i.key as issue,
       to_char(t.as_of at time zone 'UTC', 'YYYY-MM-DD"T"HH24:MI:SS.US"Z"') as as_of,
       coalesce(case when route_event.seq is not null then route_event.route else i.route end, '') as route_used,
       coalesce(route_event.seq::text, 'issues-row') as route_source,
       coalesce(claim.actor ->> 'kind', '') || case when claim.actor is null then '' else ':' end
           || coalesce(claim.actor ->> 'id', '') as claim_used,
       coalesce(claim_event.seq::text, 'issues-row') as claim_source,
       coalesce(i.route, '') as route_now,
       coalesce(i.claimed_by ->> 'kind', '') || case when i.claimed_by is null then '' else ':' end
           || coalesce(i.claimed_by ->> 'id', '') as claim_now,
       coalesce(i.closed_at is not null, false)::text as closed_now
from target t
join issues i on i.key = t.issue_key
left join lateral (
    select e.seq, e.payload ->> 'route' as route
    from events e
    where e.issue_key = i.key and e.type like 'issue.%' and e.payload -> 'route' is not null and e.created_at <= t.as_of
    order by e.seq desc
    limit 1
) route_event on true
left join lateral (
    select e.seq, e.payload -> 'claim' -> 'actor' as actor
    from events e
    where e.issue_key = i.key and e.type like 'issue.%' and e.payload -> 'claim' is not null and e.created_at <= t.as_of
    order by e.seq desc
    limit 1
) claim_event on true
cross join lateral (
    select case when claim_event.seq is not null then claim_event.actor else i.claimed_by end as actor
) claim;

-- thread: the thread the comment is in, oldest first - the ask and every reply under it for an ask
-- thread, else the root comment and every reply to it.
with this as (
    select c.id, c.ask_id, coalesce(c.reply_to, c.id) as root
    from comments c
    where c.id = :'comment'::uuid
),
thread as (
    select 'ask' as kind, a.id, a.author, a.created_at, a.state = 'resolved' as resolved,
           null::jsonb as resolved_by, null::timestamptz as resolved_at, a.state
    from this
    join asks a on a.id = this.ask_id
    union all
    select case when c.id = this.root and this.ask_id is null then 'root' else 'reply' end,
           c.id, c.author, c.created_at, c.resolved, c.resolved_by, c.resolved_at, ''
    from this
    join comments c
      on (this.ask_id is not null and c.ask_id = this.ask_id)
      or (this.ask_id is null and (c.id = this.root or c.reply_to = this.root))
)
select 'thread' as section,
       t.kind,
       t.id::text as id,
       case when t.id = (select id from this) then 'true' else 'false' end as is_comment,
       coalesce(t.author ->> 'kind', '') || ':' || coalesce(t.author ->> 'id', '') as author,
       to_char(t.created_at at time zone 'UTC', 'YYYY-MM-DD"T"HH24:MI:SS.US"Z"') as created_at,
       case when t.resolved then 'true' else 'false' end as resolved,
       coalesce(coalesce(t.resolved_by ->> 'kind', '') || ':' || (t.resolved_by ->> 'id'), '') as resolved_by,
       coalesce(to_char(t.resolved_at at time zone 'UTC', 'YYYY-MM-DD"T"HH24:MI:SS.US"Z"'), '') as resolved_at,
       t.state as ask_state
from thread t
order by t.created_at, t.id;
