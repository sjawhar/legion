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

func decodeRoleClaim(value []byte) (RoleClaim, error) {
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
		claim, err := decodeRoleClaim(entry.Value)
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

// roleClaim reads role's claim from the bucket, waiting no longer than ctx (bus.Bounded).
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
	claim, err := decodeRoleClaim(entry.Value())
	if err != nil {
		return RoleClaim{}, nil, err
	}
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
	return r.SetRoleWithPrevious(context.Background(), sessionID, machineID, role, "", soft, supersedable...)
}

// SetRoleWithPrevious records the session ID that a successful soft claim
// continues. The predecessor is provenance only: the listener has already
// evaluated the soft-claim rules against it, and a predecessor it decided may
// not lose the role reaches this call as provenance with no place in
// supersedable, so this call answers *ErrRoleHeld naming the holder exactly as
// it does for any other live holder.
//
// Each bucket call waits no longer than ctx (bus.Bounded): POST /v1/roles/set passes its request's
// own context. A call ctx abandoned may still land after this answer. The claim's create or update
// is conditional on the revision the claim was read at, so it lands only if no other claim did
// since, and makes sessionID the holder it asked to be; the interest write is UpsertWithin's.
//
// The claim alone says who holds the role (RolesHeldBy, and role delivery), so the two cleanups
// that follow are best-effort tidying of interest topics, within ctx like the rest: the rollback of
// sessionID's topic when its claim did not land, and the removal of the role's topic from the old
// holder's record once it did. A topic either leaves behind gives nobody the role. A claim that
// landed is a success whatever its old holder's cleanup did: one that failed is logged at WARN, and
// one ctx ended before it finished at INFO, since nothing failed there.
func (r *Registry) SetRoleWithPrevious(ctx context.Context, sessionID, machineID, role, previousSessionID string, soft bool, supersedable ...string) (Interest, error) {
	roleTopic := contracts.RoleTopicPrefix + role
	oldClaim, entry, err := r.roleClaim(ctx, role)
	if err != nil {
		return Interest{}, err
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
	revision, answered, err := bus.Bounded(ctx, func() (uint64, error) {
		if entry == nil {
			return roles.Create(role, data)
		}
		return roles.Update(role, data, entry.Revision())
	})
	if !answered {
		err = fmt.Errorf("write role %s's claim: %w", role, err)
	}
	if err != nil {
		current, holderErr := r.RoleClaim(ctx, role)
		holder := current.HolderSessionID
		if holderErr == nil && holder == sessionID {
			return item, nil
		}
		if rollbackErr := r.removeInterestTopics(ctx, sessionID, []string{roleTopic}); rollbackErr != nil {
			return Interest{}, errors.Join(err, holderErr, rollbackErr)
		}
		if holderErr != nil {
			return Interest{}, errors.Join(err, holderErr)
		}
		// A soft claim that lost the CAS race lost to a concurrent claimant;
		// report that as held rather than as a storage failure.
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
// within a deadline (resolveRoleHolder, cmd/listener/role_holder.go). An unclaimed role returns an
// empty holder without an error.
func (r *Registry) RoleHolder(role string) (string, error) {
	claim, err := r.RoleClaim(context.Background(), role)
	if err != nil {
		return "", err
	}
	return claim.HolderSessionID, nil
}
