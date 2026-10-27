package docs

import (
	"context"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/reearth/ygo/crdt"

	"github.com/sjawhar/envoy/internal/dispatch/events"
	"github.com/sjawhar/envoy/internal/dispatch/model"
	"github.com/sjawhar/envoy/internal/dispatch/pmdoc"
	"github.com/sjawhar/envoy/internal/dispatch/store/storetest"
)

// roomLoadComments is how many anchored comments a benchmark document of 30 or more versions
// carries: the production median per spec.
const roomLoadComments = 25

// roomLoadYoung is how many versions the compacted+15young layout appends after compaction and
// leaves as rows of their own.
const roomLoadYoung = 15

var roomLoadActor = model.Actor{Kind: "user", ID: "alice"}

// BenchmarkRoomLoad measures what ygo runs when it loads a document's room: the persistence
// adapter's LoadDoc and the decode of its update into a fresh document. Each document is a ~98 KB
// spec of 400 paragraphs whose every version rewrites 5% of them through the upload write path,
// with 25 anchored comments once it has 30 versions or more. Its history is compacted to one row
// (compacted), or to one row followed by 15 young versions left as rows of their own
// (compacted+15young).
func BenchmarkRoomLoad(b *testing.B) {
	for _, versions := range []int{1, 30, 60, 120, 240} {
		for _, young := range []int{0, roomLoadYoung} {
			name := fmt.Sprintf("versions=%d/compacted", versions)
			if young > 0 {
				name = fmt.Sprintf("versions=%d/compacted+%dyoung", versions, young)
			}
			// The fixture is built once, by the sub-benchmark's first run, and kept for its later
			// runs: the framework calls the sub-benchmark again for the requested iteration count,
			// and a 240-version history takes minutes to write.
			var fixture *roomLoadFixture
			var cleanups []func()
			b.Run(name, func(b *testing.B) {
				if fixture == nil {
					fixture = newRoomLoadFixture(roomLoadFixtureTB{TB: b, cleanups: &cleanups}, versions, young)
				}
				fixture.run(b)
			})
			for index := len(cleanups) - 1; index >= 0; index-- {
				cleanups[index]()
			}
		}
	}
}

// roomLoadFixtureTB defers the cleanups a fixture registers to the end of its sub-benchmark's
// last run rather than the end of each run, so the fixture outlives the framework's first,
// one-iteration call.
type roomLoadFixtureTB struct {
	testing.TB
	cleanups *[]func()
}

func (tb roomLoadFixtureTB) Cleanup(cleanup func()) {
	*tb.cleanups = append(*tb.cleanups, cleanup)
}

type roomLoadFixture struct {
	service    *Service
	artifactID string
	rows       int
}

// newRoomLoadFixture writes a document of versions versions, compacts its history to one row,
// appends young more versions, and evicts its room.
func newRoomLoadFixture(tb testing.TB, versions, young int) *roomLoadFixture {
	tb.Helper()
	started := time.Now()
	database := storetest.Open(tb)
	ctx := context.Background()
	service := New(Deps{Store: database, Events: events.NewBroker(), ServerURL: "https://dispatch.example", Settle: time.Hour})
	tb.Cleanup(func() {
		if err := service.Shutdown(context.Background()); err != nil {
			tb.Errorf("shutdown room load benchmark service: %v", err)
		}
	})
	versioned, ok := service.persistence.(*PgVersioned)
	if !ok {
		tb.Fatalf("service persistence = %T, want *PgVersioned", service.persistence)
	}
	artifactID := createRoomLoadDocument(tb, service, roomLoadMarkdown(0))

	comments := 0
	if versions >= 30 {
		comments = roomLoadComments
	}
	next := 0 // the next comment to write
	for version := 1; version <= versions; version++ {
		writeRoomLoadVersion(tb, service, artifactID, version)
		// Comment i follows version 1 + i*(versions-1)/comments, spreading the comments over
		// the history as well as over the document.
		for next < comments && 1+next*(versions-1)/comments == version {
			writeRoomLoadComment(tb, service, artifactID, next)
			next++
		}
	}
	if next != comments {
		tb.Fatalf("wrote %d comments, want %d", next, comments)
	}
	evictRoomLoadRoom(tb, service, artifactID)

	if _, err := database.Pool.Exec(ctx, `update doc_updates set created_at = created_at - interval '1 hour' where artifact_id = $1`, artifactID); err != nil {
		tb.Fatalf("backdate document updates: %v", err)
	}
	if _, err := versioned.Compact(ctx, artifactID, 1); err != nil {
		tb.Fatalf("compact document: %v", err)
	}
	if rows := countRoomLoadRows(tb, service, artifactID); rows != 1 {
		tb.Fatalf("compacted document holds %d rows, want 1", rows)
	}

	for version := versions + 1; version <= versions+young; version++ {
		writeRoomLoadVersion(tb, service, artifactID, version)
	}
	evictRoomLoadRoom(tb, service, artifactID)
	fixture := &roomLoadFixture{service: service, artifactID: artifactID, rows: countRoomLoadRows(tb, service, artifactID)}
	if fixture.rows != 1+young {
		tb.Fatalf("document holds %d rows after %d young versions, want %d", fixture.rows, young, 1+young)
	}
	tb.Logf("setup of %d versions + %d young: %v", versions, young, time.Since(started).Round(time.Millisecond))
	return fixture
}

func (fixture *roomLoadFixture) run(b *testing.B) {
	if live := fixture.service.srv.GetDoc(fixture.artifactID); live != nil {
		b.Fatal("the document's room is resident")
	}
	if rows := countRoomLoadRows(b, fixture.service, fixture.artifactID); rows != fixture.rows {
		b.Fatalf("document holds %d rows, want %d", rows, fixture.rows)
	}
	adapter := &servicePersistenceAdapter{store: fixture.service.persistence, service: fixture.service}
	b.ReportAllocs()
	b.ResetTimer()
	var stateBytes int
	for range b.N {
		update, err := adapter.LoadDoc(fixture.artifactID)
		if err != nil {
			b.Fatal(err)
		}
		if err := crdt.ApplyUpdateV1(newDocumentCopy(), update, nil); err != nil {
			b.Fatal(err)
		}
		stateBytes = len(update)
	}
	b.StopTimer()
	b.ReportMetric(float64(fixture.rows), "rows/op")
	b.ReportMetric(float64(stateBytes), "state-bytes/op")
}

// roomLoadMarkdown is version of a ~98 KB spec: 400 paragraphs in 20 sections, of which version
// rewrites the 5% where (paragraph+version)%20 == 0.
func roomLoadMarkdown(version int) string {
	var b strings.Builder
	b.WriteString("# Spec\n\n")
	for p := range 400 {
		if p%20 == 0 {
			fmt.Fprintf(&b, "## Section %d\n\n", p/20)
		}
		rev := 0
		if version > 0 && (p+version)%20 == 0 {
			rev = version
		}
		fmt.Fprintf(&b, "Paragraph %d revision %d: %s\n\n", p, rev, strings.Repeat("lorem ipsum dolor sit amet ", 8))
	}
	return b.String()
}

// createRoomLoadDocument is version 0: the document's first stored version, holding markdown, as
// createIssueDocument writes one.
func createRoomLoadDocument(tb testing.TB, service *Service, markdown string) string {
	tb.Helper()
	tree, err := pmdoc.Parse(markdown)
	if err != nil {
		tb.Fatalf("parse benchmark document: %v", err)
	}
	markdown, err = pmdoc.Render(pmdoc.StripAnchorMarks(tree))
	if err != nil {
		tb.Fatalf("render benchmark document: %v", err)
	}
	ctx := context.Background()
	tx, err := service.store.Pool.Begin(ctx)
	if err != nil {
		tb.Fatal(err)
	}
	defer tx.Rollback(ctx)
	if _, err := tx.Exec(ctx, `insert into projects (key, name) values ('BENCH', 'Benchmark')`); err != nil {
		tb.Fatal(err)
	}
	if _, err := tx.Exec(ctx, `
		insert into issues (key, project_key, number, title, created_by, rank)
		values ('BENCH-1', 'BENCH', 1, 'Benchmark', '{"kind":"user","id":"alice"}', 'U')
	`); err != nil {
		tb.Fatal(err)
	}
	var artifactID string
	if err := tx.QueryRow(ctx, `
		insert into artifacts (issue_key, project_key, slug, name, kind, is_primary, created_by)
		values ('BENCH-1', 'BENCH', 'spec', 'spec.md', 'doc', false, '{"kind":"user","id":"alice"}')
		returning id::text
	`).Scan(&artifactID); err != nil {
		tb.Fatal(err)
	}
	if _, err := tx.Exec(ctx, `
		insert into artifact_versions (artifact_id, number, markdown, authors)
		values ($1, 1, $2, '[{"kind":"user","id":"alice"}]')
	`, artifactID, markdown); err != nil {
		tb.Fatal(err)
	}
	if err := tx.Commit(ctx); err != nil {
		tb.Fatal(err)
	}
	return artifactID
}

// writeRoomLoadVersion writes version through the upload write path.
func writeRoomLoadVersion(tb testing.TB, service *Service, artifactID string, version int) {
	tb.Helper()
	if _, err := joinedReplaceText(service, artifactID, roomLoadMarkdown(version), roomLoadActor); err != nil {
		tb.Fatalf("write version %d: %v", version, err)
	}
}

// writeRoomLoadComment anchors comment index on its own paragraph's opening words, as the comment
// route does: the quote's mark and the comment's projection in one transaction.
func writeRoomLoadComment(tb testing.TB, service *Service, artifactID string, index int) {
	tb.Helper()
	markID := fmt.Sprintf("comment-%02d", index)
	quote := fmt.Sprintf("Paragraph %d revision", 8+16*index)
	err := joinedWrite(service, func(ctx context.Context) error {
		if _, err := service.MarkQuote(ctx, artifactID, MarkSpec{Kind: MarkComment, ID: markID, By: roomLoadActor}, quote, nil); err != nil {
			return err
		}
		return service.ProjectMark(ctx, artifactID, markID, MarkRecord{
			Kind:      "comment",
			By:        ActorRef(roomLoadActor),
			CreatedAt: time.Now().UTC().Format(time.RFC3339),
			Text:      fmt.Sprintf("Comment %d on this paragraph.", index),
			Replies:   []MarkReply{},
		}, roomLoadActor)
	})
	if err != nil {
		tb.Fatalf("write comment %d: %v", index, err)
	}
}

// evictRoomLoadRoom closes the document's room, which the write path leaves resident, and checks
// none is. The room's persistence worker compacts the history as it exits, which would fold the
// young versions the benchmark lays out; it leaves a room whose advisory lock another holder has
// for a later compaction (compactIfIdle), so the eviction runs while this holds that lock. The
// eviction appends nothing that would wait for it: the write path made every update durable.
func evictRoomLoadRoom(tb testing.TB, service *Service, artifactID string) {
	tb.Helper()
	ctx := context.Background()
	conn, err := service.store.Pool.Acquire(ctx)
	if err != nil {
		tb.Fatalf("acquire the room lock's connection: %v", err)
	}
	defer conn.Release()
	if _, err := conn.Exec(ctx, `select pg_advisory_lock(hashtext($1))`, artifactID); err != nil {
		tb.Fatalf("lock document room: %v", err)
	}
	evicted := service.Evict(ctx, artifactID)
	if _, err := conn.Exec(ctx, `select pg_advisory_unlock(hashtext($1))`, artifactID); err != nil {
		tb.Fatalf("unlock document room: %v", err)
	}
	if evicted != nil {
		tb.Fatalf("evict document room: %v", evicted)
	}
	if live := service.srv.GetDoc(artifactID); live != nil {
		tb.Fatal("the document's room is still resident after its eviction")
	}
}

func countRoomLoadRows(tb testing.TB, service *Service, artifactID string) int {
	tb.Helper()
	var rows int
	if err := service.store.Pool.QueryRow(context.Background(), `select count(*) from doc_updates where artifact_id = $1`, artifactID).Scan(&rows); err != nil {
		tb.Fatalf("count document updates: %v", err)
	}
	return rows
}
