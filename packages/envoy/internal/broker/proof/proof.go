package proof

import (
	"context"
	"crypto/ecdsa"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/go-jose/go-jose/v4"
)

var ErrInvalid = errors.New("proof invalid")

// jtiRetentionMargin is how long past the end of a proof's acceptance window its jti is kept.
const jtiRetentionMargin = time.Minute

// Subject is who a verified proof authenticates: exactly one of EnrollmentID (an "eid" proof) or
// LauncherID (an "lid" proof, a key-bound launcher credential authenticating directly) is set.
type Subject struct {
	EnrollmentID string
	LauncherID   string
}

type Verifier struct {
	Skew   time.Duration
	Lookup func(ctx context.Context, enrollmentID string) (thumbprint string, live bool, err error)
	// LookupLauncher answers an "lid" proof's key exactly as Lookup answers an "eid" proof's:
	// the launcher credential's own pinned thumbprint, and whether it is still live — unexpired,
	// unrevoked, and its whole issuance chain still re-verifies
	// (enroll.Service.AuthenticateLauncher).
	LookupLauncher func(ctx context.Context, launcherID string) (thumbprint string, live bool, err error)
	Replay         func(ctx context.Context, jti string, expires time.Time) (fresh bool, err error)
}

// Verify returns the Subject a proof authenticates. Lookup, LookupLauncher and Replay errors are
// returned unwrapped (never ErrInvalid) so callers can distinguish an internal failure from an
// invalid proof; every other failure is ErrInvalid wrapped with a reason that never quotes the
// proof or the key.
func (v *Verifier) Verify(ctx context.Context, compact, method, url string, now time.Time) (Subject, error) {
	sig, err := jose.ParseSigned(compact, []jose.SignatureAlgorithm{jose.ES256})
	if err != nil || len(sig.Signatures) != 1 {
		return Subject{}, fmt.Errorf("%w: not a single ES256 JWS", ErrInvalid)
	}
	header := sig.Signatures[0].Protected
	if typ, _ := header.ExtraHeaders[jose.HeaderType].(string); typ != typeHeader {
		return Subject{}, fmt.Errorf("%w: typ is not %s", ErrInvalid, typeHeader)
	}
	if header.JSONWebKey == nil {
		return Subject{}, fmt.Errorf("%w: no embedded key", ErrInvalid)
	}
	pub, ok := header.JSONWebKey.Key.(*ecdsa.PublicKey)
	if !ok || pub.Curve.Params().Name != "P-256" {
		return Subject{}, fmt.Errorf("%w: key is not EC P-256", ErrInvalid)
	}
	payload, err := sig.Verify(pub)
	if err != nil {
		return Subject{}, fmt.Errorf("%w: signature", ErrInvalid)
	}
	var c claims
	if err := json.Unmarshal(payload, &c); err != nil || c.JTI == "" {
		return Subject{}, fmt.Errorf("%w: claims", ErrInvalid)
	}
	if (c.EnrollmentID == "") == (c.LauncherID == "") {
		return Subject{}, fmt.Errorf("%w: exactly one of eid/lid is required", ErrInvalid)
	}
	issued := time.Unix(c.IssuedAt, 0)
	if issued.After(now.Add(v.Skew)) || issued.Before(now.Add(-v.Skew)) {
		return Subject{}, fmt.Errorf("%w: iat outside %s", ErrInvalid, v.Skew)
	}
	if c.Method != method || c.URL != url {
		return Subject{}, fmt.Errorf("%w: htm/htu do not match the request", ErrInvalid)
	}
	var thumbprint string
	var live bool
	if c.EnrollmentID != "" {
		thumbprint, live, err = v.Lookup(ctx, c.EnrollmentID)
	} else {
		thumbprint, live, err = v.LookupLauncher(ctx, c.LauncherID)
	}
	if err != nil {
		return Subject{}, err
	}
	if !live {
		return Subject{}, fmt.Errorf("%w: not live", ErrInvalid)
	}
	presented, err := thumbprintOf(*header.JSONWebKey)
	if err != nil {
		return Subject{}, fmt.Errorf("%w: thumbprint", ErrInvalid)
	}
	if presented != thumbprint {
		return Subject{}, fmt.Errorf("%w: key is not the enrolled key", ErrInvalid)
	}
	// The jti is remembered for as long as a proof carrying it can still pass the iat check above
	// (until issued+Skew), plus a margin for the clock skew between this process and the database
	// that prunes expired jtis.
	fresh, err := v.Replay(ctx, c.JTI, issued.Add(v.Skew+jtiRetentionMargin))
	if err != nil {
		return Subject{}, err
	}
	if !fresh {
		return Subject{}, fmt.Errorf("%w: replayed jti", ErrInvalid)
	}
	return Subject{EnrollmentID: c.EnrollmentID, LauncherID: c.LauncherID}, nil
}
