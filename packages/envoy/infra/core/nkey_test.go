package core_test

import (
	"fmt"
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

// HoldsSeed finds a seed UserSeedPublicKey accepts anywhere in a text: whole, inside a path, a URL
// or a message, percent-encoded, or inside a longer run of base32 letters. A value that is no seed
// as nkeys spells one is not found.
func TestHoldsSeed(t *testing.T) {
	rows := []struct {
		name string
		text string
		want bool
	}{
		{"the seed", readmeSeed, true},
		{"in a file path", "/tmp/example/" + readmeSeed, true},
		{"in a URL's path", "https://example.invalid/" + readmeSeed + "/x", true},
		{"in a URL's query", "https://example.invalid/?k=" + readmeSeed, true},
		{"in a URL's userinfo", "https://" + readmeSeed + "@example.invalid/", true},
		{"percent-encoded", "https://example.invalid/" + percentEncoded(readmeSeed), true},
		{"partly percent-encoded, beside a percent that encodes nothing", "%zz/" + readmeSeed[:10] + percentEncoded(readmeSeed[10:]), true},
		{"inside a longer run of base32 letters", "AB" + readmeSeed + "CD", true},
		{"in an error message", "failed to get shared config profile, " + readmeSeed, true},
		{"lowercase", strings.ToLower(readmeSeed), false},
		{"truncated", readmeSeed[:57], false},
		{"one character changed", readmeSeed[:5] + "A" + readmeSeed[6:], false},
		{"another spelling", strings.TrimSuffix(readmeSeed, "Y") + "Z", false},
		{"a public key", readmePublicKey, false},
		{"a region", "us-east-1", false},
		{"empty", "", false},
	}
	require.True(t, strings.HasSuffix(readmeSeed, "Y"), "the spelling row changes a final Y")
	for _, row := range rows {
		t.Run(row.name, func(t *testing.T) {
			require.Equal(t, row.want, core.HoldsSeed(row.text))
		})
	}
}

// percentEncoded spells every byte of text as %XX.
func percentEncoded(text string) string {
	var b strings.Builder
	for i := range len(text) {
		fmt.Fprintf(&b, "%%%02X", text[i])
	}
	return b.String()
}
