package testnats

import (
	"testing"

	"github.com/nats-io/nkeys"
)

// User is a fresh nkey user's seed and public key, the public key being what StartNkeyAuthorized
// and StartNkeyPublishAllowed take.
func User(t testing.TB) (seed, public string) {
	t.Helper()
	user, err := nkeys.CreateUser()
	if err != nil {
		t.Fatalf("create nkey user: %v", err)
	}
	raw, err := user.Seed()
	if err != nil {
		t.Fatalf("user seed: %v", err)
	}
	if public, err = user.PublicKey(); err != nil {
		t.Fatalf("user public key: %v", err)
	}
	return string(raw), public
}
