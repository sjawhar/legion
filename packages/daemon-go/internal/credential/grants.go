// Package credential owns the daemon-only, short-lived grants that one worker command redeems.
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

var (
	// ErrUnauthenticated means Mint was not given the live, authenticated claim a route obtained.
	ErrUnauthenticated = errors.New("grant requires an authenticated claim")
	// ErrExpired means the grant's sixty-second window elapsed before redemption.
	ErrExpired = errors.New("grant expired")
	// ErrUsed is deliberately also the answer for an unknown id: a bearer must not become an oracle.
	ErrUsed = errors.New("grant is unavailable")
)

// Grant is the in-memory capability returned by Mint or MintController. ID is the bearer and is
// never persisted or logged. CapabilityHash is deliberately private: routes use StillMatches after
// a slow GitHub lease to make sure the registered claim did not change while that await was live.
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
	g.mu.Lock()
	g.issued[id] = grant
	g.mu.Unlock()
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
	g.mu.Lock()
	g.issued[id] = grant
	g.mu.Unlock()
	return grant, nil
}

// Redeem consumes id exactly once. It removes expired grants too, so neither a retry nor an
// expired bearer grows the process-local map.
func (g *Grants) Redeem(id string) (Grant, error) {
	g.mu.Lock()
	defer g.mu.Unlock()
	grant, ok := g.issued[id]
	if !ok {
		return Grant{}, ErrUsed
	}
	delete(g.issued, id)
	if !g.now().Before(grant.ExpiresAt) {
		return Grant{}, ErrExpired
	}
	return grant, nil
}

// StillMatches says whether current is the same authenticated claim that minted grant. It is the
// post-await fence for credential routes: a replacement registration changes CapabilityHash.
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
