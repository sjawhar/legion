package seed

import (
	"context"
	"testing"

	"github.com/stretchr/testify/require"
)

// testStoreContract runs, on stores open opens, each holding an empty secret named name, what
// generate relies on from every Store beyond Describe's metadata. Every store's tests run it.
//
// Above all, Put refuses a second first version with an error matching ErrRefused and writes
// nothing. generate's checks before Put cannot see a run that overlaps them: two runs can each find
// the secret empty, and only the store sees both writes, so a Put that wrote the second would have
// its seed served while the first run's key, already printed, fails verify.
func testStoreContract(t *testing.T, open func(t *testing.T) (s Store, name string)) {
	t.Helper()
	const first, second = "example-first-value", "example-second-value"
	ctx := context.Background()

	t.Run("Put writes an empty secret's first version, which Current serves", func(t *testing.T) {
		s, name := open(t)
		version, err := s.Put(ctx, name, []byte(first))
		require.NoError(t, err)
		require.NotEmpty(t, version)
		held, err := s.Versions(ctx, name)
		require.NoError(t, err)
		require.Equal(t, 1, held)
		served, value, err := s.Current(ctx, name)
		require.NoError(t, err)
		require.Equal(t, version, served)
		require.Equal(t, first, string(value))
	})

	t.Run("a second first version is refused with ErrRefused, and nothing is written", func(t *testing.T) {
		s, name := open(t)
		version, err := s.Put(ctx, name, []byte(first))
		require.NoError(t, err)
		again, err := s.Put(ctx, name, []byte(second))
		require.ErrorIs(t, err, ErrRefused)
		require.Empty(t, again, "a refusal answers no version id: nothing was written")
		held, err := s.Versions(ctx, name)
		require.NoError(t, err)
		require.Equal(t, 1, held)
		served, value, err := s.Current(ctx, name)
		require.NoError(t, err)
		require.Equal(t, version, served)
		require.Equal(t, first, string(value), "the first version is still the one served")
	})
}
