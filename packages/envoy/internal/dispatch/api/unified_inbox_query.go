package api

import (
	"github.com/sjawhar/envoy/internal/dispatch/refs"
	"github.com/sjawhar/envoy/internal/dispatch/store"
)

// Membership comes from the private inbox edges, minus read state. In particular,
// no legacy membership predicate may hide a wrong edge from the comparison.
// $1 is the canonical login; $2 is the raw actor id that keys Clear.
const unifiedInboxDelivered = `
 delivered as (
  select g.cause, g.from_kind as kind, g.from_id as id, g.thread_id, g.session_id, g.created_at as at
  from ` + refs.PersonInboxEdgesSQL + ` g
  where g.kind = 'delivered_to' and g.to_kind = 'person' and g.to_id = $1
    and (g.cause <> 'thread_reply' or not exists (
      select 1 from user_threads u
      where u.login = $1 and u.thread_id = g.thread_id::uuid and not u.unread))
    and (g.cause <> 'direct_reply' or (
      g.created_at > coalesce(greatest(
          (select read_through from user_agent_read where login = $1 and session_id = g.session_id),
          (select cleared_before from user_agent_state where login = $2 and session_id = g.session_id)
        ), '-infinity'::timestamptz)
      and not exists (
        select 1 from user_agent_reply_read
        where login = $1 and reply_id = g.from_id::uuid)))
 )`

// Detail joins must not subtract membership: an edge to a missing source is still
// unexpected. Page parameters follow the two identity parameters.
const unifiedInboxQuery = `with` + unifiedInboxDelivered + `,
 page as (
  select cause, kind, id, thread_id, session_id, at
  from delivered order by at desc, cause, kind, id desc limit $3 offset $4
 ),
 rendered as (
  select p.cause, p.kind, p.id, p.thread_id, p.at as created_at,
   coalesce(case p.cause
     when 'ask' then ia.question
     when 'direct_reply' then dm.body
     else coalesce(tc.body, tm.body, concat_ws(' - ',
       nullif(array_to_string(array(select jsonb_array_elements_text(
         case when jsonb_typeof(ta.answer->'selected') = 'array' then ta.answer->'selected' else '[]'::jsonb end)), ', '), ''),
       nullif(ta.answer->>'text', ''))) end, '') as body,
   coalesce(case p.cause
     when 'ask' then ia.author
     when 'direct_reply' then dm.author
     else coalesce(tc.author, tm.author, jsonb_build_object('kind','user','id',ta.answer->>'user')) end,
     jsonb_build_object('kind','user','id','')) as author,
   coalesce(case p.cause
     when 'ask' then '/issues/' || ii.key || '/asks/' || p.id
     when 'direct_reply' then '/agents/' || p.session_id || '/live'
     else case when u.issue_key is not null
       then '/issues/' || u.issue_key || '/' ||
         case u.kind when 'ask' then 'asks' when 'comment' then 'comments' else 'messages' end ||
         '/' || p.thread_id
       else '/projects/' || ar.project_key || '/documents/' || ar.slug || '?' || u.kind || '=' || p.thread_id
     end end, '') as href,
   coalesce(case p.cause
     when 'ask' then ia.question
     when 'direct_reply' then droot.body
     else coalesce(ta.question, tcroot.body, tmroot.body) end, '') as subject,
   sn.snoozed_until
  from page p
  left join asks ia on p.cause = 'ask' and ia.id = p.id::uuid
  left join issues ii on p.cause = 'ask' and ii.key = ia.issue_key
  left join user_ask_snooze sn on p.cause = 'ask' and sn.login = $1 and sn.ask_id = p.id::uuid
  left join user_threads u on p.cause = 'thread_reply' and u.login = $1 and u.thread_id = p.thread_id::uuid
  left join asks ta on p.cause = 'thread_reply' and u.kind = 'ask' and ta.id = u.thread_id
  left join comments tc on p.cause = 'thread_reply' and u.kind <> 'message' and tc.id = p.id::uuid
  left join messages tm on p.cause = 'thread_reply' and u.kind = 'message' and tm.id = p.id::uuid
  left join comments tcroot on p.cause = 'thread_reply' and u.kind = 'comment' and tcroot.id = p.thread_id::uuid
  left join messages tmroot on p.cause = 'thread_reply' and u.kind = 'message' and tmroot.id = p.thread_id::uuid
  left join artifacts ar on p.cause = 'thread_reply' and ar.id = u.artifact_id
  left join messages dm on p.cause = 'direct_reply' and dm.id = p.id::uuid
  left join messages droot on p.cause = 'direct_reply' and droot.id = p.thread_id::uuid
 )
 select (select count(*)::int from delivered),
   coalesce(jsonb_agg(to_jsonb(rendered) order by created_at desc, cause, kind, id desc), '[]'::jsonb)
 from rendered`

const unifiedInboxMembershipQuery = `with` + unifiedInboxDelivered + `
 select cause, kind, id from delivered`

// Each reference query uses its route's own SQL and parameters, not a copy of
// those predicates. The Inbox is narrowed to the dashboard's default viewer.
const legacyInboxAsksQuery = `select 'ask'::text, 'ask'::text, a.id::text
 from asks a` + inboxOwnerJoins + inboxOpenAskFilter

const legacyInboxThreadsQuery = `select 'thread_reply'::text,
 case when u.kind = 'message' then 'message'
      when u.kind = 'ask' and u.last_reply_id = u.thread_id then 'ask'
      else 'comment' end,
 u.last_reply_id::text ` + myRepliesUnread

const legacyInboxDirectRepliesQuery = `with recursive ` + unreadDirectRepliesCTE + `
 select 'direct_reply'::text, 'message'::text, reply_id::text from unread_direct_replies`

// Filter public mentions before LIMIT. Cast the input, not the uuid primary key;
// block node ids are not uuids, so the cast must be safe on every union arm.
var inboxMentionVisibleSQL = `(
 (g.from_kind='comment' and exists(
   select 1 from comments visible left join artifacts owner on owner.id=visible.artifact_id
   where visible.id=` + store.CastableUUID("g.from_id") + ` and (visible.issue_key is not null or owner.project_key is not null)))
 or (g.from_kind='message' and exists(
   select 1 from messages visible where visible.id=` + store.CastableUUID("g.from_id") + ` and visible.issue_key is not null))
 or (g.from_kind='block' and exists(
   select 1 from document_blocks visible join artifacts owner on owner.id=visible.artifact_id
   where visible.node_id=g.from_id and (owner.issue_key is not null or owner.project_key is not null)))
)`
