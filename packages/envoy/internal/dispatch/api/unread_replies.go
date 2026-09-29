package api

// unreadDirectRepliesCTE is the one definition of "a reply this login has not read in this
// session": a session's own reply, anywhere under a root this viewer targeted at this session
// (an issue-less message whose own target is `session:<id>`, a broadcast's copy included), newer
// than the later of the login's read mark and Clear for that session. That scope is deliberate.
// A root the session only received a delivery of - one targeted at a role, or at another
// session - is not a direct message this viewer sent it, so it is never unread for them; the
// conversation window still lists it by activity, like any other conversation the session is in. It is the recursive part of a
// `with recursive` query and ends in two relations: `unread_direct_replies(session_id, root_id,
// reply_id, created_at)`, and the marks it reads them against,
// `direct_marks(session_id, read_through, cleared_before, at)`, which is also what
// `GET /api/v1/me/agents/state` answers with.
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
//
// The login is bound once by unreadDirectRepliesArgs, referenced only inside this fragment
// (direct_roots and direct_marks), and never inside a union arm - the window's candidates union
// takes $3 alone. The hazard a future edit opens is exactly that: put a login predicate into one
// candidate branch (filtering the delivered branch to roots the viewer authored, say) and the
// surface genuinely doubles, with only the call-site mutation's test standing behind it.
//
// unreadDirectRepliesArgs builds them, so no call site spells the canonical login itself.
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
	-- $1 (canonical) keys user_agent_read and $2 (raw) keys user_agent_state: the deliberate
	-- asymmetry between 0048's convention and 0033's, which nothing here is free to swap. Swapping
	-- them is caught by TestAViewersRepliesCountAndClearWhateverTheCasingOfTheirLogin alone.
	-- That the Clear write stays raw is held by TestClearIsKeyedOnTheRawActorID; why it stays raw
	-- is held by neither that test nor any other, because canonicalising the write and this read
	-- together is self-consistent and green. The reason is the deploy: a Dispatch image predating
	-- user_agent_read wrote cleared_before under the raw actor id and must still read it back
	-- across a rolling deploy, which is rehearsed rather than tested (LEGION-301, #1533).
	direct_marks as (
		select coalesce(marked.session_id, cleared.session_id) as session_id,
		       marked.read_through, cleared.cleared_before,
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

// unreadDirectRepliesArgs is unreadDirectRepliesCTE's parameter list for one caller: their
// canonical login, their raw actor id, and the session to narrow to (nil for every session). A
// query appending its own parameters starts at $4.
func unreadDirectRepliesArgs(actorID string, sessionID *string) []any {
	return []any{canonicalLogin(actorID), actorID, sessionID}
}
