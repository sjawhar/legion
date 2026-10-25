package embedqueue

import (
	"context"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"

	"github.com/sjawhar/envoy/internal/dispatch/embed"
	"github.com/sjawhar/envoy/internal/dispatch/retry"
	"github.com/sjawhar/envoy/internal/dispatch/store"
	"github.com/sjawhar/envoy/internal/dispatch/store/storetest"
)

// The identities the coverage tests configure: one place's, and another's a row may carry from
// before a switch or from the other side of a rolling deploy.
const (
	configuredIdentity = "azure:example.services.ai.azure.com/embed-v-4-0"
	foreignIdentity    = embed.BedrockModel
)

// placeEmbedder is a fake embedder of a chosen identity: every text embeds, and every call is
// recorded, so a test can tell which identity a stored vector came from and count calls.
type placeEmbedder struct {
	identity string
	limits   embed.Limits
	calls    [][]string
}

func newPlaceEmbedder(identity string) *placeEmbedder {
	return &placeEmbedder{identity: identity, limits: new(embed.BedrockClient).Limits()}
}

func (e *placeEmbedder) Identity() string     { return e.identity }
func (e *placeEmbedder) Limits() embed.Limits { return e.limits }

func (e *placeEmbedder) Embed(_ context.Context, texts []string, _ embed.InputType) ([][]float32, error) {
	e.calls = append(e.calls, append([]string(nil), texts...))
	return fakeVectors(len(texts)), nil
}

// querier is what the seeding helpers write through: the pool, or one connection whose triggers
// are off (withTriggersOff).
type querier interface {
	Exec(ctx context.Context, sql string, args ...any) (pgconn.CommandTag, error)
	QueryRow(ctx context.Context, sql string, args ...any) pgx.Row
}

// withTriggersOff runs fn on one connection whose session_replication_role is replica, the
// setting a logical-replication apply or a restore runs under: no ordinary trigger fires for what
// fn writes, the embedding triggers included, exactly as for content written before 0073-0077.
func withTriggersOff(t *testing.T, database *store.Store, fn func(q querier)) {
	t.Helper()
	ctx := context.Background()
	conn, err := database.Pool.Acquire(ctx)
	if err != nil {
		t.Fatalf("acquire a connection: %v", err)
	}
	defer conn.Release()
	if _, err := conn.Exec(ctx, `set session_replication_role = replica`); err != nil {
		t.Fatalf("turn triggers off: %v", err)
	}
	defer func() {
		if _, err := conn.Exec(ctx, `reset session_replication_role`); err != nil {
			t.Fatalf("turn triggers back on: %v", err)
		}
	}()
	fn(conn)
}

func mustExec(t *testing.T, q querier, sql string, args ...any) {
	t.Helper()
	if _, err := q.Exec(context.Background(), sql, args...); err != nil {
		t.Fatalf("%s: %v", strings.TrimSpace(sql), err)
	}
}

// seedEveryKind writes one or more items of every kind meaning search covers under project, the
// texts chosen to catch a coverage text that is not the trigger's: Unicode, a comment longer than
// embed's maxInputChars (32,000 runes, stored whole and cut only when sent), an unanswered and an
// answered ask both with options, and a project document whose latest version is the one that
// counts. It returns how many items it wrote.
func seedEveryKind(t *testing.T, q querier, project string) int {
	t.Helper()
	ctx := context.Background()
	issueKey := project + "-1"
	mustExec(t, q, `insert into projects (key, name) values ($1, 'Test') on conflict (key) do nothing`, project)
	mustExec(t, q, `
		insert into issues (key, project_key, number, title, created_by, rank)
		values ($1, $2, 1, 'Ünïcödé title — 日本語 ✓ 🚀', '{"kind":"user","id":"alice"}', 'A')
	`, issueKey, project)
	var artifactID string
	if err := q.QueryRow(ctx, `
		insert into artifacts (project_key, slug, name, kind, created_by, ref_key)
		values ($1, 'notes-md', 'notes.md', 'doc', '{"kind":"user","id":"alice"}', $1 || '/notes-md')
		returning id::text
	`, project).Scan(&artifactID); err != nil {
		t.Fatalf("seed project document: %v", err)
	}
	mustExec(t, q, `insert into artifact_versions (artifact_id, number, markdown, authors) values ($1, 1, '# First draft', '[]')`, artifactID)
	mustExec(t, q, `insert into artifact_versions (artifact_id, number, markdown, authors) values ($1, 2, '# Latest — naïve café', '[]')`, artifactID)
	mustExec(t, q, `insert into comments (issue_key, author, body) values ($1, '{"kind":"user","id":"alice"}', $2)`,
		issueKey, strings.Repeat("ü long comment ", 2500))
	mustExec(t, q, `
		insert into asks (issue_key, author, question, options)
		values ($1, '{"kind":"session","id":"asker"}', 'Ship it?', '[{"label":"Yes"},{"label":"No"}]')
	`, issueKey)
	mustExec(t, q, `
		insert into asks (issue_key, author, question, options, state, answer)
		values ($1, '{"kind":"session","id":"asker"}', 'Which region?', '[{"label":"us"},{"label":"eu"}]', 'answered', '{"text":"eu, for latency","option":"eu"}')
	`, issueKey)
	mustExec(t, q, `insert into messages (issue_key, author, body) values ($1, '{"kind":"user","id":"alice"}', 'Message — ✉')`, issueKey)
	return 6
}

// drain runs ProcessBatch until a call finds nothing eligible, failing on any failed row.
func drain(t *testing.T, deps Deps) {
	t.Helper()
	for range 1000 {
		succeeded, failed, blocked, throttled, err := ProcessBatch(context.Background(), deps)
		if err != nil || failed != 0 || blocked || throttled {
			t.Fatalf("ProcessBatch = %d, %d, %v, %v, %v", succeeded, failed, blocked, throttled, err)
		}
		if succeeded == 0 {
			return
		}
	}
	t.Fatal("drain never emptied the queue")
}

// rowState is every column of an embeddings row as text, and xmin, which any update of the row
// moves (even one that writes the values it already holds): two equal snapshots mean no row
// was written in between.
type rowState struct {
	textSnapshot, contentHash, embeddedHash, model, deadModel, embedding string
	attempts, confirmed                                                  int
	dead                                                                 bool
	nextAttemptAt, pendingSince, embeddedAt, xmin                        string
}

func snapshotRows(t *testing.T, database *store.Store) map[string]rowState {
	t.Helper()
	rows, err := database.Pool.Query(context.Background(), `
		select kind || '/' || id, text_snapshot, content_hash, coalesce(embedded_hash, ''), coalesce(model, ''),
		       coalesce(dead_model, ''), coalesce(embedding::text, ''), attempt_count, confirmed_failures, dead,
		       next_attempt_at::text, pending_since::text, coalesce(embedded_at::text, ''), xmin::text
		from embeddings`)
	if err != nil {
		t.Fatalf("snapshot embeddings: %v", err)
	}
	defer rows.Close()
	states := map[string]rowState{}
	for rows.Next() {
		var key string
		var s rowState
		if err := rows.Scan(&key, &s.textSnapshot, &s.contentHash, &s.embeddedHash, &s.model, &s.deadModel, &s.embedding,
			&s.attempts, &s.confirmed, &s.dead, &s.nextAttemptAt, &s.pendingSince, &s.embeddedAt, &s.xmin); err != nil {
			t.Fatalf("scan embeddings: %v", err)
		}
		states[key] = s
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("snapshot embeddings: %v", err)
	}
	return states
}

func cover(t *testing.T, deps Deps) {
	t.Helper()
	if err := Cover(context.Background(), deps); err != nil {
		t.Fatalf("Cover: %v", err)
	}
}

func coverage(t *testing.T, database *store.Store, identity string) (items, current, lacking, failed int64, oldest *int64) {
	t.Helper()
	report, err := Coverage(context.Background(), database, identity)
	if err != nil {
		t.Fatalf("Coverage: %v", err)
	}
	return report.Total.Items, report.Total.Current, report.Total.Lacking, report.Total.Failed, report.Total.OldestLackingSeconds
}

// The server embeds content no trigger saw with no command: items written with the triggers off
// have no row, and Run's pass at startup and its scan loop give every one a vector.
func TestRunEmbedsItemsNoTriggerSawWithNoCommand(t *testing.T) {
	database := storetest.Open(t)
	var seeded int
	withTriggersOff(t, database, func(q querier) { seeded = seedEveryKind(t, q, "PRET") })
	var rows int
	if err := database.Pool.QueryRow(context.Background(), `select count(*) from embeddings`).Scan(&rows); err != nil {
		t.Fatalf("count rows: %v", err)
	}
	if rows != 0 {
		t.Fatalf("%d embeddings rows after writing with the triggers off, want 0", rows)
	}
	if items, current, lacking, _, oldest := coverage(t, database, configuredIdentity); items != int64(seeded) || current != 0 || lacking != int64(seeded) || oldest == nil {
		t.Fatalf("coverage before Run: items %d current %d lacking %d oldest %v, want %d, 0, %d and an age", items, current, lacking, oldest, seeded, seeded)
	}

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		defer close(done)
		Run(ctx, Deps{Store: database, Embedder: newPlaceEmbedder(configuredIdentity)})
	}()
	deadline := time.Now().Add(30 * time.Second)
	for {
		items, current, lacking, failed, oldest := coverage(t, database, configuredIdentity)
		if items == int64(seeded) && current == items && lacking == 0 && failed == 0 && oldest == nil {
			break
		}
		if time.Now().After(deadline) {
			cancel()
			t.Fatalf("after 30s of Run: items %d current %d lacking %d failed %d oldest %v, want every item current", items, current, lacking, failed, oldest)
		}
		time.Sleep(100 * time.Millisecond)
	}
	cancel()
	select {
	case <-done:
	case <-time.After(10 * time.Second):
		t.Fatal("Run did not return after its context ended")
	}
}

// Run with no embedder starts nothing: no pass adds a row.
func TestRunWithNoEmbedderDoesNothing(t *testing.T) {
	database := storetest.Open(t)
	withTriggersOff(t, database, func(q querier) { seedEveryKind(t, q, "NOEM") })
	Run(context.Background(), Deps{Store: database})
	var rows int
	if err := database.Pool.QueryRow(context.Background(), `select count(*) from embeddings`).Scan(&rows); err != nil {
		t.Fatalf("count rows: %v", err)
	}
	if rows != 0 {
		t.Fatalf("%d embeddings rows after Run with no embedder, want 0", rows)
	}
}

// coverageSources' texts and hashes are the triggers' own: once every item the triggers enqueued
// is embedded, two passes and updates that fire the triggers without changing any text write no
// row, and nothing is embedded again.
func TestCoverChangesNothingTheTriggersAlreadyEnqueued(t *testing.T) {
	database := storetest.Open(t)
	seeded := seedEveryKind(t, database.Pool, "PARI")
	embedder := newPlaceEmbedder(configuredIdentity)
	deps := Deps{Store: database, Embedder: embedder}
	drain(t, deps)
	if items, current, _, _, _ := coverage(t, database, configuredIdentity); items != int64(seeded) || current != items {
		t.Fatalf("after the drain: items %d current %d, want %d current", items, current, seeded)
	}
	before := snapshotRows(t, database)
	calls := len(embedder.calls)

	cover(t, deps)
	cover(t, deps)
	mustExec(t, database.Pool, `update issues set title = title, status = 'backlog' where key = 'PARI-1'`)
	mustExec(t, database.Pool, `update comments set body = body, resolved = true`)
	mustExec(t, database.Pool, `update asks set question = question, answer = answer, urgency = 'high'`)
	mustExec(t, database.Pool, `
		insert into artifact_versions (artifact_id, number, markdown, authors)
		select artifact_id, number + 1, markdown, authors from artifact_versions where number = 2`)
	drain(t, deps)

	after := snapshotRows(t, database)
	if len(after) != len(before) {
		t.Fatalf("%d rows after, %d before", len(after), len(before))
	}
	for key, was := range before {
		if now := after[key]; now != was {
			t.Errorf("%s changed:\n before %+v\n after  %+v", key, was, now)
		}
	}
	if len(embedder.calls) != calls {
		t.Errorf("embedder called %d more times, want 0: %v", len(embedder.calls)-calls, embedder.calls[calls:])
	}
}

// A row whose text changed with the triggers off is repaired to the item's text with fresh
// attempts, and then embedded at it.
func TestCoverRepairsAStaleRow(t *testing.T) {
	database := storetest.Open(t)
	seedIssue(t, database, "STLE", "STLE-1", "Old title")
	embedder := newPlaceEmbedder(configuredIdentity)
	deps := Deps{Store: database, Embedder: embedder}
	drain(t, deps)
	withTriggersOff(t, database, func(q querier) {
		mustExec(t, q, `update issues set title = 'Restored title' where key = 'STLE-1'`)
	})
	mustExec(t, database.Pool, `
		update embeddings set dead = true, dead_model = $1, attempt_count = 4, confirmed_failures = 3,
		       next_attempt_at = now() + interval '1 hour', pending_since = now() - interval '1 day'
		where kind = 'issue' and id = 'STLE-1'`, configuredIdentity)

	started := time.Now()
	cover(t, deps)
	var text, hash, wantHash string
	var attempts, confirmed int
	var dead bool
	var deadModel *string
	var nextAttempt, pendingSince time.Time
	if err := database.Pool.QueryRow(context.Background(), `
		select text_snapshot, content_hash, encode(sha256(convert_to('Restored title', 'UTF8')), 'hex'), attempt_count,
		       confirmed_failures, dead, dead_model, next_attempt_at, pending_since
		from embeddings where kind = 'issue' and id = 'STLE-1'`,
	).Scan(&text, &hash, &wantHash, &attempts, &confirmed, &dead, &deadModel, &nextAttempt, &pendingSince); err != nil {
		t.Fatalf("read repaired row: %v", err)
	}
	if text != "Restored title" || hash != wantHash || attempts != 0 || confirmed != 0 || dead || deadModel != nil {
		t.Errorf("repaired row: text %q hash %q attempts %d confirmed %d dead %t dead_model %v, want the item's text and hash, fresh counters, alive", text, hash, attempts, confirmed, dead, deadModel)
	}
	if nextAttempt.After(time.Now()) || pendingSince.Before(started.Add(-time.Second)) {
		t.Errorf("next_attempt_at %v, pending_since %v; want both now", nextAttempt, pendingSince)
	}
	drain(t, deps)
	if last := embedder.calls[len(embedder.calls)-1]; len(last) != 1 || last[0] != "Restored title" {
		t.Errorf("last embed call %v, want the repaired text", last)
	}
	if _, current, _, _, _ := coverage(t, database, configuredIdentity); current != 1 {
		t.Errorf("current = %d after the drain, want 1", current)
	}
}

// The repair is a compare-and-swap: a trigger write that commits after the pass read the row and
// before it updated it wins, keeping the trigger's text, hash, claim and counters. The trigger's
// transaction holds the row while the pass's update waits on it, so the interleaving is exact.
func TestCoverKeepsATriggerWriteThatLandsBetweenItsReadAndItsUpdate(t *testing.T) {
	database := storetest.Open(t)
	ctx := context.Background()
	seedIssue(t, database, "CASW", "CASW-1", "First title")
	withTriggersOff(t, database, func(q querier) {
		mustExec(t, q, `update issues set title = 'Restored title' where key = 'CASW-1'`)
	})
	claimedUntil := time.Now().Add(time.Hour).Truncate(time.Microsecond)
	writer, err := database.Pool.Begin(ctx)
	if err != nil {
		t.Fatalf("begin the writer: %v", err)
	}
	defer writer.Rollback(ctx)
	if _, err := writer.Exec(ctx, `update issues set title = 'Live title' where key = 'CASW-1'`); err != nil {
		t.Fatalf("live write: %v", err)
	}
	if _, err := writer.Exec(ctx, `
		update embeddings set attempt_count = 2, confirmed_failures = 1, next_attempt_at = $1
		where kind = 'issue' and id = 'CASW-1'`, claimedUntil); err != nil {
		t.Fatalf("claim the live row: %v", err)
	}

	passDone := make(chan error, 1)
	go func() {
		passDone <- Cover(ctx, Deps{Store: storetest.SecondProcess(t, database), Embedder: newPlaceEmbedder(configuredIdentity)})
	}()
	if waiting := storetest.WaitForLockWaiters(t, writer, 1, 10*time.Second); waiting < 1 {
		t.Fatal("the pass never waited on the live write's row; the interleaving did not happen")
	}
	if err := writer.Commit(ctx); err != nil {
		t.Fatalf("commit the live write: %v", err)
	}
	if err := <-passDone; err != nil {
		t.Fatalf("Cover: %v", err)
	}

	var text, hash, liveHash string
	var attempts, confirmed int
	var nextAttempt time.Time
	if err := database.Pool.QueryRow(ctx, `
		select text_snapshot, content_hash, encode(sha256(convert_to('Live title', 'UTF8')), 'hex'), attempt_count, confirmed_failures, next_attempt_at
		from embeddings where kind = 'issue' and id = 'CASW-1'`,
	).Scan(&text, &hash, &liveHash, &attempts, &confirmed, &nextAttempt); err != nil {
		t.Fatalf("read the row: %v", err)
	}
	if text != "Live title" || hash != liveHash || attempts != 2 || confirmed != 1 || !nextAttempt.Equal(claimedUntil) {
		t.Errorf("row after the pass: text %q hash %q attempts %d confirmed %d next %v; want the live write's 'Live title', its hash, 2, 1, %v",
			text, hash, attempts, confirmed, nextAttempt, claimedUntil)
	}
}

// A vector another identity made leaves the index's rows: a row embedded at its current text goes
// pending now and is embedded again under the configured identity, a row already pending keeps its
// claim, backoff and pending_since, and a second pass changes nothing.
func TestCoverDropsAnotherIdentitysVectors(t *testing.T) {
	database := storetest.Open(t)
	ctx := context.Background()
	seedIssue(t, database, "FRGN", "FRGN-1", "Embedded elsewhere")
	seedIssue(t, database, "FRGN", "FRGN-2", "Pending elsewhere")
	drain(t, Deps{Store: database, Embedder: newPlaceEmbedder(foreignIdentity)})
	mustExec(t, database.Pool, `update issues set title = 'Pending elsewhere, edited' where key = 'FRGN-2'`)
	claimedUntil := time.Now().Add(time.Hour).Truncate(time.Microsecond)
	waitingSince := time.Now().Add(-3 * time.Hour).Truncate(time.Microsecond)
	mustExec(t, database.Pool, `
		update embeddings set attempt_count = 3, next_attempt_at = $1, pending_since = $2
		where kind = 'issue' and id = 'FRGN-2'`, claimedUntil, waitingSince)

	embedder := newPlaceEmbedder(configuredIdentity)
	deps := Deps{Store: database, Embedder: embedder}
	started := time.Now()
	cover(t, deps)
	read := func(id string) (hasVector bool, model, embeddedHash *string, attempts int, next, pending time.Time) {
		t.Helper()
		if err := database.Pool.QueryRow(ctx, `
			select embedding is not null, model, embedded_hash, attempt_count, next_attempt_at, pending_since
			from embeddings where kind = 'issue' and id = $1`, id,
		).Scan(&hasVector, &model, &embeddedHash, &attempts, &next, &pending); err != nil {
			t.Fatalf("read %s: %v", id, err)
		}
		return
	}
	if hasVector, model, embeddedHash, attempts, next, pending := read("FRGN-1"); hasVector || model != nil || embeddedHash != nil || attempts != 0 || next.After(time.Now()) || pending.Before(started.Add(-time.Second)) {
		t.Errorf("FRGN-1: vector %t model %v embedded_hash %v attempts %d next %v pending %v; want no vector and pending from now", hasVector, model, embeddedHash, attempts, next, pending)
	}
	if hasVector, model, _, attempts, next, pending := read("FRGN-2"); hasVector || model != nil || attempts != 3 || !next.Equal(claimedUntil) || !pending.Equal(waitingSince) {
		t.Errorf("FRGN-2: vector %t model %v attempts %d next %v pending %v; want no vector and its claim, backoff and pending_since kept (3, %v, %v)", hasVector, model, attempts, next, pending, claimedUntil, waitingSince)
	}

	before := snapshotRows(t, database)
	cover(t, deps)
	if after := snapshotRows(t, database); fmt.Sprint(after) != fmt.Sprint(before) {
		t.Errorf("a second pass changed rows:\n before %v\n after  %v", before, after)
	}

	drain(t, deps)
	var model string
	if err := database.Pool.QueryRow(ctx, `select model from embeddings where kind = 'issue' and id = 'FRGN-1'`).Scan(&model); err != nil {
		t.Fatalf("read FRGN-1's model: %v", err)
	}
	if model != configuredIdentity {
		t.Errorf("FRGN-1 model = %q after the drain, want %q", model, configuredIdentity)
	}
}

// A row confirmed dead under another identity, or under none (dead before 0117), gets the
// configured identity's attempts once; a row dead under the configured identity stays dead, and so
// does a re-admitted row once it dies again here.
func TestCoverReadmitsARowDeadUnderAnotherIdentityOnce(t *testing.T) {
	database := storetest.Open(t)
	ctx := context.Background()
	for _, id := range []string{"DEDA-1", "DEDA-2", "DEDA-3"} {
		seedIssue(t, database, "DEDA", id, "Title of "+id)
	}
	mustExec(t, database.Pool, `update embeddings set dead = true, confirmed_failures = 10, attempt_count = 12, dead_model = $1 where id = 'DEDA-1'`, foreignIdentity)
	mustExec(t, database.Pool, `update embeddings set dead = true, confirmed_failures = 10, attempt_count = 12, dead_model = $1 where id = 'DEDA-2'`, configuredIdentity)
	mustExec(t, database.Pool, `update embeddings set dead = true, confirmed_failures = 10, attempt_count = 12, dead_model = null where id = 'DEDA-3'`)

	embedder := newPlaceEmbedder(configuredIdentity)
	deps := Deps{Store: database, Embedder: embedder}
	cover(t, deps)
	read := func(id string) (dead bool, deadModel *string, confirmed, attempts int) {
		t.Helper()
		if err := database.Pool.QueryRow(ctx, `select dead, dead_model, confirmed_failures, attempt_count from embeddings where kind = 'issue' and id = $1`, id).
			Scan(&dead, &deadModel, &confirmed, &attempts); err != nil {
			t.Fatalf("read %s: %v", id, err)
		}
		return
	}
	for _, id := range []string{"DEDA-1", "DEDA-3"} {
		if dead, deadModel, confirmed, attempts := read(id); dead || deadModel != nil || confirmed != 0 || attempts != 0 {
			t.Errorf("%s: dead %t dead_model %v confirmed %d attempts %d, want re-admitted with fresh counters", id, dead, deadModel, confirmed, attempts)
		}
	}
	if dead, deadModel, _, _ := read("DEDA-2"); !dead || deadModel == nil || *deadModel != configuredIdentity {
		t.Errorf("DEDA-2: dead %t dead_model %v, want still dead under the configured identity", dead, deadModel)
	}

	row := pendingRow{kind: "issue", id: "DEDA-1", contentHash: contentHashOf(t, database, "issue", "DEDA-1")}
	for row.confirmedFailures < retry.DeadLetterAttempts {
		retryRows(ctx, deps, []pendingRow{row}, true)
		row.attempts++
		row.confirmedFailures++
	}
	cover(t, deps)
	if dead, deadModel, _, _ := read("DEDA-1"); !dead || deadModel == nil || *deadModel != configuredIdentity {
		t.Errorf("DEDA-1 after dying again here: dead %t dead_model %v, want it to stay dead", dead, deadModel)
	}
}

// seedCoverageWork gives a database every kind of work the pass does, the same in any database it
// is called on: 1,200 issues no trigger saw (more than two statements' worth), and among 40 the
// triggers did see, 10 stale, 10 embedded by another identity and 10 dead under one.
func seedCoverageWork(t *testing.T, database *store.Store) {
	t.Helper()
	withTriggersOff(t, database, func(q querier) {
		mustExec(t, q, `insert into projects (key, name) values ('CONC', 'Test')`)
		mustExec(t, q, `
			insert into issues (key, project_key, number, title, created_by, rank)
			select 'CONC-' || n, 'CONC', n, 'Unseen ' || n, '{"kind":"user","id":"alice"}', 'A' from generate_series(1, 1200) n`)
	})
	seedManyIssues(t, database, "CONC", 40, func(i int) string { return fmt.Sprintf("Seen %d", i) })
	withTriggersOff(t, database, func(q querier) {
		mustExec(t, q, `update issues set title = title || ', restored' where number between 1201 and 1210`)
	})
	mustExec(t, database.Pool, `
		update embeddings set embedding = $1::vector, model = $2, embedded_hash = content_hash, embedded_at = now()
		where id in (select key from issues where number between 1211 and 1220)`, embed.Literal(oneVector()), foreignIdentity)
	mustExec(t, database.Pool, `
		update embeddings set dead = true, confirmed_failures = 10, dead_model = $1
		where id in (select key from issues where number between 1221 and 1230)`, foreignIdentity)
}

// normalizedRows is every embeddings row without its timestamps or xmin, which differ between
// two databases however equal their work was.
func normalizedRows(t *testing.T, database *store.Store) map[string]rowState {
	t.Helper()
	rows := snapshotRows(t, database)
	for key, row := range rows {
		row.nextAttemptAt, row.pendingSince, row.embeddedAt, row.xmin = "", "", "", ""
		rows[key] = row
	}
	return rows
}

// Two passes at once, in two processes, finish and leave the state one pass leaves.
func TestTwoConcurrentPassesLeaveTheStateOnePassLeaves(t *testing.T) {
	single := storetest.Open(t)
	seedCoverageWork(t, single)
	cover(t, Deps{Store: single, Embedder: newPlaceEmbedder(configuredIdentity)})

	concurrent := storetest.Open(t)
	seedCoverageWork(t, concurrent)
	var wg sync.WaitGroup
	errs := make([]error, 2)
	for i, database := range []*store.Store{concurrent, storetest.SecondProcess(t, concurrent)} {
		wg.Go(func() {
			errs[i] = Cover(context.Background(), Deps{Store: database, Embedder: newPlaceEmbedder(configuredIdentity)})
		})
	}
	wg.Wait()
	for i, err := range errs {
		if err != nil {
			t.Fatalf("concurrent pass %d: %v", i, err)
		}
	}

	want, got := normalizedRows(t, single), normalizedRows(t, concurrent)
	if len(want) != 1240 || len(got) != len(want) {
		t.Fatalf("%d rows after two concurrent passes, %d after one; want 1,240 each", len(got), len(want))
	}
	for key, row := range want {
		if got[key] != row {
			t.Errorf("%s: two passes left %+v, one left %+v", key, got[key], row)
		}
	}
}

// A rolling deploy that changes the identity converges: a vector the old configuration's process
// commits after the new one's pass is dropped by its next pass and embedded again under the new.
func TestTheNextPassDropsAVectorTheOldConfigurationCommittedAfterThePass(t *testing.T) {
	database := storetest.Open(t)
	ctx := context.Background()
	seedIssue(t, database, "ROLL", "ROLL-1", "Rolling title")
	current := Deps{Store: database, Embedder: newPlaceEmbedder(configuredIdentity)}
	cover(t, current)
	drain(t, Deps{Store: storetest.SecondProcess(t, database), Embedder: newPlaceEmbedder(foreignIdentity)})
	var model string
	if err := database.Pool.QueryRow(ctx, `select model from embeddings where kind = 'issue' and id = 'ROLL-1'`).Scan(&model); err != nil || model != foreignIdentity {
		t.Fatalf("model after the old process's drain = %q, %v; want %q", model, err, foreignIdentity)
	}
	cover(t, current)
	drain(t, current)
	if err := database.Pool.QueryRow(ctx, `select model from embeddings where kind = 'issue' and id = 'ROLL-1'`).Scan(&model); err != nil || model != configuredIdentity {
		t.Fatalf("model after the next pass and a drain = %q, %v; want %q", model, err, configuredIdentity)
	}
}

// Coverage over every state an item can be in: no row (aging from when it was written), stale
// hash, another identity's vector, no vector, dead, current; then with no embedder; then an empty
// backlog.
func TestCoverageCountsEveryStateAnItemCanBeIn(t *testing.T) {
	database := storetest.Open(t)
	ctx := context.Background()
	withTriggersOff(t, database, func(q querier) {
		mustExec(t, q, `insert into projects (key, name) values ('CVRG', 'Test')`)
		mustExec(t, q, `
			insert into issues (key, project_key, number, title, created_by, rank, created_at)
			values ('CVRG-1', 'CVRG', 1, 'No row', '{"kind":"user","id":"alice"}', 'A', now() - interval '2 hours')`)
	})
	for i, title := range []string{"Current", "Stale", "Foreign", "No vector", "Dead"} {
		seedIssue(t, database, "CVRG", fmt.Sprintf("CVRG-%d", i+2), title)
	}
	drain(t, Deps{Store: database, Embedder: newPlaceEmbedder(configuredIdentity)})
	withTriggersOff(t, database, func(q querier) {
		mustExec(t, q, `update issues set title = 'Stale, restored' where key = 'CVRG-3'`)
	})
	mustExec(t, database.Pool, `update embeddings set model = $1 where id = 'CVRG-4'`, foreignIdentity)
	mustExec(t, database.Pool, `update embeddings set embedding = null where id = 'CVRG-5'`)
	mustExec(t, database.Pool, `update embeddings set embedding = null, embedded_hash = null, dead = true, dead_model = $1 where id = 'CVRG-6'`, configuredIdentity)
	// Foreign's row waited longer than the item has existed: its age counts from the item's
	// write. No vector's row became pending after it: its age counts from pending_since.
	mustExec(t, database.Pool, `update embeddings set pending_since = now() - interval '5 hours' where id = 'CVRG-4'`)
	mustExec(t, database.Pool, `update issues set created_at = now() - interval '3 hours' where key in ('CVRG-4', 'CVRG-5')`)
	mustExec(t, database.Pool, `update embeddings set pending_since = now() - interval '30 minutes' where id = 'CVRG-5'`)

	report, err := Coverage(ctx, database, configuredIdentity)
	if err != nil {
		t.Fatalf("Coverage: %v", err)
	}
	if report.Identity == nil || *report.Identity != configuredIdentity || report.Place == nil || *report.Place != "azure" || report.Model == nil || *report.Model != "embed-v-4-0" {
		t.Errorf("identity %v place %v model %v, want %q, azure, embed-v-4-0", report.Identity, report.Place, report.Model, configuredIdentity)
	}
	issues := report.Kinds.Issue
	if issues.Items != 6 || issues.Current != 1 || issues.Lacking != 4 || issues.Failed != 1 {
		t.Errorf("issue counts %+v, want 6 items, 1 current, 4 lacking, 1 failed", issues)
	}
	if issues.OldestLackingSeconds == nil || *issues.OldestLackingSeconds < 3*3600 || *issues.OldestLackingSeconds > 3*3600+60 {
		t.Errorf("oldest lacking = %v, want about 3 hours (Foreign, aged from its write)", issues.OldestLackingSeconds)
	}
	if report.Total != issues {
		t.Errorf("total %+v, want the issue counts %+v (no other kind has items)", report.Total, issues)
	}

	// No row's own age counts from its write.
	mustExec(t, database.Pool, `update issues set created_at = now() where key in ('CVRG-4', 'CVRG-5')`)
	if _, _, _, _, oldest := coverage(t, database, configuredIdentity); oldest == nil || *oldest < 2*3600 || *oldest > 2*3600+60 {
		t.Errorf("oldest lacking = %v, want about 2 hours (No row, aged from its write)", oldest)
	}

	unconfigured, err := Coverage(ctx, database, "")
	if err != nil {
		t.Fatalf("Coverage with no embedder: %v", err)
	}
	if unconfigured.Identity != nil || unconfigured.Place != nil || unconfigured.Model != nil {
		t.Errorf("no embedder: identity %v place %v model %v, want all null", unconfigured.Identity, unconfigured.Place, unconfigured.Model)
	}
	if got := unconfigured.Total; got.Items != 6 || got.Current != 0 || got.Failed != 1 || got.Lacking != 5 {
		t.Errorf("no embedder: total %+v, want 6 items, 0 current, 1 failed, 5 lacking", got)
	}
}

func TestCoverageOfAnEmptyBacklogHasNoOldestAge(t *testing.T) {
	database := storetest.Open(t)
	seeded := seedEveryKind(t, database.Pool, "EMPT")
	drain(t, Deps{Store: database, Embedder: newPlaceEmbedder(foreignIdentity)})
	report, err := Coverage(context.Background(), database, foreignIdentity)
	if err != nil {
		t.Fatalf("Coverage: %v", err)
	}
	if report.Place == nil || *report.Place != "bedrock" || report.Model == nil || *report.Model != foreignIdentity {
		t.Errorf("place %v model %v, want bedrock and %q", report.Place, report.Model, foreignIdentity)
	}
	if got := report.Total; got.Items != int64(seeded) || got.Current != got.Items || got.Lacking != 0 || got.Failed != 0 || got.OldestLackingSeconds != nil {
		t.Errorf("total %+v, want %d items all current and no oldest age", got, seeded)
	}
	for kind, counts := range map[string]int64{
		"issue": report.Kinds.Issue.Items, "document": report.Kinds.Document.Items, "comment": report.Kinds.Comment.Items,
		"ask": report.Kinds.Ask.Items, "message": report.Kinds.Message.Items,
	} {
		want := int64(1)
		if kind == "ask" {
			want = 2
		}
		if counts != want {
			t.Errorf("%s items = %d, want %d", kind, counts, want)
		}
	}
}

func TestIdentityPlace(t *testing.T) {
	for identity, want := range map[string][2]string{
		"azure:example.services.ai.azure.com/embed-v-4-0":        {"azure", "embed-v-4-0"},
		"azure:example.services.ai.azure.com/models/embed-v-4-0": {"azure", "embed-v-4-0"},
		"us.cohere.embed-v4:0":                                   {"bedrock", "us.cohere.embed-v4:0"},
	} {
		if place, name := identityPlace(identity); place != want[0] || name != want[1] {
			t.Errorf("identityPlace(%q) = %q, %q, want %q, %q", identity, place, name, want[0], want[1])
		}
	}
}
