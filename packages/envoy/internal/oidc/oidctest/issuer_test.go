package oidctest

import (
	"encoding/json"
	"net/http"
	"net/url"
	"strings"
	"testing"
)

// TestADeviceCodeExpires covers the one shape of the device grant no caller can reach by waiting:
// a device authorization whose life has run out. ExpireDeviceCodes ends the codes issued so far,
// so the person's approval is refused for a code that has expired, and the device's next poll is
// answered expired_token rather than kept pending for ever.
func TestADeviceCodeExpires(t *testing.T) {
	const clientID, clientSecret = "device-dev", "device-dev-client-secret"
	issuer := New(t)
	issuer.EnableDeviceFlow(issuer.PublishKey(t, "signing-key"), clientID, clientSecret)
	issuer.SignInByDefault(map[string]any{"sub": "alice@d.example", "email": "alice@d.example"})

	response, err := http.PostForm(issuer.URL()+"/device_authorization", url.Values{"client_id": {clientID}})
	if err != nil {
		t.Fatal(err)
	}
	var authorization struct {
		DeviceCode string `json:"device_code"`
		UserCode   string `json:"user_code"`
	}
	err = json.NewDecoder(response.Body).Decode(&authorization)
	response.Body.Close()
	if err != nil || authorization.DeviceCode == "" || authorization.UserCode == "" {
		t.Fatalf("the device authorization answered %+v (%v), want a device code and a user code", authorization, err)
	}

	issuer.ExpireDeviceCodes()

	if err := issuer.ApproveDeviceCode(authorization.UserCode); err == nil || !strings.Contains(err.Error(), "expired") {
		t.Errorf("approving the expired user code: %v, want a refusal saying it has expired", err)
	}
	response, err = http.PostForm(issuer.URL()+"/token", url.Values{
		"grant_type": {deviceCodeGrantType}, "device_code": {authorization.DeviceCode},
		"client_id": {clientID}, "client_secret": {clientSecret},
	})
	if err != nil {
		t.Fatal(err)
	}
	var answer struct {
		Error string `json:"error"`
	}
	err = json.NewDecoder(response.Body).Decode(&answer)
	status := response.StatusCode
	response.Body.Close()
	if err != nil || status != http.StatusBadRequest || answer.Error != "expired_token" {
		t.Fatalf("the poll after the device code expired: status %d, %+v (%v), want 400 expired_token", status, answer, err)
	}
}
