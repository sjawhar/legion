package store

// canonicalUUIDForm matches the lower-case hyphenated uuid every id Dispatch hands out is spelled
// as, and that spelling only: the form the server writes into an anchor's artifact_id and compares
// as text.
const canonicalUUIDForm = `^[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$`

// CanonicalUUID is the SQL expression that reads text as a uuid where text is a canonical id and
// answers null where it is not, so an index build over it never raises over a row an earlier
// schema allowed: an ask's anchor artifact_id, which asks_anchor_artifact (migration 0032) casts
// only where block_artifact_id is null, can hold a value no index has ever cast, and an unguarded
// cast answers SQLSTATE 22P02 for the whole statement rather than null for that one row.
//
// The guard is a regex because an index expression must be immutable: `~` is immutable, while
// pg_input_is_valid is only stable and create index refuses it. This expression is safe in an
// index, and must stay equal to the literal SQL migration 0148 spells out, since a migration's
// text is immutable and Postgres reaches an expression index only through a predicate comparing
// its exact expression: docs.TestRecordedMarkRefsReadsThroughTheAnchorIndexes fails the moment
// an edit here diverges from 0148.
func CanonicalUUID(text string) string {
	return `(case when ` + text + ` ~ '` + canonicalUUIDForm + `' then (` + text + `)::uuid end)`
}

// CastableUUID is the SQL expression that reads text as a uuid wherever ::uuid accepts it - upper
// case, braces, no hyphens, as well as the canonical form - and answers null only where that cast
// would raise, so one unreadable value resolves to nothing rather than answering SQLSTATE 22P02
// for the whole statement.
//
// pg_input_is_valid is the cast's own input function, so this accepts exactly what the cast
// accepts. It is only stable, so create index refuses it: this expression cannot be indexed.
// pg_input_is_valid is Postgres 16+, the supported floor.
func CastableUUID(text string) string {
	return `(case when pg_input_is_valid(` + text + `, 'uuid') then (` + text + `)::uuid end)`
}
