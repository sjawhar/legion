package session

import (
	"errors"
	"testing"
	"time"

	natsgo "github.com/nats-io/nats.go"
)

// Each window is the age a record may reach, not the age it may not: a record exactly one live
// window old is still live, and one exactly one gone horizon old is still unresponsive. A
// boundary that fell the other way would call a session stalled at the instant its own refresh
// was due, and would release the claims of a session the reaper has not yet decided is gone.
func TestStateOfAtItsBoundaries(t *testing.T) {
	const (
		live    = 5 * time.Minute
		horizon = time.Hour
	)
	now := time.Date(2026, 10, 31, 12, 0, 0, 0, time.UTC)
	for _, tc := range []struct {
		name string
		age  time.Duration
		want State
	}{
		{"just inside the live window", live - time.Millisecond, Live},
		{"exactly the live window", live, Live},
		{"just past the live window", live + time.Millisecond, Unresponsive},
		{"inside the gone horizon", horizon - time.Minute, Unresponsive},
		{"exactly the gone horizon", horizon, Unresponsive},
		{"just past the gone horizon", horizon + time.Millisecond, Gone},
	} {
		t.Run(tc.name, func(t *testing.T) {
			entry := SessionEntry{Port: 1, UpdatedAt: now.Add(-tc.age).UnixMilli()}
			if got := StateOf(entry, now, live, horizon); got != tc.want {
				t.Fatalf("StateOf(a record %v old) = %s, want %s", tc.age, got, tc.want)
			}
		})
	}
}

// A record no registration dates cannot be judged live: there is no moment to measure it from.
func TestStateOfARecordWithNoRegistrationTimeIsGone(t *testing.T) {
	if got := StateOf(SessionEntry{Port: 1}, time.Now(), LiveWindow, GoneHorizon); got != Gone {
		t.Fatalf("StateOf(a record with no updated_at) = %s, want %s", got, Gone)
	}
}

// IsLive answers on the package's own windows, for a caller holding a record and no registry.
func TestIsLiveMeasuresThePackagesOwnLiveWindow(t *testing.T) {
	// A record's time is whole milliseconds, so the clock here is too: a now with nanoseconds in
	// it would make the record below a fraction older than the window and prove nothing.
	now := time.UnixMilli(time.Now().UnixMilli())
	if !IsLive(SessionEntry{Port: 1, UpdatedAt: now.Add(-LiveWindow).UnixMilli()}, now) {
		t.Fatalf("a record exactly one live window (%v) old is not live", LiveWindow)
	}
	if IsLive(SessionEntry{Port: 1, UpdatedAt: now.Add(-LiveWindow - time.Millisecond).UnixMilli()}, now) {
		t.Fatalf("a record past the live window (%v) reads live", LiveWindow)
	}
}

// statedRegistry is a registry whose bucket keeps records far longer than its live window, which
// is what the listener runs once the maximum age goes up: no row's deadline is anywhere near, so
// only its own registration says whether its session is live, unresponsive or gone. Its clock is
// fixed, so every row's age is the one this test wrote.
func statedRegistry(t *testing.T, now time.Time) *SessionRegistry {
	t.Helper()
	const bucket = 2 * time.Hour
	r := newTestRegistry(bucket)
	r.now = func() time.Time { return now }
	rows := map[string]cachedSession{}
	for id, age := range map[string]time.Duration{
		"ses_live":         time.Minute,
		"ses_unresponsive": 30 * time.Minute,
		"ses_gone":         90 * time.Minute,
	} {
		rows[id] = cachedSession{
			entry:     SessionEntry{Port: len(id), UpdatedAt: now.Add(-age).UnixMilli()},
			expiresAt: now.Add(bucket - age),
		}
	}
	hold(t, r, rows)
	return r
}

// The default listing is the live sessions alone, so a caller that cannot tell the states apart —
// an older plugin, an older Dispatch — never takes a stalled session for a working one; a caller
// that asks for the unresponsive rows gets them with the state and the last registration that say
// so. A gone row is listed either way for nobody.
func TestListCarriesTheStateAndAnswersUnresponsiveRowsOnlyWhenAsked(t *testing.T) {
	now := time.Date(2026, 10, 31, 12, 0, 0, 0, time.UTC)
	r := statedRegistry(t, now)

	live, err := r.List()
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(live) != 1 || live[0].SessionID != "ses_live" || live[0].State != Live {
		t.Fatalf("List() = %+v, want ses_live alone, live", live)
	}
	if want := now.Add(-time.Minute).UnixMilli(); live[0].UpdatedAt != want {
		t.Fatalf("the live row's last registration = %d, want %d", live[0].UpdatedAt, want)
	}

	all, err := r.List(IncludeUnresponsive())
	if err != nil {
		t.Fatalf("List(IncludeUnresponsive): %v", err)
	}
	if len(all) != 2 || all[0].SessionID != "ses_live" || all[1].SessionID != "ses_unresponsive" {
		t.Fatalf("List(IncludeUnresponsive) = %+v, want ses_live and ses_unresponsive", all)
	}
	if all[1].State != Unresponsive {
		t.Fatalf("the stalled row's state = %s, want %s", all[1].State, Unresponsive)
	}
	if want := now.Add(-30 * time.Minute).UnixMilli(); all[1].UpdatedAt != want {
		t.Fatalf("the stalled row's last registration = %d, want %d", all[1].UpdatedAt, want)
	}
}

// Lookup is the read that tells a session which has stopped answering from one that has ended:
// the first keeps its claims and has messages stored for it, the second is replaced. Get cannot
// tell them apart once the bucket keeps a record past the live window — it answers the stalled
// session's entry, since the row's deadline is the bucket's backstop and nothing else.
func TestLookupTellsAnUnresponsiveSessionFromAGoneOne(t *testing.T) {
	now := time.Date(2026, 10, 31, 12, 0, 0, 0, time.UTC)
	r := statedRegistry(t, now)

	for id, want := range map[string]State{"ses_live": Live, "ses_unresponsive": Unresponsive, "ses_gone": Gone} {
		entry, state := r.Lookup(id)
		if state != want {
			t.Fatalf("Lookup(%s) = %s, want %s", id, state, want)
		}
		if entry.UpdatedAt == 0 {
			t.Fatalf("Lookup(%s) answered no record, so no caller can say when it was last seen", id)
		}
	}
	entry, state := r.Lookup("ses_never_registered")
	if state != Gone || entry.Port != 0 {
		t.Fatalf("Lookup(an id with no record) = %+v, %s; want the zero record and %s", entry, state, Gone)
	}
	if _, err := r.Get("ses_never_registered"); !errors.Is(err, natsgo.ErrKeyNotFound) {
		t.Fatalf("Get(an id with no record) = %v, want ErrKeyNotFound", err)
	}
	if got, err := r.Get("ses_unresponsive"); err != nil || got.Port == 0 {
		t.Fatalf("Get(a stalled session) = %+v, %v; want its entry, which is why a caller that must not take it for live asks Lookup", got, err)
	}
}

// The envoy_active_sessions gauge counts what LiveCount counts. Once the bucket's maximum age is
// hours, "has not passed its deadline" is every record the bucket holds, so a count made that way
// would report a box of stalled sessions as a box of working ones.
func TestLiveCountCountsOnlyLiveRowsUnderALongBucketMaximumAge(t *testing.T) {
	now := time.Date(2026, 10, 31, 12, 0, 0, 0, time.UTC)
	r := statedRegistry(t, now)
	if live, size := r.LiveCount(), r.CacheSize(); live != 1 || size != 3 {
		t.Fatalf("LiveCount %d of CacheSize %d; want 1 live row of the 3 the bucket still holds", live, size)
	}
}
