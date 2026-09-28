package api

// unreadDirectRepliesCTE is the one definition of "a reply this login has not read in this
// session": a session's own reply, anywhere under a direct message this login sent that session
// (an issue-less message targeted at it, a broadcast's copy included), newer than the later of
// the login's read mark and Clear for that session. It is the recursive part of a
// `with recursive` query and ends in the relation `unread_direct_replies(session_id, root_id,
// reply_id, created_at)`.
//
// Both endpoints that read it build on this fragment rather than on a predicate of their own:
// `GET /api/v1/me/agents/state` counts its rows per session (`unread_replies`), and
// `GET /api/v1/agents/{session_id}/messages` unions its roots into the conversation window. The
// two disagreeing is exactly LEGION-301's overflow defect - a reply counted, never shown, then
// marked read by a watermark that only moves forward - so one edit has to move both.
//
// Parameters, in this order, so a query appending its own starts at $4:
//
//	$1  the caller's canonical login (keys the read mark, and matches the author of their own
//	    direct messages, which the identity source may spell in any casing)
//	$2  the caller's raw actor id (keys the Clear, user_agent_state, migration 0033)
//	$3  one session id to narrow to, or null for every session
const unreadDirectRepliesCTE = `
	direct_roots as (
		select id as root_id, substr(target, length('session:') + 1) as session_id
		from messages
		where issue_key is null and in_reply_to is null and target like 'session:%'
		  and author->>'kind' = 'user' and lower(author->>'id') = $1
		  and ($3::text is null or target = 'session:' || $3::text)
	),
	direct_thread as (
		select m.id, m.author, m.created_at, direct_roots.root_id, direct_roots.session_id
		from messages m join direct_roots on m.in_reply_to = direct_roots.root_id
		union all
		select m.id, m.author, m.created_at, direct_thread.root_id, direct_thread.session_id
		from messages m join direct_thread on m.in_reply_to = direct_thread.id
	),
	direct_marks as (
		select coalesce(marked.session_id, cleared.session_id) as session_id,
		       greatest(marked.read_through, cleared.cleared_before) as at
		from (
			select session_id, read_through from user_agent_read
			where login = $1 and ($3::text is null or session_id = $3::text)
		) marked
		full join (
			select session_id, cleared_before from user_agent_state
			where login = $2 and ($3::text is null or session_id = $3::text)
		) cleared on cleared.session_id = marked.session_id
	),
	unread_direct_replies as (
		select direct_thread.session_id, direct_thread.root_id,
		       direct_thread.id as reply_id, direct_thread.created_at
		from direct_thread
		left join direct_marks on direct_marks.session_id = direct_thread.session_id
		where direct_thread.author->>'kind' = 'session'
		  and direct_thread.created_at > coalesce(direct_marks.at, '-infinity'::timestamptz)
	)`
