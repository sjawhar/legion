package outbox

import (
	"context"
	"errors"
	"fmt"
	"net"
	"reflect"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgproto3"

	"github.com/sjawhar/envoy/internal/dispatch/model"
	"github.com/sjawhar/envoy/internal/dispatch/store/storetest"
)

// One query reads the sessions the primary documents of a whole batch's issues credit, keyed by
// issue: a document that is not primary credits no session here, and an issue with no primary
// document, or with no row, has no entry.
func TestLoadPrimaryDocumentAuthorsReadsEveryIssueOfABatchInOneQuery(t *testing.T) {
	database := storetest.Open(t)
	seedIssue(t, database, "T-1", nil)
	seedSecondIssue(t, database, "T-2")
	alice := model.Actor{Kind: "user", ID: "alice"}
	writer := model.Actor{Kind: "session", ID: "session-writer"}
	editor := model.Actor{Kind: "session", ID: "session-editor"}
	seedSpec(t, database, "T-1", alice, []model.Actor{writer, alice}, []model.Actor{editor, writer})
	seedSpec(t, database, "T-2", editor)
	seedDocument(t, database, new("T-1"), "notes", model.Actor{Kind: "session", ID: "session-notes"})

	authors, err := loadPrimaryDocumentAuthors(context.Background(), database.Pool, []string{"T-1", "T-2", "T-9"})
	if err != nil {
		t.Fatalf("load primary document authors: %v", err)
	}
	want := map[string][]string{
		"T-1": {writer.ID, editor.ID},
		"T-2": {editor.ID},
	}
	if !reflect.DeepEqual(authors, want) {
		t.Fatalf("authors = %v, want %v", authors, want)
	}
}

// One query reads the recipients of a whole batch's replies, keyed by the comment each replies
// under: two threads give two entries, a legacy nested reply finds its thread's root, a comment
// with no row gives none, and a cyclic reply_to chain ends at the depth bound rather than
// spinning, with the authors of the comments the walk visited.
func TestLoadCommentRecipientsReadsEveryThreadOfABatchInOneQuery(t *testing.T) {
	database := storetest.Open(t)
	seedIssue(t, database, "T-1", nil)
	alice := model.Actor{Kind: "user", ID: "alice"}
	writer := model.Actor{Kind: "session", ID: "session-writer"}
	rooter := model.Actor{Kind: "session", ID: "session-rooter"}
	replier := model.Actor{Kind: "session", ID: "session-replier"}
	spec := seedDocument(t, database, new("T-1"), "spec", writer, []model.Actor{writer, alice}, nil)
	first := seedThreadComment(t, database, "T-1", spec, alice, nil)
	seedThreadComment(t, database, "T-1", "", replier, &first)
	second := seedThreadComment(t, database, "T-1", "", rooter, nil)
	nested := seedThreadComment(t, database, "T-1", "", replier, &second)
	seedThreadComment(t, database, "T-1", "", writer, &nested)
	cyclic := seedThreadComment(t, database, "T-1", "", rooter, nil)
	loop := seedThreadComment(t, database, "T-1", "", replier, &cyclic)
	if _, err := database.Pool.Exec(context.Background(), `update comments set reply_to = $1 where id = $2`, loop, cyclic); err != nil {
		t.Fatalf("create a reply_to cycle: %v", err)
	}

	done := make(chan struct{})
	var recipients map[string][]string
	var err error
	go func() {
		defer close(done)
		recipients, err = loadCommentRecipients(context.Background(), database.Pool,
			[]string{first, nested, cyclic, "00000000-0000-0000-0000-000000000099"})
	}()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("loadCommentRecipients did not end on a cyclic reply_to chain")
	}
	if err != nil {
		t.Fatalf("load comment recipients: %v", err)
	}
	want := map[string][]string{
		first:  {replier.ID, writer.ID},
		nested: {rooter.ID, replier.ID, writer.ID},
		cyclic: {rooter.ID, replier.ID},
	}
	if !reflect.DeepEqual(recipients, want) {
		t.Fatalf("recipients = %v, want %v", recipients, want)
	}
}

// refusingServer listens for Postgres connections and refuses each at its startup message with a
// FATAL error of code, as a server at max_connections (53300) or still starting (57P03) does, and
// returns its address.
func refusingServer(t *testing.T, code string) string {
	t.Helper()
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	t.Cleanup(func() { _ = listener.Close() })
	go func() {
		for {
			conn, err := listener.Accept()
			if err != nil {
				return
			}
			go func() {
				defer conn.Close()
				backend := pgproto3.NewBackend(conn, conn)
				for {
					startup, err := backend.ReceiveStartupMessage()
					if err != nil {
						return
					}
					switch startup.(type) {
					case *pgproto3.SSLRequest, *pgproto3.GSSEncRequest:
						if _, err := conn.Write([]byte("N")); err != nil {
							return
						}
						continue
					}
					break
				}
				backend.Send(&pgproto3.ErrorResponse{Severity: "FATAL", Code: code, Message: "sorry, too many clients already"})
				_ = backend.Flush()
			}()
		}
	}()
	return listener.Addr().String()
}

// A recipient statement is run again over fewer ids only when Postgres refused it over the rows it
// read (refusedStatement). A connection Postgres refused - a *pgconn.ConnectError that carries the
// server's too_many_connections as a *pgconn.PgError - and a server or connection failure on a live
// connection, the admin_shutdown a failover sends or a connection_failure, would fail every run
// alike, and so is no refused statement; nor is any error once ctx ended. invalid_parameter_value,
// what jsonb_array_elements answers a credit list that is no array with, is.
func TestARefusedConnectionOrAFailingServerIsNoRefusedStatement(t *testing.T) {
	ctx := context.Background()
	host, port, err := net.SplitHostPort(refusingServer(t, "53300"))
	if err != nil {
		t.Fatalf("split the server's address: %v", err)
	}
	_, refusedConnection := pgconn.Connect(ctx, fmt.Sprintf("host=%s port=%s user=outbox database=dispatch sslmode=disable connect_timeout=5", host, port))
	var connect *pgconn.ConnectError
	var carried *pgconn.PgError
	if !errors.As(refusedConnection, &connect) || !errors.As(refusedConnection, &carried) || carried.Code != "53300" {
		t.Fatalf("connecting to the refusing server answered %v, want a *pgconn.ConnectError carrying a *pgconn.PgError of code 53300", refusedConnection)
	}
	ended, end := context.WithCancel(ctx)
	end()
	statement := func(code string) error {
		return fmt.Errorf("load document session authors: %w", &pgconn.PgError{Severity: "ERROR", Code: code})
	}
	for _, tc := range []struct {
		name string
		ctx  context.Context
		err  error
		want bool
	}{
		{name: "a connection refused for too_many_connections (53300)", ctx: ctx, err: fmt.Errorf("load document session authors: %w", refusedConnection), want: false},
		{name: "admin_shutdown (57P01) on a live connection", ctx: ctx, err: statement("57P01"), want: false},
		{name: "connection_failure (08006)", ctx: ctx, err: statement("08006"), want: false},
		{name: "invalid_parameter_value (22023)", ctx: ctx, err: statement("22023"), want: true},
		{name: "invalid_parameter_value (22023) once ctx ended", ctx: ended, err: statement("22023"), want: false},
	} {
		if got := refusedStatement(tc.ctx, tc.err); got != tc.want {
			t.Errorf("refusedStatement(%s) = %v, want %v", tc.name, got, tc.want)
		}
	}
}
