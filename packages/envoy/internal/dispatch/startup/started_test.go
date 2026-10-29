package startup

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"reflect"
	"sync"
	"testing"
	"time"

	"github.com/sjawhar/envoy/internal/contracts"
	"github.com/sjawhar/envoy/internal/dispatch/auth"
	"github.com/sjawhar/envoy/internal/dispatch/processes"
	"github.com/sjawhar/envoy/internal/dispatch/store"
	"github.com/sjawhar/envoy/internal/dispatch/store/storetest"
)

func TestMain(m *testing.M) { os.Exit(storetest.Main(m)) }

// The two builds the fleet rolls between.
const (
	c1 = "c1c1c1c1c1c1c1c1c1c1c1c1c1c1c1c1c1c1c1c1"
	c2 = "c2c2c2c2c2c2c2c2c2c2c2c2c2c2c2c2c2c2c2c2"
)

// recordingPublisher stands in for the bus client: it records every envelope it is asked to
// publish while it is connected and not failing, and refuses the rest. A held publisher's
// publishes answer only once release is called, as a stream's acknowledgement takes what time it
// takes; a publish whose ctx ends first records nothing and returns ctx's error, as the bus client
// sends nothing once its ctx has ended. A hanging publisher's publish never answers before its ctx
// ends.
type recordingPublisher struct {
	// held, when set, holds every publish until release closes it.
	held         chan struct{}
	mu           sync.Mutex
	disconnected bool
	hanging      bool
	failure      error
	published    []contracts.Envelope
	// running counts the publishes that have not returned.
	running int
}

func (p *recordingPublisher) Connected() bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	return !p.disconnected
}

func (p *recordingPublisher) PublishReportingDuplicateWithin(ctx context.Context, item contracts.Envelope) (bool, error) {
	p.mu.Lock()
	p.running++
	hanging, answered := p.hanging, p.held
	p.mu.Unlock()
	defer func() {
		p.mu.Lock()
		defer p.mu.Unlock()
		p.running--
	}()
	switch {
	case hanging:
		answered = nil // a nil channel never answers
	case answered == nil:
		answered = make(chan struct{})
		close(answered)
	}
	select {
	case <-ctx.Done():
		return false, ctx.Err()
	case <-answered:
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.failure != nil {
		return false, p.failure
	}
	p.published = append(p.published, item)
	return false, nil
}

func (p *recordingPublisher) hang(hanging bool) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.hanging = hanging
}

// release answers every held publish, and lets every later one answer at once.
func (p *recordingPublisher) release() {
	close(p.held)
}

func (p *recordingPublisher) inFlight() int {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.running
}

func (p *recordingPublisher) fail(err error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.failure = err
}

func (p *recordingPublisher) envelopes() []contracts.Envelope {
	p.mu.Lock()
	defer p.mu.Unlock()
	return append([]contracts.Envelope(nil), p.published...)
}

// register registers a server process serving commit, and deregisters it when the test ends
// unless the test has.
func register(t *testing.T, database *store.Store, commit string) *processes.Registration {
	t.Helper()
	keys, err := auth.ParseSigningKeys("startup-test-key")
	if err != nil {
		t.Fatal(err)
	}
	registration, err := processes.Register(context.Background(), database.Pool, processes.Instance{
		Commit: commit, SigningKeys: keys.Fingerprints(),
	})
	if err != nil {
		t.Fatalf("register a process serving %q: %v", commit, err)
	}
	t.Cleanup(func() { _ = registration.Deregister(context.Background()) })
	return registration
}

func deregister(t *testing.T, registrations ...*processes.Registration) {
	t.Helper()
	for _, r := range registrations {
		if err := r.Deregister(context.Background()); err != nil {
			t.Fatal(err)
		}
	}
}

// announcement is one row of server_build_announcements.
type announcement struct {
	seq         int64
	commit      string
	announcedAt time.Time
	published   bool
}

func announcements(t *testing.T, database *store.Store) []announcement {
	t.Helper()
	rows, err := database.Pool.Query(context.Background(),
		`select seq, commit, announced_at, published_at is not null from server_build_announcements order by seq`)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	var all []announcement
	for rows.Next() {
		var a announcement
		if err := rows.Scan(&a.seq, &a.commit, &a.announcedAt, &a.published); err != nil {
			t.Fatal(err)
		}
		all = append(all, a)
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	return all
}

// requireEnvelopes requires one announcement row per commit of commits, in that order, each
// published, and the publisher to hold exactly their envelopes, each the one Envelope builds from
// its row.
func requireEnvelopes(t *testing.T, database *store.Store, publisher *recordingPublisher, commits ...string) {
	t.Helper()
	got := publisher.envelopes()
	rows := announcements(t, database)
	if len(got) != len(commits) || len(rows) != len(commits) {
		t.Fatalf("published %d envelopes from %d announcements %+v, want %d naming %v: %+v", len(got), len(rows), rows, len(commits), commits, got)
	}
	for i, row := range rows {
		want, err := Envelope(row.commit, row.seq, row.announcedAt)
		if err != nil {
			t.Fatal(err)
		}
		if row.commit != commits[i] || !row.published || !reflect.DeepEqual(got[i], want) {
			t.Fatalf("announcement %d = %+v published as %+v, want %s published as %+v", i, row, got[i], commits[i], want)
		}
	}
}

func requireFleetCommit(t *testing.T, a *Announcer, want string) {
	t.Helper()
	got := a.FleetCommit()
	switch {
	case want == "" && got != nil:
		t.Fatalf("FleetCommit = %q, want nil", *got)
	case want != "" && (got == nil || *got != want):
		t.Fatalf("FleetCommit = %v, want %q", got, want)
	}
}

// The fleet's build is announced once each time every live process comes to serve one commit
// the latest announcement does not name: not while a roll has C1 and C2 live, once when the last
// C1 process goes, not again when every process restarts on C2, and again, under a new seq, when
// the fleet rolls back to C1. Each envelope is the one its row builds, its started_at the
// database's announced_at.
func TestTheFleetsBuildIsAnnouncedOnceEachTimeItChanges(t *testing.T) {
	database := storetest.Open(t)
	publisher := &recordingPublisher{}
	announcer := New(Deps{Pool: database.Pool, Publisher: publisher})
	ctx := context.Background()

	old := register(t, database, c1)
	rolled := register(t, database, c2)
	announcer.pass(ctx)
	requireEnvelopes(t, database, publisher)
	requireFleetCommit(t, announcer, "")

	deregister(t, old)
	announcer.pass(ctx)
	requireEnvelopes(t, database, publisher, c2)
	requireFleetCommit(t, announcer, c2)

	deregister(t, rolled)
	restarted := []*processes.Registration{register(t, database, c2), register(t, database, c2)}
	announcer.pass(ctx)
	announcer.pass(ctx)
	requireEnvelopes(t, database, publisher, c2)

	deregister(t, restarted...)
	register(t, database, c1)
	announcer.pass(ctx)
	requireEnvelopes(t, database, publisher, c2, c1)
	requireFleetCommit(t, announcer, c1)
}

// A build nothing stamped names nothing, so a fleet any of whose live processes carries no commit
// is never announced: not alone, and not beside a stamped one.
func TestAnUnstampedBuildIsNeverAnnounced(t *testing.T) {
	database := storetest.Open(t)
	publisher := &recordingPublisher{}
	announcer := New(Deps{Pool: database.Pool, Publisher: publisher})
	unstamped := register(t, database, "")
	announcer.pass(context.Background())
	requireEnvelopes(t, database, publisher)

	register(t, database, c1)
	announcer.pass(context.Background())
	requireEnvelopes(t, database, publisher)
	requireFleetCommit(t, announcer, "")

	deregister(t, unstamped)
	announcer.pass(context.Background())
	requireEnvelopes(t, database, publisher, c1)
}

// Two processes passing at once, each on a pool of its own, announce one build once and publish
// it once between them, though one of them reads the announcement as unpublished while the
// other's publish of it is in flight: the publish answers only once a process has passed
// meanwhile, and that pass finds the lock held, reads the announcement and leaves the publish to
// the lock's holder. Each then names the build as the fleet's.
func TestTwoAnnouncersAtOnceAnnounceAndPublishOnce(t *testing.T) {
	database := storetest.Open(t)
	register(t, database, c1)
	publisher := &recordingPublisher{held: make(chan struct{})}
	announcers := []*Announcer{
		New(Deps{Pool: database.Pool, Publisher: publisher}),
		New(Deps{Pool: storetest.SecondProcess(t, database).Pool, Publisher: publisher}),
	}
	// Ends the passes only if a regression keeps them from ever overlapping the publish: a pass
	// takes milliseconds, and the overlap comes within a few of them.
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	start := make(chan struct{})
	finished := make(chan struct{}, len(announcers))
	var passes sync.WaitGroup
	for _, a := range announcers {
		passes.Go(func() {
			<-start
			// A count of passes alone does not reach the publish. A pass that misses the lock and
			// finds nothing announced ends in a fraction of a millisecond, so the process that
			// misses it can spend twenty passes inside the other's announcing transaction, before
			// the announcement commits, and never read it. So each process passes until one of its
			// passes began while the other's publish was in flight, or the publish has answered.
			overlapped := false
			done := func() bool { return overlapped || len(publisher.envelopes()) > 0 }
			for i := 0; ctx.Err() == nil && (i < 20 || !done()); i++ {
				inFlight := publisher.inFlight() > 0
				a.pass(ctx)
				overlapped = overlapped || inFlight
			}
			finished <- struct{}{}
		})
	}
	close(start)
	// The first process to finish is not the one whose publish is held, so it has passed while
	// that publish was in flight.
	<-finished
	publisher.release()
	passes.Wait()
	if err := ctx.Err(); err != nil {
		t.Errorf("the passes ran until the bound ended them, so no process finished passing while the other's publish was held: %v", err)
	}
	requireEnvelopes(t, database, publisher, c1)
	for _, a := range announcers {
		requireFleetCommit(t, a, c1)
	}
}

// An announcement made while the publish fails, or while this process's bus is down, stays
// unpublished, and is published once by the first pass whose publish succeeds, this process's
// or another's.
func TestAnAnnouncementIsPublishedOnceAPublishSucceeds(t *testing.T) {
	database := storetest.Open(t)
	register(t, database, c1)
	ctx := context.Background()

	down := &recordingPublisher{disconnected: true}
	New(Deps{Pool: database.Pool, Publisher: down}).pass(ctx)
	if rows := announcements(t, database); len(rows) != 1 || rows[0].published {
		t.Fatalf("announcements after a pass with the bus down = %+v, want one unpublished", rows)
	}

	failing := &recordingPublisher{}
	failing.fail(errors.New("stream unavailable"))
	announcer := New(Deps{Pool: database.Pool, Publisher: failing})
	announcer.pass(ctx)
	announcer.pass(ctx)
	if rows := announcements(t, database); len(rows) != 1 || rows[0].published {
		t.Fatalf("announcements after failed publishes = %+v, want one unpublished", rows)
	}
	requireFleetCommit(t, announcer, c1)

	failing.fail(nil)
	announcer.pass(ctx)
	announcer.pass(ctx)
	requireEnvelopes(t, database, failing, c1)
	if got := down.envelopes(); len(got) != 0 {
		t.Fatalf("a publisher with no connection was asked to publish %+v", got)
	}
}

// A publish that does not answer within publishTimeout is abandoned with its context, so nothing
// of it runs on once its pass has given up and moved on: the announcement stays unpublished, and
// a later pass whose publish answers publishes it.
func TestAPublishThatOutlivesItsBoundIsAbandoned(t *testing.T) {
	before := publishTimeout
	publishTimeout = 200 * time.Millisecond
	t.Cleanup(func() { publishTimeout = before })
	database := storetest.Open(t)
	register(t, database, c1)
	ctx := context.Background()

	publisher := &recordingPublisher{}
	publisher.hang(true)
	announcer := New(Deps{Pool: database.Pool, Publisher: publisher})
	announcer.pass(ctx)
	if running := publisher.inFlight(); running != 0 {
		t.Fatalf("%d publishes still running after the pass that gave up on them", running)
	}
	if rows := announcements(t, database); len(rows) != 1 || rows[0].published {
		t.Fatalf("announcements after an abandoned publish = %+v, want one unpublished", rows)
	}
	if got := publisher.envelopes(); len(got) != 0 {
		t.Fatalf("an abandoned publish published %+v", got)
	}

	publisher.hang(false)
	announcer.pass(ctx)
	requireEnvelopes(t, database, publisher, c1)
}

// fixturePath is the server.started envelope the Legion daemon's intake decodes, shared with it
// and with packages/contracts: the build this package publishes is the one the daemon reads.
const fixturePath = "../../../../daemon/internal/intake/testdata/dispatch/server-started.json"

// The envelope of announcement 7 of commit 0f1e2d3… at a fixed time is, field for field, the
// shared fixture.
func TestTheBuiltEnvelopeIsTheSharedFixture(t *testing.T) {
	data, err := os.ReadFile(fixturePath)
	if err != nil {
		t.Fatal(err)
	}
	var fixture contracts.Envelope
	if err := json.Unmarshal(data, &fixture); err != nil {
		t.Fatal(err)
	}
	built, err := Envelope("0f1e2d3c4b5a69788796a5b4c3d2e1f00f1e2d3c", 7, time.Date(2026, 10, 27, 12, 0, 0, 345_000_000, time.UTC))
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(built, fixture) {
		t.Fatalf("built envelope = %+v\nwant the fixture %+v", built, fixture)
	}
}
