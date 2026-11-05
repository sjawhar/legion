package refs

// PersonInboxEdgesSQL is current inbox membership, private to the own-inbox
// read and comparison. It is not historical delivery and is not in AllEdgesSQL.
const PersonInboxEdgesSQL = "graph_inbox_edges"

// AllEdgesSQL combines citations, identity and recorded delivery without changing
// graph_edges, preserving existing readers during rollout and rollback.
const AllEdgesSQL = `(
	select from_kind, from_id, kind, to_kind, to_id, created_at, source_seq from graph_edges
	union all
	select from_kind, from_id, kind, to_kind, to_id, created_at, source_seq from graph_person_edges
)`
