package supervise

import (
	"errors"
	"strings"

	"github.com/jackc/pgx/v5/pgconn"
)

// writeOwed is the rule for a claim write the store refused: a write is owed unless the server
// answered and refused the statement. A server that answered has read the statement and decided
// against it — a constraint it breaks, a column it has not got, a fence it does not pass — and it
// would decide the same way again, so making that write a second time only buys the same refusal.
// Every other failure leaves the write unmade, or its fate unknown: the connection never came up,
// or it died with the statement in flight, or a deadline ended the wait before any answer. Those
// are owed, because dropping one loses a claim fact the daemon's memory already holds and the row
// does not, and the write is the only thing that would ever put it there. A nil error is no
// refusal and owes nothing.
//
// Two SQLSTATEs the server does answer with are not decisions about the statement, and are owed
// with the rest: class 08, the connection exception, where the connection itself is what failed,
// and 57P01, the backend terminated under the statement — the shutdown a managed restart of the
// database sends. Judging those two by their class and code is what keeps the rule to one
// question about the server's answer.
//
// The identities below are what pgx v5.11.0 returns, measured in this pod on 2026-11-07, and they
// are the only evidence this rule rests on:
//
// |what was done|error|Go type|SQLSTATE|pgconn.SafeToRetry|
// |---|---|---|---|---|
// |a pool at 127.0.0.1:1, then Exec|failed to connect to `user=… database=…`: … dial error: … connection refused|*pgconn.ConnectError|none|false|
// |Exec under a one second context against a listener that accepts and never answers|context deadline exceeded|context.deadlineExceededError|none|false|
// |pg_terminate_backend of the pool's one backend, then Exec|FATAL: terminating connection due to administrator command (SQLSTATE 57P01)|*pgconn.PgError|57P01|false|
//
// Those rows are why the rule cannot be written as a list of error types. The first is a pgx
// type, the second one the standard library returns through a path that reaches no pgx type at
// all, and the third is neither; what they share is not a type but the absence of an answer about
// the statement, which no list can be kept in step with. They are also why the rule cannot be
// pgconn.SafeToRetry: it is false for all three — it reports whether pgx knows the statement
// never reached the server, not whether the server decided against it — so a rule built on it
// would drop the dial failure and the cut-short write, the two that are most plainly owed. Only
// asking whether the server answered covers every row.
func writeOwed(err error) bool {
	if err == nil {
		return false
	}
	var pgErr *pgconn.PgError
	if errors.As(err, &pgErr) {
		return strings.HasPrefix(pgErr.Code, "08") || pgErr.Code == "57P01"
	}
	return true
}
