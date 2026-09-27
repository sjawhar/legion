package record

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"
)

// ErrChainBroken is every reason ChainVerifier.Verify refuses: the record does not exist, its
// stored body does not reproduce its own id, its embedded request object does not verify, it was
// never approved, or its approval assertion no longer verifies against the approver's live keys.
// A caller that only cares "does this still authenticate" collapses every case with
// errors.Is(err, ErrChainBroken); one that needs the reason keeps the wrapped detail.
var ErrChainBroken = errors.New("credential request chain does not verify")

// ChainVerifier re-derives a credential-request record's full issuance chain from first
// principles, rather than trusting a downstream row (a launcher credential, a grant) that merely
// names the record's id: the stored body must reproduce the record's own content-addressed id,
// the request object it embeds must still be a genuinely well-formed, signed proof — checked as
// of the record's own creation time, not now, since a request object's own ~10-minute exp is long
// past by the time anything built from it is ever used again; re-verifying it proves provenance,
// not freshness — and the decisive approval event must carry a still-valid assertion by the login
// the record itself names as approver.
//
// enroll.AuthenticateLauncher (AGENTC-393 Plan A Task 7) and requests.Machine's own chain check
// (Task 6) each build one of these against the same underlying tables through these narrow func
// fields, so neither package need import the other's store access — or, transitively, each
// other.
type ChainVerifier struct {
	// Audience and Skew re-verify the embedded request object exactly as VerifyRequestObject
	// enforced them when the record was first created.
	Audience string
	Skew     time.Duration

	// FetchRecord resolves a record id to its stored canonical body and creation time. found is
	// false when no such record exists — including, per the caller's own query, one that exists
	// but is not of the kind the caller cares about.
	FetchRecord func(ctx context.Context, recordID string) (body string, createdAt time.Time, found bool, err error)

	// FetchApproval resolves a record's decisive "approved" event: the assertion its approver
	// signed over ApproveChallenge(recordID). found is false when the record has no approved
	// event (denied, cancelled, expired, or still pending).
	FetchApproval func(ctx context.Context, recordID string) (assertion json.RawMessage, found bool, err error)

	// VerifyAssertion re-verifies the approval assertion against the approver's currently live
	// key set — approvers.Service.VerifyAssertion's own contract, run inside a transaction the
	// caller always rolls back so a re-check never persists a side effect. That re-check's own
	// authenticator signature counter was already advanced once, for real, by the original,
	// committed decision this assertion approved; rolling back a later re-verification's own
	// transaction cannot undo that earlier commit, so every honest re-check of the exact same
	// stored assertion legitimately fails the counter-monotonicity check on its own
	// (approvers.ErrCounterReplay) even though the signature, origin, rpID, challenge and key
	// liveness all still check out. A caller wiring this against a real approvers.Service must
	// therefore treat ErrCounterReplay alone as success — never any other error, since the
	// counter check runs strictly after every cryptographic and liveness check, so a forged
	// signature, wrong origin/rpID/challenge, or a since-revoked/tombstoned key all fail before
	// the counter is ever reached and never wrap ErrCounterReplay.
	VerifyAssertion func(ctx context.Context, login string, challenge [32]byte, assertion json.RawMessage) error
}

// Verify re-derives recordID's full issuance chain and returns its parsed Body once every link
// holds.
func (c *ChainVerifier) Verify(ctx context.Context, recordID string) (Body, error) {
	canonical, createdAt, found, err := c.FetchRecord(ctx, recordID)
	if err != nil {
		return Body{}, err
	}
	if !found {
		return Body{}, fmt.Errorf("%w: no such record", ErrChainBroken)
	}
	body, err := ParseBody(canonical)
	if err != nil {
		return Body{}, fmt.Errorf("%w: stored body does not parse: %s", ErrChainBroken, err)
	}
	if body.ID() != recordID {
		return Body{}, fmt.Errorf("%w: stored body does not reproduce its own id", ErrChainBroken)
	}
	if _, err := VerifyRequestObject(body.Request, c.Audience, c.Skew, createdAt); err != nil {
		return Body{}, fmt.Errorf("%w: request object: %s", ErrChainBroken, err)
	}
	assertion, found, err := c.FetchApproval(ctx, recordID)
	if err != nil {
		return Body{}, err
	}
	if !found {
		return Body{}, fmt.Errorf("%w: no approved event", ErrChainBroken)
	}
	if err := c.VerifyAssertion(ctx, body.Approver, ApproveChallenge(recordID), assertion); err != nil {
		return Body{}, fmt.Errorf("%w: approval assertion: %s", ErrChainBroken, err)
	}
	return body, nil
}
