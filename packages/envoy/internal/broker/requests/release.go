// packages/envoy/internal/broker/requests/release.go
// The one release loop of a grant, which both of its releases run: the session's own (Values,
// machine_read.go) and the proxy's (AirlockValues, airlock.go). It reads or mints each granted
// name, splits the names the airlock holds back into their slots, and writes the one grant.used
// row the release writes, with the detail of whichever caller wrote it. One loop rather than
// two, because a fix to the split, to that row, or to what a narrowing may reach must not land
// in one release and not in the other.
package requests

import (
	"context"
	"fmt"
	"time"

	"github.com/sjawhar/envoy/internal/broker/secrets"
)

// releaseRead is what one caller wants of the shared read. The session's release asks for every
// granted name and may narrow; the proxy's asks for the names behind the airlock alone, since
// the session was handed the rest at its own release.
type releaseRead struct {
	// narrowing is this release's GitHub App narrowing, nil for a release that asks for none.
	// It reaches the github-app minter of every github-app name the grant holds.
	narrowing *secrets.GitHubNarrowing
	// airlockOnly leaves every name the airlock does not hold back out of the read: no value of
	// it is read, and none is minted.
	airlockOnly bool
}

// releasedName is one granted name as a release read it.
type releasedName struct {
	name string
	// value is what the store or the minter answered. It is what the session is handed for a
	// name the airlock does not hold back, and goes no further than the loop for one it does,
	// which the session holds placeholders for instead.
	value string
	// split is how that value takes apart into slots, for a name the airlock holds back: the
	// real value behind each slot, which the proxy swaps into a request, and how the shape the
	// session holds is filled with those slots' placeholders. airlockSplitOf always answers a
	// map, so a nil one is what says the airlock does not hold this name back (behindAirlock).
	split airlockSplit
	// expires is when a minted value stops working, zero for a stored one, and kind the policy
	// kind that says what the slots are. The proxy holds both beside the values: the first is
	// what it mints again before, the second what says how to spend them.
	expires time.Time
	kind    string
}

// behindAirlock reports whether the airlock's rules hold this name back, which is what says
// whether its split was taken.
func (r releasedName) behindAirlock() bool { return r.split.values != nil }

// readRelease reads every name checked found granted — or, for the proxy, the names behind the
// airlock alone — in the grant's own name order, and takes the split of each name the airlock
// holds back. Once every value is read, each stored one is checked against the policy as it
// stands then (recheckStoredSources), so a retag that lands during the reads never releases
// what the secret stores once it is a kind.
func (m *Machine) readRelease(ctx context.Context, grantID string, checked grantRelease, want releaseRead) ([]releasedName, error) {
	read := make([]releasedName, 0, len(checked.granted))
	for _, g := range checked.granted {
		entry := m.Airlock.Lookup(g.name)
		if entry == nil && want.airlockOnly {
			continue
		}
		served := checked.set.Secrets[g.name]
		minted, err := m.readValue(ctx, grantID, checked.enrollmentID, g, served, want.narrowing)
		if err != nil {
			return nil, err
		}
		released := releasedName{name: g.name, value: minted.Value, expires: minted.Expires, kind: served.Kind()}
		if entry != nil {
			if released.split, err = airlockSplitOf(entry, released.kind, minted.Value); err != nil {
				return nil, fmt.Errorf("release %s behind the airlock: %w", g.name, err)
			}
		}
		read = append(read, released)
	}
	if err := m.recheckStoredSources(ctx, grantID, checked.granted); err != nil {
		return nil, err
	}
	return read, nil
}

// releasedNames is every name a read answered, in the grant's own name order: what the
// grant.used row records as having left the broker.
func releasedNames(read []releasedName) []string {
	names := make([]string, len(read))
	for i, released := range read {
		names[i] = released.name
	}
	return names
}

// auditGrantUsed writes the one grant.used row a release writes. detail is the caller's own: the
// names it released and, for the session's release, the narrowing it applied; for the proxy's,
// that the values went behind the airlock. The row says the values did leave the broker, so each
// caller writes it last, once everything that release hands over is minted and recorded.
func (m *Machine) auditGrantUsed(ctx context.Context, grantID string, checked grantRelease, detail auditDetail) error {
	_, err := m.Store.Pool.Exec(ctx, `insert into audit (kind, enrollment_id, request_id, grant_id, actor, detail) values ('grant.used',$1,$2,$3,$4,$5)`,
		checked.enrollmentID, checked.requestID, grantID, "session:"+checked.enrollmentID, detail)
	return err
}
