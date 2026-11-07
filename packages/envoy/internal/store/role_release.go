// The release of a session's role claims: who a release is authorised by
// (RoleReleaseAuthority), which claims it may free, the conditional delete that frees one, the
// mark it leaves on a claim it may not free (RoleClaim.HandedBack), and the two sweeps a
// deregistration releases through. It is a concern of its own, split from roles.go the way
// role_renewal.go, interest_writes.go and reaper.go are: nothing here takes a role or grants
// one, and every decision here is read off one claim record and the authority the caller
// stated - never off the session bucket, because a session id names no process and a release
// that cannot say which process it is must free nothing the live holder's process holds.

package store

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"time"

	"github.com/nats-io/nats.go"
	"github.com/sjawhar/envoy/internal/bus"
	"github.com/sjawhar/envoy/internal/contracts"
)

// RoleReleaseAuthority is how a release of a session's role claims is authorised: the process
// that asked for it, or an administrator's force. Registry.Remove takes one, stated by its
// caller rather than inferred from the rest of the request, because the two routes that reach
// Remove are authorised differently and nothing in the shape of a request says which: POST
// /v1/interests/unsubscribe is a client handing back the roles its own process holds, and DELETE
// /v1/interests/<session_id> is an operator removing a session's interests whoever holds them.
//
// Against a claim that names the process holding it, the zero value frees nothing, and that is
// the point of the type: a session id names no process, so a release that cannot say which
// process it is looks exactly like the release a second process that resumed a live session's
// id would send, and the live holder's claim is what that release would take.
type RoleReleaseAuthority struct {
	// ClientInstanceID is the client instance of the process asking for the release: it frees
	// the claims naming that same process (RoleClaim.ClientInstanceID), and no claim another
	// process holds - an absent instance is no match on either side, where
	// session.InstanceMatches, written for a soft claim's predecessor, admits one.
	ClientInstanceID string
	// Force frees every claim the session holds, whoever holds it. It is the administrator's
	// release, and DELETE /v1/interests/<session_id> is its one caller: an operator clearing a
	// session's interests states no process and must still be able to free the roles that
	// session took.
	Force bool
}

// frees reports whether this release may delete claim, the stored claim of a role the release
// names, from the session sessionID. The claim must be that session's, and then one of three
// things must hold: the release forces it, it names the very process the claim names, or the
// claim names no process at all.
//
// That last arm is the carve-out for a claim written before claims named a process. Such a
// claim keeps the rule it was written under - a session's own deregistration releases it - so
// that every claim standing at the deploy is still freed by its holder's clean stop, rather than
// waiting out the role claim reaper. It closes nothing that was open: a second process that
// resumed the session's id could already free that claim before this field existed, and the
// first registration of the holder's own process adopts the claim and names it
// (Registry.renewRoleClaim), after which only that process frees it.
func (a RoleReleaseAuthority) frees(claim RoleClaim, sessionID string) bool {
	if claim.HolderSessionID != sessionID {
		return false
	}
	return a.Force || claim.ClientInstanceID == "" || claim.HeldByProcess(a.ClientInstanceID)
}

// releaseRoleClaims releases the claim of each role topic in topics that release authorises
// (RoleReleaseAuthority.frees), and leaves every other topic alone: a topic that is not a role
// lane's, and a role name a subscription wildcard reaches, which names no one role to free.
//
// It answers the lane topics it left standing: the ones whose claim still names sessionID and
// which this release did not free. Those are the topics Registry.Remove keeps in the session's
// interest record, because one record serves every process running under one session id, and a
// release from a process that does not hold the claim must not take the lane topic the process
// that does holds its deliveries by. Every other topic named is the caller's to remove - one
// that is not a role lane's, a wildcard, a role no record claims, a role another session holds,
// and a role this release did free.
//
// marks is its caller's decision that this release is a hand-back of the roles whose lane topics
// it names, so a claim it may not free is marked handed back rather than left as it stands
// (releaseReadRoleClaim). It is stated by the caller and never inferred here: whether a list of
// topics is the caller's own hand-back or the expansion of a record a deregistration read is
// something only Registry.Remove knows.
func (r *Registry) releaseRoleClaims(ctx context.Context, sessionID string, topics []string, release RoleReleaseAuthority, marks bool) ([]string, error) {
	var kept []string
	for _, topic := range topics {
		if !strings.HasPrefix(topic, contracts.RoleTopicPrefix) {
			continue
		}
		role := strings.TrimPrefix(topic, contracts.RoleTopicPrefix)
		if role == "" || strings.ContainsAny(role, "*>") {
			continue
		}
		stands, err := r.releaseRoleClaim(ctx, sessionID, role, release, marks)
		if err != nil {
			return nil, err
		}
		if stands {
			kept = append(kept, topic)
		}
	}
	return kept, nil
}

// releaseRoleClaim reads role's claim and, when release authorises freeing it from sessionID
// (RoleReleaseAuthority.frees), deletes it (releaseReadRoleClaim), the read and the delete each
// waiting no longer than ctx (bus.Bounded). A claim the release does not authorise is left
// exactly as it stands and is not an error: the session is removing its interests either way,
// and a release from another process of that session id, or one that names no process at all,
// says nothing about the claim the holder's own process still holds. Such a claim is marked
// handed back where marks says this release is a hand-back of this very role
// (releaseReadRoleClaim), which is the one thing it leaves on a claim it may not free.
//
// It answers whether the claim stands as sessionID's, which releaseReadRoleClaim decides.
//
// A claim stored under a key NATS refuses (bus.ErrRefused: too long, or outside its key
// alphabet) has a holder nothing can tell, so it is left to the role reaper, which deletes it,
// or skips one no KV call can delete. Nothing can say whose that claim is either, so it stands
// for no topic, and the one the caller named is removed from the record like any other.
func (r *Registry) releaseRoleClaim(ctx context.Context, sessionID, role string, release RoleReleaseAuthority, marks bool) (bool, error) {
	claim, err := r.RoleClaim(ctx, role)
	if errors.Is(err, bus.ErrRefused) {
		r.logger().Warn("role registry left a stored claim it cannot read to the role reaper", slog.Int("key_bytes", len(role)), slog.String("error", err.Error()))
		return false, nil
	}
	if err != nil {
		return false, err
	}
	return r.releaseReadRoleClaim(ctx, sessionID, role, claim, release, marks)
}

// roleClaimReleaseAttempts is how many conditional writes one release of one claim sends before
// it leaves the claim as it stands: the first, and two more over a claim its own holder renewed
// under the one before. It bounds the release's deletes (releaseReadRoleClaim) and the writes of
// the hand-back mark (markRoleClaimHandedBack) alike, since each meets the same race against the
// holder's own renewal. Two further attempts are chosen because each renewal that costs an
// attempt is one registration of the holder, which a client sends once a heartbeat interval, so
// three writes cover a hand-back racing far more registrations than one request's budget can
// ever meet; and the attempts must be bounded at all because the loop runs inside an HTTP
// handler, so a process registering in a tight loop would otherwise hold the release - and the
// deregistration behind it - for as long as it kept writing.
const roleClaimReleaseAttempts = 3

// releaseReadRoleClaim deletes the claim a read of role has just returned, as RoleClaim decoded
// it, when release authorises freeing it from sessionID (RoleReleaseAuthority.frees), so a
// caller that has read the claim already releases it without reading it again. A zero revision
// is a role no record claims: RoleClaim stamps on every claim the revision it was read at, and
// the bucket's revisions start at one, so only the zero claim it returns for a key the bucket
// does not hold carries none. Such a role, a claim naming another holder, and a claim this
// release does not authorise are none of them freed here, and none of the three is an error:
// the claim left standing is not this release's to delete.
//
// It answers whether the claim stands as sessionID's: the role's claim still names that session
// and this release did not free it. That is what keeps the role's lane topic in the session's
// interest record (releaseRoleClaims, Registry.Remove). A role this release freed, a role no
// record claims and a claim another session holds each answer false, and so does every failure.
//
// A claim this release may not free is marked handed back instead, where marks says this release
// is its caller's own hand-back of this very role (markRoleClaimHandedBack, which states the
// claims it marks). Nothing else of the record changes, and a release that is not a hand-back of
// this role (marks false) leaves the claim untouched.
//
// The delete is conditional on the revision read and waits no longer than ctx (bus.Bounded). A
// delete ctx abandoned may still land after this answer, and that is safe: it deletes only the
// revision it read, the claim this release authorised, and a claim written since is kept.
//
// A delete that lost that condition (nats.ErrKeyExists) met a record written since the read, and
// the two writers it could have met are told apart by reading the claim again rather than by
// answering success to both: the release goes on while it may still free the record the re-read
// returned (RoleReleaseAuthority.frees), sending its delete again at the revision that write
// left, up to roleClaimReleaseAttempts deletes in all. Two writers read that way. One is the
// holder's own renewal, which moves RenewedAt and nothing a release weighs
// (Registry.renewRoleClaim); that is the race a client hands a role back in, since pi-envoy
// registers outside its task queue while a queued task unsubscribes the role's topic, so the
// registration's renewal and the hand-back's delete overlap, and a lost delete answered as
// success left the role with the process that gave it up. The other is a claim that named no
// process adopted by the very process this release is made for, which the release may free
// exactly as it could free the claim that named none. Weighing the re-read against the claim
// first read instead would lose that one: the record now names a process where it named none,
// so the release stopped there and answered success over a claim still standing. The
// administrator's force meets the same case.
//
// A re-read this release may no longer free is the claim another process holds now. It is not
// success: it is the claim the first read met, under a process now named, so it is handed to
// the mark exactly as the first read is - marked handed back where marks says this release is
// its caller's own hand-back of this role, and otherwise left as it stands. That is the one
// path by which a hand-back whose delete lost to an adoption still has its effect. A re-read
// that finds no record is a role already free and is no error: the bucket answered
// nats.ErrKeyNotFound, which RoleClaim reads as a claim at a zero revision.
//
// A claim renewed under every attempt is left standing and fails the release, naming the role:
// the caller asked to free a claim this release authorises and it is still held, and a release
// that could not free it must not report that it did. A read of the claim that did not answer
// fails it the same way, since the release cannot tell which record it met.
func (r *Registry) releaseReadRoleClaim(ctx context.Context, sessionID, role string, claim RoleClaim, release RoleReleaseAuthority, marks bool) (bool, error) {
	for attempt := 1; ; attempt++ {
		if claim.Revision == 0 {
			return false, nil
		}
		if !release.frees(claim, sessionID) {
			stands := claim.HolderSessionID == sessionID
			if !marks {
				return stands, nil
			}
			return stands, r.markRoleClaimHandedBack(ctx, sessionID, role, claim)
		}
		epoch := r.roleViewEpoch()
		roles := r.roles()
		revision := claim.Revision
		_, answered, err := bus.Bounded(ctx, func() (struct{}, error) {
			return struct{}{}, roles.Delete(role, nats.LastRevision(revision))
		})
		switch {
		case !answered:
			return false, fmt.Errorf("release role %s's claim held by %s: %w", role, sessionID, err)
		case err == nil:
			r.forgetRoleHolder(role, revision, epoch)
			return false, nil
		case errors.Is(err, nats.ErrKeyNotFound):
			return false, nil
		case !errors.Is(err, nats.ErrKeyExists):
			return false, err
		case attempt == roleClaimReleaseAttempts:
			return false, fmt.Errorf("release role %s's claim held by %s: the claim was renewed under each of this release's %d conditional deletes and is left standing", role, sessionID, roleClaimReleaseAttempts)
		}
		if claim, err = r.RoleClaim(ctx, role); err != nil {
			return false, fmt.Errorf("re-read role %s's claim held by %s: %w", role, sessionID, err)
		}
	}
}

// markRoleClaimHandedBack writes claim back with RoleClaim.HandedBack set, the mark a hand-back
// leaves on the claim it was not authorised to free, so that no registration renews it again
// (Registry.renewRoleClaim) and the role claim reaper frees the role one live window later. Its
// one caller is releaseReadRoleClaim, over a release its caller stated to be a hand-back of this
// very role; claim is that release's own read of the record.
//
// It marks only a claim of sessionID that names a process and is not marked already: a claim of
// another session is no business of this release, a claim naming no process was freed outright
// by the release itself (RoleReleaseAuthority.frees), and a claim already marked stands as it is.
// None of the three is an error, and neither is a role no record claims.
//
// The write is conditional on the revision the claim was read at, as the release's delete is, and
// a lost compare-and-swap (nats.ErrKeyExists) is read the same way: the claim is read again and
// the mark written at the revision that record stands at, up to roleClaimReleaseAttempts writes
// in all, while it is still the same session's claim held by the same process and still unmarked.
// That race is the holder's own renewal, which moves RenewedAt and nothing weighed here. A claim
// that moved to another session or another process is another claim and is left alone, and a
// record gone (nats.ErrKeyNotFound, or a re-read at a zero revision) is a role already free;
// neither is an error.
//
// A mark that could not be written under any attempt, and a read or write that did not answer,
// fail the release naming the role, exactly as an exhausted delete does: the caller asked for a
// hand-back, and a release that left no mark must not report that it made one.
func (r *Registry) markRoleClaimHandedBack(ctx context.Context, sessionID, role string, claim RoleClaim) error {
	for attempt := 1; ; attempt++ {
		if claim.Revision == 0 || claim.HolderSessionID != sessionID || claim.ClientInstanceID == "" || claim.HandedBack {
			return nil
		}
		marked := claim
		marked.HandedBack = true
		data, err := json.Marshal(marked)
		if err != nil {
			return err
		}
		epoch := r.roleViewEpoch()
		roles := r.roles()
		read := claim.Revision
		revision, answered, err := bus.Bounded(ctx, func() (uint64, error) {
			return roles.Update(role, data, read)
		})
		switch {
		case !answered:
			return fmt.Errorf("mark role %s's claim held by %s handed back: %w", role, sessionID, err)
		case err == nil:
			r.setOwnRoleHolder(role, sessionID, revision, epoch)
			return nil
		case errors.Is(err, nats.ErrKeyNotFound):
			return nil
		case !errors.Is(err, nats.ErrKeyExists):
			return fmt.Errorf("mark role %s's claim held by %s handed back: %w", role, sessionID, err)
		case attempt == roleClaimReleaseAttempts:
			return fmt.Errorf("mark role %s's claim held by %s handed back: the claim was written again under each of this release's %d conditional writes and is left unmarked", role, sessionID, roleClaimReleaseAttempts)
		}
		stood := claim
		if claim, err = r.RoleClaim(ctx, role); err != nil {
			return fmt.Errorf("re-read role %s's claim held by %s: %w", role, sessionID, err)
		}
		if claim.ClientInstanceID != stood.ClientInstanceID {
			return nil
		}
	}
}

// releaseReadExpiredRoleClaim drops role, over a claim the caller has read already and found
// expired, once a restored claim has exhausted sessionTTL. A listener restart therefore gives an
// existing holder one sessionTTL to register again, while an already-expired holder cannot block
// a new claimant forever.
//
// The claim's own renewal is weighed by the caller and not here: the one caller
// (ReapRoleClaims) skips a claim its holder still answers for before it reaches this call, over
// this very claim value, so a claim that gets here is one no renewal keeps and no later instant
// can make renewed again. sessionTTL is the other grant, and it is this call's own: the restart
// grace, measured from this registry's own start and given only to a claim this registry
// restored at that start. A sessionTTL of zero grants no restart grace.
//
// The holder it releases from is the one the claim names: the caller found that holder gone by
// reading this very claim (ReapRoleClaims), so a holder named beside the claim could only be
// that same session, or one the caller never found gone.
//
// Its result distinguishes a confirmed deletion from a concurrent removal or replacement, so
// callers never report that a lost conditional delete released a claim. A failure is reported
// retained, its error naming the release. A zero revision is a role no record claims, as it is
// for releaseReadRoleClaim, and reports the claim missing.
//
// It makes no read of its own, so the reaper's pass, which found the holder gone in the claim
// its scan carried, spends one bucket call on an expired claim, the conditional delete
// (ReapRoleClaims). That delete waits no longer than ctx (bus.Bounded), the pass's loop context
// there.
//
// The delete is conditional on the revision the caller's read returned, so a holder that claimed
// the role again between that read and this delete keeps the role and is reported
// ExpiredRoleClaimSuperseded. A delete ctx abandoned may still land after this answer. That is
// safe to leave running: it deletes the claim only if the bucket still holds the very revision
// that read returned, naming the holder the caller has just found gone from the session
// bucket, so at worst a lapsed holder's claim is released a moment after the caller was told the
// bus did not answer, and a fresh claim that landed meanwhile is never touched.
func (r *Registry) releaseReadExpiredRoleClaim(ctx context.Context, role string, claim RoleClaim, sessionTTL time.Duration) (ExpiredRoleClaimRelease, error) {
	if claim.Revision == 0 {
		return ExpiredRoleClaimMissing, nil
	}
	r.mu.RLock()
	restoredRevision, restored := r.restoredRoleRevisions[role]
	r.mu.RUnlock()
	if restored && restoredRevision == claim.Revision && sessionTTL > 0 && r.now().Sub(r.openedAt) < sessionTTL {
		return ExpiredRoleClaimRetained, nil
	}
	epoch := r.roleViewEpoch()
	roles := r.roles()
	_, _, err := bus.Bounded(ctx, func() (struct{}, error) {
		return struct{}{}, roles.Delete(role, nats.LastRevision(claim.Revision))
	})
	switch {
	case err == nil:
		r.forgetRoleHolder(role, claim.Revision, epoch)
		return ExpiredRoleClaimReleased, nil
	case errors.Is(err, nats.ErrKeyNotFound):
		return ExpiredRoleClaimMissing, nil
	case errors.Is(err, nats.ErrKeyExists):
		return ExpiredRoleClaimSuperseded, nil
	default:
		return ExpiredRoleClaimRetained, fmt.Errorf("release role %s's expired claim held by %s: %w", role, claim.HolderSessionID, err)
	}
}

// releaseAllRoleClaims releases every claim the role claim view names sessionID the holder of
// that release authorises freeing (RoleReleaseAuthority.frees), in the order RolesHeldBy sorts
// them, and leaves a claim the release does not authorise exactly as it stands: a release that
// authorises none still reads the session's roles and frees none of them. The view is where the
// roles come from because it answers from the watcher's copy and makes no bucket call
// (RolesHeldBy), so a deregistration costs the roles its session holds rather than the bucket's
// size: reading them from a list of every key of the bucket instead is one scan of the whole
// bucket per deregistration, which is what answered the route 503. A role the session holds
// whose topic its own interest record has lost is released here all the same, because the view
// names the holder of a role rather than the topics the record kept. It marks no claim handed
// back: a deregistration names no role's lane topic and so hands no one role back, it is the
// whole of the session's interests, and a session that goes on running after it holds its
// unfreed claims exactly as it did before.
//
// It is the second of the two sources a deregistration releases from. Remove runs the first, the
// pass over the role topics of the session's own interest record (releaseRoleClaims), before it,
// and every release that pass makes leaves the view (forgetRoleHolder), so the sweep that follows
// reads no claim twice. A claim in neither source - one another listener wrote that this
// listener's watch has not delivered, or a copy up to the stale bound behind the bucket - is left
// to the role claim reaper (ReapRoleClaims), which releases it once its holder is gone; a session
// that deregisters and goes on heartbeating (an unsubscribe-all) never becomes a gone holder, so
// a claim missed here stands for the rest of that session's life, where the scan of the whole
// bucket this view replaced released it at the deregistration itself.
//
// A view that cannot be served (ErrRoleViewNotReady) names no roles at all, which is not the
// answer that the session holds none, so a deregistration it refuses falls back to
// releaseAllRoleClaimsByScan, the scan of the bucket itself, which releases each claim from the
// record that scan delivered rather than reading it again, and says there what that costs.
func (r *Registry) releaseAllRoleClaims(ctx context.Context, sessionID string, release RoleReleaseAuthority) error {
	roles, err := r.RolesHeldBy(sessionID)
	// RolesHeldBy refuses only with ErrRoleViewNotReady - RoleViewErr's error is the one error it
	// has - and the scan is the answer to any refusal, so no refusal releases nothing silently.
	if err != nil {
		return r.releaseAllRoleClaimsByScan(ctx, sessionID, err, release)
	}
	for _, role := range roles {
		if _, err := r.releaseRoleClaim(ctx, sessionID, role, release, false); err != nil {
			return err
		}
	}
	return nil
}

// releaseAllRoleClaimsByScan releases every claim sessionID holds that the role bucket holds a
// record for, read from one scan of the bucket. It is releaseAllRoleClaims' fallback, so it runs
// only while the role claim view cannot say which roles the session holds, and it logs one INFO
// line naming the session and carrying viewErr, the refusal RolesHeldBy answered that caller
// with, each time it does: a listener whose view is not serving then says so on every
// deregistration that pays for the scan, rather than only in /healthz. The refusal is carried in
// rather than read from the view again here, because a view that became servable in between
// would leave the line with no reason at all for a scan this deregistration is paying for.
//
// The scan itself runs under bus.Bounded over roleClaimsUntil, so the fallback waits no longer
// than the caller's context and the listing it walks away from ends there too: an unbounded scan
// inside an HTTP handler is what holds a deregistration past the route's budget, and one left
// running past it reads the bucket for a caller that is gone.
//
// Every claim comes from that one scan, which carries each record's value and revision, so the
// sweep sends nothing per key but the deletes it owes. A claim stored under a key NATS refuses
// (bus.KeyValue.RefusesRead, the refusal a read of it would answer before sending) has a holder
// nothing can tell, so it is left to the role reaper and the sweep goes on, as releaseRoleClaim
// leaves it; so is a record this build cannot decode. A claim the scan did read and the release
// authorises freeing (RoleReleaseAuthority.frees) is the session's to free, and a delete that
// fails leaves a claim the session holds unreleased with nothing else in this deregistration to
// release it, so that error fails the sweep; a claim the release does not authorise is left
// standing, unmarked and no error, as the deregistration this runs for marks nothing
// (releaseAllRoleClaims). Those two - a listing that did not answer or failed, and a delete of
// a claim the session holds - are what the sweep fails on, so a spent budget reaches the route
// as the listing's wording or the delete's.
//
// What it leaves behind it counts rather than logs per claim (claimsLeft). This runs only while
// the role claim view cannot be served, which is itself the outage, and it runs once per
// deregistration: a bucket holding thousands of such claims, met by a fleet deregistering under
// that same outage, would write thousands of WARNs per deregistration and amplify what they
// report. Each class is counted inside the loop and written after it in one line, logged only
// when its own count is not zero, which carries what an operator needs of it - which
// deregistration met these claims (session_id), how many it left (claims), which one it met first
// and the error that left it. The lines are written however the sweep ends, a failed delete
// included, which is why they are deferred: the claims left behind are the ones the sweep met,
// whether or not a release behind them failed.
//
// The two classes keep a line each rather than sharing one line with both counts, because they
// are different facts with different remedies: a key no call of any build can read
// (bus.ErrRefused), which the role reaper deletes, or an operator purges by hand when no call can
// delete it either, and a record no build can decode (ErrMalformedRoleClaim), which stays in the
// bucket until something rewrites it and which the role reaper does no more than count skipped. A
// line each also means neither is written for a deregistration that met none of its class, where
// one shared line would be written for either and read as both. The refused key is named by its
// size (key_bytes), as the per-claim line it replaces named it: a key is refused for being longer
// than the bucket allows or for being outside its alphabet, and its size tells those apart. The
// error carries the key's text when the key was refused for its characters rather than its
// length, as the per-claim line's did. releaseRoleClaim writes that same
// refusal over the release of one named role, and an operator must tell the two lines apart: this
// one names no role, and carries the session whose deregistration met these claims and how many
// of them there were.
//
// Reading from the scan departs from the per-key reads in one way, and only one: a claim
// sessionID wins after the scan has delivered that key is seen at the value the scan carries and
// left standing, where a read after the listing would have found it and released it. That moves
// the exposure earlier rather than narrowing it: a per-key read fixed the key's value at its own
// Get, which came after the whole listing had finished, so every claim won while the listing ran
// was still caught, and the scan fixes it at that key's delivery instead, so the interval runs
// from there to the end of the sweep and is logically the wider of the two. In wall clock it is
// about twenty times shorter, since the whole operation fell from a 503 at 3.5 s to a 204 in
// 188 ms over 20,002 claims. Nothing closes it. What stands behind leaving it open: Remove reads
// the session's interest record before this sweep (interest_writes.go), so a role the session
// held then is released from that record; a claim left here is left to the role claim reaper,
// which releases it once its holder is gone, which a session that deregisters and goes on
// heartbeating never becomes. The other direction stays safe either way, because the delete
// releaseReadRoleClaim sends is conditional on the revision the scan read: a claim another
// session wins after the scan is kept, not deleted.
func (r *Registry) releaseAllRoleClaimsByScan(ctx context.Context, sessionID string, viewErr error, release RoleReleaseAuthority) error {
	r.logger().Info("role registry fell back to a scan of the role bucket to release a session's claims; its claim view cannot be served",
		slog.String("session_id", sessionID),
		slog.String("reason", viewErr.Error()),
	)
	// report is a method value, so each line is written from what its counter holds when the
	// defer runs rather than from what it holds here; the defers run in reverse, which leaves the
	// refused key's line written first, as the one defer that wrote both left it.
	refusedKeys := claimsLeft{
		message: "role registry left stored claims it cannot read to the role reaper while releasing a session's claims; key_bytes and error are the first claim's",
		example: func(key string) slog.Attr { return slog.Int("key_bytes", len(key)) },
	}
	undecodable := claimsLeft{
		message: "role registry could not read stored claims while releasing a session's claims; they stay in the bucket, unreleased by this deregistration, and role and error are the first claim's",
		example: func(role string) slog.Attr { return slog.String("role", role) },
	}
	defer undecodable.report(r.logger(), sessionID)
	defer refusedKeys.report(r.logger(), sessionID)
	claims, answered, err := bus.Bounded(ctx, func() ([]scannedRoleClaim, error) { return r.roleClaimsUntil(ctx) })
	if !answered || err != nil {
		return fmt.Errorf("list the role bucket's keys to release %s's claims: %w", sessionID, err)
	}
	for _, scanned := range claims {
		role := scanned.Role
		if err := r.roles().RefusesRead(role); err != nil {
			refusedKeys.met(role, err)
			continue
		}
		if scanned.Err != nil {
			undecodable.met(role, scanned.Err)
			continue
		}
		if _, err := r.releaseReadRoleClaim(ctx, sessionID, role, scanned.Claim, release, false); err != nil {
			return err
		}
	}
	return nil
}

// claimsLeft counts the claims one run of releaseAllRoleClaimsByScan left in the bucket for one
// reason, keeps the first of them - the role it is stored under and the error that left it - and
// writes that reason's summary line (report), so a bucket of thousands of such claims costs one
// line and not one per claim - which matters because the sweep runs only while the bus is
// degraded, once per deregistration. message is the line it writes and example names the first
// claim met in it; the two reasons differ in those two fields and in nothing else.
type claimsLeft struct {
	message string
	example func(role string) slog.Attr

	count int
	role  string
	err   error
}

// met counts role's claim, left by err, keeping the first role and error met.
func (c *claimsLeft) met(role string, err error) {
	c.count++
	if c.count == 1 {
		c.role, c.err = role, err
	}
}

// report writes the one line for the claims counted, naming the deregistration that met them
// (sessionID), and writes nothing at all for a reason this run met none of.
func (c *claimsLeft) report(log *slog.Logger, sessionID string) {
	if c.count == 0 {
		return
	}
	log.Warn(c.message,
		slog.String("session_id", sessionID),
		slog.Int("claims", c.count),
		c.example(c.role),
		slog.String("error", c.err.Error()),
	)
}
