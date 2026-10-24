package secrets_test

import (
	"bufio"
	"context"
	"encoding/json"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/sjawhar/envoy/internal/broker/secrets"
	"github.com/sjawhar/envoy/internal/dispatch/githubapp/githubapptest"
)

// googleCanary is the refresh token a Google login fixture stores, which no error may quote.
const googleCanary = "refresh-token-canary-5b1e"

func googleLoginValue() string {
	return `{"type":"authorized_user","client_id":"client-id","client_secret":"client-secret","refresh_token":"` + googleCanary + `"}`
}

// echoStatusLine is an upstream that answers every request with a status line whose status code is
// echo(request), which Go's HTTP client refuses as malformed, quoting the field it read.
func echoStatusLine(t *testing.T, echo func(*http.Request) string) string {
	t.Helper()
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = listener.Close() })
	go func() {
		for {
			conn, err := listener.Accept()
			if err != nil {
				return
			}
			go func() {
				defer conn.Close()
				request, err := http.ReadRequest(bufio.NewReader(conn))
				if err != nil {
					return
				}
				_, _ = conn.Write([]byte("HTTP/1.1 " + echo(request) + " OK\r\nContent-Length: 0\r\n\r\n"))
			}()
		}
	}()
	return "http://" + listener.Addr().String()
}

// appJWT is the App JWT a GitHub request carries.
func appJWT(r *http.Request) string { return strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ") }

// refreshToken is the refresh token a Google token request carries.
func refreshToken(r *http.Request) string {
	if err := r.ParseForm(); err != nil {
		return ""
	}
	return r.PostForm.Get("refresh_token")
}

// TestAMintErrorNeverQuotesWhatTheUpstreamEchoes: an upstream that echoes the credential a mint
// sent, in a status line Go's HTTP client refuses, in the body of a refusal or an outage, or as
// Google's error_subtype, never gets it into the minter's error, which the broker logs and answers
// the session with. Each answer still names its class.
func TestAMintErrorNeverQuotesWhatTheUpstreamEchoes(t *testing.T) {
	key, _ := githubapptest.Key(t)
	app := githubAppValue(t, "123456", key)
	var jwt string
	recordJWT := func(r *http.Request) string { jwt = appJWT(r); return jwt }
	answer := func(status int, body func(*http.Request) string) string {
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(status)
			_, _ = w.Write([]byte(body(r)))
		}))
		t.Cleanup(server.Close)
		return server.URL
	}
	for name, tc := range map[string]struct {
		mint   func(ctx context.Context) error
		canary func() string
		class  string
	}{
		"github status line": {
			mint: func(ctx context.Context) error {
				_, err := secrets.GitHubApp{Base: echoStatusLine(t, recordJWT)}.Mint(ctx, mintFor(app, "acme"))
				return err
			},
			canary: func() string { return jwt }, class: "upstream",
		},
		"github refusal body": {
			mint: func(ctx context.Context) error {
				_, err := secrets.GitHubApp{Base: answer(http.StatusUnauthorized, recordJWT)}.Mint(ctx, mintFor(app, "acme"))
				return err
			},
			canary: func() string { return jwt }, class: "refused",
		},
		"github outage body": {
			mint: func(ctx context.Context) error {
				_, err := secrets.GitHubApp{Base: answer(http.StatusBadGateway, recordJWT)}.Mint(ctx, mintFor(app, "acme"))
				return err
			},
			canary: func() string { return jwt }, class: "upstream",
		},
		"google status line": {
			mint: func(ctx context.Context) error {
				_, err := secrets.GoogleLogin{TokenURL: echoStatusLine(t, refreshToken)}.Mint(ctx, secrets.MintRequest{Value: googleLoginValue()})
				return err
			},
			canary: func() string { return googleCanary }, class: "upstream",
		},
		"google refusal body": {
			mint: func(ctx context.Context) error {
				_, err := secrets.GoogleLogin{TokenURL: answer(http.StatusBadRequest, refreshToken)}.Mint(ctx, secrets.MintRequest{Value: googleLoginValue()})
				return err
			},
			canary: func() string { return googleCanary }, class: "refused",
		},
		"google outage body": {
			mint: func(ctx context.Context) error {
				_, err := secrets.GoogleLogin{TokenURL: answer(http.StatusServiceUnavailable, refreshToken)}.Mint(ctx, secrets.MintRequest{Value: googleLoginValue()})
				return err
			},
			canary: func() string { return googleCanary }, class: "upstream",
		},
		"google error_subtype": {
			mint: func(ctx context.Context) error {
				url := answer(http.StatusBadRequest, func(r *http.Request) string {
					reply, _ := json.Marshal(map[string]string{"error": "invalid_grant", "error_subtype": refreshToken(r)})
					return string(reply)
				})
				_, err := secrets.GoogleLogin{TokenURL: url}.Mint(ctx, secrets.MintRequest{Value: googleLoginValue()})
				return err
			},
			canary: func() string { return googleCanary }, class: "refused",
		},
	} {
		t.Run(name, func(t *testing.T) {
			err := tc.mint(t.Context())
			canary := tc.canary()
			if canary == "" {
				t.Fatal("the upstream never saw the credential, so nothing was echoed")
			}
			if err == nil || strings.Contains(err.Error(), canary) {
				t.Fatalf("Mint error quotes the credential the upstream echoed (err == nil: %t)", err == nil)
			}
			if got := secrets.MintErrorClass(err); got != tc.class {
				t.Fatalf("Mint error %q has class %s; want %s", err, got, tc.class)
			}
		})
	}
}

// TestAGoogleRefusalNamesOnlyTheSubtypesGoogleDocuments: invalid_rapt and rapt_required, the two
// subtypes of a login that must sign in again, are named as Google answers them; any other value is
// named "unknown".
func TestAGoogleRefusalNamesOnlyTheSubtypesGoogleDocuments(t *testing.T) {
	for subtype, want := range map[string]string{"invalid_rapt": "(invalid_rapt)", "rapt_required": "(rapt_required)", "something_new": "(unknown)"} {
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(http.StatusBadRequest)
			_ = json.NewEncoder(w).Encode(map[string]string{"error": "invalid_grant", "error_subtype": subtype})
		}))
		_, err := secrets.GoogleLogin{TokenURL: server.URL}.Mint(t.Context(), secrets.MintRequest{Value: googleLoginValue()})
		server.Close()
		if err == nil || !strings.Contains(err.Error(), want) || secrets.MintErrorClass(err) != "refused" {
			t.Fatalf("Mint with error_subtype %q = %v; want refused naming %s", subtype, err, want)
		}
	}
}
