package seed

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

// holdsSeed finds a seed core.UserSeedPublicKey accepts anywhere in a text: whole, inside a path, a
// URL or a message, percent-encoded, or inside a longer run of base32 letters. A value that is no
// seed as nkeys spells one is not found.
func TestHoldsSeed(t *testing.T) {
	// nats-io's vector: nkeys v0.4.15 README.md line 45, and its public key, line 49.
	const seed = "SUAKYRHVIOREXV7EUZTBHUHL7NUMHPMAS7QMDU3GTIUWEI5LDNOXD43IZY"
	const publicKey = "UD466L6EBCM3YY5HEGHJANNTN4LSKTSUXTH7RILHCKEQMQHTBNLHJJXT"
	rows := []struct {
		name string
		text string
		want bool
	}{
		{"the seed", seed, true},
		{"in a file path", "/tmp/example/" + seed, true},
		{"in a URL's path", "https://example.invalid/" + seed + "/x", true},
		{"in a URL's query", "https://example.invalid/?k=" + seed, true},
		{"in a URL's userinfo", "https://" + seed + "@example.invalid/", true},
		{"percent-encoded", "https://example.invalid/" + percentEncoded(seed), true},
		{"partly percent-encoded, beside a percent that encodes nothing", "%zz/" + seed[:10] + percentEncoded(seed[10:]), true},
		{"inside a longer run of base32 letters", "AB" + seed + "CD", true},
		{"in an error message", "failed to get shared config profile, " + seed, true},
		{"lowercase", strings.ToLower(seed), false},
		{"truncated", seed[:57], false},
		{"one character changed", seed[:5] + "A" + seed[6:], false},
		{"another spelling", strings.TrimSuffix(seed, "Y") + "Z", false},
		{"a public key", publicKey, false},
		{"a region", "us-east-1", false},
		{"empty", "", false},
	}
	require.True(t, strings.HasSuffix(seed, "Y"), "the spelling row changes a final Y")
	for _, row := range rows {
		t.Run(row.name, func(t *testing.T) {
			require.Equal(t, row.want, holdsSeed(row.text))
		})
	}
}
