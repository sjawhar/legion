package main

import (
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgproto3"
)

// The wire bytes in these tests are written with pgproto3's encoders, in the message order pgx
// v5 sends: a pipeline (pgconn.Pipeline: Parse/Bind/Describe/Execute per statement, one Sync), a
// batch in the statement-cache mode (one round trip that prepares every new statement with
// Parse/Describe and a Sync, then one that runs them with Bind/Describe/Execute and a Sync), and a
// simple Query. Each stream is fed to the parser in chunks of 1 to 7 bytes, so every frame and
// the startup cross chunk boundaries.

type encoder interface {
	Encode(dst []byte) ([]byte, error)
}

func wire(t *testing.T, messages ...encoder) []byte {
	t.Helper()
	var b []byte
	for _, m := range messages {
		var err error
		if b, err = m.Encode(b); err != nil {
			t.Fatalf("encode %T: %v", m, err)
		}
	}
	return b
}

// feed hands data to parse in chunks of 1 to 7 bytes, all at at.
func feed(parse func([]byte, time.Time), data []byte, at time.Time) {
	for i, size := 0, 1; i < len(data); size = size%7 + 1 {
		end := min(i+size, len(data))
		parse(data[i:end], at)
		i = end
	}
}

// startedConn is a connection whose client asked for TLS, was refused, and started up; the
// server's answer to the startup has arrived.
func startedConn(t *testing.T) (*wireCounters, *wireConn) {
	t.Helper()
	counters := &wireCounters{}
	c := newWireConn(counters)
	at := time.Unix(1000, 0)
	feed(c.frontend, wire(t, &pgproto3.SSLRequest{}), at)
	feed(c.backend, []byte{'N'}, at)
	feed(c.frontend, wire(t, &pgproto3.StartupMessage{ProtocolVersion: pgproto3.ProtocolVersionNumber,
		Parameters: map[string]string{"user": "postgres", "database": "dispatch"}}), at)
	feed(c.backend, wire(t, &pgproto3.AuthenticationOk{}, &pgproto3.ParameterStatus{Name: "server_version", Value: "17"},
		&pgproto3.BackendKeyData{ProcessID: 7, SecretKey: []byte{1, 2, 3, 4}}, &pgproto3.ReadyForQuery{TxStatus: 'I'}), at)
	return counters, c
}

// statement is one statement of a pipeline as pgconn sends it: the unnamed statement parsed,
// bound and run.
func statement(sql string) []encoder {
	return []encoder{
		&pgproto3.Parse{Query: sql}, &pgproto3.Bind{Parameters: [][]byte{[]byte("p")}},
		&pgproto3.Describe{ObjectType: 'P'}, &pgproto3.Execute{},
	}
}

func checkCounts(t *testing.T, counters *wireCounters, c *wireConn, roundTrips, room int64) {
	t.Helper()
	if c.lost.Load() {
		t.Fatal("the parser gave the connection up")
	}
	s := counters.snapshot()
	if s.RoundTrips != roundTrips || s.RoomRoundTrips != room {
		t.Fatalf("round trips = %d, room = %d; want %d and %d", s.RoundTrips, s.RoomRoundTrips, roundTrips, room)
	}
}

func TestProxyCountsAPipelineAsOneRoomRoundTrip(t *testing.T) {
	counters, c := startedConn(t)
	if s := counters.snapshot(); s.RoundTrips != 0 {
		t.Fatalf("the startup counted %d round trips, want 0", s.RoundTrips)
	}
	var msgs []encoder
	msgs = append(msgs, statement(`select pg_advisory_xact_lock(hashtext($1))`)...)
	msgs = append(msgs, statement(`insert into doc_updates (artifact_id, version, update, content_changed) values ($1, $2, $3, $4)`)...)
	msgs = append(msgs, statement(`select pg_notify('dispatch_doc_changed', $1)`)...)
	msgs = append(msgs, &pgproto3.Sync{})
	feed(c.frontend, wire(t, msgs...), time.Now())
	checkCounts(t, counters, c, 1, 1)
}

func TestProxyCountsABatchAsItsTwoRoundTrips(t *testing.T) {
	counters, c := startedConn(t)
	// pgx prepares the batch's new statements in one round trip, then runs them in another.
	feed(c.frontend, wire(t,
		&pgproto3.Parse{Name: "stmtcache_1", Query: `select count(*) from issues where project = $1`}, &pgproto3.Describe{ObjectType: 'S', Name: "stmtcache_1"},
		&pgproto3.Parse{Name: "stmtcache_2", Query: `update issues set rank = $2 where key = $1`}, &pgproto3.Describe{ObjectType: 'S', Name: "stmtcache_2"},
		&pgproto3.Sync{},
		&pgproto3.Bind{PreparedStatement: "stmtcache_1"}, &pgproto3.Describe{ObjectType: 'P'}, &pgproto3.Execute{},
		&pgproto3.Bind{PreparedStatement: "stmtcache_2"}, &pgproto3.Describe{ObjectType: 'P'}, &pgproto3.Execute{},
		&pgproto3.Sync{},
	), time.Now())
	checkCounts(t, counters, c, 2, 0)
}

func TestProxyCountsASimpleQueryAsOneRoundTrip(t *testing.T) {
	counters, c := startedConn(t)
	feed(c.frontend, wire(t, &pgproto3.Query{String: "begin; select 1; commit"}), time.Now())
	checkCounts(t, counters, c, 1, 0)
	feed(c.frontend, wire(t, &pgproto3.Query{String: "DELETE FROM doc_checkpoints WHERE artifact_id = 'x'"}), time.Now())
	checkCounts(t, counters, c, 2, 1)
}

func TestProxyClassifiesANamedStatementReusedByName(t *testing.T) {
	counters, c := startedConn(t)
	feed(c.frontend, wire(t,
		&pgproto3.Parse{Name: "stmtcache_doc", Query: `select update from doc_updates where artifact_id = $1 order by version`},
		&pgproto3.Describe{ObjectType: 'S', Name: "stmtcache_doc"}, &pgproto3.Sync{},
		&pgproto3.Parse{Name: "stmtcache_issue", Query: `select key from issues where id = $1`},
		&pgproto3.Describe{ObjectType: 'S', Name: "stmtcache_issue"}, &pgproto3.Sync{},
	), time.Now())
	checkCounts(t, counters, c, 2, 1)
	// Later round trips name the statements only: the room one counts as room work by its name.
	feed(c.frontend, wire(t, &pgproto3.Bind{PreparedStatement: "stmtcache_doc"}, &pgproto3.Describe{ObjectType: 'P'}, &pgproto3.Execute{}, &pgproto3.Sync{}), time.Now())
	checkCounts(t, counters, c, 3, 2)
	feed(c.frontend, wire(t, &pgproto3.Bind{PreparedStatement: "stmtcache_issue"}, &pgproto3.Describe{ObjectType: 'P'}, &pgproto3.Execute{}, &pgproto3.Sync{}), time.Now())
	checkCounts(t, counters, c, 4, 2)
}

func TestProxyCountsTheDocumentAdvisoryLockAsRoomWork(t *testing.T) {
	for _, tc := range []struct {
		sql  string
		room bool
	}{
		{`select pg_advisory_xact_lock(hashtext($1))`, true},
		{`select pg_advisory_lock(hashtext($1))`, true},
		{`select pg_try_advisory_lock(hashtext($1))`, true},
		{`SELECT pg_advisory_unlock(hashtext($1))`, true},
		{`select pg_advisory_xact_lock($1)`, false}, // the events commit-order lock
		{`select pg_advisory_xact_lock(hashtext('issue-rank:' || $1))`, false},
		{`select pg_advisory_xact_lock(hashtext('dispatch-repo-project'), hashtext($1))`, false},
	} {
		counters, c := startedConn(t)
		msgs := append(statement(tc.sql), &pgproto3.Sync{})
		feed(c.frontend, wire(t, msgs...), time.Now())
		want := int64(0)
		if tc.room {
			want = 1
		}
		if s := counters.snapshot(); s.RoundTrips != 1 || s.RoomRoundTrips != want {
			t.Errorf("%s: round trips = %d, room = %d; want 1 and %d", tc.sql, s.RoundTrips, s.RoomRoundTrips, want)
		}
	}
}

func TestProxyTimesAVersionInsertFromSyncToReadyForQuery(t *testing.T) {
	counters, c := startedConn(t)
	sent := time.Unix(2000, 0)
	answered := sent.Add(7 * time.Millisecond)
	msgs := append(statement(`insert into artifact_versions as v (artifact_id, number, markdown, authors, named, summary, doc_update_version) values ($1, $2, $3, $4, $5, $6, $7)`),
		&pgproto3.Sync{})
	answer := wire(t, &pgproto3.ParseComplete{}, &pgproto3.BindComplete{}, &pgproto3.NoData{},
		&pgproto3.CommandComplete{CommandTag: []byte("INSERT 0 1")}, &pgproto3.ReadyForQuery{TxStatus: 'I'})
	// The server's goroutine may parse the answer before the client's parses the Sync: the two
	// are paired by position either way.
	feed(c.backend, answer, answered)
	feed(c.frontend, wire(t, msgs...), sent)
	// A round trip that inserts nothing is not timed: a query, or the insert's own prepare.
	feed(c.frontend, wire(t, &pgproto3.Query{String: "select 1"}), sent)
	feed(c.backend, wire(t, &pgproto3.RowDescription{}, &pgproto3.CommandComplete{CommandTag: []byte("SELECT 1")}, &pgproto3.ReadyForQuery{TxStatus: 'I'}), answered)
	feed(c.frontend, wire(t, &pgproto3.Parse{Name: "stmtcache_v", Query: `INSERT INTO artifact_versions (artifact_id) values ($1)`},
		&pgproto3.Describe{ObjectType: 'S', Name: "stmtcache_v"}, &pgproto3.Sync{}), sent)
	feed(c.backend, wire(t, &pgproto3.ParseComplete{}, &pgproto3.ParameterDescription{}, &pgproto3.NoData{}, &pgproto3.ReadyForQuery{TxStatus: 'I'}), answered)
	checkCounts(t, counters, c, 3, 0)
	s := counters.snapshot()
	if s.VersionInserts != 1 || s.VersionInsertNs != int64(7*time.Millisecond) {
		t.Fatalf("version inserts = %d taking %s, want 1 taking 7ms", s.VersionInserts, time.Duration(s.VersionInsertNs))
	}
	if len(s.RecentVersionInserts) != 1 || !s.RecentVersionInserts[0].At.Equal(answered) {
		t.Fatalf("recent version inserts = %+v, want one at %s", s.RecentVersionInserts, answered)
	}
}

func TestProxyCountsAFlushThatAwaitsAnswers(t *testing.T) {
	counters, c := startedConn(t)
	// A Flush after messages that are answered is a round trip; a Flush with none is not; the
	// Sync after is one more.
	feed(c.frontend, wire(t,
		&pgproto3.Parse{Query: `select version from doc_updates where artifact_id = $1`}, &pgproto3.Describe{ObjectType: 'S'}, &pgproto3.Flush{},
		&pgproto3.Flush{}, &pgproto3.Sync{},
	), time.Now())
	checkCounts(t, counters, c, 2, 1)
	if s := counters.snapshot(); s.FlushRoundTrips != 1 {
		t.Fatalf("flush round trips = %d, want 1", s.FlushRoundTrips)
	}
}

func TestProxyFollowsLargeFrames(t *testing.T) {
	counters, c := startedConn(t)
	big := make([]byte, 200_000)
	feed(c.frontend, wire(t, &pgproto3.Parse{Query: `insert into doc_updates values ($1)`},
		&pgproto3.Bind{Parameters: [][]byte{big}}, &pgproto3.Execute{}, &pgproto3.Sync{}), time.Now())
	feed(c.backend, wire(t, &pgproto3.DataRow{Values: [][]byte{big}}, &pgproto3.ReadyForQuery{TxStatus: 'I'}), time.Now())
	checkCounts(t, counters, c, 1, 1)
	if len(c.front.frames.buf) != 0 || len(c.back.frames.buf) != 0 {
		t.Fatalf("the parser holds %d and %d bytes after whole frames", len(c.front.frames.buf), len(c.back.frames.buf))
	}
}

func TestProxyGivesUpAnEncryptedConnection(t *testing.T) {
	counters := &wireCounters{}
	c := newWireConn(counters)
	feed(c.frontend, wire(t, &pgproto3.SSLRequest{}), time.Now())
	feed(c.backend, []byte{'S', 0x16, 0x03}, time.Now())
	if !c.lost.Load() || counters.snapshot().UnparsedConnections != 1 {
		t.Fatal("an accepted SSLRequest did not give the connection up")
	}
}
