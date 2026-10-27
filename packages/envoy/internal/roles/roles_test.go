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

// claims is a ClaimStore in memory: holders read in turn on each RoleClaim (the last one repeats),
// releases answered in turn on each ReleaseExpiredRoleClaim, after onRelease when it is set.
type claims struct {
	holders   []string
	readErr   error
	releases  []store.ExpiredRoleClaimRelease
	release   error
	onRelease func()
	reads     int
	released  []string
}

func (c *claims) RoleClaim(context.Context, string) (store.RoleClaim, error) {
	if c.readErr != nil {
		return store.RoleClaim{}, c.readErr
	}
	holder := c.holders[min(c.reads, len(c.holders)-1)]
	c.reads++
	return store.RoleClaim{HolderSessionID: holder}, nil
}

func (c *claims) ReleaseExpiredRoleClaim(_ context.Context, _, sessionID string, _ time.Duration) (store.ExpiredRoleClaimRelease, error) {
	c.released = append(c.released, sessionID)
	if c.onRelease != nil {
		c.onRelease()
	}
	if c.release != nil {
		return store.ExpiredRoleClaimRetained, c.release
	}
	answer := c.releases[0]
	c.releases = c.releases[1:]
	return answer, nil
}

// sessions is a SessionLookup in memory: entries are the registered sessions, readErr fails every
// read.
type sessions struct {
	entries map[string]session.SessionEntry
	readErr error
}

func (s sessions) Holder(_ context.Context, sessionID string, live func(session.SessionEntry) bool) (session.SessionEntry, error) {
	if s.readErr != nil {
		return session.SessionEntry{}, s.readErr
	}
	entry, ok := s.entries[sessionID]
	if !ok || !live(entry) {
		return session.SessionEntry{}, nats.ErrKeyNotFound
	}
	return entry, nil
}

func (sessions) TTL() time.Duration { return time.Minute }

func anyEntry(session.SessionEntry) bool { return true }

func TestResolveHolderReadsEachState(t *testing.T) {
	live := sessions{entries: map[string]session.SessionEntry{"ses_live": {Title: "live"}}}
	for _, tc := range []struct {
		name     string
		claims   *claims
		want     roles.State
		holder   string
		release  store.ExpiredRoleClaimRelease
		released []string
	}{
		{"a live holder", &claims{holders: []string{"ses_live"}}, roles.Live, "ses_live", 0, nil},
		{"no claim", &claims{holders: []string{""}}, roles.Unclaimed, "", 0, nil},
		{"a gone holder, released", &claims{holders: []string{"ses_gone"}, releases: []store.ExpiredRoleClaimRelease{store.ExpiredRoleClaimReleased}},
			roles.Lapsed, "ses_gone", store.ExpiredRoleClaimReleased, []string{"ses_gone"}},
		{"a gone holder whose claim was already gone", &claims{holders: []string{"ses_gone"}, releases: []store.ExpiredRoleClaimRelease{store.ExpiredRoleClaimMissing}},
			roles.Lapsed, "ses_gone", store.ExpiredRoleClaimMissing, []string{"ses_gone"}},
		{"a gone holder whose claim was kept", &claims{holders: []string{"ses_gone"}, releases: []store.ExpiredRoleClaimRelease{store.ExpiredRoleClaimRetained}},
			roles.Lapsed, "ses_gone", store.ExpiredRoleClaimRetained, []string{"ses_gone"}},
		{"a gone holder superseded by a live one", &claims{holders: []string{"ses_gone", "ses_live"}, releases: []store.ExpiredRoleClaimRelease{store.ExpiredRoleClaimSuperseded}},
			roles.Live, "ses_live", 0, []string{"ses_gone"}},
		{"a claim superseded on every attempt", &claims{holders: []string{"ses_gone", "ses_other"},
			releases: []store.ExpiredRoleClaimRelease{store.ExpiredRoleClaimSuperseded, store.ExpiredRoleClaimSuperseded}},
			roles.Churned, "", 0, []string{"ses_gone", "ses_other"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			resolution, err := roles.ResolveHolder(context.Background(), tc.claims, live, "reviewer", anyEntry)
			if err != nil {
				t.Fatalf("resolve: %v", err)
			}
			if resolution.State != tc.want || resolution.Holder != tc.holder || resolution.Release != tc.release {
				t.Fatalf("resolution %+v, want state %d holder %q release %d", resolution, tc.want, tc.holder, tc.release)
			}
			if tc.want == roles.Live && resolution.Entry.Title != "live" {
				t.Fatalf("the live holder's entry is %+v", resolution.Entry)
			}
			if len(tc.claims.released) != len(tc.released) {
				t.Fatalf("released %v, want %v", tc.claims.released, tc.released)
			}
		})
	}
}

// live decides the holder: one it refuses is released as one that is gone.
func TestResolveHolderReleasesAHolderLiveRefuses(t *testing.T) {
	held := &claims{holders: []string{"ses_live"}, releases: []store.ExpiredRoleClaimRelease{store.ExpiredRoleClaimReleased}}
	registered := sessions{entries: map[string]session.SessionEntry{"ses_live": {}}}
	resolution, err := roles.ResolveHolder(context.Background(), held, registered, "reviewer", func(session.SessionEntry) bool { return false })
	if err != nil || resolution.State != roles.Lapsed || len(held.released) != 1 {
		t.Fatalf("resolution %+v, %v, released %v: want the refused holder's claim released", resolution, err, held.released)
	}
}

// A read that fails names its step, and the holder once the claim named one.
func TestResolveHolderNamesTheStepThatFailed(t *testing.T) {
	failure := errors.New("no answer")
	for _, tc := range []struct {
		name     string
		claims   *claims
		sessions sessions
		step     roles.Step
		holder   string
	}{
		{"the claim", &claims{readErr: failure}, sessions{}, roles.ClaimRead, ""},
		{"the session", &claims{holders: []string{"ses_holder"}}, sessions{readErr: failure}, roles.SessionRead, "ses_holder"},
		{"the release", &claims{holders: []string{"ses_gone"}, release: failure}, sessions{}, roles.Release, "ses_gone"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			resolution, err := roles.ResolveHolder(context.Background(), tc.claims, tc.sessions, "reviewer", anyEntry)
			readErr, ok := errors.AsType[*roles.ReadError](err)
			if !ok || readErr.Step != tc.step || readErr.Holder != tc.holder || !errors.Is(err, failure) || resolution.Holder != tc.holder {
				t.Fatalf("resolve returned %+v, %v; want a read error at %s about %q wrapping the failure", resolution, err, tc.step, tc.holder)
			}
		})
	}
}

// No attempt starts once the context has ended: a release that loses to a fresh claim while the
// context ends reads the claim no second time, and the answer is the context's error, naming the
// role, rather than a read error or a resolution.
func TestResolveHolderStartsNoAttemptOnceTheContextEnded(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	raced := &claims{holders: []string{"ses_gone", "ses_live"}, releases: []store.ExpiredRoleClaimRelease{store.ExpiredRoleClaimSuperseded}, onRelease: cancel}
	live := sessions{entries: map[string]session.SessionEntry{"ses_live": {}}}
	resolution, err := roles.ResolveHolder(ctx, raced, live, "reviewer", anyEntry)
	if _, isReadErr := errors.AsType[*roles.ReadError](err); isReadErr || !errors.Is(err, context.Canceled) || err.Error() != "resolve role reviewer's holder: context canceled" {
		t.Fatalf("resolve under a context that ended returned %+v, %v; want the context's error naming the role", resolution, err)
	}
	if raced.reads != 1 {
		t.Fatalf("the claim was read %d times, want once: an attempt started after the context ended", raced.reads)
	}
}
