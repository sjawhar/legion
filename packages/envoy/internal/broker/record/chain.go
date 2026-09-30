package record

import (
	"context"
	"errors"
	"fmt"
	"time"
)

// ErrChainBroken is every reason ChainVerifier.Verify refuses: the record does not exist, its
// stored body does not reproduce its own id, its embedded request object does not verify, or it
// does not carry exactly one terminal decision that is an approval by its approver. A caller that
// only cares "does this still authenticate" collapses every case with errors.Is(err,
// ErrChainBroken); one that needs the reason keeps the wrapped detail.
var ErrChainBroken = errors.New("credential request chain does not verify")

// ErrNotApprover is a decision on a record by a login other than the approver the record names.
var ErrNotApprover = errors.New("only the record's approver may decide it")

// TerminalEvent is one terminal decision event a record carries — approved, denied, expired or
// cancelled — and the login that decided it, "" for an event no human decided.
type TerminalEvent struct {
	Event string
	Login string
}

// ChainVerifier re-derives a credential-request record's full issuance chain rather than
// trusting a downstream row (a launcher credential, a grant) that merely names the record's id:
// the stored body must reproduce the record's own content-addressed id, the request object it
// embeds must still be a well-formed, signed proof — checked as of the record's own creation
// time, not now, since a request object's own ~10-minute exp is long past by the time anything
// built from it is used again; re-verifying it proves provenance, not freshness — and the record
// must carry exactly one terminal decision, an approval by the login the record names as its
// approver.
//
// store.Store.ChainVerifier builds the one enroll.AuthenticateLauncher and requests.Machine's own
// chain check each use, by record kind, through these narrow func fields, so this package needs no
// store access of its own.
type ChainVerifier struct {
	// Audience and Skew re-verify the embedded request object exactly as VerifyRequestObject
	// enforced them when the record was first created.
	Audience string
	Skew     time.Duration

	// FetchRecord resolves a record id to its stored canonical body and creation time. found is
	// false when no such record exists — including, per the caller's own query, one that exists
	// but is not of the kind the caller cares about.
	FetchRecord func(ctx context.Context, recordID string) (body string, createdAt time.Time, found bool, err error)

	// FetchDecisions resolves every terminal decision event the record carries, oldest first.
	// The broker writes at most one (credential_request_decision's partial unique index holds it
	// to that), so a second is a row something other than the broker wrote.
	FetchDecisions func(ctx context.Context, recordID string) ([]TerminalEvent, error)
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
	body, err := VerifyBodyReproducesID(canonical, recordID)
	if err != nil {
		if errors.Is(err, ErrBodyIDMismatch) {
			return Body{}, fmt.Errorf("%w: stored body does not reproduce its own id", ErrChainBroken)
		}
		return Body{}, fmt.Errorf("%w: stored body does not parse: %s", ErrChainBroken, err)
	}
	if _, err := VerifyRequestObject(body.Request, c.Audience, c.Skew, createdAt); err != nil {
		return Body{}, fmt.Errorf("%w: request object: %s", ErrChainBroken, err)
	}
	decisions, err := c.FetchDecisions(ctx, recordID)
	if err != nil {
		return Body{}, err
	}
	switch {
	case len(decisions) == 0:
		return Body{}, fmt.Errorf("%w: no approved event", ErrChainBroken)
	case len(decisions) > 1:
		return Body{}, fmt.Errorf("%w: %d terminal decision events, want exactly one", ErrChainBroken, len(decisions))
	case decisions[0].Event != "approved":
		return Body{}, fmt.Errorf("%w: no approved event (decided %s)", ErrChainBroken, decisions[0].Event)
	}
	if _, err := body.ApproverLogin(decisions[0].Login); err != nil {
		return Body{}, fmt.Errorf("%w: approved by %q, not the record's approver %q", ErrChainBroken, decisions[0].Login, body.Approver)
	}
	return body, nil
}
