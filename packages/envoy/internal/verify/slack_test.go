package verify

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"testing"
	"time"
)

func TestSlack(t *testing.T) {
	body := []byte(`{"type":"event_callback"}`)
	ts := fmt.Sprintf("%d", time.Now().Unix())
	mac := hmac.New(sha256.New, []byte("secret"))
	mac.Write([]byte(fmt.Sprintf("v0:%s:%s", ts, string(body))))
	sig := "v0=" + hex.EncodeToString(mac.Sum(nil))
	if !Slack("secret", body, ts, sig) {
		t.Fatal("expected signature to verify")
	}
	if Slack("wrong", body, ts, sig) {
		t.Fatal("expected wrong secret to fail")
	}
	old := fmt.Sprintf("%d", time.Now().Add(-10*time.Minute).Unix())
	if Slack("secret", body, old, sig) {
		t.Fatal("expected stale timestamp to fail")
	}
}
