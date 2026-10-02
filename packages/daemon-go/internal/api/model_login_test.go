package api

import (
	"encoding/json"
	"strings"
	"testing"
)

func TestStateCarriesModelLoginFailureWithoutCredentials(t *testing.T) {
	encoded, err := json.Marshal(State{ModelLogin: &ModelLoginView{State: "error", Error: "Cognito login refused"}})
	if err != nil {
		t.Fatal(err)
	}
	text := string(encoded)
	for _, want := range []string{`"modelLogin":{"state":"error","error":"Cognito login refused"}`, `"issues":{}`, `"pendingStatusWrites":[]`} {
		if !strings.Contains(text, want) {
			t.Fatalf("state JSON %s does not contain %s", text, want)
		}
	}
	for _, secret := range []string{"accessToken", "refreshToken", "password"} {
		if strings.Contains(text, secret) {
			t.Fatalf("state JSON exposes %q: %s", secret, text)
		}
	}
}
