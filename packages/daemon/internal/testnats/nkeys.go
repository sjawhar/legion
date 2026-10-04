package testnats

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/nats-io/nkeys"
)

// User is a fresh nkey user's seed and public key, the public key being what StartNkeyAuthorized
// takes.
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

// Account is a fresh nkey account's seed: a valid nkey seed, but not a user's.
func Account(t testing.TB) string {
	t.Helper()
	account, err := nkeys.CreateAccount()
	if err != nil {
		t.Fatalf("create nkey account: %v", err)
	}
	raw, err := account.Seed()
	if err != nil {
		t.Fatalf("account seed: %v", err)
	}
	return string(raw)
}

// SeedFile writes contents to a 0600 file of its own under t's temporary directory and returns its
// path.
func SeedFile(t testing.TB, contents string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "nats.seed")
	if err := os.WriteFile(path, []byte(contents), 0o600); err != nil {
		t.Fatalf("write the seed file: %v", err)
	}
	return path
}

// UserSeed is a fresh nkey user's seed (User without the public key).
func UserSeed(t testing.TB) string {
	t.Helper()
	seed, _ := User(t)
	return seed
}
