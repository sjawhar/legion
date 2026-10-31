package session

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
	"time"

	natsgo "github.com/nats-io/nats.go"
	"github.com/sjawhar/envoy/internal/bus"
	"github.com/sjawhar/envoy/internal/contracts"
	"github.com/sjawhar/envoy/internal/metrics"
)

// counterValue reads name's unlabelled sample from met's own scrape, the only way a counter's
// value is observable, which is also how an operator reads it.
func counterValue(t *testing.T, met *metrics.Registry, name string) int64 {
	t.Helper()
	recorder := httptest.NewRecorder()
	met.Handler().ServeHTTP(recorder, httptest.NewRequest(http.MethodGet, "/metrics", nil))
	for _, line := range strings.Split(recorder.Body.String(), "\n") {
		sample, found := strings.CutPrefix(line, name+" ")
		if !found {
			continue
		}
		value, err := strconv.ParseInt(strings.TrimSpace(sample), 10, 64)
		if err != nil {
			t.Fatalf("read the %s sample %q: %v", name, line, err)
		}
		return value
	}
	return 0
}

// reapingRegistry opens a registry whose records outlive the horizon in the bucket - the maximum
// age is far longer than the windows - so what the reaper deletes is decided by a record's own age
// and never by the bucket's backstop having dropped it first. That is the deployed shape this
// change exists for: ENVOY_SESSION_MAX_AGE past the gone horizon, so a stalled session's record
// stands long enough to be read as unresponsive.
func reapingRegistry(t *testing.T, client *bus.Client, now *clock, options ...SessionRegistryOption) *SessionRegistry {
	t.Helper()
	r, err := OpenSessionRegistry(client.Conn, append([]SessionRegistryOption{
		WithSessionReplicas(1),
		WithSessionTTL(time.Hour),
		WithSessionClock(now.Now),
		WithSessionLiveWindow(time.Minute),
		WithSessionGoneHorizon(2 * time.Minute),
	}, options...)...)
	if err != nil {
		t.Fatalf("open session registry: %v", err)
	}
	t.Cleanup(r.StopWatch)
	waitFor(t, 5*time.Second, r.watcher.Following)
	return r
}

// A pass deletes the record of a session past the gone horizon, and keeps the record of one that
// registered again between the pass's decision and its delete: the delete names the revision the
// pass judged, so a session that came back in that window - the very thing a box under load does -
// is not deleted on the strength of a reading that is no longer true.
//
// The two candidates are judged in one call, as a pass judges them, and only then is the race run,
// so this pins the ordering the pass depends on and not a single conditional delete.
func TestAReaperPassDeletesARecordPastTheHorizonAndKeepsOneRegisteredSince(t *testing.T) {
	client := setupNATS(t)
	now := &clock{}
	r := reapingRegistry(t, client, now)
	for _, id := range []string{"ses_gone", "ses_back"} {
		if err := r.Put(id, SessionEntry{Port: 1, MachineID: "m1"}); err != nil {
			t.Fatalf("register %s: %v", id, err)
		}
	}
	now.advance(3 * time.Minute)

	candidates := r.goneCandidates(now.Now())
	if len(candidates) != 2 {
		t.Fatalf("the pass judged %+v, want both records gone", candidates)
	}
	// The registration that lands between the decision and the delete.
	if err := r.Put("ses_back", SessionEntry{Port: 1, MachineID: "m1"}); err != nil {
		t.Fatalf("re-register ses_back: %v", err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	for _, candidate := range candidates {
		deleted, err := r.deleteGone(ctx, candidate)
		if err != nil {
			t.Fatalf("delete %s: %v", candidate.sessionID, err)
		}
		if want := candidate.sessionID == "ses_gone"; deleted != want {
			t.Fatalf("delete %s = %t, want %t", candidate.sessionID, deleted, want)
		}
	}

	if _, err := r.Refresh(ctx, "ses_gone"); !errors.Is(err, natsgo.ErrKeyNotFound) {
		t.Fatalf("the bucket's record of ses_gone after the pass = %v, want it deleted", err)
	}
	entry, err := r.Refresh(ctx, "ses_back")
	if err != nil {
		t.Fatalf("the bucket's record of ses_back after the pass: %v", err)
	}
	if state := r.EntryState(entry); state != Live {
		t.Fatalf("ses_back after the pass = %s, want it live on the registration that landed", state)
	}
}

// A pass deletes only the records past the horizon, counts each one it deleted, and leaves an
// unresponsive session's record alone: the session that stopped answering keeps everything that
// depends on its record standing, and nothing but the horizon takes it away.
func TestAReaperPassKeepsAnUnresponsiveRecordAndCountsTheGoneOne(t *testing.T) {
	client := setupNATS(t)
	now := &clock{}
	r := reapingRegistry(t, client, now)
	if err := r.Put("ses_gone", SessionEntry{Port: 1, MachineID: "m1"}); err != nil {
		t.Fatalf("register ses_gone: %v", err)
	}
	now.advance(90 * time.Second)
	if err := r.Put("ses_stalled", SessionEntry{Port: 2, MachineID: "m1"}); err != nil {
		t.Fatalf("register ses_stalled: %v", err)
	}
	// ses_gone is now 2m30s old (past the 2m horizon) and ses_stalled 1m (past the 1m live
	// window, inside the horizon).
	now.advance(60 * time.Second)
	if _, state := r.Lookup("ses_stalled"); state != Unresponsive {
		t.Fatalf("ses_stalled = %s, want %s", state, Unresponsive)
	}

	met := metrics.New()
	reaper := SessionReaper{Reaped: met.NewCounter("reaped", "test"), StoredAfterLastSeen: met.NewCounter("stored", "test")}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if reaped := r.ReapSessions(ctx, reaper); reaped != 1 {
		t.Fatalf("the pass reaped %d records, want only the one past the horizon", reaped)
	}
	if _, err := r.Refresh(ctx, "ses_gone"); !errors.Is(err, natsgo.ErrKeyNotFound) {
		t.Fatalf("the bucket's record of ses_gone after the pass = %v, want it deleted", err)
	}
	if _, state := r.Lookup("ses_stalled"); state != Unresponsive {
		t.Fatalf("ses_stalled after the pass = %s, want its record still standing as %s", state, Unresponsive)
	}
}

// The count on a reaped session's log line is the messages the stream stored after that session's
// last registration, not every message it ever held for it: a session whose record reached the
// horizon is reported by how much piled up while it was not answering.
func TestAReapedSessionsStoredCountStartsAtItsLastRegistration(t *testing.T) {
	client := setupNATS(t)
	now := &clock{}
	r := reapingRegistry(t, client, now)
	const sessionID = "ses_counted"
	publish := func(eventID string) {
		t.Helper()
		item := contracts.Envelope{
			EventID:        eventID,
			Source:         "agent",
			SourceSession:  "ses_sender",
			Topic:          contracts.AgentSubject(sessionID),
			DedupeKey:      eventID,
			IssuedAt:       time.Now().UnixMilli(),
			PayloadSummary: "stored for a session that stopped answering",
		}
		if err := client.Publish(item); err != nil {
			t.Fatalf("publish %s: %v", eventID, err)
		}
	}
	publish("before-1")
	publish("before-2")
	// The stream stamps a message with the server's clock in milliseconds, as the registration
	// records its own: a pause here keeps the two phases on either side of the registration
	// rather than inside one millisecond of it.
	time.Sleep(10 * time.Millisecond)
	if err := r.Put(sessionID, SessionEntry{Port: 1, MachineID: "m1", SelfSubscribed: true}); err != nil {
		t.Fatalf("register %s: %v", sessionID, err)
	}
	time.Sleep(10 * time.Millisecond)
	publish("after-1")
	publish("after-2")
	publish("after-3")
	now.advance(3 * time.Minute)

	met := metrics.New()
	stored := met.NewCounter("stored", "test")
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if reaped := r.ReapSessions(ctx, SessionReaper{Client: client, StoredAfterLastSeen: stored}); reaped != 1 {
		t.Fatalf("the pass reaped %d records, want the one past the horizon", reaped)
	}
	if got := counterValue(t, met, "stored"); got != 3 {
		t.Fatalf("messages stored after the last registration = %d, want the 3 published after it", got)
	}
}

// The bucket's maximum age is settled on every pass, so an age another process raised reaches a
// running registry within one pass and with no restart: every row's deadline moves with it, and a
// record then stands long enough for the gone horizon to be reached. Without that, a deployment
// that raises the age has to restart every listener before any of them stops reading a stalled
// session as dead.
func TestAPassAdoptsAMaximumAgeAnotherProcessSet(t *testing.T) {
	client := setupNATS(t)
	now := &clock{}
	r, err := OpenSessionRegistry(client.Conn, WithSessionReplicas(1), WithSessionTTL(time.Minute), WithSessionClock(now.Now))
	if err != nil {
		t.Fatalf("open session registry: %v", err)
	}
	t.Cleanup(r.StopWatch)
	waitFor(t, 5*time.Second, r.watcher.Following)
	if got := r.TTL(); got != time.Minute {
		t.Fatalf("the registry opened on a %s maximum age, want the minute the bucket was created with", got)
	}

	js, err := client.Conn.JetStream()
	if err != nil {
		t.Fatalf("jetstream: %v", err)
	}
	if _, err := bus.SetKeyValueMaxAge(js, bus.SessionBucket, 90*time.Minute); err != nil {
		t.Fatalf("another process raising the bucket's maximum age: %v", err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	r.ReapSessions(ctx, SessionReaper{})
	if got := r.TTL(); got != 90*time.Minute {
		t.Fatalf("the maximum age in use after one pass = %s, want the 90m another process set", got)
	}
}

// A registry opened with a maximum age writes it on the bucket, so a bucket an older build created
// at the 5-minute default keeps a stalled session's record past the gone horizon from this
// listener's first pass onwards, and the age it reports is the age the bucket really carries.
func TestOpeningWithAMaximumAgeWritesItOnTheBucket(t *testing.T) {
	client := setupNATS(t)
	first, err := OpenSessionRegistry(client.Conn, WithSessionReplicas(1), WithSessionTTL(time.Minute))
	if err != nil {
		t.Fatalf("open the session registry at a minute: %v", err)
	}
	first.StopWatch()

	raised, err := OpenSessionRegistry(client.Conn, WithSessionReplicas(1), WithSessionTTL(time.Minute), WithSessionMaxAge(90*time.Minute))
	if err != nil {
		t.Fatalf("open the session registry with a maximum age: %v", err)
	}
	t.Cleanup(raised.StopWatch)
	if got := raised.TTL(); got != 90*time.Minute {
		t.Fatalf("the maximum age in use = %s, want the 90m the registry was opened with", got)
	}

	js, err := client.Conn.JetStream()
	if err != nil {
		t.Fatalf("jetstream: %v", err)
	}
	info, err := js.StreamInfo("KV_" + bus.SessionBucket)
	if err != nil {
		t.Fatalf("read the session bucket's stream: %v", err)
	}
	if info.Config.MaxAge != 90*time.Minute {
		t.Fatalf("the bucket's maximum age = %s, want the 90m written on it", info.Config.MaxAge)
	}
}
