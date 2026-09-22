package api

import (
	"context"
	"encoding/hex"
	"sync"

	"github.com/sjawhar/legion/daemon/internal/claim"
	"github.com/sjawhar/legion/daemon/internal/supervise"
)

// BootTokenStore finds the claim whose current launch minted a boot token, by the token's hash —
// the store's ClaimByBootTokenHash.
type BootTokenStore interface {
	ClaimByBootTokenHash(ctx context.Context, hash []byte) (supervise.Claim, bool, error)
}

// BootToken is what a boot token resolves to: the claim its launch was minted for and that
// launch's generation. Stale is a token whose claim has launched again since, so the pane holding
// it answers for a generation the claim has left.
type BootToken struct {
	Claim      claim.Token
	Generation uint64
	Stale      bool
}

// BootTokens resolves the boot token a pane presents — the shim's hello on every connection, the
// agent's registration once — to the launch it was minted for.
//
// The claim row carries only its current launch's hash, and that is durable: a daemon restarted
// under a live pane resolves the pane's token from the store, which is how the shim's reconnect
// hello is accepted after a restart. A token of a launch the claim has since replaced is no longer
// on the row; this process still recognises it as stale when it saw that launch persisted, which
// is the shipped daemon's rule too (an in-memory mint record beside the persisted hash —
// packages/daemon/src/daemon/api.ts:326-341). After a restart such a token is simply unknown.
type BootTokens struct {
	store BootTokenStore

	mu     sync.Mutex
	minted map[string]BootToken
}

// NewBootTokens resolves against store.
func NewBootTokens(store BootTokenStore) *BootTokens {
	return &BootTokens{store: store, minted: map[string]BootToken{}}
}

// Resolve is the launch token was minted for, and false for a token no launch minted — or none
// this process saw and the store no longer holds. The token itself is hashed here and never kept.
func (b *BootTokens) Resolve(ctx context.Context, token string) (BootToken, bool, error) {
	hash := supervise.HashBootToken(token)
	c, ok, err := b.store.ClaimByBootTokenHash(ctx, hash)
	if err != nil {
		return BootToken{}, false, err
	}
	if ok {
		return BootToken{Claim: c.Token, Generation: c.Generation}, true, nil
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	launch, ok := b.minted[hex.EncodeToString(hash)]
	if !ok {
		return BootToken{}, false, nil
	}
	launch.Stale = true
	return launch, true, nil
}

// Recording is store with every launch it persists remembered: a claim written with a boot token
// hash is a launch that token was minted for. Machines write through it.
func (b *BootTokens) Recording(store supervise.Store) supervise.Store {
	return recordingStore{Store: store, tokens: b}
}

type recordingStore struct {
	supervise.Store
	tokens *BootTokens
}

func (s recordingStore) PutClaim(ctx context.Context, c supervise.Claim) error {
	if err := s.Store.PutClaim(ctx, c); err != nil {
		return err
	}
	if len(c.BootTokenHash) > 0 {
		s.tokens.mu.Lock()
		s.tokens.minted[hex.EncodeToString(c.BootTokenHash)] = BootToken{Claim: c.Token, Generation: c.Generation}
		s.tokens.mu.Unlock()
	}
	return nil
}
