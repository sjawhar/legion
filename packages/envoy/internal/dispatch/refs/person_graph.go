package refs

// AllEdgesSQL is the complete derived graph relation for new named reads. graph_edges retains
// the relations old builds read; graph_person_edges adds identity and recorded delivery. Keeping
// the union here leaves old readers' result sets and query plans unchanged, including rollback.
const AllEdgesSQL = `(
	select from_kind, from_id, kind, to_kind, to_id, created_at, source_seq from graph_edges
	union all
	select from_kind, from_id, kind, to_kind, to_id, created_at, source_seq from graph_person_edges
)`
