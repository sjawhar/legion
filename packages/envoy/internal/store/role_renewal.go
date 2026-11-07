// A role claim's renewal: the heartbeat that says its holder's process is still answering for
// the claim, written by the registration every client repeats while it runs (RenewRoleClaims,
// renewRoleClaim), and the one test of it every decision that turns on a renewal reads
// (Registry.ClaimRenewedWithin). It is a concern of its own, split from roles.go the way
// interest_writes.go and reaper.go are: nothing here takes a role or gives one away, and nothing
// here reads a session record - a claim's renewal is the holder's own word, and a role moves on
// the claim alone.

package store

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"slices"
	"time"

	"github.com/nats-io/nats.go"
	"github.com/sjawhar/envoy/internal/bus"
	"github.com/sjawhar/envoy/internal/contracts"
)

// RenewRoleClaims stamps the moment clientInstanceID's process last answered on every claim
// sessionID holds that names that process (RoleClaim.RenewedAt), which is what makes a claim's
// renewal its holder's heartbeat rather than the instant the role was taken: the listener calls
// it from POST /v1/interests/subscribe, the registration every client repeats while it runs. The
// roles it renews are the ones the role claim watcher's copy names (RolesHeldBy), so the set
// costs no bucket scan, and each renewal reads and writes that role's own record.
//
// topics is the registering session's interest record as that registration left it, and a role
// whose lane topic (contracts.RoleTopicPrefix + role) the record no longer names is not renewed:
// a client that hands a role back removes that topic (Registry.Remove), so the hand-back stops
// the renewal even where it freed no claim, and the role claim reaper frees the role one live
// window later. Without that condition a client whose release states no process - one built
// before a release named one - would hand a role back and go on renewing the claim for as long
// as its process ran, since the claim view names it the holder however its record reads. A claim
// taken through POST /v1/roles/set writes that lane topic onto the holder's record, and a
// registration merges its topics into the record rather than replacing them (Registry.Upsert),
// so a holder renews its claim without ever registering the lane topic itself, which no client
// does (packages/pi-envoy registers the subjects it opened, and opens none for a role lane).
//
// An empty topics is a registration that stated no topics at all, which says nothing about the
// lanes its session still follows, and it renews every claim its process holds, as a registration
// did before the condition above: the condition reads a hand-back out of a list of topics, and a
// client that sends no list has handed nothing back. Without that arm such a client would lose
// every role it holds one live window after it started, trading a bug bounded by the oldest
// client's lifetime for one that is not.
//
// A registration that states no process (an empty clientInstanceID) renews nothing: an absent
// instance names no process, so it matches no claim, and a claim naming none is adopted by the
// process that registers, never by the one that cannot say which it is.
//
// Every failure is returned, not raised: a view that cannot answer (ErrRoleViewNotReady, wrapped)
// after a listener restart, a read or write that did not answer, a record this build cannot read.
// Each role is attempted whatever the roles before it did, and the errors are joined, so one
// unreadable claim does not cost the session its other renewals. The caller is a heartbeat and
// must not fail over any of them (subscribeHandler logs and goes on): a registration refused
// because the claim view is still refilling would take every session's heartbeat down with it.
//
// Each bucket call waits no longer than ctx (bus.Bounded): the route passes its request's own
// context. A write ctx abandoned may still land after this answer, which is safe - it is
// conditional on the revision it read and renews that claim alone.
func (r *Registry) RenewRoleClaims(ctx context.Context, sessionID, clientInstanceID string, topics []string) error {
	if clientInstanceID == "" {
		return nil
	}
	roles, err := r.RolesHeldBy(sessionID)
	if err != nil {
		return fmt.Errorf("renew %s's role claims: %w", sessionID, err)
	}
	var failures []error
	for _, role := range roles {
		if len(topics) > 0 && !slices.Contains(topics, contracts.RoleTopicPrefix+role) {
			continue
		}
		if err := r.renewRoleClaim(ctx, role, sessionID, clientInstanceID); err != nil {
			failures = append(failures, err)
		}
	}
	return errors.Join(failures...)
}

// renewRoleClaim writes role's claim back with its renewal moved to now, conditional on the very
// revision this call read it at, and only while the stored claim still names sessionID as its
// holder and either names clientInstanceID as the process holding it or names no process at all.
// That second arm is the adoption of a claim written before claims named a process: the first
// registration of the session that holds it writes the holder's instance in, and every renewal
// after that takes the first arm. A claim naming another session or another process is left
// exactly as it stands - the renewal is the holder's own heartbeat, and no other process's
// heartbeat may move it - and so is a role with no claim record at all, which the read answers
// as a zero claim and so a zero revision (RoleClaim).
//
// Only RenewedAt, and on adoption ClientInstanceID, are written: HolderSessionID, ClaimedAt and
// PreviousSessionID are the claim's own provenance and are carried over untouched, so a renewal
// never re-dates a claim or rewrites who took it from whom.
//
// A lost compare-and-swap (nats.ErrKeyExists for the wrong last sequence, nats.ErrKeyNotFound for
// a claim deleted in between) means another claim landed after this call read the record. The
// renewal is dropped and logged, never read again and retried: the record is now another
// claimant's, and a renewal retried into it would stamp this process's heartbeat on a claim it
// does not hold. The next registration renews the claim that stands, if it is still this
// session's.
func (r *Registry) renewRoleClaim(ctx context.Context, role, sessionID, clientInstanceID string) error {
	claim, err := r.RoleClaim(ctx, role)
	if err != nil {
		return fmt.Errorf("renew role %s's claim held by %s: %w", role, sessionID, err)
	}
	if claim.Revision == 0 || claim.HolderSessionID != sessionID {
		return nil
	}
	if claim.ClientInstanceID != "" && !claim.HeldByProcess(clientInstanceID) {
		return nil
	}
	renewed := claim
	renewed.ClientInstanceID = clientInstanceID
	renewed.RenewedAt = r.now().UnixMilli()
	data, err := json.Marshal(renewed)
	if err != nil {
		return err
	}
	epoch := r.roleViewEpoch()
	roles := r.roles()
	revision, answered, err := bus.Bounded(ctx, func() (uint64, error) {
		return roles.Update(role, data, claim.Revision)
	})
	switch {
	case !answered:
		return fmt.Errorf("renew role %s's claim held by %s: %w", role, sessionID, err)
	case errors.Is(err, nats.ErrKeyExists), errors.Is(err, nats.ErrKeyNotFound):
		r.logger().Info("registry dropped a role claim renewal a claim landed under",
			slog.String("role", role),
			slog.String("session_id", sessionID),
			slog.String("client_instance_id", clientInstanceID),
			slog.Uint64("read_revision", claim.Revision),
		)
		return nil
	case err != nil:
		return fmt.Errorf("renew role %s's claim held by %s: %w", role, sessionID, err)
	}
	r.setOwnRoleHolder(role, sessionID, revision, epoch)
	return nil
}

// ClaimRenewedWithin reports whether claim's holder answered for it less than liveWindow ago
// (RoleClaim.LastAnsweredAt), as of this registry's clock. It is the one test of a claim's
// renewal, and every decision that turns on one reads it through this method - this registry's
// own role claim reaper (ReapRoleClaims) and POST /v1/roles/set weighing a claim held under the
// claimant's own session id - so the sweep and the route can never disagree over which claims a
// holder still renews, nor over the instant they weigh them at: there is one clock, this
// registry's, which a test moves (WithClock) and both readings then move with.
//
// A liveWindow of zero is a caller that configured no live window, and keeps nothing; so is a
// claim that carries neither a renewal nor a claim time, whose renewal is older than any window.
// A claim an earlier build wrote carries no renewal and is weighed at the moment it was claimed,
// which LastAnsweredAt says: it is renewed while that moment is inside the window, and stale
// once it is older.
func (r *Registry) ClaimRenewedWithin(claim RoleClaim, liveWindow time.Duration) bool {
	return liveWindow > 0 && r.now().Sub(time.UnixMilli(claim.LastAnsweredAt())) < liveWindow
}
