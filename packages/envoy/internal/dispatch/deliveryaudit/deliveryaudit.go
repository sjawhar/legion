// Package deliveryaudit holds the read-only SQL that audits whether a person's Dispatch comments
// reached the agent sessions they were owed to (sjawhar/legion#2366): comments.sql lists every
// person's comment of a window with each session or role it was owed or addressed to, and trace.sql
// shows everything recorded about one comment's delivery. An operator runs them with psql
// (README.md); the package's test runs the same embedded bytes through pgx, with each psql variable
// bound as a parameter, over records the real API handlers and outbox wrote.
package deliveryaudit

import _ "embed"

// CommentsSQL is comments.sql: one select over the psql variables from and to.
//
//go:embed comments.sql
var CommentsSQL string

// TraceSQL is trace.sql: several selects over the psql variable comment.
//
//go:embed trace.sql
var TraceSQL string
