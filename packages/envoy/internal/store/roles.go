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
	// ClientInstanceID is the client instance of the process that holds the claim, which the
	// claimant named when it took the role. A session id alone does not name that process: a
	// second process can resume a live session's id, and it carries a client instance of its
	// own, so this field is what tells the two apart and keeps the second from taking,
	// releasing or reaping the first's role. Empty where the claimant named none - a claim an
	// earlier build wrote, or one whose caller has no instance - and an empty instance is
	// never evidence that two claims come from the same process.
	ClientInstanceID string `json:"client_instance_id,omitempty"`
	// RenewedAt is the wall clock in milliseconds at which the holder's process last answered
	// for this claim; a claim write stamps it with that write's own ClaimedAt. Zero means the
	// record carries no renewal, which a claim an earlier build wrote does not, and such a
	// claim is read at the time it was claimed rather than at the epoch: every reader of a
	// renewal reads it through LastAnsweredAt, which says so.
	RenewedAt int64 `json:"renewed_at,omitempty"`
	// HandedBack is the mark a hand-back left on this claim: the release named this role's own
	// lane topic and stated no process, so it was not authorised to free the claim
	// (RoleReleaseAuthority.frees) and marked it instead (Registry.releaseReadRoleClaim). It is
	// written by that release alone - never by the administrator's force, which frees the claim
	// outright, never by the empty-topics deregistration, and never onto a claim that names no
	// process, which its own session's release frees. Its readers are the renewal, which renews
	// no marked claim whoever registers (Registry.renewRoleClaim), so the role claim reaper
	// frees the role once that renewal has lapsed, a live window and up to one reap interval
	// later, and GET /v1/roles/<role>, which reports it as
	// handed_back. Absent means no hand-back of that shape has reached this claim: a claim
	// granted since clears the mark, because every grant writes a fresh record
	// (SetRoleWithPrevious).
	HandedBack bool `json:"handed_back,omitempty"`
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

// LastAnsweredAt is the wall clock in milliseconds at which this claim's holder last answered for
// it: RenewedAt where the record carries one, and ClaimedAt, the moment the claim was written,
// where it does not. It is the one reading of a claim's renewal, so a claim stored before this
// build, which carries no RenewedAt, is read at the time it was claimed rather than at the epoch,
// where every window would already have passed. A claim carrying neither is zero, which is older
// than any window.
func (c RoleClaim) LastAnsweredAt() int64 {
	if c.RenewedAt > 0 {
		return c.RenewedAt
	}
	return c.ClaimedAt
}

// HeldByProcess reports whether instance names the very process this claim records as the
// holder's. It is the one strict test of a claim against a process, which every decision that
// weighs the two reads through: the release (RoleReleaseAuthority.frees), a claim of a role held
// under the claimant's own session id (POST /v1/roles/set), and the renewal
// (Registry.renewRoleClaim, which also writes where the claim names no process at all).
//
// An absent id on either side names no process, so it matches nothing: a claim an earlier build
// wrote names none, and a caller that states none says nothing about which process it is. That
// is the opposite of session.InstanceMatches, the loose test the predecessor rule reads, where
// an absent id on either side is a client predating the field and the claim goes through.
func (c RoleClaim) HeldByProcess(instance string) bool {
	return instance != "" && c.ClientInstanceID == instance
}

// ExpiredRoleClaimRelease reports what releaseReadExpiredRoleClaim's conditional delete did to
// an expired role claim: ExpiredRoleClaimReleased is the delete that landed, the one outcome the
// sweep counts as reaped (ReapRoleClaims); ExpiredRoleClaimMissing a role whose record was gone
// before the delete, or that never had one; ExpiredRoleClaimSuperseded a claim written again
// between the caller's read and the delete, which the delete's own revision kept; and
// ExpiredRoleClaimRetained a claim left standing, either for the restart grace of a listener
// that has just opened (sessionTTL) or by a delete that failed, which the error beside it names.
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
//
// A record that carries no client instance id, no renewal, or neither is read, not refused: a
// claim an earlier build wrote names no process and no renewal, and both fields are left zero, as
// they are for the bare session id. A missing field is never ErrMalformedRoleClaim, which is a
// record this build cannot read at all.
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

// roleClaimWrite is one conditional write of one role's claim, as its caller means it: which
// records the write may be made over, the write itself, and what the view owes a write that
// landed. Three callers make such a write - the release's delete (releaseReadRoleClaim), the
// hand-back's mark (markRoleClaimHandedBack) and the renewal (renewRoleClaim) - and what they
// share is the protocol, not the meaning: the bounded call, the bucket's answers read one way,
// the bounded re-read, and the bound on attempts. writeRoleClaim runs that, each caller states
// the rest here, and nothing is flattened: a lost race means something different to each of
// them, which each says by what it does with the outcome.
type roleClaimWrite struct {
	// role is the claim's key: what the write is sent against and what a re-read reads.
	role string
	// attempts is how many conditional writes may be sent in all, counting the first. One is a
	// write that is never retried, which is what a lost race means to the renewal: the record
	// is another claimant's now, and a renewal written into it would stamp this process's
	// heartbeat on a claim it does not hold.
	attempts int
	// writes reports whether this write may be made over claim, the record as it was last
	// read. It is asked before every attempt, so a retry is made only over a record its caller
	// still means to write, and a caller comparing the re-read against the claim it first read
	// closes over that one.
	writes func(claim RoleClaim) bool
	// send sends the write itself, conditional on the revision claim was read at, and answers
	// the revision the record stands at after it, as the bucket's writes do; a delete answers
	// none and its caller reads the revision it deleted off claim.
	send func(claim RoleClaim) (uint64, error)
	// landed moves this registry's own copy of the role's holder to what the write left
	// (setOwnRoleHolder, forgetRoleHolder), over the claim the write was made at, the revision
	// it answered and the epoch of the handle it was sent through, read before the write.
	landed func(claim RoleClaim, revision, epoch uint64)
}

// roleClaimWriteOutcome is what one run of writeRoleClaim did to the role's record, for the
// callers that answer for each of them differently. A failure is the zero value and comes with
// the error that caused it; every other outcome carries none.
type roleClaimWriteOutcome uint8

const (
	// roleClaimWriteFailed is a bucket call that did not answer, a re-read that failed, or an
	// answer that is neither a landed write nor a lost race. Nothing was written.
	roleClaimWriteFailed roleClaimWriteOutcome = iota
	// roleClaimWriteLanded is the conditional write the bucket took; landed has been called.
	roleClaimWriteLanded
	// roleClaimWriteStood is a record this write may not be made over: the caller's own test
	// refused it, or no record claims the role at all (a zero revision), which no conditional
	// write can be made over either. The claim writeRoleClaim answers beside it is the record
	// it stopped at, which is the caller's to read.
	roleClaimWriteStood
	// roleClaimWriteMissing is a write the bucket answered nats.ErrKeyNotFound: the record was
	// gone when it arrived.
	roleClaimWriteMissing
	// roleClaimWriteRaced is every attempt lost to a write that landed since the read it was
	// conditional on (nats.ErrKeyExists), the last of them at the attempt bound.
	roleClaimWriteRaced
)

// writeRoleClaim sends w over claim, the record its caller read, conditional on the revision
// that read returned, and sends it again over a re-read of the record while the write loses
// that condition and w allows another attempt. Each bucket call waits no longer than ctx
// (bus.Bounded); a call ctx abandoned may still land after this answer, and it is conditional on
// the revision it was sent at, so it touches no record written since - each caller says what
// that costs it.
//
// It answers what became of the record (roleClaimWriteOutcome), the claim it stopped at, and the
// error of a call that failed. The claim is the caller's own when nothing was re-read, and the
// re-read's record otherwise, so a caller that must weigh the record it left reads it here
// rather than reading the bucket again.
func (r *Registry) writeRoleClaim(ctx context.Context, claim RoleClaim, w roleClaimWrite) (roleClaimWriteOutcome, RoleClaim, error) {
	for attempt := 1; ; attempt++ {
		if claim.Revision == 0 || !w.writes(claim) {
			return roleClaimWriteStood, claim, nil
		}
		at := claim
		epoch := r.roleViewEpoch()
		revision, answered, err := bus.Bounded(ctx, func() (uint64, error) { return w.send(at) })
		switch {
		case !answered:
			return roleClaimWriteFailed, claim, err
		case err == nil:
			w.landed(at, revision, epoch)
			return roleClaimWriteLanded, at, nil
		case errors.Is(err, nats.ErrKeyNotFound):
			return roleClaimWriteMissing, claim, nil
		case !errors.Is(err, nats.ErrKeyExists):
			return roleClaimWriteFailed, claim, err
		case attempt == w.attempts:
			return roleClaimWriteRaced, claim, nil
		}
		if claim, err = r.RoleClaim(ctx, w.role); err != nil {
			return roleClaimWriteFailed, claim, err
		}
	}
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

// RoleClaimRequest is one claim of a role, as SetRoleWithPrevious takes it: who is claiming,
// under which process, what it is claiming, and the preconditions it is claiming under. The
// fields are named because a claim states five strings in a row - the session, the machine, the
// process, the role and the predecessor - and any two of them swapped would still compile and
// would still write a claim, naming the wrong session or taking the role from the wrong one.
//
// The zero value of a field is a caller that states nothing there: no process, no predecessor,
// no precondition and nothing supersedable, which is the hard last-claim-wins claim SetRole
// makes.
type RoleClaimRequest struct {
	// SessionID is the session the claim makes the holder of Role, and MachineID the machine
	// its interest record is written for (UpsertWithin).
	SessionID string
	MachineID string
	// ClientInstanceID is the client instance of the claimant's own process, which the claim
	// records as the process that holds the role (RoleClaim.ClientInstanceID). A caller with
	// no instance to give states none, and the claim then names no process.
	ClientInstanceID string
	// Role is the role claimed, whose lane topic the claim writes onto the holder's interest
	// record (contracts.RoleTopicPrefix).
	Role string
	// PreviousSessionID is the session a soft claim continues, recorded as the claim's own
	// provenance and weighed nowhere here. It is written only on a soft claim.
	PreviousSessionID string
	// Soft is a claim that takes the role only from a holder listed in Supersedable; a hard
	// claim takes it from whoever holds it.
	Soft bool
	// IfClaimRevision, when it is not nil, is the revision the caller read the role's claim
	// at, and the claim lands only while the bucket still holds that very revision; 0 names a
	// role with no claim record at all.
	IfClaimRevision *uint64
	// Supersedable lists the holders a soft claim may take the role from, which the caller has
	// established may lose it. A soft claim that finds any other holder is refused.
	Supersedable []string
}

// SetRole makes sessionID the holder of role. A hard claim (soft=false) is
// last-claim-wins: it takes the role from whoever holds it. A soft claim
// succeeds only if the role is unheld or held by a session listed in
// supersedable; any other holder returns *ErrRoleHeld and leaves the claim
// untouched, and a holder that is sessionID itself is no exception. A session
// id names no process - a second process can resume a live session's id - so
// a claim whose holder is the claimant's own session id is weighed by the
// caller like any other holder's, and reaches here listed in supersedable or
// refused. Callers decide what "supersedable" means — the listener passes the
// ids it has established may lose the role: a holder that is not live; the
// claimant's own declared predecessor when that predecessor is not live
// either, when its record carries the claimant's own client instance id, or
// when either it or the claimant carries no client instance id; and the
// claimant's own session id when the standing claim names the claimant's own
// process, or when nobody has renewed that claim inside the live window
// (Registry.ClaimRenewedWithin). So a resumed session can recover a role its
// own earlier session held without ever taking one from a live peer, from a
// live session of another client instance, or from another process holding
// the role under the very session id it is claiming as its own.
//
// The claim it writes names no client instance (RoleClaim.ClientInstanceID): its callers hold
// none to give, so each leaves RoleClaimRequest.ClientInstanceID empty, and the claim says which
// session holds the role without saying which process does.
func (r *Registry) SetRole(sessionID, machineID, role string, soft bool, supersedable ...string) (Interest, error) {
	return r.SetRoleWithPrevious(context.Background(), RoleClaimRequest{
		SessionID:    sessionID,
		MachineID:    machineID,
		Role:         role,
		Soft:         soft,
		Supersedable: supersedable,
	})
}

// SetRoleWithPrevious writes req's claim, and records the session ID that a successful soft claim
// continues (RoleClaimRequest.PreviousSessionID). The predecessor is provenance only: the
// listener has already evaluated the soft-claim rules against it, and a predecessor it decided
// may not lose the role reaches this call as provenance with no place in Supersedable, so this
// call answers *ErrRoleHeld naming the holder exactly as it does for any other live holder.
//
// A soft claim is refused the role whenever its holder is not listed in Supersedable, and a
// holder equal to req.SessionID is weighed like any other: the claimant's own session id buys it
// nothing here, because a session id names no process and a second process that resumed that id
// would otherwise take the role from the first by claiming under the id it shares. The one
// caller that can tell the two apart lists that holder itself (POST /v1/roles/set, which reads
// the standing claim's own process and renewal), so a claim arriving with its own session id as
// the holder and nothing in Supersedable is *ErrRoleHeld naming that holder.
//
// req.ClientInstanceID is the client instance of the claimant's own process, which the claim
// records as the process that holds the role (RoleClaim.ClientInstanceID), and the claim is
// stamped claimed and renewed at the same moment (ClaimedAt and RenewedAt, one reading of this
// registry's clock), so a claim is live from the instant it lands. A caller with no instance to
// give states none, and the claim then names no process.
//
// The record it writes is a fresh claim and never the standing one amended, so the hand-back
// mark goes with the claim it was written on (RoleClaim.HandedBack): a role granted again to
// the very process that handed it back is that process's to renew from this write on, which is
// how a holder takes back a role it gave up without waiting out the role claim reaper.
//
// Each bucket call waits no longer than ctx (bus.Bounded): POST /v1/roles/set passes its request's
// own context. A call ctx abandoned may still land after this answer. A hard claim that names no
// IfClaimRevision is last-claim-wins, so its write names no revision: it lands whatever the bucket
// holds, and two honest concurrent claimants both land, the later write holding the role. A soft
// claim's create or update is conditional on the revision its claim was read at, so it lands only
// if no other claim did since; the loser of that race is answered *ErrRoleHeld for the holder it
// then reads, never a storage failure. A claim that names an IfClaimRevision is conditional too,
// hard or soft, since that revision is the precondition described below. Either way the claim that
// lands makes req.SessionID the holder it asked to be; the interest write is UpsertWithin's.
//
// The claim alone says who holds the role (RolesHeldBy, and role delivery), so the two cleanups
// that follow are best-effort tidying of interest topics, within ctx like the rest: the rollback of
// req.SessionID's topic when its claim did not land, and the removal of the role's topic from the
// old holder's record once it did. A topic either leaves behind gives nobody the role. A claim that
// landed is a success whatever its old holder's cleanup did: one that failed is logged at WARN, and
// one ctx ended before it finished at INFO, since nothing failed there.
//
// req.IfClaimRevision, when it is not nil, is the revision the caller read the role's claim at,
// and this call takes the role only while the bucket still holds that very revision: another one,
// a record where the caller expected none (IfClaimRevision 0), or none where it expected one is
// *ErrRoleClaimRevisionMismatch, and so is a claim that passed that check and then lost the
// write's own compare-and-swap to a claim that landed in between. It writes no claim record
// either way, so the role is left exactly as the claim that stands left it: the precondition is
// weighed before the claimant's interest record is touched, and a claim that lost the
// compare-and-swap rolls that record's topic back unless the winning claim names that same
// session, whose record then keeps the lane topic of the role it now holds. It is the
// precondition a scheduler needs that last-claim-wins and the soft rules cannot give it: a read of
// the role and the claim that follows are one decision, and a claim that landed between them
// refuses this one rather than being overwritten by it. A nil IfClaimRevision claims as before.
func (r *Registry) SetRoleWithPrevious(ctx context.Context, req RoleClaimRequest) (Interest, error) {
	roleTopic := contracts.RoleTopicPrefix + req.Role
	oldClaim, err := r.RoleClaim(ctx, req.Role)
	if err != nil {
		return Interest{}, err
	}
	// Before the soft rules and before any write: the caller's own precondition, weighed on the
	// read this call made itself, so the revision compared is the one its conditional write is
	// conditional on.
	if req.IfClaimRevision != nil && *req.IfClaimRevision != oldClaim.Revision {
		return Interest{}, &ErrRoleClaimRevisionMismatch{
			Role:     req.Role,
			Expected: *req.IfClaimRevision,
			Actual:   oldClaim.Revision,
			Holder:   oldClaim.HolderSessionID,
		}
	}

	oldSessionID := oldClaim.HolderSessionID
	if req.Soft && oldSessionID != "" && !slices.Contains(req.Supersedable, oldSessionID) {
		return Interest{}, &ErrRoleHeld{Role: req.Role, Holder: oldSessionID}
	}

	item, err := r.UpsertWithin(ctx, Interest{SessionID: req.SessionID, MachineID: req.MachineID}, []string{roleTopic})
	if err != nil {
		return Interest{}, err
	}

	now := r.now().UnixMilli()
	claim := RoleClaim{
		HolderSessionID:   req.SessionID,
		ClaimedAt:         now,
		PreviousSessionID: "",
		ClientInstanceID:  req.ClientInstanceID,
		RenewedAt:         now,
	}
	if req.Soft {
		claim.PreviousSessionID = req.PreviousSessionID
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
	// silently. A claim carrying IfClaimRevision keeps it for the same reason, hard or soft: the
	// revision it requires is weighed a second time by the write itself, so a claim that landed
	// between this call's read and its write refuses this one rather than being overwritten by it.
	// Both losers are answered below.
	revision, answered, err := bus.Bounded(ctx, func() (uint64, error) {
		switch {
		case !req.Soft && req.IfClaimRevision == nil:
			return roles.Put(req.Role, data)
		case oldClaim.Revision == 0:
			return roles.Create(req.Role, data)
		default:
			return roles.Update(req.Role, data, oldClaim.Revision)
		}
	})
	if !answered {
		err = fmt.Errorf("write role %s's claim: %w", req.Role, err)
	}
	if err != nil {
		current, holderErr := r.RoleClaim(ctx, req.Role)
		holder := current.HolderSessionID
		// A conditional claim that lost the write's own compare-and-swap (nats.ErrKeyExists)
		// lost it to a claim that landed after the read this call weighed its precondition on:
		// the precondition failing a moment later, not a storage failure, and the caller is
		// told exactly that rather than reading an unexplained failure or another session's
		// name. A winner naming the claimant itself is no exception here, where it is one for
		// an unconditional claim just below: the revision the caller sequenced its decision on
		// is gone either way, and the claim that stands is not the one it made.
		//
		// The claim that stands is this claimant's own concurrent success only where it names
		// the claimant's session and the very process the claimant stated
		// (RoleClaim.HeldByProcess): a session id names no process, so a second process that
		// resumed this id could be the one holding that claim, and answering success over it
		// tells this claimant it holds a role another process holds - which is how two
		// processes of one session id came to answer for one role. A claimant that states no
		// process is told success by no claim at all: an absent instance names no process and
		// matches nothing, here as everywhere else a claim is weighed against a process, and a
		// claim naming none is the one record any process of that session id could have
		// written. Such a claimant is answered the failure its write met, and the role stays
		// with whatever claim stands.
		heldBySession := holderErr == nil && holder == req.SessionID
		heldByClaimant := heldBySession && current.HeldByProcess(req.ClientInstanceID)
		conditionalLoss := req.IfClaimRevision != nil && holderErr == nil && errors.Is(err, nats.ErrKeyExists)
		if heldByClaimant && !conditionalLoss {
			return item, nil
		}
		// The topic goes back only where the role did not end up with this session, which is
		// the session's and not the process's: one interest record serves every process
		// running under one session id, so a claim refused because the claim that won names
		// this session - the claimant's own process's, another process's, or a claim that
		// names none - leaves that record alone. It carries the lane topic of the role that
		// session now holds, the interest half of the claim that stands.
		if !heldBySession {
			if rollbackErr := r.removeInterestTopics(ctx, req.SessionID, []string{roleTopic}); rollbackErr != nil {
				return Interest{}, errors.Join(err, holderErr, rollbackErr)
			}
		}
		if holderErr != nil {
			return Interest{}, errors.Join(err, holderErr)
		}
		if conditionalLoss {
			return Interest{}, &ErrRoleClaimRevisionMismatch{
				Role:     req.Role,
				Expected: *req.IfClaimRevision,
				Actual:   current.Revision,
				Holder:   holder,
			}
		}
		// A soft claim that lost its conditional write (wrong last sequence, nats.ErrKeyExists)
		// lost to a concurrent claimant; report that as held rather than as a storage failure.
		if req.Soft && holder != "" {
			return Interest{}, &ErrRoleHeld{Role: req.Role, Holder: holder}
		}
		return Interest{}, err
	}
	r.setOwnRoleHolder(req.Role, req.SessionID, revision, epoch)

	if oldSessionID != "" && oldSessionID != req.SessionID {
		err := r.removeInterestTopics(ctx, oldSessionID, []string{roleTopic})
		switch {
		case err == nil, errors.Is(err, nats.ErrKeyNotFound):
		case errors.Is(err, bus.ErrRefused):
			// The old holder's id is a key NATS refuses (too long, or outside its key alphabet): an
			// earlier build registered it, or a stored claim names it. The interest reaper removes
			// the old holder's interest once its session is gone, or skips one no KV call can delete.
			r.logger().Warn("registry role claim left an old holder's interest it cannot write to the reaper",
				slog.String("role", req.Role),
				slog.Int("key_bytes", len(oldSessionID)),
				slog.String("error", err.Error()),
			)
		case ctx.Err() != nil:
			r.logger().Info("registry role claim left its old holder's topic: the request ended first",
				slog.String("role", req.Role),
				slog.String("old_session_id", oldSessionID),
				slog.String("new_session_id", req.SessionID),
			)
		default:
			r.logger().Warn("registry role claim old holder cleanup failed",
				slog.String("role", req.Role),
				slog.String("old_session_id", oldSessionID),
				slog.String("new_session_id", req.SessionID),
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
