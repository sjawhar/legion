package store

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"maps"
	"slices"
	"sort"
	"strings"
	"time"

	"github.com/nats-io/nats.go"
	"github.com/nats-io/nats.go/jetstream"
	"github.com/sjawhar/envoy/internal/bus"
	"github.com/sjawhar/envoy/internal/contracts"
	"github.com/sjawhar/envoy/internal/kvwatch"
)

// RoleClaim is the durable ownership record for a role lane. Role claims are
// independent of interest records: a listener restart can temporarily age an
// interest out before its still-running holder re-registers.
type RoleClaim struct {
	HolderSessionID   string `json:"holder_session_id"`
	ClaimedAt         int64  `json:"claimed_at"`
	PreviousSessionID string `json:"previous_session_id"`
	// Revision is the role bucket revision the claim was read at, which RoleClaim stamps on
	// every claim it decodes and nothing stores: it belongs to the entry, not to the record,
	// so it is never marshalled back into the bucket. Zero means no claim record was read -
	// an unclaimed role, a claim built to be written, or the holder-only claim a watched copy
	// of the bucket gives (roles.ResolveHolderFrom's firstHolder). It is what a conditional
	// claim names as its precondition (SetRoleWithPrevious's ifClaimRevision) and what GET
	// /v1/roles/<role> reports as claim_revision, so a caller can read a role and then claim
	// it only while no other claim has landed.
	Revision uint64 `json:"-"`
}

// ExpiredRoleClaimRelease reports the conditional-delete outcome for an
// expired role claim.
type ExpiredRoleClaimRelease uint8

const (
	ExpiredRoleClaimReleased ExpiredRoleClaimRelease = iota
	ExpiredRoleClaimMissing
	ExpiredRoleClaimSuperseded
	ExpiredRoleClaimRetained
)

// roleRevisions reads the revision of every role claim kv holds, for the grace a restored claim's
// holder gets to register again. It takes them from one scan over the bucket's existing keys
// (kvwatch.ScanExistingKeys), the way the interest, session and CI caches read theirs, so readiness
// costs one pass over the bucket rather than a round trip per stored claim. A claim whose key this
// build cannot read gets a revision here; it still gets no grace, because the grace is read by
// releaseReadExpiredRoleClaim, the only reader of these revisions, over a claim its caller has
// read from the bucket, and no caller can read that one.
//
// A snapshot short of the bucket is missing claims that are in it, and each one it misses is a
// restored holder that loses its grace and is released a session TTL early, so an incomplete scan
// fails the caller, and only a complete one logs. A scan that delivers nothing for idle is one.
func roleRevisions(kv bus.KeyValue, idle time.Duration, log *slog.Logger) (map[string]uint64, error) {
	revisions := map[string]uint64{}
	scan, err := kvwatch.ScanExistingKeys(kv, jetstream.AllKeys, "role revisions", idle, log, func(entry kvwatch.Entry) error {
		revisions[entry.Key] = entry.Revision
		return nil
	}, jetstream.MetaOnly())
	if err != nil {
		return nil, err
	}
	log.Info("restored role claims", slog.Int("restored", len(revisions)), slog.Int("delete_markers", scan.Markers))
	return revisions, nil
}

// scannedRoleClaim is one role claim record as a scan of the role bucket delivered it: the key it
// is stored under, the claim decoded from its value with the entry's revision stamped on it, and
// the error DecodeRoleClaim answered, nil when the record decoded. A sweep reading from the scan
// has of each record exactly what a Get of its key gave it before - the value and the revision -
// so the decode failure travels with the record rather than failing the scan: a stored record this
// build cannot read costs its own role and no more, as it did when each key was read on its own.
type scannedRoleClaim struct {
	Role  string
	Claim RoleClaim
	Err   error
}

// roleClaimsUntil lists every role claim the role bucket holds, sorted by role, from one scan of
// its existing keys (kvwatch.ScanExistingKeys) with delete markers left out: nats.go's Keys runs
// its legacy watcher (bus.KeyValue says why none may run). An empty bucket lists no claim. A scan
// short of the bucket fails the caller rather than list part of it, where nats.go's Keys would
// hand over what had arrived when its watcher gave up, so a sweep never takes a partial list for
// every claim.
//
// The scan carries each record's value, and not its metadata alone, because its callers want the
// claim: a listing of keys costs every caller a Get of its own per key, which is a round trip per
// stored claim per pass against the bucket whose read load is what this reads one pass of instead.
//
// It refuses at the first key it is handed once ctx has ended, so a listing its caller walked
// away from ends there. The scan must end on its own account, because it has a context of its
// own (kvwatch.ScanExistingKeys builds one): one left running goes on holding an ordered
// consumer and appending to a map nobody reads until the bucket's last key arrives or its idle
// bound elapses (kvwatch.ScanIdleTimeout, 10 s), while the client's retry starts a second scan
// of the whole bucket - against the bucket already too far behind to serve the claim view, which
// is the only reason the deregistration's listing runs at all. The error a visit returns ends
// the scan, and the scan then stops and drains its watcher (kvwatch.drainScan), so the listing
// costs the bucket nothing past the answer its caller already has.
//
// The claims are collected into a map keyed by the stored key, the higher revision winning, and
// only then sorted into a slice. The map is what makes a key delivered twice one claim: an ordered
// consumer delivers a key's last-per-subject entry and then any update to it that lands while the
// scan runs, and a sweep handed that key twice would act on the role twice, the second time at the
// older of the two revisions. The slice is sorted by role because that is the order a listing of
// the bucket's keys returned, which the sweeps' callers read their logs and their fixtures in.
//
// Nothing an entry's value aliases is retained: kvwatch.Entry.Value shares the read entry's bytes
// (kvwatch/entry.go), while RoleClaim holds only strings encoding/json allocated out of them. What
// is held to the end of the scan is one scannedRoleClaim per stored claim - a role string, the
// claim's two strings and its two numbers - which at the fleet's ~5,000 claims is under about 1 MB.
func (r *Registry) roleClaimsUntil(ctx context.Context) ([]scannedRoleClaim, error) {
	claims := map[string]scannedRoleClaim{}
	_, err := kvwatch.ScanExistingKeys(r.roles(), jetstream.AllKeys, "role claims", kvwatch.ScanIdleTimeout, r.logger(), func(entry kvwatch.Entry) error {
		if err := ctx.Err(); err != nil {
			return err
		}
		if scanned, ok := claims[entry.Key]; ok && scanned.Claim.Revision >= entry.Revision {
			return nil
		}
		claim, decodeErr := DecodeRoleClaim(entry.Value)
		claim.Revision = entry.Revision
		claims[entry.Key] = scannedRoleClaim{Role: entry.Key, Claim: claim, Err: decodeErr}
		return nil
	}, jetstream.IgnoreDeletes())
	if err != nil {
		return nil, err
	}
	return slices.SortedFunc(maps.Values(claims), func(a, b scannedRoleClaim) int { return strings.Compare(a.Role, b.Role) }), nil
}

// ErrMalformedRoleClaim is a role claim record the bucket holds and this build cannot read: one
// that names no holder, or JSON that is not a claim. The bus answered it, so it is not a read that
// failed, and reading it again returns the same record; each error wrapping it names which.
var ErrMalformedRoleClaim = errors.New("malformed role claim")

// DecodeRoleClaim reads a stored role claim record: a structured claim naming a holder, or a bare
// session id an earlier build stored. Any other record is ErrMalformedRoleClaim. Dispatch's outbox
// reads the role bucket through it too (roles.Buckets), so the two readers cannot drift.
func DecodeRoleClaim(value []byte) (RoleClaim, error) {
	var claim RoleClaim
	err := json.Unmarshal(value, &claim)
	if err == nil {
		if strings.TrimSpace(claim.HolderSessionID) == "" {
			return RoleClaim{}, fmt.Errorf("%w: it names no holder", ErrMalformedRoleClaim)
		}
		return claim, nil
	}
	if json.Valid(value) {
		return RoleClaim{}, fmt.Errorf("%w: %w", ErrMalformedRoleClaim, err)
	}
	// Accept bare session-ID records so persisted claims remain routable;
	// a later claim normalizes the row to the structured format.
	holder := strings.TrimSpace(string(value))
	if holder == "" {
		return RoleClaim{}, fmt.Errorf("%w: a legacy record with an empty holder", ErrMalformedRoleClaim)
	}
	return RoleClaim{HolderSessionID: holder}, nil
}

// newRoleHolderCopy is an empty copy of the role claim bucket, which keeps the session each claim
// it can read names. A claim this build cannot read (ErrMalformedRoleClaim) gives its role to
// nobody, so the copy keeps no entry for it, as a delete marker leaves none.
func newRoleHolderCopy() kvwatch.Copy[string] {
	return kvwatch.NewCopy(kvwatch.Rows[string]{Decode: func(entry kvwatch.Entry) (string, bool, error) {
		claim, err := DecodeRoleClaim(entry.Value)
		if err != nil {
			return "", false, err
		}
		return claim.HolderSessionID, true, nil
	}})
}

// roleCache is the registry as the role claim cache its watcher feeds (kvwatch.Cache). The hooks
// are its own type's, not the registry's methods, so nothing but the watcher calls them.
type roleCache struct{ r *Registry }

// BeginRead starts a read of the role claim bucket (kvwatch.Copy.BeginRead). Every holder stays.
func (c roleCache) BeginRead(read kvwatch.Read) {
	r := c.r
	r.holdersMu.Lock()
	defer r.holdersMu.Unlock()
	r.roleHolders.BeginRead(read)
}

// Apply takes one record the read delivered (kvwatch.Copy.Delivered), and logs a claim it could
// not read, which gave its role to nobody, as the interest watcher logs a value it cannot decode.
func (c roleCache) Apply(entry kvwatch.Entry, _ kvwatch.Read) {
	r := c.r
	r.holdersMu.Lock()
	malformed := r.roleHolders.Delivered(entry)
	r.holdersMu.Unlock()
	if malformed != nil {
		r.logger().Warn("role claim watcher found a claim it cannot read; it names no holder",
			slog.String("role", entry.Key),
			slog.Uint64("revision", entry.Revision),
			slog.String("error", malformed.Error()),
		)
	}
}

// CompleteRead removes every holder and fence entry the read found no evidence of: the bucket no
// longer holds that role's claim.
func (c roleCache) CompleteRead(kvwatch.Read) {
	r := c.r
	r.holdersMu.Lock()
	defer r.holdersMu.Unlock()
	r.roleHolders.CompleteRead()
}

// setOwnRoleHolder records a claim write this registry made through a handle of epoch
// (kvwatch.Copy.Put): a write through a handle on a bucket since recreated is dropped rather than
// fencing out the new bucket's lower revisions, and so is one the fence finds older than the
// holder the copy already took.
func (r *Registry) setOwnRoleHolder(role, holder string, revision, epoch uint64) {
	if r.roleWatcher == nil {
		return
	}
	r.holdersMu.Lock()
	defer r.holdersMu.Unlock()
	r.roleHolders.Put(r.roleWatcher, epoch, role, holder, revision)
}

// forgetRoleHolder records that the claim of role at revision was deleted through a handle of
// epoch. A delete does not return its marker's revision, so the view is fenced one past the
// revision deleted, as deleteInterest fences the interest cache: a claim written since is later
// still, and applies.
func (r *Registry) forgetRoleHolder(role string, revision, epoch uint64) {
	if r.roleWatcher == nil {
		return
	}
	r.holdersMu.Lock()
	defer r.holdersMu.Unlock()
	r.roleHolders.Evict(r.roleWatcher, epoch, role, revision+1)
}

// roleViewEpoch is the epoch a claim write through the role handle carries, and 0 when no watcher
// follows the claims, which only a registry a unit test built leaves: such a write reaches no
// view (setOwnRoleHolder, forgetRoleHolder).
func (r *Registry) roleViewEpoch() uint64 {
	if r.roleWatcher == nil {
		return 0
	}
	_, epoch := r.roleWatcher.Handle()
	return epoch
}

// ErrRoleViewNotReady is RolesHeldBy's answer when its view of the role claims cannot be served,
// so it cannot say which roles a session holds: the watcher has not completed a read of the bucket,
// its copy has not followed the bucket for longer than the stale bound
// (kvwatch.Watcher.Servable), the watcher has ended and not been replaced, or no watcher follows
// the claims. It is a read that did not answer, never the answer that a session holds no roles.
var ErrRoleViewNotReady = errors.New("the role claim view cannot be served")

// RolesHeldBy returns the roles whose claims name sessionID, sorted, from the role claim
// watcher's copy of the bucket, so it makes no bucket call. A session holds a role exactly when
// the role's claim names it; its interest record's topics say nothing about that. It answers only
// while that copy can be served (kvwatch.Watcher.Servable): a read of the bucket has completed and
// the copy follows it, or stopped following it no longer ago than the stale bound. Otherwise it
// returns ErrRoleViewNotReady, wrapped, with the reason RoleViewErr gives, the one test of the
// view's state, which /healthz makes too. A read whose scan timed out is read again by the
// watcher itself, so a view it refuses becomes servable without a Rewatch.
//
// The copy keeps every holder through a read and removes the ones the read found no evidence of
// only once it completes (roleCache), so a read of it during a refill answers the copy it had, not
// a part of the bucket, and no answer is ever a session's roles stripped by a read in flight.
func (r *Registry) RolesHeldBy(sessionID string) ([]string, error) {
	if err := r.RoleViewErr(); err != nil {
		return nil, err
	}
	roles := []string{}
	r.holdersMu.RLock()
	for role, holder := range r.roleHolders.All() {
		if holder == sessionID {
			roles = append(roles, role)
		}
	}
	r.holdersMu.RUnlock()
	sort.Strings(roles)
	return roles, nil
}

// RolesBySession is RolesHeldBy for every session at once: each holder's roles, sorted, read in
// one pass over the claim copy under one lock, for a caller that would otherwise ask per row
// (GET /v1/sessions, whose answer carries every listed session's roles). It refuses on the same
// terms, and a session no claim names is simply absent from the map, which a caller reads as no
// roles.
func (r *Registry) RolesBySession() (map[string][]string, error) {
	if err := r.RoleViewErr(); err != nil {
		return nil, err
	}
	held := map[string][]string{}
	r.holdersMu.RLock()
	for role, holder := range r.roleHolders.All() {
		held[holder] = append(held[holder], role)
	}
	r.holdersMu.RUnlock()
	for _, roles := range held {
		sort.Strings(roles)
	}
	return held, nil
}

// RoleViewErr is nil while the role claim view would answer a read, and otherwise the wrapped
// ErrRoleViewNotReady that RolesHeldBy refuses that read with, naming the same reason: it is the
// one test of the view's state, so a report that the view answers and a read it refuses can never
// disagree. It makes no bucket call, so a caller reports the view without waiting on the bus.
//
// /healthz reports it as a degraded dependency, and it is deliberately not part of Ping or
// WatchErr: those feed the listener's self-health monitor, which restarts the listener over a
// terminal failure that survives its recovery attempts, and no rebuild completes a read the
// bucket keeps timing out on. What does complete it is the role claim watcher's own re-read,
// which needs nothing from the listener.
func (r *Registry) RoleViewErr() error {
	if r.roleWatcher == nil {
		return fmt.Errorf("%w: no watcher follows the role claims", ErrRoleViewNotReady)
	}
	if err := r.roleWatcher.Err(); err != nil {
		return fmt.Errorf("%w: %w", ErrRoleViewNotReady, err)
	}
	if err := r.roleWatcher.Servable(); err != nil {
		return fmt.Errorf("%w: %w", ErrRoleViewNotReady, err)
	}
	return nil
}

func (r *Registry) releaseRoleClaims(ctx context.Context, sessionID string, topics []string) error {
	for _, topic := range topics {
		if !strings.HasPrefix(topic, contracts.RoleTopicPrefix) {
			continue
		}
		role := strings.TrimPrefix(topic, contracts.RoleTopicPrefix)
		if role == "" || strings.ContainsAny(role, "*>") {
			continue
		}
		if err := r.releaseRoleClaim(ctx, sessionID, role); err != nil {
			return err
		}
	}
	return nil
}

// releaseRoleClaim reads role's claim and, when sessionID holds it, deletes it
// (releaseReadRoleClaim), the read and the delete each waiting no longer than ctx (bus.Bounded). A
// claim stored under a key NATS refuses (bus.ErrRefused: too long, or outside its key alphabet)
// has a holder nothing can tell, so it is left to the role reaper, which deletes it, or skips one
// no KV call can delete.
func (r *Registry) releaseRoleClaim(ctx context.Context, sessionID, role string) error {
	claim, err := r.RoleClaim(ctx, role)
	if errors.Is(err, bus.ErrRefused) {
		r.logger().Warn("role registry left a stored claim it cannot read to the role reaper", slog.Int("key_bytes", len(role)), slog.String("error", err.Error()))
		return nil
	}
	if err != nil {
		return err
	}
	return r.releaseReadRoleClaim(ctx, sessionID, role, claim)
}

// releaseReadRoleClaim deletes the claim a read of role has just returned, as RoleClaim decoded
// it, when sessionID holds it, so a caller that has read the claim already releases it without
// reading it again. A zero revision is a role no record claims: RoleClaim stamps on every claim
// the revision it was read at, and the bucket's revisions start at one, so only the zero claim it
// returns for a key the bucket does not hold carries none. Such a role, and a claim naming
// another holder, are not this session's to release; neither is touched. The delete is
// conditional on the revision read and waits no longer than ctx (bus.Bounded). A delete ctx
// abandoned may still land after this answer, and that is safe: it deletes only the revision it
// read, sessionID's claim, which the caller asked to release, and a claim written since is kept.
func (r *Registry) releaseReadRoleClaim(ctx context.Context, sessionID, role string, claim RoleClaim) error {
	if claim.Revision == 0 || claim.HolderSessionID != sessionID {
		return nil
	}
	epoch := r.roleViewEpoch()
	roles := r.roles()
	_, answered, err := bus.Bounded(ctx, func() (struct{}, error) {
		return struct{}{}, roles.Delete(role, nats.LastRevision(claim.Revision))
	})
	if !answered {
		return fmt.Errorf("release role %s's claim held by %s: %w", role, sessionID, err)
	}
	if err != nil && !errors.Is(err, nats.ErrKeyExists) && !errors.Is(err, nats.ErrKeyNotFound) {
		return err
	}
	if err == nil {
		r.forgetRoleHolder(role, claim.Revision, epoch)
	}
	return nil
}

// releaseReadExpiredRoleClaim drops role, over a claim the caller has read already and found
// expired, once a restored claim has exhausted the session registry's TTL. A listener restart
// therefore gives an existing holder one TTL to register again, while an already-expired holder
// cannot block a new claimant forever.
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

// releaseAllRoleClaims releases every claim the role claim view names sessionID the holder of, in
// the order RolesHeldBy sorts them. The view is where the roles come from because it answers from
// the watcher's copy and makes no bucket call (RolesHeldBy), so a deregistration costs the roles
// its session holds rather than the bucket's size: reading them from a list of every key of the
// bucket instead is one scan of the whole bucket per deregistration, which is what answered the
// route 503.
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
func (r *Registry) releaseAllRoleClaims(ctx context.Context, sessionID string) error {
	roles, err := r.RolesHeldBy(sessionID)
	// RolesHeldBy refuses only with ErrRoleViewNotReady - RoleViewErr's error is the one error it
	// has - and the scan is the answer to any refusal, so no refusal releases nothing silently.
	if err != nil {
		return r.releaseAllRoleClaimsByScan(ctx, sessionID, err)
	}
	for _, role := range roles {
		if err := r.releaseRoleClaim(ctx, sessionID, role); err != nil {
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
// leaves it; so is a record this build cannot decode. A claim the scan did read is the session's
// to release, and a delete that fails leaves a claim the session holds unreleased with nothing
// else in this deregistration to release it, so that error fails the sweep. Those two - a listing
// that did not answer or failed, and a delete of a claim the session holds - are what the sweep
// fails on, so a spent budget reaches the route as the listing's wording or the delete's.
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
func (r *Registry) releaseAllRoleClaimsByScan(ctx context.Context, sessionID string, viewErr error) error {
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
		if err := r.releaseReadRoleClaim(ctx, sessionID, role, scanned.Claim); err != nil {
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

// ErrRoleHeld reports a soft claim that found the role held by another session.
// The holder is carried on the error so callers can report who has it.
type ErrRoleHeld struct {
	Role   string
	Holder string
}

func (e *ErrRoleHeld) Error() string {
	return fmt.Sprintf("role %s is held by %s", e.Role, e.Holder)
}

// ErrRoleClaimRevisionMismatch reports a conditional claim the role bucket did not hold the
// revision for: the caller named the revision it had read the role's claim at
// (SetRoleWithPrevious's ifClaimRevision) and the bucket holds another one, either when the claim
// was read or by the time its conditional write reached the bucket. Expected is the revision the
// caller named, Actual the revision the bucket holds as this call last read it - zero on either
// side names no claim record at all - and Holder the session the stored claim names, empty when
// there is none. No claim record was written: the role is left exactly as the claim that stands
// left it, and a claim that lost the write's compare-and-swap rolled its own topic back unless
// the winning claim names the claimant itself.
type ErrRoleClaimRevisionMismatch struct {
	Role     string
	Expected uint64
	Actual   uint64
	Holder   string
}

func (e *ErrRoleClaimRevisionMismatch) Error() string {
	return fmt.Sprintf("role %s's claim is at revision %d, not the %d this claim required", e.Role, e.Actual, e.Expected)
}

// SetRole makes sessionID the holder of role. A hard claim (soft=false) is
// last-claim-wins: it takes the role from whoever holds it. A soft claim
// succeeds only if the role is unheld, already held by sessionID, or held by
// a session listed in supersedable; any other holder returns *ErrRoleHeld
// and leaves the claim untouched. Callers decide what "supersedable" means —
// the listener passes the ids it has established may lose the role: a holder
// that is not live, and the claimant's own declared predecessor when that
// predecessor is not live either, when its record carries the claimant's own
// client instance id, or when either it or the claimant carries no client
// instance id. So a resumed session can recover a role its own earlier
// session held without ever taking one from a live peer, or from a live
// session of another client instance.
func (r *Registry) SetRole(sessionID, machineID, role string, soft bool, supersedable ...string) (Interest, error) {
	return r.SetRoleWithPrevious(context.Background(), sessionID, machineID, role, "", soft, nil, supersedable...)
}

// SetRoleWithPrevious records the session ID that a successful soft claim
// continues. The predecessor is provenance only: the listener has already
// evaluated the soft-claim rules against it, and a predecessor it decided may
// not lose the role reaches this call as provenance with no place in
// supersedable, so this call answers *ErrRoleHeld naming the holder exactly as
// it does for any other live holder.
//
// Each bucket call waits no longer than ctx (bus.Bounded): POST /v1/roles/set passes its request's
// own context. A call ctx abandoned may still land after this answer. A hard claim that names no
// ifClaimRevision is last-claim-wins, so its write names no revision: it lands whatever the bucket
// holds, and two honest concurrent claimants both land, the later write holding the role. A soft
// claim's create or update is conditional on the revision its claim was read at, so it lands only
// if no other claim did since; the loser of that race is answered *ErrRoleHeld for the holder it
// then reads, never a storage failure. A claim that names an ifClaimRevision is conditional too,
// hard or soft, since that revision is the precondition described below. Either way the claim that
// lands makes sessionID the holder it asked to be; the interest write is UpsertWithin's.
//
// The claim alone says who holds the role (RolesHeldBy, and role delivery), so the two cleanups
// that follow are best-effort tidying of interest topics, within ctx like the rest: the rollback of
// sessionID's topic when its claim did not land, and the removal of the role's topic from the old
// holder's record once it did. A topic either leaves behind gives nobody the role. A claim that
// landed is a success whatever its old holder's cleanup did: one that failed is logged at WARN, and
// one ctx ended before it finished at INFO, since nothing failed there.
//
// ifClaimRevision, when it is not nil, is the revision the caller read the role's claim at, and
// this call takes the role only while the bucket still holds that very revision: another one, a
// record where the caller expected none (ifClaimRevision 0), or none where it expected one is
// *ErrRoleClaimRevisionMismatch, and so is a claim that passed that check and then lost the
// write's own compare-and-swap to a claim that landed in between. It writes no claim record
// either way, so the role is left exactly as the claim that stands left it: the precondition is
// weighed before the claimant's interest record is touched, and a claim that lost the
// compare-and-swap rolls that record's topic back unless the winning claim names that same
// session, whose record then keeps the lane topic of the role it now holds. It is the
// precondition a scheduler needs that last-claim-wins and the soft rules cannot give it: a read of
// the role and the claim that follows are one decision, and a claim that landed between them
// refuses this one rather than being overwritten by it. A nil ifClaimRevision claims as before.
func (r *Registry) SetRoleWithPrevious(ctx context.Context, sessionID, machineID, role, previousSessionID string, soft bool, ifClaimRevision *uint64, supersedable ...string) (Interest, error) {
	roleTopic := contracts.RoleTopicPrefix + role
	oldClaim, err := r.RoleClaim(ctx, role)
	if err != nil {
		return Interest{}, err
	}
	// Before the soft rules and before any write: the caller's own precondition, weighed on the
	// read this call made itself, so the revision compared is the one its conditional write is
	// conditional on.
	if ifClaimRevision != nil && *ifClaimRevision != oldClaim.Revision {
		return Interest{}, &ErrRoleClaimRevisionMismatch{
			Role:     role,
			Expected: *ifClaimRevision,
			Actual:   oldClaim.Revision,
			Holder:   oldClaim.HolderSessionID,
		}
	}

	oldSessionID := oldClaim.HolderSessionID
	if soft && oldSessionID != "" && oldSessionID != sessionID && !slices.Contains(supersedable, oldSessionID) {
		return Interest{}, &ErrRoleHeld{Role: role, Holder: oldSessionID}
	}

	item, err := r.UpsertWithin(ctx, Interest{SessionID: sessionID, MachineID: machineID}, []string{roleTopic})
	if err != nil {
		return Interest{}, err
	}

	claim := RoleClaim{
		HolderSessionID:   sessionID,
		ClaimedAt:         r.now().UnixMilli(),
		PreviousSessionID: "",
	}
	if soft {
		claim.PreviousSessionID = previousSessionID
	}
	data, err := json.Marshal(claim)
	if err != nil {
		return Interest{}, err
	}
	epoch := r.roleViewEpoch()
	roles := r.roles()
	// A hard claim that names no precondition writes unconditionally. The conditional write this
	// once made refused the loser of two honest concurrent claims with JetStream's wrong last
	// sequence (error 10071, which nats.go answers as an *nats.APIError matching
	// nats.ErrKeyExists), which storeAnswered counts as the store's own answer, so POST
	// /v1/roles/set wrote it as a 500 - an answer that route's contract does not have, since a hard
	// claim has no refusal at all. A bounded retry over a freshly read revision would only narrow
	// the window: one conditional write lands per revision, so N claimants of one role need N
	// rounds and the unluckiest loses N-1 of them, which no constant bound covers. Writing the
	// claim the way its contract reads - the last write wins - removes the race instead of
	// shrinking it, and leaves the holder one of the claimants either way.
	//
	// A soft claim keeps the conditional write, because its refusal is the point: it read a holder
	// it was allowed to take the role from, and a claim that landed since must not be taken
	// silently. A claim carrying ifClaimRevision keeps it for the same reason, hard or soft: the
	// revision it requires is weighed a second time by the write itself, so a claim that landed
	// between this call's read and its write refuses this one rather than being overwritten by it.
	// Both losers are answered below.
	revision, answered, err := bus.Bounded(ctx, func() (uint64, error) {
		switch {
		case !soft && ifClaimRevision == nil:
			return roles.Put(role, data)
		case oldClaim.Revision == 0:
			return roles.Create(role, data)
		default:
			return roles.Update(role, data, oldClaim.Revision)
		}
	})
	if !answered {
		err = fmt.Errorf("write role %s's claim: %w", role, err)
	}
	if err != nil {
		current, holderErr := r.RoleClaim(ctx, role)
		holder := current.HolderSessionID
		// A conditional claim that lost the write's own compare-and-swap (nats.ErrKeyExists)
		// lost it to a claim that landed after the read this call weighed its precondition on:
		// the precondition failing a moment later, not a storage failure, and the caller is
		// told exactly that rather than reading an unexplained failure or another session's
		// name. A winner naming the claimant itself is no exception here, where it is one for
		// an unconditional claim just below: the revision the caller sequenced its decision on
		// is gone either way, and the claim that stands is not the one it made.
		heldByClaimant := holderErr == nil && holder == sessionID
		conditionalLoss := ifClaimRevision != nil && holderErr == nil && errors.Is(err, nats.ErrKeyExists)
		if heldByClaimant && !conditionalLoss {
			return item, nil
		}
		// The topic goes back only where the role did not end up with this session. A
		// conditional claim refused because the claim that won named the claimant itself
		// leaves that record alone: it carries the lane topic of the role that session now
		// holds, and taking it out would stop the fan-out to the holder.
		if !heldByClaimant {
			if rollbackErr := r.removeInterestTopics(ctx, sessionID, []string{roleTopic}); rollbackErr != nil {
				return Interest{}, errors.Join(err, holderErr, rollbackErr)
			}
		}
		if holderErr != nil {
			return Interest{}, errors.Join(err, holderErr)
		}
		if conditionalLoss {
			return Interest{}, &ErrRoleClaimRevisionMismatch{
				Role:     role,
				Expected: *ifClaimRevision,
				Actual:   current.Revision,
				Holder:   holder,
			}
		}
		// A soft claim that lost its conditional write (wrong last sequence, nats.ErrKeyExists)
		// lost to a concurrent claimant; report that as held rather than as a storage failure.
		if soft && holder != "" {
			return Interest{}, &ErrRoleHeld{Role: role, Holder: holder}
		}
		return Interest{}, err
	}
	r.setOwnRoleHolder(role, sessionID, revision, epoch)

	if oldSessionID != "" && oldSessionID != sessionID {
		err := r.removeInterestTopics(ctx, oldSessionID, []string{roleTopic})
		switch {
		case err == nil, errors.Is(err, nats.ErrKeyNotFound):
		case errors.Is(err, bus.ErrRefused):
			// The old holder's id is a key NATS refuses (too long, or outside its key alphabet): an
			// earlier build registered it, or a stored claim names it. The interest reaper removes
			// the old holder's interest once its session is gone, or skips one no KV call can delete.
			r.logger().Warn("registry role claim left an old holder's interest it cannot write to the reaper",
				slog.String("role", role),
				slog.Int("key_bytes", len(oldSessionID)),
				slog.String("error", err.Error()),
			)
		case ctx.Err() != nil:
			r.logger().Info("registry role claim left its old holder's topic: the request ended first",
				slog.String("role", role),
				slog.String("old_session_id", oldSessionID),
				slog.String("new_session_id", sessionID),
			)
		default:
			r.logger().Warn("registry role claim old holder cleanup failed",
				slog.String("role", role),
				slog.String("old_session_id", oldSessionID),
				slog.String("new_session_id", sessionID),
				slog.String("error", err.Error()),
			)
		}
	}
	return item, nil
}

// RoleClaim reads role's claim from the bucket, waiting no longer than ctx (bus.Bounded). The
// claim it returns carries the entry's revision (RoleClaim.Revision), which is what a conditional
// claim and GET /v1/roles/<role> compare against; a role with no claim record returns a zero
// claim without an error, so a zero revision names a role no record claims, and a record that is
// not a claim returns ErrMalformedRoleClaim.
//
// That revision is the whole of what the entry the read answered says beyond the claim itself,
// which is why this hands back no entry: a caller testing whether the bucket holds a record for
// the role reads claim.Revision == 0, and one writing conditionally over the record it read names
// claim.Revision, as releaseReadRoleClaim already does.
func (r *Registry) RoleClaim(ctx context.Context, role string) (RoleClaim, error) {
	roles := r.roles()
	entry, answered, err := bus.Bounded(ctx, func() (nats.KeyValueEntry, error) { return roles.Get(role) })
	if !answered {
		return RoleClaim{}, fmt.Errorf("read role %s's claim: %w", role, err)
	}
	if errors.Is(err, nats.ErrKeyNotFound) {
		return RoleClaim{}, nil
	}
	if err != nil {
		return RoleClaim{}, err
	}
	claim, err := DecodeRoleClaim(entry.Value())
	if err != nil {
		return RoleClaim{}, err
	}
	claim.Revision = entry.Revision()
	return claim, nil
}

// RoleHolder returns the authoritative session ID for role, read with no deadline of its own. Only
// tests call it, to read what a claim left behind; every route and the role lane read RoleClaim
// within a deadline, through the one resolution (roles.ResolveHolder and roles.ResolveHolderFrom,
// internal/roles). An unclaimed role returns an empty holder without an error.
func (r *Registry) RoleHolder(role string) (string, error) {
	claim, err := r.RoleClaim(context.Background(), role)
	if err != nil {
		return "", err
	}
	return claim.HolderSessionID, nil
}
