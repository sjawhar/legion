package store

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"
)

// What a claim write refused by a database the daemon cannot reach really carries, measured
// against pgx rather than described: this is the one error identity the incident behind this
// change actually produced, and the rule that decides whether such a write is owed
// (internal/supervise) rests on these three facts about it.
//
// The write carries the prefix the incident's own log line carried, so the line an operator read
// is the line this statement writes. It unwraps to *pgconn.ConnectError, a pgx type of its own.
// No *pgconn.PgError unwraps from it, which is the half that decides it: no server answered about
// the statement at all, so there is no SQLSTATE to judge, and the row is certainly unwritten.
// pgconn.SafeToRetry is false for it, although nothing was ever sent — it reports whether pgx
// knows the statement never reached the server, not whether the server decided against it.
//
// Those last two are why the rule in internal/supervise is neither a list of error types nor
// pgconn.SafeToRetry. A list would have to name this pgx type, the standard library's deadline
// error and the server's own answers, and be kept in step with all three; SafeToRetry would drop
// the write that is most plainly owed, this one. Asking whether a server answered covers them
// all. The rule itself is unexported and lives in internal/supervise, which imports this package,
// so this test measures the error rather than the rule: it is the evidence the rule is written
// against.
//
// It needs no database, and must not have one: an address nothing listens on is the whole point.
// Open is not the way in, because it waits for the server to answer before it hands back a Store;
// the pool it would have built is built here instead, which connects lazily, so the claim write
// is what meets the refusal — exactly as a daemon whose database went away meets it.
func TestTheOwedWriteErrorsOfAnUnreachableDatabaseCarryNoSQLSTATE(t *testing.T) {
	ctx := context.Background()
	pool, err := pgxpool.New(ctx, "postgres://legion:secret@127.0.0.1:1/legion")
	if err != nil {
		t.Fatalf("build a pool at an address nothing listens on: %v", err)
	}
	defer pool.Close()
	s := &Store{pool: pool}

	err = s.PutClaim(ctx, tmuxClaim("legion-LEGION-209-implementer"))

	if err == nil {
		t.Fatal("the claim write succeeded; nothing listens on 127.0.0.1:1, so it had no server to write to")
	}
	if !strings.HasPrefix(err.Error(), "put claim ") {
		t.Errorf("the write failed with %q, want the %q prefix the incident's log line carried", err, "put claim ")
	}
	var connErr *pgconn.ConnectError
	if !errors.As(err, &connErr) {
		t.Errorf("the write failed with %T, want a *pgconn.ConnectError to unwrap from it", err)
	}
	var pgErr *pgconn.PgError
	if errors.As(err, &pgErr) {
		t.Errorf("a server error unwraps from the write as %v (SQLSTATE %s); no server answered it", pgErr, pgErr.Code)
	}
	if pgconn.SafeToRetry(err) {
		t.Error("pgconn.SafeToRetry is true for a dial that was refused; the rule that owes this write cannot be built on it")
	}
}
