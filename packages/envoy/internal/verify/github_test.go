package verify

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"testing"
)

func TestGithub(t *testing.T) {
	body := []byte(`{"hello":"world"}`)
	mac := hmac.New(sha256.New, []byte("secret"))
	mac.Write(body)
	sig := "sha256=" + hex.EncodeToString(mac.Sum(nil))
	if !Github("secret", body, sig) {
		t.Fatal("expected signature to verify")
	}
	if Github("wrong", body, sig) {
		t.Fatal("expected wrong secret to fail")
	}
	if Github("secret", body, "sha256=deadbeef") {
		t.Fatal("expected wrong digest to fail")
	}
}
