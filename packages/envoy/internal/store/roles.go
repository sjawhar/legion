package store

import (
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"slices"
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

func decodeRoleClaim(value []byte) (RoleClaim, error) {
	var claim RoleClaim
	err := json.Unmarshal(value, &claim)
	if err == nil {
		if strings.TrimSpace(claim.HolderSessionID) == "" {
			return RoleClaim{}, fmt.Errorf("role claim has no holder")
		}
		return claim, nil
	}
	if json.Valid(value) {
		return RoleClaim{}, fmt.Errorf("decode role claim: %w", err)
	}
	// Accept bare session-ID records so persisted claims remain routable;
	// a later claim normalizes the row to the structured format.
	holder := strings.TrimSpace(string(value))
	if holder == "" {
		return RoleClaim{}, fmt.Errorf("decode legacy role claim: empty holder")
	}
	return RoleClaim{HolderSessionID: holder}, nil
}

func (r *Registry) roleClaim(role string) (RoleClaim, nats.KeyValueEntry, error) {
	entry, err := r.roles().Get(role)
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

func (r *Registry) releaseRoleClaims(sessionID string, topics []string) error {
	for _, topic := range topics {
		if !strings.HasPrefix(topic, contracts.RoleTopicPrefix) {
			continue
		}
		role := strings.TrimPrefix(topic, contracts.RoleTopicPrefix)
		if role == "" || strings.ContainsAny(role, "*>") {
			continue
		}
		if err := r.releaseRoleClaim(sessionID, role); err != nil {
			return err
		}
	}
	return nil
}

// releaseRoleClaim deletes role's claim when sessionID holds it. A claim stored under a key NATS
// refuses (bus.ErrRefused: too long, or outside its key alphabet) has a holder nothing can tell, so
// it is left to the role reaper, which deletes it, or skips one no KV call can delete.
func (r *Registry) releaseRoleClaim(sessionID, role string) error {
	claim, entry, err := r.roleClaim(role)
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
	err = r.roles().Delete(role, nats.LastRevision(entry.Revision()))
	if err != nil && !errors.Is(err, nats.ErrKeyExists) && !errors.Is(err, nats.ErrKeyNotFound) {
		return err
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
// a claim.
func (r *Registry) ReleaseExpiredRoleClaim(role, sessionID string, sessionTTL time.Duration) (ExpiredRoleClaimRelease, error) {
	claim, entry, err := r.roleClaim(role)
	if err != nil {
		return ExpiredRoleClaimRetained, err
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
	err = r.roles().Delete(role, nats.LastRevision(entry.Revision()))
	switch {
	case err == nil:
		return ExpiredRoleClaimReleased, nil
	case errors.Is(err, nats.ErrKeyNotFound):
		return ExpiredRoleClaimMissing, nil
	case errors.Is(err, nats.ErrKeyExists):
		return ExpiredRoleClaimSuperseded, nil
	default:
		return ExpiredRoleClaimRetained, err
	}
}

func (r *Registry) releaseAllRoleClaims(sessionID string) error {
	roles, err := r.roleKeys()
	if err != nil {
		return err
	}
	for _, role := range roles {
		if err := r.releaseRoleClaim(sessionID, role); err != nil {
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
// the listener passes the ids it has established are no longer live, so a
// resumed session can recover a role its dead predecessor held without ever
// taking one from a live peer.
func (r *Registry) SetRole(sessionID, machineID, role string, soft bool, supersedable ...string) (Interest, error) {
	return r.SetRoleWithPrevious(sessionID, machineID, role, "", soft, supersedable...)
}

// SetRoleWithPrevious records the session ID that a successful soft claim
// continues. The predecessor is provenance only after the listener has
// evaluated the existing soft-claim rules.
func (r *Registry) SetRoleWithPrevious(sessionID, machineID, role, previousSessionID string, soft bool, supersedable ...string) (Interest, error) {
	roleTopic := contracts.RoleTopicPrefix + role
	oldClaim, entry, err := r.roleClaim(role)
	if err != nil {
		return Interest{}, err
	}

	oldSessionID := oldClaim.HolderSessionID
	if soft && oldSessionID != "" && oldSessionID != sessionID && !slices.Contains(supersedable, oldSessionID) {
		return Interest{}, &ErrRoleHeld{Role: role, Holder: oldSessionID}
	}

	item, err := r.Upsert(Interest{SessionID: sessionID, MachineID: machineID}, []string{roleTopic})
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
	if entry == nil {
		_, err = r.roles().Create(role, data)
	} else {
		_, err = r.roles().Update(role, data, entry.Revision())
	}
	if err != nil {
		holder, holderErr := r.RoleHolder(role)
		if holderErr == nil && holder == sessionID {
			return item, nil
		}
		if rollbackErr := r.removeInterestTopics(sessionID, []string{roleTopic}); rollbackErr != nil {
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

	if oldSessionID != "" && oldSessionID != sessionID {
		err := r.removeInterestTopics(oldSessionID, []string{roleTopic})
		if errors.Is(err, bus.ErrRefused) {
			// The old holder's id is a key NATS refuses (too long, or outside its key alphabet): an
			// earlier build registered it, or a stored claim names it. The claim is written and the
			// caller named no such key, so it succeeds; the interest reaper removes the old holder's
			// interest once its session is gone, or skips one no KV call can delete.
			r.logger().Warn("registry role claim left an old holder's interest it cannot write to the reaper",
				slog.String("role", role),
				slog.Int("key_bytes", len(oldSessionID)),
				slog.String("error", err.Error()),
			)
		} else if err != nil && !errors.Is(err, nats.ErrKeyNotFound) {
			r.logger().Warn("registry role claim old holder cleanup failed",
				slog.String("role", role),
				slog.String("old_session_id", oldSessionID),
				slog.String("new_session_id", sessionID),
				slog.String("error", err.Error()),
			)
			return Interest{}, err
		}
	}
	return item, nil
}

// RoleClaim returns the durable claim for role. An unclaimed role returns a
// zero-value claim without an error.
func (r *Registry) RoleClaim(role string) (RoleClaim, error) {
	claim, _, err := r.roleClaim(role)
	return claim, err
}

// RoleHolder returns the authoritative session ID for role. An unclaimed role
// returns an empty holder without an error.
func (r *Registry) RoleHolder(role string) (string, error) {
	claim, err := r.RoleClaim(role)
	if err != nil {
		return "", err
	}
	return claim.HolderSessionID, nil
}
