package main

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
)

// modelTokenCognito is a fake Cognito for `legion model-token` that counts the requests it gets. It
// signs the pod in, or, with refuse, refuses the custom challenge echoing the answer back in its
// message and request id, as a hostile endpoint could.
func modelTokenCognito(t *testing.T, refuse bool) (*httptest.Server, *atomic.Int32) {
	t.Helper()
	var calls atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		var body struct{ ChallengeResponses map[string]string }
		_ = json.NewDecoder(r.Body).Decode(&body)
		w.Header().Set("Content-Type", "application/x-amz-json-1.1")
		switch r.Header.Get("X-Amz-Target") {
		case "AWSCognitoIdentityProviderService.InitiateAuth":
			_ = json.NewEncoder(w).Encode(map[string]any{"ChallengeName": "CUSTOM_CHALLENGE", "Session": "session"})
		case "AWSCognitoIdentityProviderService.RespondToAuthChallenge":
			if refuse {
				echoed := body.ChallengeResponses["ANSWER"]
				w.Header().Set("X-Amzn-ErrorType", "NotAuthorizedException")
				w.Header().Set("X-Amzn-RequestId", echoed)
				w.WriteHeader(http.StatusBadRequest)
				_ = json.NewEncoder(w).Encode(map[string]string{"__type": "NotAuthorizedException", "message": "refused " + echoed})
				return
			}
			_ = json.NewEncoder(w).Encode(map[string]any{"AuthenticationResult": map[string]any{
				"AccessToken": "access-token", "RefreshToken": "refresh-token", "ExpiresIn": 3600,
			}})
		}
	}))
	t.Cleanup(server.Close)
	return server, &calls
}

// rawModelTokenCognito returns a Cognito fake that sends malformed bytes containing a request's
// own challenge answer after its valid InitiateAuth response.
func rawModelTokenCognito(t *testing.T, malformed func(answer string) string) string {
	t.Helper()
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = listener.Close() })
	go func() {
		for {
			connection, err := listener.Accept()
			if err != nil {
				return
			}
			func() {
				defer connection.Close()
				request, err := http.ReadRequest(bufio.NewReader(connection))
				if err != nil {
					return
				}
				defer request.Body.Close()
				var body struct{ ChallengeResponses map[string]string }
				if err := json.NewDecoder(request.Body).Decode(&body); err != nil {
					return
				}
				switch request.Header.Get("X-Amz-Target") {
				case "AWSCognitoIdentityProviderService.InitiateAuth":
					response := `{"ChallengeName":"CUSTOM_CHALLENGE","Session":"session"}`
					_, _ = fmt.Fprintf(connection, "HTTP/1.1 200 OK\r\nContent-Type: application/x-amz-json-1.1\r\nContent-Length: %d\r\nConnection: close\r\n\r\n%s", len(response), response)
				case "AWSCognitoIdentityProviderService.RespondToAuthChallenge":
					_, _ = fmt.Fprint(connection, malformed(body.ChallengeResponses["ANSWER"]))
				}
			}()
		}
	}()
	return "http://" + listener.Addr().String()
}

// modelTokenFlags is every flag `legion model-token` needs, with a valid value.
func modelTokenFlags(t *testing.T, endpoint string) [][2]string {
	return [][2]string{
		{"region", "example-region-1"}, {"client-id", "client-id"}, {"username", "machine-user"},
		{"service-account-token-file", writeFile(t, "token", "pod-token")}, {"endpoint", endpoint},
	}
}

// `legion model-token` is what Oh My Pi runs as the model apiKey: it prints the access token alone
// on success, and on a refused sign-in prints nothing to stdout and exits 1 with Cognito's error
// code, never the endpoint's own text, which can echo the pod's token.
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
			server, _ := modelTokenCognito(t, tc.refuse)
			args := []string{"legion", "model-token"}
			for _, flag := range modelTokenFlags(t, server.URL) {
				args = append(args, "--"+flag[0], flag[1])
			}

			var stdout, stderr bytes.Buffer
			code := run(context.Background(), args, &stdout, &stderr)
			if code != tc.code || stdout.String() != tc.stdout || !strings.Contains(stderr.String(), tc.stderr) {
				t.Fatalf("exit %d stdout %q stderr %q; want %d, %q, and %q", code, stdout.String(), stderr.String(), tc.code, tc.stdout, tc.stderr)
			}
			if strings.Contains(stderr.String(), "pod-token") {
				t.Fatalf("stderr %q repeats the endpoint's echo of the pod's token", stderr.String())
			}
		})
	}
}

// A malformed HTTP answer can quote the challenge answer in net/http's error, so the command must
// replace it with its own fixed error before it reaches stderr.
func TestModelTokenHidesTokenFromMalformedHTTPAnswer(t *testing.T) {
	for _, tc := range []struct {
		name      string
		malformed func(answer string) string
	}{
		{name: "malformed status", malformed: func(answer string) string {
			return "HTTP/1.1 " + answer + " x\r\n\r\n"
		}},
		{name: "non HTTP response", malformed: func(answer string) string {
			return answer + "\r\n\r\n"
		}},
		{name: "malformed header", malformed: func(answer string) string {
			return "HTTP/1.1 200 OK\r\n" + answer + "\r\n\r\n"
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			args := []string{"legion", "model-token"}
			for _, flag := range modelTokenFlags(t, rawModelTokenCognito(t, tc.malformed)) {
				args = append(args, "--"+flag[0], flag[1])
			}

			var stdout, stderr bytes.Buffer
			code := run(context.Background(), args, &stdout, &stderr)
			if code != 1 || stdout.String() != "" {
				t.Fatalf("exit %d stdout %q; want 1 and nothing on stdout", code, stdout.String())
			}
			if !strings.Contains(stderr.String(), "the endpoint's answer is not an HTTP response") {
				t.Fatalf("stderr %q; want the fixed malformed-response error", stderr.String())
			}
			if strings.Contains(stderr.String(), "pod-token") {
				t.Fatalf("stderr %q repeats the endpoint's malformed answer", stderr.String())
			}
		})
	}
}

// A required flag left out, or given empty, is refused by name before Cognito is called.
func TestModelTokenRefusesAMissingFlagBeforeCallingCognito(t *testing.T) {
	for _, missing := range []string{"region", "client-id", "username", "service-account-token-file"} {
		for _, form := range []string{"absent", "empty"} {
			t.Run(missing+"/"+form, func(t *testing.T) {
				server, calls := modelTokenCognito(t, false)
				args := []string{"legion", "model-token"}
				for _, flag := range modelTokenFlags(t, server.URL) {
					switch {
					case flag[0] != missing:
						args = append(args, "--"+flag[0], flag[1])
					case form == "empty":
						args = append(args, "--"+flag[0], "")
					}
				}

				var stdout, stderr bytes.Buffer
				code := run(context.Background(), args, &stdout, &stderr)
				if code != 1 || stdout.String() != "" {
					t.Fatalf("exit %d, stdout %q; want 1 and nothing on stdout", code, stdout.String())
				}
				if want := "legion model-token: --" + missing + " is required"; !strings.Contains(stderr.String(), want) {
					t.Fatalf("stderr = %q, want %q", stderr.String(), want)
				}
				if calls.Load() != 0 {
					t.Fatalf("Cognito got %d requests, want none", calls.Load())
				}
			})
		}
	}
}
