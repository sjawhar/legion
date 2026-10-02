package modellogin

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

func TestCognitoClientUsesPasswordAndRefreshAuthenticationFlows(t *testing.T) {
	var calls []map[string]any
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if got := r.Header.Get("X-Amz-Target"); got != "AWSCognitoIdentityProviderService.InitiateAuth" {
			t.Fatalf("X-Amz-Target = %q", got)
		}
		var request map[string]any
		if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
			t.Fatal(err)
		}
		calls = append(calls, request)
		switch request["AuthFlow"] {
		case "USER_PASSWORD_AUTH":
			_ = json.NewEncoder(w).Encode(map[string]any{"AuthenticationResult": map[string]any{
				"AccessToken": "access-1", "RefreshToken": "refresh-1", "ExpiresIn": 3600,
			}})
		case "REFRESH_TOKEN_AUTH":
			_ = json.NewEncoder(w).Encode(map[string]any{"AuthenticationResult": map[string]any{
				"AccessToken": "access-2", "ExpiresIn": 1800,
			}})
		default:
			t.Fatalf("unexpected AuthFlow %#v", request["AuthFlow"])
		}
	}))
	defer server.Close()

	client := NewCognitoClient(CognitoConfig{Endpoint: server.URL, Region: "example-region-1", ClientID: "example-client-id"})
	initial, err := client.PasswordAuth(context.Background(), "machine-user", "machine-password")
	if err != nil {
		t.Fatalf("PasswordAuth: %v", err)
	}
	if initial != (Result{AccessToken: "access-1", RefreshToken: "refresh-1", ExpiresIn: time.Hour}) {
		t.Fatalf("PasswordAuth result = %#v", initial)
	}
	refreshed, err := client.RefreshAuth(context.Background(), "refresh-1")
	if err != nil {
		t.Fatalf("RefreshAuth: %v", err)
	}
	if refreshed != (Result{AccessToken: "access-2", ExpiresIn: 30 * time.Minute}) {
		t.Fatalf("RefreshAuth result = %#v", refreshed)
	}
	if len(calls) != 2 {
		t.Fatalf("InitiateAuth calls = %d, want 2", len(calls))
	}
	passwordParameters, ok := calls[0]["AuthParameters"].(map[string]any)
	if !ok || passwordParameters["USERNAME"] != "machine-user" || passwordParameters["PASSWORD"] != "machine-password" {
		t.Fatalf("password auth parameters = %#v", calls[0]["AuthParameters"])
	}
	refreshParameters, ok := calls[1]["AuthParameters"].(map[string]any)
	if !ok || refreshParameters["REFRESH_TOKEN"] != "refresh-1" {
		t.Fatalf("refresh auth parameters = %#v", calls[1]["AuthParameters"])
	}
	for _, call := range calls {
		if call["ClientId"] != "example-client-id" {
			t.Fatalf("client id = %#v", call["ClientId"])
		}
	}
}
