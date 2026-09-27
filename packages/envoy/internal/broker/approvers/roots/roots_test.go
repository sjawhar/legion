package roots

import (
	"encoding/hex"
	"testing"

	"crypto/sha256"
)

// wantSHA256 pins each embedded root's exact bytes. A silent swap of a trust root — a wrong file,
// a truncated download, a tampered PEM — must fail the build rather than quietly widening or
// narrowing what production attestation trusts.
var wantSHA256 = map[string]string{
	"yubico-ca-1.pem":      "9271d914d48d05487666703586aea27d9a69ad0c8ddf8c2fc4c8734a04285887",
	"yubico-fido-ca-1.pem": "bb479d380ef940c5469e008b12a2f35137b884696963dc845c0ddce37f2f9bbf",
	"yubico-fido-ca-2.pem": "c3429d7a5d990ec702a1f74e1ab713651c65e5678f05db4162bebcf34e33d0cd",
}

func TestEmbeddedRootsMatchPinnedSHA256(t *testing.T) {
	if len(Names) != len(wantSHA256) {
		t.Fatalf("Names has %d entries, want %d", len(Names), len(wantSHA256))
	}
	for _, name := range Names {
		data, err := Files.ReadFile(name)
		if err != nil {
			t.Fatalf("read embedded %s: %v", name, err)
		}
		sum := sha256.Sum256(data)
		got := hex.EncodeToString(sum[:])
		want, ok := wantSHA256[name]
		if !ok {
			t.Fatalf("no pinned hash for %s", name)
		}
		if got != want {
			t.Fatalf("%s sha256 = %s, want %s (the trust root's bytes changed)", name, got, want)
		}
	}
}
