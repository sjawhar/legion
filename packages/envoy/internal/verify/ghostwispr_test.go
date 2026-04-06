package verify

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"testing"
)

func ghostWisprSignature(secret string, body []byte) string {
	mac := hmac.New(sha256.New, []byte(secret))
	_, _ = mac.Write(body)
	return "sha256=" + hex.EncodeToString(mac.Sum(nil))
}

func TestGhostWispr(t *testing.T) {
	body := []byte(`{"event_type":"session_ended","payload":{"session_id":"20260326041405"}}`)
	alteredBody := []byte(`{"event_type":"session_ended","payload":{"session_id":"different"}}`)
	validSignature := ghostWisprSignature("secret", body)

	tests := []struct {
		name      string
		secret    string
		body      []byte
		signature string
		want      bool
	}{
		{name: "accepts matching signature", secret: "secret", body: body, signature: validSignature, want: true},
		{name: "skips verification when secret is empty", secret: "", body: body, signature: "", want: true},
		{name: "rejects wrong secret", secret: "wrong", body: body, signature: validSignature, want: false},
		{name: "rejects tampered body", secret: "secret", body: alteredBody, signature: validSignature, want: false},
		{name: "rejects wrong digest", secret: "secret", body: body, signature: "sha256=deadbeef", want: false},
		{name: "rejects signature with extra characters", secret: "secret", body: body, signature: validSignature + "00", want: false},
		{name: "rejects empty signature when secret is configured", secret: "secret", body: body, signature: "", want: false},
		{name: "rejects signature without required prefix", secret: "secret", body: body, signature: validSignature[len("sha256="):], want: false},
		{name: "accepts uppercase prefix after normalization", secret: "secret", body: body, signature: " SHA256=" + validSignature[len("sha256="):] + " ", want: true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := GhostWispr(tt.secret, tt.body, tt.signature); got != tt.want {
				t.Fatalf("GhostWispr(secret=%q, body=%q, signature=%q) = %v, want %v", tt.secret, string(tt.body), tt.signature, got, tt.want)
			}
		})
	}
}
