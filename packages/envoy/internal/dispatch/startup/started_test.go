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
// publish while it is connected and not failing, and refuses the rest. delay is how long each
// publish takes before it answers, as a stream's acknowledgement does.
type recordingPublisher struct {
	delay        time.Duration
	mu           sync.Mutex
	disconnected bool
	failure      error
	published    []contracts.Envelope
}

func (p *recordingPublisher) Connected() bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	return !p.disconnected
}

func (p *recordingPublisher) PublishReportingDuplicate(item contracts.Envelope) (bool, error) {
	time.Sleep(p.delay)
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.failure != nil {
		return false, p.failure
	}
	p.published = append(p.published, item)
	return false, nil
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
// it once between them, though each publish takes long enough for the other process to read the
// announcement as unpublished meanwhile.
func TestTwoAnnouncersAtOnceAnnounceAndPublishOnce(t *testing.T) {
	database := storetest.Open(t)
	register(t, database, c1)
	publisher := &recordingPublisher{delay: 100 * time.Millisecond}
	announcers := []*Announcer{
		New(Deps{Pool: database.Pool, Publisher: publisher}),
		New(Deps{Pool: storetest.SecondProcess(t, database).Pool, Publisher: publisher}),
	}
	start := make(chan struct{})
	var passes sync.WaitGroup
	for _, a := range announcers {
		passes.Go(func() {
			<-start
			for range 20 {
				a.pass(context.Background())
			}
		})
	}
	close(start)
	passes.Wait()
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
