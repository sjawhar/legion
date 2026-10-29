package store

// canonicalUUIDForm matches the lower-case hyphenated uuid every id Dispatch hands out is spelled
// as, and that spelling only: the form the server writes into an anchor's artifact_id and compares
// as text.
const canonicalUUIDForm = `^[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$`

// CanonicalUUID is the SQL expression that reads text as a uuid where text is a canonical id and
// answers null where it is not, so neither an index build nor a read raises over a row an earlier
// schema allowed. An ask's anchor artifact_id is the case this exists for: asks_anchor_artifact
// (migration 0032) casts it only where block_artifact_id is null, so an ask carrying both columns
// can hold an artifact_id no index has ever cast, and an unguarded cast answers SQLSTATE 22P02 for
// the whole statement rather than null for that one row.
//
// The guard is a regex because an index expression must be immutable: `~` is immutable, while
// pg_input_is_valid is only stable and create index refuses it. Every string the regex accepts
// casts to uuid, so this is the plain cast on every row that matters; a non-canonical spelling of
// a real id - upper case, braces, no hyphens - resolves to null, which costs a reader nothing,
// since the ids the server passes and compares as text are lower-case and never matched such a
// spelling either.
//
// asks_anchor_artifact_id (migration 0137) indexes exactly CanonicalUUID("anchor->>'artifact_id'"),
// written out as literal SQL because a migration's text is immutable. Postgres reaches an
// expression index only through a predicate comparing its exact expression, so an edit here that
// 0137 does not carry costs the asks read its index scan:
// docs.TestRecordedMarkRefsReadsThroughTheAnchorIndexes fails the moment the two diverge.
func CanonicalUUID(text string) string {
	return `(case when ` + text + ` ~ '` + canonicalUUIDForm + `' then (` + text + `)::uuid end)`
}
