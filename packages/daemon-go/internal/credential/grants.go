// Package credential owns the daemon-only, short-lived grants a worker's commands redeem.
package credential

import (
	"crypto/rand"
	"encoding/hex"
	"errors"
	"sync"
	"time"

	"github.com/sjawhar/legion/daemon/internal/claim"
	"github.com/sjawhar/legion/daemon/internal/supervise"
)

const ttl = 60 * time.Second

// expiredRetention is how long an expired grant's record is kept, so a command that outran its
// grant still learns it expired; after it, Mint prunes the record and the id is unavailable.
const expiredRetention = time.Hour

var (
	// ErrUnauthenticated means Mint was not given the live, authenticated claim a route obtained.
	ErrUnauthenticated = errors.New("grant requires an authenticated claim")
	// ErrExpired means the grant's sixty-second window elapsed before redemption.
	ErrExpired = errors.New("grant expired")
	// ErrUnavailable is deliberately the one answer for an unknown id and for a pruned one: a
	// bearer must not become an oracle.
	ErrUnavailable = errors.New("grant is unavailable")
)

// Grant is the in-memory capability returned by Mint or MintController. ID is the bearer and is
// never persisted or logged. CapabilityHash is deliberately private: routes compare it through
// StillMatches, so a grant authenticates only while its claim holds the capability that minted it.
type Grant struct {
	ID         string
	Issue      string
	Project    string
	Tree       string
	Role       claim.Role
	Claim      claim.Token
	ExpiresAt  time.Time
	Controller bool

	capabilityHash []byte
}

// Grants holds process-local grants. A daemon restart invalidates them by design.
type Grants struct {
	now func() time.Time

	mu     sync.Mutex
	issued map[string]Grant
}

// New constructs a grant service. A nil clock is the wall clock.
func New(now func() time.Time) *Grants {
	if now == nil {
		now = time.Now
	}
	return &Grants{now: now, issued: make(map[string]Grant)}
}

// Mint records a 60-second grant for the authenticated claim a route already proved with the exact
// same capability comparison as /claims/ready and /claims/exit. The capability hash is copied so a
// later registration replacement cannot retain the old session's authority.
func (g *Grants) Mint(c supervise.Claim) (Grant, error) {
	if c.Token == "" || c.Project == "" || c.Tree == "" || c.Issue == "" || c.Role == "" || len(c.CapabilityHash) == 0 {
		return Grant{}, ErrUnauthenticated
	}
	id, err := grantID()
	if err != nil {
		return Grant{}, err
	}
	grant := Grant{
		ID:             id,
		Issue:          c.Issue,
		Project:        c.Project,
		Tree:           c.Tree,
		Role:           c.Role,
		Claim:          c.Token,
		ExpiresAt:      g.now().Add(ttl),
		capabilityHash: append([]byte(nil), c.CapabilityHash...),
	}
	g.record(grant)
	return grant, nil
}

// MintController records the operator's sixty-second grant. Authorization happens at the route:
// the service keeps no bearer and cannot accidentally copy one into a grant record.
func (g *Grants) MintController() (Grant, error) {
	id, err := grantID()
	if err != nil {
		return Grant{}, err
	}
	grant := Grant{ID: id, ExpiresAt: g.now().Add(ttl), Controller: true}
	g.record(grant)
	return grant, nil
}

// record stores a freshly minted grant and prunes every record past its retention, so the
// process-local map holds only the last hour's grants however many commands never redeem theirs.
func (g *Grants) record(grant Grant) {
	cutoff := g.now().Add(-expiredRetention)
	g.mu.Lock()
	defer g.mu.Unlock()
	for id, held := range g.issued {
		if held.ExpiresAt.Before(cutoff) {
			delete(g.issued, id)
		}
	}
	g.issued[grant.ID] = grant
}

// Redeem answers id's grant for as long as its sixty seconds last. A grant is the credential of one
// bash command, which may run `legion gh` several times and whose git may call the credential
// helper more than once, so it serves every redemption until it expires — as the shipped daemon's
// resolveGrant does. Redeem knows nothing of the claim: a route refuses a grant whose claim no
// longer holds the capability that minted it (StillMatches), on every redemption.
func (g *Grants) Redeem(id string) (Grant, error) {
	g.mu.Lock()
	grant, ok := g.issued[id]
	g.mu.Unlock()
	if !ok {
		return Grant{}, ErrUnavailable
	}
	if !g.now().Before(grant.ExpiresAt) {
		return Grant{}, ErrExpired
	}
	return grant, nil
}

// StillMatches says whether current is the same authenticated claim that minted grant. It is the
// revocation fence: every redemption passes it, and the credential routes ask it again after their
// GitHub await, since a replacement registration changes CapabilityHash.
func (g *Grants) StillMatches(grant Grant, current supervise.Claim) bool {
	if grant.Controller || current.Token != grant.Claim || len(current.CapabilityHash) != len(grant.capabilityHash) {
		return false
	}
	var different byte
	for i := range grant.capabilityHash {
		different |= current.CapabilityHash[i] ^ grant.capabilityHash[i]
	}
	return different == 0
}

func grantID() (string, error) {
	var raw [32]byte
	if _, err := rand.Read(raw[:]); err != nil {
		return "", err
	}
	return hex.EncodeToString(raw[:]), nil
}
