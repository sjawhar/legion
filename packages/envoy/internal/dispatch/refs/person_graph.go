package refs

// PersonInboxEdgesSQL is the typed relation for the named own-inbox query. On
// 20k comments and 100k events, the unrestricted union took 19.769 ms and scanned
// events twice: arbitrary stored edge kinds prevent planner pruning. This same
// sibling remains in AllEdgesSQL for general readers; no source is dual-written.
const PersonInboxEdgesSQL = "graph_inbox_edges"

// MentionEdgesSQL is the typed identity-mention sibling for named mention reads.
// It remains part of AllEdgesSQL; this avoids planning unrelated citation/delivery arms.
const MentionEdgesSQL = "graph_mention_edges"

// AllEdgesSQL is the complete derived graph for new named reads. The sibling relations
// add identity, recorded delivery, indexed inbox delivery, mentions and participation without changing graph_edges,
// so existing builds retain their result sets and query plans during rollout and rollback.
const AllEdgesSQL = `(
	select from_kind, from_id, kind, to_kind, to_id, created_at, source_seq from graph_edges
	where to_kind not in ('person_mention', 'session_mention', 'role_mention')
	union all
	select from_kind, from_id, kind, to_kind, to_id, created_at, source_seq from graph_person_edges
	union all
	select from_kind, from_id, kind, to_kind, to_id, created_at, source_seq from ` + PersonInboxEdgesSQL + `
	union all
	select from_kind, from_id, kind, to_kind, to_id, created_at, source_seq from ` + MentionEdgesSQL + `
	union all
	select from_kind, from_id, kind, to_kind, to_id, created_at, source_seq from graph_thread_edges
)`
