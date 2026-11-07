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
	"time"

	"github.com/nats-io/nats.go"
	"github.com/sjawhar/envoy/internal/bus"
)

// RenewRoleClaims stamps the moment clientInstanceID's process last answered on every claim
// sessionID holds that names that process (RoleClaim.RenewedAt), which is what makes a claim's
// renewal its holder's heartbeat rather than the instant the role was taken: the listener calls
// it from POST /v1/interests/subscribe, the registration every client repeats while it runs. The
// roles it renews are the ones the role claim watcher's copy names (RolesHeldBy), so the set
// costs no bucket scan, and each renewal reads and writes that role's own record.
//
// A registration that states no process (an empty clientInstanceID) renews the claims of its own
// session that name no process, and adopts none: an absent instance names no process, so it
// matches no claim that names one, and a claim naming none is adopted by the process that
// registers and never by the one that cannot say which it is. That arm is what keeps every claim
// standing at a deploy renewed: such a claim names no process until a registration that states
// one adopts it, and a client built before registrations carried an instance renews it meanwhile.
//
// A hand-back is read off the claim and not out of this registration. A release that hands one
// role back and may not free it marks the claim instead (RoleClaim.HandedBack, written by
// Registry.Remove's release path), and renewRoleClaim renews no marked claim, so the role claim
// reaper frees the role one live window later. That replaces the lane-topic condition this call
// once carried, which renewed a role only while the registering session's interest record still
// named that role's lane topic: the mark says what the hand-back did, so the renewal no longer
// has to read a hand-back out of a list of topics, a registration carrying no topic list is no
// longer a case of its own, and a holder that still holds its role is renewed whatever topics it
// registers.
//
// Every failure is returned, not raised: a view that cannot answer (ErrRoleViewNotReady, wrapped)
// after a listener restart, a read or write that did not answer, a record this build cannot read.
// Each role is attempted whatever the roles before it did, and the errors are joined, so one
// unreadable claim does not cost the session its other renewals. The caller is a heartbeat and
// must not fail over any of them (subscribeHandler logs and goes on): a registration refused
// because the claim view is still refilling would take every session's heartbeat down with it.
//
// Every failure is also recorded against sessionID as the moment this listener could not answer
// that session's renewal (recordRenewalRefusal, Registry.renewalRefusals), because a registration
// this call refuses is one the holder sent and nothing moved: the view that cannot be served
// names no roles at all, so no claim of that session is renewed while it refuses, and a reaper
// reading the claim's own renewal would free a live holder's role for a failure of the
// listener's. The role claim reaper keeps that session's claims for one live window past each
// refusal (ReapRoleClaims), the same window it keeps them for past each renewal.
//
// Each bucket call waits no longer than ctx (bus.Bounded): the route passes its request's own
// context. A write ctx abandoned may still land after this answer, which is safe - it is
// conditional on the revision it read and renews that claim alone.
func (r *Registry) RenewRoleClaims(ctx context.Context, sessionID, clientInstanceID string) error {
	roles, err := r.RolesHeldBy(sessionID)
	if err != nil {
		r.recordRenewalRefusal(sessionID)
		return fmt.Errorf("renew %s's role claims: %w", sessionID, err)
	}
	var failures []error
	for _, role := range roles {
		if err := r.renewRoleClaim(ctx, role, sessionID, clientInstanceID); err != nil {
			failures = append(failures, err)
		}
	}
	if len(failures) == 0 {
		return nil
	}
	r.recordRenewalRefusal(sessionID)
	return errors.Join(failures...)
}

// recordRenewalRefusal stamps this registry's clock against sessionID as the moment a
// registration of that session reached this listener and left with no renewal written
// (RenewRoleClaims, which calls this for every error it returns). It is the one writer of
// Registry.renewalRefusals, and the stamp replaces whatever stood there, so a session whose
// registrations keep being refused carries the latest refusal and one that stops registering
// stops moving it.
//
// The map is made here rather than at Open because a listener that answers every renewal never
// needs one, and a registry a test builds as a literal carries none.
func (r *Registry) recordRenewalRefusal(sessionID string) {
	refused := r.now()
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.renewalRefusals == nil {
		r.renewalRefusals = make(map[string]time.Time)
	}
	r.renewalRefusals[sessionID] = refused
}

// renewalRefusedWithin reports whether this listener failed to answer sessionID's renewal less
// than liveWindow ago (Registry.renewalRefusals), as of this registry's clock: the same window,
// read off the same clock, that ClaimRenewedWithin weighs a renewal that was written against, so
// a claim is kept exactly one live window past the last thing that happened to it, whether that
// was a renewal or this listener's refusal to write one.
//
// A liveWindow of zero is a caller that configured no live window and keeps nothing, as it keeps
// no renewed claim.
func (r *Registry) renewalRefusedWithin(sessionID string, liveWindow time.Duration) bool {
	if liveWindow <= 0 {
		return false
	}
	now := r.now()
	r.mu.RLock()
	defer r.mu.RUnlock()
	refused, recorded := r.renewalRefusals[sessionID]
	return recorded && now.Sub(refused) < liveWindow
}

// pruneRenewalRefusals drops every refusal that renewalRefusedWithin would no longer keep a
// claim for, which is what bounds Registry.renewalRefusals: the role claim reaper calls it once
// a pass (ReapRoleClaims), so the map holds no more than the sessions this listener refused
// inside one live window, and a session that stops registering leaves it at the first pass after
// its last refusal. A pass given no live window keeps no claim by a refusal and so keeps no
// refusal either.
func (r *Registry) pruneRenewalRefusals(liveWindow time.Duration) {
	now := r.now()
	r.mu.Lock()
	defer r.mu.Unlock()
	for sessionID, refused := range r.renewalRefusals {
		if liveWindow <= 0 || now.Sub(refused) >= liveWindow {
			delete(r.renewalRefusals, sessionID)
		}
	}
}

// renewRoleClaim writes role's claim back with its renewal moved to now, conditional on the very
// revision this call read it at, and only while the stored claim still names sessionID as its
// holder and the process it names answers to clientInstanceID. A registration that states a
// process renews the claim naming that very process (RoleClaim.HeldByProcess), and adopts one
// naming none by writing its instance in: the first registration of the session holding a claim
// written before claims named a process, after which every renewal takes the first arm. A
// registration that states no process renews the claims of its session that name none, leaving
// them naming none, and matches no claim that names one - an absent instance names no process,
// so it can neither be a holder's heartbeat nor adopt what another process holds.
//
// A claim marked handed back (RoleClaim.HandedBack) is renewed by nobody, its own process
// included: the mark is the release's word that the holder gave the role up, so the claim ages
// out and the role claim reaper frees the role one live window later. A claim naming another
// session or another process is left exactly as it stands - the renewal is the holder's own
// heartbeat, and no other process's heartbeat may move it - and so is a role with no claim
// record at all, which the read answers as a zero claim and so a zero revision (RoleClaim).
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
	if claim.Revision == 0 || claim.HolderSessionID != sessionID || claim.HandedBack {
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
