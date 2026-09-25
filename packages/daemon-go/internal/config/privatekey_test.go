package config

import (
	"encoding/base64"
	"strings"
	"testing"
)

// A stored App key is the base64 of its PEM text, written however the store's writer wrapped and
// padded it; every spelling Node's Buffer.from accepts decodes to the same key, and anything that
// is not a PEM block is refused.
func TestDecodePrivateKey(t *testing.T) {
	pem := "-----BEGIN PRIVATE KEY-----\n" + strings.Repeat("MIIEv?gIBADANBgkqhkiG9w0BAQEFAASC>", 3) + "\n-----END PRIVATE KEY-----"
	standard := base64.StdEncoding.EncodeToString([]byte(pem))
	if !strings.ContainsAny(standard, "+/") {
		t.Fatalf("the fixture's base64 %q has no + or /, so it cannot tell URL-safe from standard", standard)
	}
	var wrapped strings.Builder
	for i := 0; i < len(standard); i += 64 {
		wrapped.WriteString(standard[i:min(i+64, len(standard))] + "\n")
	}
	for _, tc := range []struct{ name, encoded string }{
		{"standard, padded", standard},
		{"unpadded", strings.TrimRight(standard, "=")},
		{"URL-safe", base64.URLEncoding.EncodeToString([]byte(pem))},
		{"wrapped at 64 columns, with a trailing newline", wrapped.String()},
		{"surrounded by whitespace", "  " + standard + "\r\n"},
		{"with a dangling final sextet", strings.TrimRight(standard, "=") + "A"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, err := DecodePrivateKey(tc.encoded)
			if err != nil || got != pem {
				t.Errorf("DecodePrivateKey = %q, %v; want the PEM text", got, err)
			}
		})
	}
	for _, tc := range []struct{ name, encoded, want string }{
		{"not base64", "a PEM key, not its base64!", "not base64"},
		{"base64 of something else", base64.StdEncoding.EncodeToString([]byte("ghs_not_a_key")), "does not decode to a PEM block (-----BEGIN …)"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, err := DecodePrivateKey(tc.encoded)
			if err == nil || err.Error() != tc.want {
				t.Errorf("DecodePrivateKey = %q, %v; want the refusal %q", got, err, tc.want)
			}
		})
	}
}
