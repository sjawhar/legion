package core_test

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/sjawhar/envoy/infra/core"
)

// nats-io's own vector: a user seed and its public key, as nkeys' README prints them
// (github.com/nats-io/nkeys v0.4.15 README.md lines 45 and 49).
const (
	readmeSeed      = "SUAKYRHVIOREXV7EUZTBHUHL7NUMHPMAS7QMDU3GTIUWEI5LDNOXD43IZY"
	readmePublicKey = "UD466L6EBCM3YY5HEGHJANNTN4LSKTSUXTH7RILHCKEQMQHTBNLHJJXT"
)

// UserSeedPublicKey derives a user seed's public key, and otherwise says why the value is none,
// one reason per check in its order, without quoting the value.
func TestUserSeedPublicKey(t *testing.T) {
	public, err := core.UserSeedPublicKey([]byte(readmeSeed))
	require.NoError(t, err)
	require.Equal(t, readmePublicKey, public)

	require.True(t, strings.HasSuffix(readmeSeed, "Y"), "the spelling row changes a final Y")
	rows := []struct{ name, value, why string }{
		{"a trailing line break", readmeSeed + "\n", "the value is not base32"},
		{"lowercase", strings.ToLower(readmeSeed), "the value is not base32"},
		{"empty", "", "the value decodes to 0 bytes, not the 36 of a seed"},
		{"a user public key", readmePublicKey, "the value decodes to 35 bytes, not the 36 of a seed"},
		{"one character changed", readmeSeed[:5] + "A" + readmeSeed[6:], "the value's checksum does not match"},
		// The README seed's 32 bytes behind the prefix bytes (0, 0), with a valid checksum.
		{"another prefix byte", "AAAKYRHVIOREXV7EUZTBHUHL7NUMHPMAS7QMDU3GTIUWEI5LDNOXD43I5I", "the value is not a seed (wrong prefix byte)"},
		// The same bytes behind a seed's prefix of the account type: an account seed.
		{"an account seed", "SAAKYRHVIOREXV7EUZTBHUHL7NUMHPMAS7QMDU3GTIUWEI5LDNOXD443AI", "the value is a seed, but not a user's"},
		// Y -> Z changes only the last character's two unused bits: the same bytes, another spelling.
		{"another spelling", strings.TrimSuffix(readmeSeed, "Y") + "Z", "the value is not the canonical encoding of its seed"},
	}
	for _, row := range rows {
		t.Run(row.name, func(t *testing.T) {
			public, err := core.UserSeedPublicKey([]byte(row.value))
			require.Empty(t, public)
			require.EqualError(t, err, row.why)
		})
	}
}
