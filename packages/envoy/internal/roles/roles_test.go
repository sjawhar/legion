package roles_test

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/nats-io/nats.go"

	"github.com/sjawhar/envoy/internal/roles"
	"github.com/sjawhar/envoy/internal/session"
	"github.com/sjawhar/envoy/internal/store"
)

// claims is a ClaimStore in memory: holder is the claim every read answers, instance the client
// instance that claim names as the holder's process, handedBack the hand-back mark it carries,
// revision the role bucket revision it was read at, readErr fails every read, and reads counts
// them. It has no release and no write of any kind, as the interface it implements has none.
type claims struct {
	holder     string
	instance   string
	handedBack bool
	revision   uint64
	readErr    error
	reads      int
}

func (c *claims) RoleClaim(context.Context, string) (store.RoleClaim, error) {
	c.reads++
	if c.readErr != nil {
		return store.RoleClaim{}, c.readErr
	}
	claim := store.RoleClaim{HolderSessionID: c.holder, ClientInstanceID: c.instance, HandedBack: c.handedBack}
	if c.holder != "" {
		claim.Revision = c.revision
	}
	return claim, nil
}

// sessions is a SessionLookup in memory: entries are the records it holds, remembered what it can
// still say of a session whose record has gone, and readErr fails every read. It judges a record
// through the windows it carries, as Buckets does.
type sessions struct {
	roles.Windows
	entries    map[string]session.SessionEntry
	remembered map[string]int64
	readErr    error
}

func (s sessions) Holder(_ context.Context, sessionID string) (session.SessionEntry, error) {
	if s.readErr != nil {
		return session.SessionEntry{}, s.readErr
	}
	entry, ok := s.entries[sessionID]
	if !ok {
		return session.SessionEntry{}, nats.ErrKeyNotFound
	}
	return entry, nil
}

func (s sessions) LastSeen(sessionID string) int64 { return s.remembered[sessionID] }

// registered is a record last written age ago.
func registered(age time.Duration) session.SessionEntry {
	return session.SessionEntry{Title: "holder", UpdatedAt: time.Now().Add(-age).UnixMilli()}
}

// A resolution says what it found and takes nothing: a holder past the live window and one past
// the gone horizon are both lapsed, each named with the state its own record puts it in and the
// registration that dates it, and the claim is read once and never written. That is the whole of
// dispatch://INFRA-392's R4 for every caller of this package: an unresponsive session keeps the
// role it claimed, and so does a gone one until the role-claim reaper takes it.
func TestResolveHolderReadsEachStateAndTakesNothing(t *testing.T) {
	live := registered(time.Minute)
	stalled := registered(session.LiveWindow + time.Minute)
	dead := registered(session.GoneHorizon + time.Minute)
	held := sessions{
		entries:    map[string]session.SessionEntry{"ses_live": live, "ses_stalled": stalled, "ses_dead": dead},
		remembered: map[string]int64{"ses_departed": 1_700_000_000_000},
	}
	for _, tc := range []struct {
		name        string
		holder      string
		want        roles.State
		holderState session.State
		lastSeen    int64
	}{
		{"a live holder", "ses_live", roles.Live, session.Live, live.UpdatedAt},
		{"no claim", "", roles.Unclaimed, "", 0},
		{"a holder past the live window", "ses_stalled", roles.Lapsed, session.Unresponsive, stalled.UpdatedAt},
		{"a holder past the gone horizon", "ses_dead", roles.Lapsed, session.Gone, dead.UpdatedAt},
		{"a holder whose record has gone, still remembered", "ses_departed", roles.Lapsed, session.Gone, 1_700_000_000_000},
		{"a holder nothing dates", "ses_never", roles.Lapsed, session.Gone, 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			claimed := &claims{holder: tc.holder}
			resolution, err := roles.ResolveHolder(context.Background(), claimed, held, "reviewer")
			if err != nil {
				t.Fatalf("resolve: %v", err)
			}
			if resolution.State != tc.want || resolution.Holder != tc.holder ||
				resolution.HolderState != tc.holderState || resolution.LastSeen != tc.lastSeen {
				t.Fatalf("resolution %+v, want state %s of holder %q, holder state %q, last seen %d",
					resolution, tc.want, tc.holder, tc.holderState, tc.lastSeen)
			}
			if tc.want == roles.Live && resolution.Entry.Title != "holder" {
				t.Fatalf("the live holder's entry is %+v, want the record the lookup holds", resolution.Entry)
			}
			if claimed.reads != 1 {
				t.Fatalf("the claim was read %d times, want once: a resolution rereads nothing, having released nothing", claimed.reads)
			}
		})
	}
}

// The windows the lookup runs with decide the holder's state, and nothing compiled into this
// package does: the same record reads live under one live window and unresponsive under a shorter
// one, which is how a listener started with ENVOY_SESSION_LIVE_WINDOW and Dispatch's buckets each
// judge their own holders.
func TestResolveHolderJudgesTheHolderOnTheWindowsItIsGiven(t *testing.T) {
	entry := registered(10 * time.Minute)
	for _, tc := range []struct {
		name    string
		windows roles.Windows
		want    roles.State
	}{
		{"a live window the holder is inside", roles.Windows{Live: time.Hour, Horizon: 2 * time.Hour}, roles.Live},
		{"a live window the holder is past", roles.Windows{Live: time.Minute, Horizon: time.Hour}, roles.Lapsed},
	} {
		t.Run(tc.name, func(t *testing.T) {
			held := sessions{Windows: tc.windows, entries: map[string]session.SessionEntry{"ses_holder": entry}}
			resolution, err := roles.ResolveHolder(context.Background(), &claims{holder: "ses_holder"}, held, "reviewer")
			if err != nil || resolution.State != tc.want {
				t.Fatalf("resolution %+v, %v; want %s", resolution, err, tc.want)
			}
		})
	}
}

// A resolution given a firstHolder reads the claim from it and the claim store not at all, which
// is how the role lane spends no bucket round trip on a message it forwards.
func TestResolveHolderFromReadsTheClaimItIsGiven(t *testing.T) {
	claimed := &claims{holder: "ses_other"}
	held := sessions{entries: map[string]session.SessionEntry{"ses_cached": registered(time.Second)}}
	resolution, err := roles.ResolveHolderFrom(context.Background(), claimed, func(string) (string, error) {
		return "ses_cached", nil
	}, held, "reviewer")
	if err != nil || resolution.State != roles.Live || resolution.Holder != "ses_cached" {
		t.Fatalf("resolution %+v, %v; want the cached holder read live", resolution, err)
	}
	if claimed.reads != 0 {
		t.Fatalf("the claim store was read %d times, want not at all", claimed.reads)
	}
}

// A read that fails names its step, and the holder once the claim named one, so the role lane
// logs the read that failed and a route answers it. Nothing is decided about the holder: a
// session read that did not answer is no verdict on the session.
func TestResolveHolderNamesTheStepThatFailed(t *testing.T) {
	failure := errors.New("no answer")
	for _, tc := range []struct {
		name     string
		claims   *claims
		sessions sessions
		step     roles.Step
		holder   string
		message  string
	}{
		{"the claim", &claims{readErr: failure}, sessions{}, roles.ClaimRead, "", "no answer"},
		{"the session", &claims{holder: "ses_holder"}, sessions{readErr: failure}, roles.SessionRead, "ses_holder", "read role holder session: no answer"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			resolution, err := roles.ResolveHolder(context.Background(), tc.claims, tc.sessions, "reviewer")
			readErr, ok := errors.AsType[*roles.ReadError](err)
			if !ok || readErr.Step != tc.step || readErr.Holder != tc.holder || !errors.Is(err, failure) || resolution.Holder != tc.holder {
				t.Fatalf("resolve returned %+v, %v; want a read error at step %d about %q wrapping the failure", resolution, err, tc.step, tc.holder)
			}
			if err.Error() != tc.message {
				t.Fatalf("the error reads %q, want %q: a route answers this message", err.Error(), tc.message)
			}
			if resolution.State == roles.Lapsed || resolution.HolderState != "" {
				t.Fatalf("resolution %+v: a read that did not answer says nothing about the holder", resolution)
			}
		})
	}
}

// ClaimRevision is the revision of the claim record the resolution read, and a record stands
// behind every holder here, since nothing releases one: the claim's own revision on a live holder
// and on a lapsed one alike, zero where no claim stands, and zero for a claim read from a
// firstHolder copy, which carries no revision. That revision is what a conditional claim names as
// its precondition, so a scheduler can take the role of a holder that never came back.
func TestResolveHolderReportsTheRevisionOfTheClaimItRead(t *testing.T) {
	held := sessions{
		Windows:    roles.Windows{Live: time.Minute, Horizon: time.Hour},
		entries:    map[string]session.SessionEntry{"ses_live": registered(time.Second), "ses_stalled": registered(30 * time.Minute)},
		remembered: map[string]int64{},
	}
	for _, tc := range []struct {
		name   string
		claims *claims
		state  roles.State
		want   uint64
	}{
		{"a live holder", &claims{holder: "ses_live", revision: 7}, roles.Live, 7},
		{"a holder that stopped answering", &claims{holder: "ses_stalled", revision: 9}, roles.Lapsed, 9},
		{"a holder with no record left", &claims{holder: "ses_departed", revision: 11}, roles.Lapsed, 11},
		{"no claim", &claims{holder: "", revision: 7}, roles.Unclaimed, 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			resolution, err := roles.ResolveHolder(context.Background(), tc.claims, held, "reviewer")
			if err != nil {
				t.Fatalf("resolve: %v", err)
			}
			if resolution.State != tc.state || resolution.ClaimRevision != tc.want {
				t.Fatalf("resolution %+v, want %s at claim revision %d", resolution, tc.state, tc.want)
			}
		})
	}
	t.Run("a holder read from a firstHolder copy", func(t *testing.T) {
		bucket := &claims{holder: "ses_live", revision: 7}
		resolution, err := roles.ResolveHolderFrom(context.Background(), bucket, func(string) (string, error) {
			return "ses_live", nil
		}, held, "reviewer")
		if err != nil {
			t.Fatalf("resolve: %v", err)
		}
		if resolution.State != roles.Live || resolution.ClaimRevision != 0 || bucket.reads != 0 {
			t.Fatalf("resolution %+v after %d claim reads, want live at revision 0 with no claim read", resolution, bucket.reads)
		}
	})
}

// HolderClientInstanceID is the process the claim names as its holder, which GET /v1/roles/<role>
// answers beside the holder's session id so a client can see that a role is held under its own
// session id by another process. It is the claim's own field on a live holder and on a lapsed
// one alike, empty for a claim that names no process - one an earlier build wrote, or one whose
// claimant stated none - and empty for a claim read from a firstHolder copy, which names a
// holder and nothing else whatever the store holds.
func TestResolveHolderReportsTheProcessTheClaimNames(t *testing.T) {
	const instance = "4f1c2a9e8b7d6c5a4f1c2a9e8b7d6c5a"
	held := sessions{
		Windows: roles.Windows{Live: time.Minute, Horizon: time.Hour},
		entries: map[string]session.SessionEntry{"ses_live": registered(time.Second), "ses_stalled": registered(30 * time.Minute)},
	}
	for _, tc := range []struct {
		name   string
		claims *claims
		state  roles.State
		want   string
	}{
		{"a live holder's process", &claims{holder: "ses_live", instance: instance}, roles.Live, instance},
		{"a lapsed holder's process", &claims{holder: "ses_stalled", instance: instance}, roles.Lapsed, instance},
		{"a claim that names no process", &claims{holder: "ses_live"}, roles.Live, ""},
		{"no claim at all", &claims{}, roles.Unclaimed, ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			resolution, err := roles.ResolveHolder(context.Background(), tc.claims, held, "reviewer")
			if err != nil {
				t.Fatalf("resolve: %v", err)
			}
			if resolution.State != tc.state || resolution.HolderClientInstanceID != tc.want {
				t.Fatalf("resolution %+v, want %s held by the process %q", resolution, tc.state, tc.want)
			}
		})
	}
	t.Run("a holder read from a firstHolder copy", func(t *testing.T) {
		bucket := &claims{holder: "ses_live", instance: instance}
		resolution, err := roles.ResolveHolderFrom(context.Background(), bucket, func(string) (string, error) {
			return "ses_live", nil
		}, held, "reviewer")
		if err != nil {
			t.Fatalf("resolve: %v", err)
		}
		if resolution.State != roles.Live || resolution.HolderClientInstanceID != "" || bucket.reads != 0 {
			t.Fatalf("resolution %+v after %d claim reads, want live naming no process with no claim read", resolution, bucket.reads)
		}
	})
}

// HolderHandedBack is the hand-back mark the claim carries, which GET /v1/roles/<role> answers
// as handed_back so a client sees that the holder gave the role back and that nothing is
// renewing the claim any more. Nothing here releases a claim, so the mark reads the same on a
// live holder and on a lapsed one; a claim read from a firstHolder copy carries false whatever
// the store holds, as its process does, since the copy names a holder and nothing else.
func TestResolveHolderReportsTheHandBackMarkTheClaimCarries(t *testing.T) {
	const instance = "4f1c2a9e8b7d6c5a4f1c2a9e8b7d6c5a"
	held := sessions{
		Windows: roles.Windows{Live: time.Minute, Horizon: time.Hour},
		entries: map[string]session.SessionEntry{"ses_live": registered(time.Second), "ses_stalled": registered(30 * time.Minute)},
	}
	for _, tc := range []struct {
		name   string
		claims *claims
		state  roles.State
		want   bool
	}{
		{"a live holder that handed the role back", &claims{holder: "ses_live", instance: instance, handedBack: true}, roles.Live, true},
		{"a lapsed holder that handed the role back", &claims{holder: "ses_stalled", instance: instance, handedBack: true}, roles.Lapsed, true},
		{"a claim nobody handed back", &claims{holder: "ses_live", instance: instance}, roles.Live, false},
		{"no claim at all", &claims{}, roles.Unclaimed, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			resolution, err := roles.ResolveHolder(context.Background(), tc.claims, held, "reviewer")
			if err != nil {
				t.Fatalf("resolve: %v", err)
			}
			if resolution.State != tc.state || resolution.HolderHandedBack != tc.want {
				t.Fatalf("resolution %+v, want %s with the hand-back mark %t", resolution, tc.state, tc.want)
			}
		})
	}
	t.Run("a holder read from a firstHolder copy", func(t *testing.T) {
		bucket := &claims{holder: "ses_live", instance: instance, handedBack: true}
		resolution, err := roles.ResolveHolderFrom(context.Background(), bucket, func(string) (string, error) {
			return "ses_live", nil
		}, held, "reviewer")
		if err != nil {
			t.Fatalf("resolve: %v", err)
		}
		if resolution.State != roles.Live || resolution.HolderHandedBack || bucket.reads != 0 {
			t.Fatalf("resolution %+v after %d claim reads, want live carrying no mark with no claim read", resolution, bucket.reads)
		}
	})
}

// No read starts once the context has ended: the answer is the context's error, naming the role,
// as the read of the claim that did not happen.
func TestResolveHolderStartsNoReadOnceTheContextEnded(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	claimed := &claims{holder: "ses_holder"}
	resolution, err := roles.ResolveHolder(ctx, claimed, sessions{}, "reviewer")
	readErr, ok := errors.AsType[*roles.ReadError](err)
	if !ok || readErr.Step != roles.ClaimRead || !errors.Is(err, context.Canceled) ||
		err.Error() != "resolve role reviewer's holder: context canceled" {
		t.Fatalf("resolve under a context that ended returned %+v, %v; want the context's error naming the role", resolution, err)
	}
	if claimed.reads != 0 {
		t.Fatalf("the claim was read %d times, want not at all", claimed.reads)
	}
}
