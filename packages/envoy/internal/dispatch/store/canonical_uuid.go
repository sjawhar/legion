package store

// canonicalUUIDForm matches the lower-case hyphenated uuid every id Dispatch hands out is spelled
// as, and that spelling only: the form the server writes into an anchor's artifact_id and compares
// as text.
const canonicalUUIDForm = `^[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$`

// CanonicalUUID is the SQL expression that reads text as a uuid where text is a canonical id and
// answers null where it is not, so an index build never raises over a row an earlier schema
// allowed. An ask's anchor artifact_id is the case this exists for: asks_anchor_artifact
// (migration 0032) casts it only where block_artifact_id is null, so an ask carrying both columns
// can hold an artifact_id no index has ever cast, and an unguarded cast answers SQLSTATE 22P02 for
// the whole statement rather than null for that one row.
//
// The guard is a regex because an index expression must be immutable: `~` is immutable, while
// pg_input_is_valid is only stable and create index refuses it. This expression is the index's,
// not every reader's: asks_anchor_artifact_id (migration 0139) indexes it, the sweep read
// (docs.recordedMarksQuery) compares it so its asks leg is an index scan, and
// AsksOutsideAnchorArtifactIndex counts through it so the boot log can never describe a different
// set of rows than the index leaves out. Those three are its only callers. A read resolves an
// anchor's document through CastableUUID instead, which answers every spelling the cast accepts.
//
// asks_anchor_artifact_id (migration 0139) indexes exactly CanonicalUUID("anchor->>'artifact_id'"),
// written out as literal SQL because a migration's text is immutable. Postgres reaches an
// expression index only through a predicate comparing its exact expression, so an edit here that
// 0139 does not carry costs the asks read its index scan:
// docs.TestRecordedMarkRefsReadsThroughTheAnchorIndexes fails the moment the two diverge.
func CanonicalUUID(text string) string {
	return `(case when ` + text + ` ~ '` + canonicalUUIDForm + `' then (` + text + `)::uuid end)`
}

// CastableUUID is the SQL expression a read resolves text as a uuid through: it answers a uuid for
// every spelling Postgres casts - upper case, braces, no hyphens, as well as the canonical form -
// exactly as the unguarded cast it replaced did, and null only where that cast would raise. So a
// read keeps the document every anchor that casts names, one stored value that casts to nothing
// resolves to no document rather than answering SQLSTATE 22P02 for the whole statement, and the
// answer never depends on which of the two guards a reader was written with.
//
// pg_input_is_valid is the cast's own input function, so this accepts exactly what the cast
// accepts. It is only stable, so create index refuses it: that is why the index expression
// (migration 0139), the sweep read (docs.recordedMarksQuery) and the boot count
// (AsksOutsideAnchorArtifactIndex) keep CanonicalUUID's immutable regex, which an index can carry.
// pg_input_is_valid is Postgres 16+, the supported floor.
func CastableUUID(text string) string {
	return `(case when pg_input_is_valid(` + text + `, 'uuid') then (` + text + `)::uuid end)`
}
