package api

import "github.com/sjawhar/envoy/internal/dispatch/refs"

// The first three parameters keep unreadDirectRepliesCTE's identity convention:
// canonical login, raw actor id, optional session (nil). Page parameters follow.
// Candidates use the existing indexes; the graph proves delivery, never replaces
// the indexes with an unrestricted traversal of comments, messages or events.
const unifiedInboxCandidates = `with recursive ` + unreadDirectRepliesCTE + `,
 inbox_candidates as (
  select 'ask'::text as cause, 'ask'::text as kind, a.id, a.id as thread_id,
         a.created_at as at, a.question as body, a.author,
         '/issues/' || i.key || '/asks/' || a.id::text as href,
         a.question as subject, sn.snoozed_until
  from issues i join asks a on a.issue_key = i.key
  left join user_ask_snooze sn on sn.login = $1 and sn.ask_id = a.id
  where i.assignee = $1 and i.closed_at is null and a.state = 'open'
  union all
  select 'thread_reply',
         case when u.kind = 'message' then 'message'
              when u.kind = 'ask' and u.last_reply_id = u.thread_id then 'ask'
              else 'comment' end,
         u.last_reply_id, u.thread_id, u.replied_at,
         coalesce(rc.body, rm.body, concat_ws(' - ',
           nullif(array_to_string(array(select jsonb_array_elements_text(
             case when jsonb_typeof(a.answer->'selected') = 'array' then a.answer->'selected' else '[]'::jsonb end)), ', '), ''),
           nullif(a.answer->>'text', '')), ''),
         coalesce(rc.author, rm.author, jsonb_build_object('kind','user','id',a.answer->>'user')),
         case when u.issue_key is not null then '/issues/' || u.issue_key || '/' ||
           case u.kind when 'ask' then 'asks' when 'comment' then 'comments' else 'messages' end || '/' || u.thread_id::text
           else '/projects/' || ar.project_key || '/documents/' || ar.slug || '?' || u.kind || '=' || u.thread_id::text end,
         coalesce(a.question, root.body, mr.body, ''), null::timestamptz
  from user_threads u
  left join asks a on u.kind = 'ask' and a.id = u.thread_id
  left join issues ai on u.kind = 'ask' and ai.key = u.issue_key
  left join comments rc on u.kind <> 'message' and rc.id = u.last_reply_id
  left join messages rm on u.kind = 'message' and rm.id = u.last_reply_id
  left join comments root on u.kind = 'comment' and root.id = u.thread_id
  left join messages mr on u.kind = 'message' and mr.id = u.thread_id
  left join artifacts ar on ar.id = u.artifact_id
  where u.login = $1 and u.unread
    and not coalesce(a.state = 'open' and (u.issue_key is null or ai.closed_at is null), false)
  union all
  select 'direct_reply', 'message', d.reply_id, d.root_id, d.created_at,
         m.body, m.author, '/agents/' || d.session_id || '/live', root.body, null::timestamptz
  from unread_direct_replies d join messages m on m.id = d.reply_id
  join messages root on root.id = d.root_id
 ), delivered as (
  select c.* from inbox_candidates c
  where exists (select 1 from ` + refs.PersonInboxEdgesSQL + ` g
    where g.kind = 'delivered_to' and g.to_kind = 'person' and g.to_id = $1
      and g.from_kind = c.kind and g.from_id = c.id::text)
 )`

const unifiedInboxQuery = unifiedInboxCandidates + `,
 page as (select cause, kind, id::text, thread_id::text, at as created_at, body, author, href, subject, snoozed_until
   from delivered order by at desc, cause, kind, id desc limit $4 offset $5)
 select (select count(*)::int from delivered),
   coalesce(jsonb_agg(to_jsonb(page) order by created_at desc, cause, kind, id desc), '[]'::jsonb) from page`

// The reference uses the existing route predicates and direct-reply definition,
// not delivered or the new view. Compare membership before pagination.
const legacyInboxMembershipQuery = `with recursive ` + unreadDirectRepliesCTE + `
 select 'ask'::text, a.id::text from asks a join issues i on i.key = a.issue_key
 where a.state = 'open' and i.closed_at is null and i.assignee = $1
 union all
 select 'thread_reply', u.last_reply_id::text
 from user_threads u
 left join asks a on u.kind = 'ask' and a.id = u.thread_id
 left join issues ai on u.kind = 'ask' and ai.key = u.issue_key
 where u.login = $1 and u.unread
   and not coalesce(a.state = 'open' and (u.issue_key is null or ai.closed_at is null), false)
 union all
 select 'direct_reply', reply_id::text from unread_direct_replies`
