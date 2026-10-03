package main

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// `legion model-token` is what Oh My Pi runs as the model apiKey: it prints the access token alone
// on success, and on a refused sign-in prints nothing to stdout and exits non-zero with the reason.
func TestModelTokenPrintsTheAccessTokenOrExitsWithTheReason(t *testing.T) {
	for _, tc := range []struct {
		name   string
		refuse bool
		code   int
		stdout string
		stderr string
	}{
		{name: "signed in", code: 0, stdout: "access-token\n"},
		{name: "refused", refuse: true, code: 1, stderr: "NotAuthorizedException"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", "application/x-amz-json-1.1")
				switch r.Header.Get("X-Amz-Target") {
				case "AWSCognitoIdentityProviderService.InitiateAuth":
					_ = json.NewEncoder(w).Encode(map[string]any{"ChallengeName": "CUSTOM_CHALLENGE", "Session": "session"})
				case "AWSCognitoIdentityProviderService.RespondToAuthChallenge":
					if tc.refuse {
						w.Header().Set("X-Amzn-ErrorType", "NotAuthorizedException")
						w.WriteHeader(http.StatusBadRequest)
						_ = json.NewEncoder(w).Encode(map[string]string{"__type": "NotAuthorizedException", "message": "refused"})
						return
					}
					_ = json.NewEncoder(w).Encode(map[string]any{"AuthenticationResult": map[string]any{
						"AccessToken": "access-token", "RefreshToken": "refresh-token", "ExpiresIn": 3600,
					}})
				}
			}))
			defer server.Close()
			dir := t.TempDir()
			serviceAccountToken := filepath.Join(dir, "token")
			if err := os.WriteFile(serviceAccountToken, []byte("pod-token"), 0o600); err != nil {
				t.Fatal(err)
			}

			var stdout, stderr bytes.Buffer
			code := run(context.Background(), []string{"legion", "model-token",
				"--region", "example-region-1", "--client-id", "client-id", "--username", "machine-user",
				"--service-account-token-file", serviceAccountToken, "--cache-file", filepath.Join(dir, "model-token"),
				"--endpoint", server.URL}, &stdout, &stderr)
			if code != tc.code || stdout.String() != tc.stdout || !strings.Contains(stderr.String(), tc.stderr) {
				t.Fatalf("exit %d stdout %q stderr %q; want %d, %q, and %q", code, stdout.String(), stderr.String(), tc.code, tc.stdout, tc.stderr)
			}
		})
	}
}
