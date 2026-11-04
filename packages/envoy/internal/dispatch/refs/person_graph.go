package refs

// AllEdgesSQL is the complete derived graph for new named reads. The sibling relations
// add identity, recorded delivery and indexed inbox delivery without changing graph_edges,
// so existing builds retain their result sets and query plans during rollout and rollback.
const AllEdgesSQL = `(
	select from_kind, from_id, kind, to_kind, to_id, created_at, source_seq from graph_edges
	union all
	select from_kind, from_id, kind, to_kind, to_id, created_at, source_seq from graph_person_edges
	union all
	select from_kind, from_id, kind, to_kind, to_id, created_at, source_seq from graph_inbox_edges
)`
