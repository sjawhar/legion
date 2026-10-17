package seed

import (
	"context"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/sjawhar/envoy/internal/testnats"
)

// The exported API takes a secret's name as given; only the command's flag rule checks --name. A
// name that holds an nkey seed is refused before any call, by Check, Generate and Verify and by
// each method of both stores, with an error naming no part of it: every line, refusal and error
// after a write quotes the name, the version-2 message once in each recovery command.

const seededNameRefusal = "the secret's name holds an nkey seed, which is never a secret's name (not echoed)"

// seededNames are names that hold seed, as a store's name rule admits them, in each spelling the
// detector reads.
func seededNames(t *testing.T, prefix string) (seed string, names map[string]string) {
	t.Helper()
	seed, _ = testnats.User(t)
	return seed, map[string]string{
		"a seed as the secret's id":        prefix + seed,
		"a lowercase seed":                 prefix + strings.ToLower(seed),
		"a seed split by a hyphen":         prefix + seed[:28] + "-" + seed[28:],
		"a seed inside a longer secret id": prefix + "example-" + seed + "-seed",
	}
}

// requireNoSeed requires err to be the seeded-name refusal and to hold no part of seed.
func requireNoSeed(t *testing.T, err error, seed string) {
	t.Helper()
	require.ErrorIs(t, err, ErrRefused)
	require.EqualError(t, err, seededNameRefusal)
	require.NotContains(t, strings.ToUpper(err.Error()), seed)
}

// Generate through the exported API, on the overlap that would end in the version-2 message: an
// early writer's version 1 lands between the read of version 1 and the add. With a seed in the
// name, Generate refuses before Describe, announces no key and the fake sees no call; with the
// fake's own name, the same setup does reach the version-2 message, which quotes the name.
func TestGenerateRefusesASeedInTheNameBeforeTheVersion2Path(t *testing.T) {
	seed, names := seededNames(t, "projects/example-project/secrets/")
	for name, secret := range names {
		t.Run(name, func(t *testing.T) {
			require.True(t, gcpSecretName.MatchString(secret), "the name has a GCP secret's form, so only the seed refuses it")
			f := newFakeGCP(t)
			earlier, _ := testnats.User(t)
			f.earlyWriter = &gcpVersion{value: earlier, state: enabled}
			s := gcpStoreOn(t, f)
			announced := false
			key, version, err := Generate(context.Background(), s, secret, func(string) error { announced = true; return nil })
			requireNoSeed(t, err, seed)
			require.Empty(t, key)
			require.Empty(t, version)
			require.False(t, announced, "no key is announced")
			require.Empty(t, f.recorded(), "no call reaches the store")
			require.Empty(t, f.versions, "nothing is written")
		})
	}
	t.Run("the fake's own name reaches the version-2 message", func(t *testing.T) {
		f := newFakeGCP(t)
		earlier, _ := testnats.User(t)
		f.earlyWriter = &gcpVersion{value: earlier, state: enabled}
		s := gcpStoreOn(t, f)
		_, version, err := Generate(context.Background(), s, gcpSecret, func(string) error { return nil })
		require.Equal(t, "2", version)
		require.ErrorContains(t, err, "version 2 was written, not version 1")
		require.ErrorContains(t, err, "gcloud secrets versions destroy "+gcpSecret+"/versions/1")
	})
}

// Check and Verify refuse a seed in the name before the store is asked, so neither line, which
// begins with the name, is written.
func TestCheckAndVerifyRefuseASeedInTheName(t *testing.T) {
	seed, names := seededNames(t, "projects/example-project/secrets/")
	_, publicKey := testnats.User(t)
	for name, secret := range names {
		t.Run(name, func(t *testing.T) {
			f := newFakeGCP(t)
			s := gcpStoreOn(t, f)
			line, err := Check(context.Background(), s, secret)
			requireNoSeed(t, err, seed)
			require.Empty(t, line)
			line, err = Verify(context.Background(), s, secret, publicKey)
			requireNoSeed(t, err, seed)
			require.Empty(t, line)
			require.Empty(t, f.recorded(), "no call reaches the store")
		})
	}
}

// Each method of each store, and each mode over each store, refuses a seed in the name before any
// request, so no store error or line, each of which names its secret, carries it.
func TestBothStoresRefuseASeedInTheName(t *testing.T) {
	for _, store := range []struct {
		name, prefix string
		open         func(t *testing.T) (Store, func() int)
	}{
		{"gcp-secret-manager", "projects/example-project/secrets/", func(t *testing.T) (Store, func() int) {
			f := newFakeGCP(t)
			s := gcpStoreOn(t, f)
			return s, func() int { return len(f.recorded()) }
		}},
		{"aws-secrets-manager", "example/envoy/", func(t *testing.T) (Store, func() int) {
			f := newFakeAWS(t)
			s := awsStoreOn(t, f)
			return s, f.requests
		}},
	} {
		seed, names := seededNames(t, store.prefix)
		for name, secret := range names {
			t.Run(store.name+"/"+name, func(t *testing.T) {
				s, requests := store.open(t)
				ctx := context.Background()
				_, err := s.Describe(ctx, secret)
				requireNoSeed(t, err, seed)
				_, err = s.Versions(ctx, secret)
				requireNoSeed(t, err, seed)
				version, err := s.Put(ctx, secret, []byte("example-value"))
				requireNoSeed(t, err, seed)
				require.Empty(t, version)
				_, value, err := s.Current(ctx, secret)
				requireNoSeed(t, err, seed)
				require.Empty(t, value)
				key, version, err := Generate(ctx, s, secret, func(string) error { return nil })
				requireNoSeed(t, err, seed)
				require.Empty(t, key)
				require.Empty(t, version)
				_, err = Check(ctx, s, secret)
				requireNoSeed(t, err, seed)
				_, publicKey := testnats.User(t)
				_, err = Verify(ctx, s, secret, publicKey)
				requireNoSeed(t, err, seed)
				require.Zero(t, requests(), "no request reaches the store")
			})
		}
	}
}
