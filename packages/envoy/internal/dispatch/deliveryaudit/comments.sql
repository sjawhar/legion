-- Every person's comment created from the from variable (inclusive) to the to variable (exclusive)
-- with each agent session or role it was owed or addressed to, one row per (comment, target).
-- Read-only: one select, no temporary tables.
-- README.md holds the column contract and how to run it (psql --csv -v from=... -v to=...).
--
-- A comment's targets are the union of the ones it was owed and every notifications.agent.* or
-- notifications.role.* subject its comment.created event's published_destinations records. Owed is:
-- the issue's route when it has one, else its claimant when that is a session; for a reply on an ask
-- thread the ask's asker and every session that followed the ask then; and every session a mention
-- resolved to. Route, claim and followers are read as they stood when the outbox published the event
-- (its published_at, or its created_at while it is unpublished).
with window_comments as (
    select c.id, c.author, c.anchor, c.reply_to, c.ask_id, c.created_at,
           coalesce(c.issue_key, ar.issue_key) as owner_issue,
           ar.project_key as artifact_project,
           ar.slug as artifact_slug
    from comments c
    left join artifacts ar on ar.id = c.artifact_id
    where c.author ->> 'kind' = 'user'
      and c.created_at >= :'from'::timestamptz
      and c.created_at < :'to'::timestamptz
),
-- The comment.created event each comment's write appended (comment_create.go), found by the
-- comment id its payload carries.
created_events as (
    select distinct on (e.payload ->> 'id')
           e.payload ->> 'id' as comment_id, e.id, e.seq, e.issue_key, e.artifact_id, e.project_key,
           e.created_at, e.published_at, e.published_destinations
    from events e
    where e.type = 'comment.created'
      and e.payload ->> 'id' in (select id::text from window_comments)
    order by e.payload ->> 'id', e.id
),
base as (
    select wc.*,
           ce.id as event_id,
           ce.published_at,
           coalesce(ce.published_destinations, '{}'::text[]) as destinations,
           coalesce(ce.published_at, ce.created_at, wc.created_at) as as_of,
           -- A reply that reopens its resolved root appends comment.reopened just before its own
           -- comment.created, in the same transaction: the same owner, the same created_at (now(),
           -- the transaction's start) and the sequence number before it.
           coalesce(wc.reply_to is not null and exists (
               select 1
               from events r
               where r.type = 'comment.reopened'
                 and r.seq = ce.seq - 1
                 and r.created_at = ce.created_at
                 and r.payload ->> 'id' = wc.reply_to::text
                 and (r.issue_key = ce.issue_key or r.artifact_id = ce.artifact_id or r.project_key = ce.project_key)
           ), false) as reopened
    from window_comments wc
    left join created_events ce on ce.comment_id = wc.id::text
),
-- The issue's route and claim as at as_of: each from the latest issue event at or before it whose
-- payload carries the key (issue.created and issue.updated carry both, issue.claimed and
-- issue.released only the claim), and from the issues row only when no such event exists.
state as (
    select b.*,
           i.key is not null as has_issue,
           case when route_event.seq is not null then route_event.route else i.route end as route_value,
           case when claim_event.seq is not null then claim_event.claim -> 'actor' else i.claimed_by end as claim_actor,
           coalesce(route_event.seq::text, 'issues-row') as route_from,
           coalesce(claim_event.seq::text, 'issues-row') as claim_from
    from base b
    left join issues i on i.key = b.owner_issue
    left join lateral (
        select e.seq, e.payload ->> 'route' as route
        from events e
        where e.issue_key = i.key
          and e.type like 'issue.%'
          and e.payload -> 'route' is not null
          and e.created_at <= b.as_of
        order by e.seq desc
        limit 1
    ) route_event on true
    left join lateral (
        select e.seq, e.payload -> 'claim' as claim
        from events e
        where e.issue_key = i.key
          and e.type like 'issue.%'
          and e.payload -> 'claim' is not null
          and e.created_at <= b.as_of
        order by e.seq desc
        limit 1
    ) claim_event on true
),
-- Every change to who follows an ask, read from the records that make one: the asker follows
-- from the ask's creation, a session follows from each reply it posts on the ask (both through
-- asks.FollowAuthor, which appends no event), and ask.follower_added / ask.follower_removed add
-- and remove one.
follower_events as materialized (
    select f.payload ->> 'ask_id' as ask_id, f.payload ->> 'session_id' as session_id,
           f.created_at as at, f.id as event_id, f.type = 'ask.follower_added' as joined
    from events f
    where f.type in ('ask.follower_added', 'ask.follower_removed')
),
follower_changes as (
    select b.id as comment_id, x.session_id, x.at, x.event_id, x.joined
    from base b
    cross join lateral (
        select a.author ->> 'id' as session_id, a.created_at as at, 0::bigint as event_id, true as joined
        from asks a
        where a.id = b.ask_id and a.author ->> 'kind' = 'session'
        union all
        select r.author ->> 'id', r.created_at, 0::bigint, true
        from comments r
        where r.ask_id = b.ask_id and r.author ->> 'kind' = 'session'
        union all
        select f.session_id, f.at, f.event_id, f.joined
        from follower_events f
        where f.ask_id = b.ask_id::text
    ) x
    where b.ask_id is not null
      and x.at <= b.as_of
      and coalesce(x.session_id, '') <> ''
),
followers as (
    select distinct on (comment_id, session_id) comment_id, session_id, joined
    from follower_changes
    order by comment_id, session_id, at desc, event_id desc
),
owed as (
    -- The route's subject, as the outbox's publishRoute names it.
    select s.id as comment_id,
           case
               when s.route_value ~ '^role:[a-z0-9-]+$' then 'notifications.role.' || substr(s.route_value, 6)
               when s.route_value ~ '^session:[A-Za-z0-9_-]+$' then 'notifications.agent.' || substr(s.route_value, 9)
           end as target,
           'route' as reason
    from state s
    where coalesce(s.route_value, '') <> ''
    union all
    select s.id, 'notifications.agent.' || (s.claim_actor ->> 'id'), 'claimant'
    from state s
    where coalesce(s.route_value, '') = ''
      and s.claim_actor ->> 'kind' = 'session'
      and coalesce(s.claim_actor ->> 'id', '') <> ''
    union all
    select b.id, 'notifications.agent.' || (a.author ->> 'id'), 'asker'
    from base b
    join asks a on a.id = b.ask_id
    where a.author ->> 'kind' = 'session' and coalesce(a.author ->> 'id', '') <> ''
    union all
    select f.comment_id, 'notifications.agent.' || f.session_id, 'follower'
    from followers f
    where f.joined
    union all
    select b.id, 'notifications.agent.' || m.resolved_session_id, 'mention'
    from base b
    join comment_mentions m on m.comment_id = b.id
    where coalesce(m.resolved_session_id, '') <> ''
),
addressed as (
    select distinct b.id as comment_id, d.subject as target
    from base b
    cross join lateral unnest(b.destinations) as d(subject)
    where d.subject like 'notifications.agent.%' or d.subject like 'notifications.role.%'
),
targets as (
    select o.comment_id, o.target, string_agg(distinct o.reason, ';' order by o.reason) as owed_reasons
    from owed o
    where o.target is not null
    group by o.comment_id, o.target
    union all
    select a.comment_id, a.target, 'addressed-only'
    from addressed a
    where not exists (
        select 1 from owed o where o.comment_id = a.comment_id and o.target = a.target
    )
)
select s.id::text as comment_id,
       coalesce(s.event_id::text, '') as event_id,
       coalesce(s.owner_issue, coalesce(s.artifact_project, '') || '/' || s.artifact_slug, '') as owner,
       to_char(s.created_at at time zone 'UTC', 'YYYY-MM-DD"T"HH24:MI:SS.US"Z"') as created_at,
       coalesce(to_char(s.published_at at time zone 'UTC', 'YYYY-MM-DD"T"HH24:MI:SS.US"Z"'), '') as published_at,
       coalesce(s.author ->> 'id', '') as author,
       case
           when s.ask_id is not null then 'ask_thread'
           when s.reply_to is not null then 'reply'
           when s.anchor is not null then 'anchored'
           else 'conversation'
       end as shape,
       case when s.reopened then 'true' else 'false' end as reopened,
       coalesce(t.target, '') as target,
       case
           when t.target like 'notifications.agent.%' then 'session'
           when t.target like 'notifications.role.%' then 'role'
           else ''
       end as target_kind,
       coalesce(t.owed_reasons, '') as owed_reasons,
       case when t.target = any(s.destinations) then 'true' else 'false' end as addressed,
       coalesce(delivery.state, '') as mention_state,
       coalesce(delivery.error, '') as mention_error,
       coalesce(s.route_value, '') as route_used,
       case
           when s.claim_actor ->> 'kind' is not null
               then (s.claim_actor ->> 'kind') || ':' || coalesce(s.claim_actor ->> 'id', '')
           else ''
       end as claim_used,
       case
           when not s.has_issue then ''
           when s.route_from = s.claim_from then s.route_from
           else s.route_from || ';claim=' || s.claim_from
       end as route_source
from state s
left join targets t on t.comment_id = s.id
-- The newest delivery attempt of the mention that resolved to this target session.
left join lateral (
    select cd.state, coalesce(cd.error, cd.resolve_error) as error
    from comment_deliveries cd
    where cd.comment_id = s.id
      and (
          cd.session_id = substr(t.target, 21)
          or cd.target in (
              select m.target from comment_mentions m
              where m.comment_id = s.id and m.resolved_session_id = substr(t.target, 21)
          )
      )
    order by cd.attempt desc
    limit 1
) delivery on t.target like 'notifications.agent.%'
order by s.created_at, s.id, coalesce(t.target, '') collate "C";
