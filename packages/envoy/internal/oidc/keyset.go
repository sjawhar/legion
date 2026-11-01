package oidc

import (
	"context"
	"fmt"

	jose "github.com/go-jose/go-jose/v4"
)

// keySetAlgorithms are the signature algorithms a token is parsed under: the two a Kubernetes
// cluster signs its projected service-account tokens with, the same pair enroll's own reader of
// that token (podtoken.go) parses. A token signed with anything else is refused before any key is
// asked for.
var keySetAlgorithms = []jose.SignatureAlgorithm{jose.RS256, jose.ES256}

// keySet is a *Keys as the KeySet go-oidc's verifier asks for: it answers whether a compact JWT's
// signature verifies, from the key the token's header names.
//
// It exists so that verifying a token and the RFC 8693 exchange's own key lookup read one cache.
// go-oidc's RemoteKeySet re-reads the issuer's key set for every token naming a key id it does not
// hold — the rotation strategy OpenID Connect recommends, with no interval between two reads — so
// a token signed by a key nobody ever published, replayed in a loop, costs the issuer one read per
// token. Keys bounds those reads at one per MinRefresh and still makes a rotation visible inside
// it.
type keySet struct{ keys *Keys }

// VerifySignature resolves the key raw's header names and returns the payload that key's
// signature covers. go-oidc has parsed raw already and keeps the result on ctx under a key of its
// own, which this package cannot read, so the token is parsed a second time here.
func (s keySet) VerifySignature(ctx context.Context, raw string) ([]byte, error) {
	signed, err := jose.ParseSigned(raw, keySetAlgorithms)
	if err != nil {
		return nil, fmt.Errorf("oidc: malformed token: %w", err)
	}
	// go-oidc refuses a token carrying no signature or several before it asks a KeySet, so the
	// one signature it found is the one here.
	if len(signed.Signatures) != 1 {
		return nil, fmt.Errorf("oidc: token carries %d signatures, want one", len(signed.Signatures))
	}
	header := signed.Signatures[0].Header
	key, err := s.keys.Key(ctx, header.KeyID, header.Algorithm)
	if err != nil {
		return nil, recordKeySetError(ctx, err)
	}
	payload, err := signed.Verify(key)
	if err != nil {
		return nil, fmt.Errorf("oidc: the signature under key id %s does not verify: %w", keyIDName(header.KeyID), err)
	}
	return payload, nil
}

// keySetError is where a keySet leaves the error its key source returned, for the Verify that put
// it on the context to read once go-oidc has answered.
//
// It exists because go-oidc formats the error a KeySet returns with %v ("failed to verify
// signature: %v", verify.go in v3.21.0), so no sentinel wrapped in it survives for errors.Is.
// Without the slot an issuer whose key set could not be read (ErrKeySet) would be
// indistinguishable from a token the issuer never signed, and classify would report the outage as
// a forgery. go-oidc calls VerifySignature on the goroutine that called Verify and waits for it,
// so one Verify's slot is written and read by that Verify alone and needs no lock.
type keySetError struct{ err error }

// keySetErrorKey is the context key the slot travels under, a type of this package's own so no
// other package's value can collide with it.
type keySetErrorKey struct{}

// withKeySetError returns ctx carrying an empty slot for the key source's error, and that slot.
func withKeySetError(ctx context.Context) (context.Context, *keySetError) {
	slot := &keySetError{}
	return context.WithValue(ctx, keySetErrorKey{}, slot), slot
}

// recordKeySetError leaves err in the slot ctx carries, where it carries one, and returns it
// unchanged for go-oidc to report its own way.
func recordKeySetError(ctx context.Context, err error) error {
	if slot, ok := ctx.Value(keySetErrorKey{}).(*keySetError); ok {
		slot.err = err
	}
	return err
}
