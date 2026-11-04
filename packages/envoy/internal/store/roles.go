package store

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
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
	// Revision is the role bucket revision the claim was read at, which roleClaim stamps on
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
// build cannot read gets a revision here; it still gets no grace, because ReleaseExpiredRoleClaim,
// the only reader of these revisions, reads the claim itself first and cannot.
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

// roleKeys lists the key of every role claim the role bucket holds, sorted, from one scan of its
// existing keys (kvwatch.ScanExistingKeys) with what nats.go's Keys watches with, values and delete
// markers left out: nats.go's Keys runs its legacy watcher (bus.KeyValue says why none may run). An
// empty bucket lists no key. A scan short of the bucket fails the caller rather than list part of
// it, where nats.go's Keys would hand over what had arrived when its watcher gave up, so a sweep
// never takes a partial list for every claim.
func (r *Registry) roleKeys() ([]string, error) {
	var keys []string
	_, err := kvwatch.ScanExistingKeys(r.roles(), jetstream.AllKeys, "role claims", kvwatch.ScanIdleTimeout, r.logger(), func(entry kvwatch.Entry) error {
		keys = append(keys, entry.Key)
		return nil
	}, jetstream.MetaOnly(), jetstream.IgnoreDeletes())
	if err != nil {
		return nil, err
	}
	slices.Sort(keys)
	return keys, nil
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

// roleClaim reads role's claim from the bucket, waiting no longer than ctx (bus.Bounded). The
// claim it returns carries the entry's revision (RoleClaim.Revision), which is what a conditional
// claim and GET /v1/roles/<role> compare against; a role with no claim record returns a zero
// claim, so a zero revision names a role no record claims.
func (r *Registry) roleClaim(ctx context.Context, role string) (RoleClaim, nats.KeyValueEntry, error) {
	roles := r.roles()
	entry, answered, err := bus.Bounded(ctx, func() (nats.KeyValueEntry, error) { return roles.Get(role) })
	if !answered {
		return RoleClaim{}, nil, fmt.Errorf("read role %s's claim: %w", role, err)
	}
	if errors.Is(err, nats.ErrKeyNotFound) {
		return RoleClaim{}, nil, nil
	}
	if err != nil {
		return RoleClaim{}, nil, err
	}
	claim, err := DecodeRoleClaim(entry.Value())
	if err != nil {
		return RoleClaim{}, nil, err
	}
	claim.Revision = entry.Revision()
	return claim, entry, nil
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

// releaseRoleClaim deletes role's claim when sessionID holds it, its read and its conditional delete
// each waiting no longer than ctx (bus.Bounded). A delete ctx abandoned may still land after this
// answer, and that is safe: it deletes only the revision it read, sessionID's claim, which the
// caller asked to release, and a claim written since is kept. A claim stored under a key NATS
// refuses (bus.ErrRefused: too long, or outside its key alphabet) has a holder nothing can tell, so
// it is left to the role reaper, which deletes it, or skips one no KV call can delete.
func (r *Registry) releaseRoleClaim(ctx context.Context, sessionID, role string) error {
	claim, entry, err := r.roleClaim(ctx, role)
	if errors.Is(err, bus.ErrRefused) {
		r.logger().Warn("role registry left a stored claim it cannot read to the role reaper", slog.Int("key_bytes", len(role)), slog.String("error", err.Error()))
		return nil
	}
	if err != nil {
		return err
	}
	if entry == nil || claim.HolderSessionID != sessionID {
		return nil
	}
	epoch := r.roleViewEpoch()
	roles := r.roles()
	_, answered, err := bus.Bounded(ctx, func() (struct{}, error) {
		return struct{}{}, roles.Delete(role, nats.LastRevision(entry.Revision()))
	})
	if !answered {
		return fmt.Errorf("release role %s's claim held by %s: %w", role, sessionID, err)
	}
	if err != nil && !errors.Is(err, nats.ErrKeyExists) && !errors.Is(err, nats.ErrKeyNotFound) {
		return err
	}
	if err == nil {
		r.forgetRoleHolder(role, entry.Revision(), epoch)
	}
	return nil
}

// ReleaseExpiredRoleClaim drops role only when sessionID remains its holder and
// a restored claim has exhausted the session registry's TTL. A listener restart
// therefore gives an existing holder one TTL to register again, while an
// already-expired holder cannot block a new claimant forever.
//
// Its result distinguishes a confirmed deletion from a concurrent removal or
// replacement, so callers never report that a lost conditional delete released
// a claim. A failure is reported retained, its error naming the release.
//
// The claim's read and its conditional delete each wait no longer than ctx (bus.Bounded): an HTTP
// route passes its request's own context, role delivery and the reaper context.Background(). A
// delete ctx abandoned may still land after this answer. That is safe to leave running: it deletes
// the claim only if the bucket still holds the very revision it read, naming sessionID, whom the
// caller has just found gone from the session bucket, so at worst a lapsed holder's claim is
// released a moment after the caller was told the bus did not answer, and a fresh claim that
// landed meanwhile is never touched.
func (r *Registry) ReleaseExpiredRoleClaim(ctx context.Context, role, sessionID string, sessionTTL time.Duration) (ExpiredRoleClaimRelease, error) {
	claim, entry, err := r.roleClaim(ctx, role)
	if err != nil {
		return ExpiredRoleClaimRetained, fmt.Errorf("release role %s's expired claim held by %s: %w", role, sessionID, err)
	}
	if entry == nil {
		return ExpiredRoleClaimMissing, nil
	}
	if claim.HolderSessionID != sessionID {
		return ExpiredRoleClaimSuperseded, nil
	}
	r.mu.RLock()
	restoredRevision, restored := r.restoredRoleRevisions[role]
	r.mu.RUnlock()
	if restored && restoredRevision == entry.Revision() && sessionTTL > 0 && r.now().Sub(r.openedAt) < sessionTTL {
		return ExpiredRoleClaimRetained, nil
	}
	epoch := r.roleViewEpoch()
	roles := r.roles()
	_, _, err = bus.Bounded(ctx, func() (struct{}, error) {
		return struct{}{}, roles.Delete(role, nats.LastRevision(entry.Revision()))
	})
	switch {
	case err == nil:
		r.forgetRoleHolder(role, entry.Revision(), epoch)
		return ExpiredRoleClaimReleased, nil
	case errors.Is(err, nats.ErrKeyNotFound):
		return ExpiredRoleClaimMissing, nil
	case errors.Is(err, nats.ErrKeyExists):
		return ExpiredRoleClaimSuperseded, nil
	default:
		return ExpiredRoleClaimRetained, fmt.Errorf("release role %s's expired claim held by %s: %w", role, sessionID, err)
	}
}

func (r *Registry) releaseAllRoleClaims(ctx context.Context, sessionID string) error {
	roles, err := r.roleKeys()
	if err != nil {
		return err
	}
	for _, role := range roles {
		if err := r.releaseRoleClaim(ctx, sessionID, role); err != nil {
			return err
		}
	}
	return nil
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
	oldClaim, entry, err := r.roleClaim(ctx, role)
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
		case entry == nil:
			return roles.Create(role, data)
		default:
			return roles.Update(role, data, entry.Revision())
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

// RoleClaim returns the durable claim for role, read from the bucket within ctx (bus.Bounded). An
// unclaimed role returns a zero-value claim without an error; a record that is not a claim,
// ErrMalformedRoleClaim.
func (r *Registry) RoleClaim(ctx context.Context, role string) (RoleClaim, error) {
	claim, _, err := r.roleClaim(ctx, role)
	return claim, err
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
