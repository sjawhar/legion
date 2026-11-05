package refs

// PersonInboxEdgesSQL is current inbox membership, private to the own-inbox
// read and comparison. It is not historical delivery and is not in AllEdgesSQL.
const PersonInboxEdgesSQL = "graph_inbox_edges"

// MentionEdgesSQL is the typed identity-mention sibling for named mention reads.
// It is part of AllEdgesSQL; graph_inbox_edges is not, so the references API never
// serves current inbox membership as recorded delivery.
const MentionEdgesSQL = "graph_mention_edges"

// AllEdgesSQL combines citations, identity, recorded delivery and identity mentions
// without changing graph_edges, preserving existing readers during rollout and rollback.
const AllEdgesSQL = `(
	select from_kind, from_id, kind, to_kind, to_id, created_at, source_seq from graph_edges
	union all
	select from_kind, from_id, kind, to_kind, to_id, created_at, source_seq from graph_person_edges
	union all
	select from_kind, from_id, kind, to_kind, to_id, created_at, source_seq from ` + MentionEdgesSQL + `
)`
