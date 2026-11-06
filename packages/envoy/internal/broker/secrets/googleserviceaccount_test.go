package secrets_test

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	"golang.org/x/oauth2"

	"github.com/sjawhar/envoy/internal/broker/secrets"
)

// serviceAccountEmail is the account a google-service-account secret of a person's fleet names.
const serviceAccountEmail = "dev-aditya@theorem-agent-identities.iam.gserviceaccount.com"

// serviceAccountPath is the resource the broker must ask for that account: the generateAccessToken
// verb keeps its colon, which an escaped path would have turned into %3A.
const serviceAccountPath = "/v1/projects/-/serviceAccounts/" + serviceAccountEmail + ":generateAccessToken"

// serviceAccountValue is the stored value of such a secret.
const serviceAccountValue = `{"service_account":"` + serviceAccountEmail + `"}`

// brokerOwnToken is the broker's own Google credential, the one every request to IAM carries.
const brokerOwnToken = "broker-own-token-4c91"

// brokerSource is the fixed credential a test mints with, so no Google credential is ever looked
// for on the machine running the test.
func brokerSource() oauth2.TokenSource {
	return oauth2.StaticTokenSource(&oauth2.Token{AccessToken: brokerOwnToken, TokenType: "Bearer"})
}

// iamServer is a fake IAM Service Account Credentials API answering handle, and the count of the
// requests it received.
func iamServer(t *testing.T, handle http.HandlerFunc) (*httptest.Server, *atomic.Int64) {
	t.Helper()
	var calls atomic.Int64
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		handle(w, r)
	}))
	t.Cleanup(server.Close)
	return server, &calls
}

// TestGoogleServiceAccountMintImpersonatesTheStoredAccount pins what the broker asks Google for:
// the stored account's generateAccessToken, as the broker's own credential, for one hour at the
// cloud-platform scope, and the bare access token back.
func TestGoogleServiceAccountMintImpersonatesTheStoredAccount(t *testing.T) {
	var gotPath, gotMethod, gotAuthorization, gotContentType, gotBody string
	server, calls := iamServer(t, func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		gotPath, gotMethod = r.URL.Path, r.Method
		gotAuthorization, gotContentType, gotBody = r.Header.Get("Authorization"), r.Header.Get("Content-Type"), string(body)
		_ = json.NewEncoder(w).Encode(map[string]string{"accessToken": "impersonated-access-token", "expireTime": "2026-11-06T13:00:00Z"})
	})

	got, err := (secrets.GoogleServiceAccount{BaseURL: server.URL, HTTP: server.Client(), Source: brokerSource()}).
		Mint(t.Context(), secrets.MintRequest{Name: "GCP_FLEET", Value: serviceAccountValue})
	if err != nil {
		t.Fatalf("Mint: %v", err)
	}
	if got != "impersonated-access-token" {
		t.Fatalf("Mint = %q, want the bare access token", got)
	}
	if calls.Load() != 1 {
		t.Fatalf("the IAM endpoint saw %d calls, want 1", calls.Load())
	}
	if gotMethod != http.MethodPost || gotPath != serviceAccountPath {
		t.Fatalf("the IAM endpoint was asked %s %s, want POST %s", gotMethod, gotPath, serviceAccountPath)
	}
	if gotAuthorization != "Bearer "+brokerOwnToken {
		t.Fatalf("Authorization = %q, want the broker's own bearer token", gotAuthorization)
	}
	if gotContentType != "application/json" {
		t.Fatalf("Content-Type = %q, want application/json", gotContentType)
	}
	var sent struct {
		Scope    []string `json:"scope"`
		Lifetime string   `json:"lifetime"`
	}
	if err := json.Unmarshal([]byte(gotBody), &sent); err != nil {
		t.Fatalf("the request body %q is not JSON: %v", gotBody, err)
	}
	if len(sent.Scope) != 1 || sent.Scope[0] != "https://www.googleapis.com/auth/cloud-platform" || sent.Lifetime != "3600s" {
		t.Fatalf("the request body named scope %v and lifetime %q, want the cloud-platform scope and 3600s", sent.Scope, sent.Lifetime)
	}
}

// TestGoogleServiceAccountMintNamesGooglesVerdict: an IAM refusal, which is what a broker that may
// not impersonate the account gets, is refused; an outage is the upstream's, so an operator reads
// the class and knows whether to fix an IAM binding or wait.
func TestGoogleServiceAccountMintNamesGooglesVerdict(t *testing.T) {
	for _, tc := range []struct {
		name  string
		code  int
		body  string
		class string
	}{
		{"refused", http.StatusForbidden, `{"error":{"code":403,"status":"PERMISSION_DENIED","message":"caller does not have permission"}}`, "refused"},
		{"outage", http.StatusServiceUnavailable, `{"error":{"code":503,"status":"UNAVAILABLE","message":"backend unavailable"}}`, "upstream"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			server, _ := iamServer(t, func(w http.ResponseWriter, _ *http.Request) {
				w.WriteHeader(tc.code)
				_, _ = io.WriteString(w, tc.body)
			})

			_, err := (secrets.GoogleServiceAccount{BaseURL: server.URL, HTTP: server.Client(), Source: brokerSource()}).
				Mint(t.Context(), secrets.MintRequest{Value: serviceAccountValue})
			if err == nil || secrets.MintErrorClass(err) != tc.class {
				t.Fatalf("Mint error = %v (class %s), want class %s", err, secrets.MintErrorClass(err), tc.class)
			}
			for _, quoted := range []string{"caller does not have permission", "backend unavailable", "PERMISSION_DENIED", "UNAVAILABLE"} {
				if strings.Contains(err.Error(), quoted) {
					t.Fatalf("Mint error %q quotes Google's body %q", err, quoted)
				}
			}
		})
	}
}

// TestGoogleServiceAccountMintErrorsQuoteNoToken: neither the broker's own credential, the token
// Google minted nor the stored value reaches an error the broker logs and answers a session with,
// however the answer fails.
func TestGoogleServiceAccountMintErrorsQuoteNoToken(t *testing.T) {
	const minted = "impersonated-access-token-7ab2"
	for _, tc := range []struct {
		name   string
		status int
		body   string
	}{
		{"refusal echoing the tokens", http.StatusForbidden, `{"error":{"message":"` + brokerOwnToken + ` ` + minted + ` ` + serviceAccountEmail + `"}}`},
		{"an answer that is no JSON object", http.StatusOK, brokerOwnToken + " " + minted},
		{"an answer with no token", http.StatusOK, `{"expireTime":"2026-11-06T13:00:00Z"}`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			server, _ := iamServer(t, func(w http.ResponseWriter, _ *http.Request) {
				w.WriteHeader(tc.status)
				_, _ = io.WriteString(w, tc.body)
			})

			_, err := (secrets.GoogleServiceAccount{BaseURL: server.URL, HTTP: server.Client(), Source: brokerSource()}).
				Mint(t.Context(), secrets.MintRequest{Value: serviceAccountValue})
			if err == nil {
				t.Fatal("Mint: want an error")
			}
			if class := secrets.MintErrorClass(err); class != "refused" && class != "upstream" {
				t.Fatalf("Mint error = %v, class %s; want refused or upstream", err, class)
			}
			for _, secret := range []string{brokerOwnToken, minted, serviceAccountValue, tc.body} {
				if strings.Contains(err.Error(), secret) {
					t.Fatalf("Mint error %q quotes %q", err, secret)
				}
			}
		})
	}
}

// TestGoogleServiceAccountMintRefusesAValueThatIsNoServiceAccount: a person's address, a list of
// accounts and anything else that is not one service account email are the stored value's fault,
// and the shape is checked before any credential is resolved or any call made, so a mistyped value
// never asks Google to impersonate anything.
func TestGoogleServiceAccountMintRefusesAValueThatIsNoServiceAccount(t *testing.T) {
	for _, tc := range []struct {
		name  string
		value string
	}{
		{"a person's email", `{"service_account":"aditya@theorem.dev"}`},
		{"a person's email at the service account domain", `{"service_account":"aditya@theorem.dev.iam.gserviceaccount.com."}`},
		{"two accounts", `{"service_account":"dev-aditya@p.iam.gserviceaccount.com,dev-sami@p.iam.gserviceaccount.com"}`},
		{"an empty field", `{"service_account":""}`},
		{"a missing field", `{"project":"theorem-agent-identities"}`},
		{"no JSON object", "dev-aditya@theorem-agent-identities.iam.gserviceaccount.com"},
		{"a path of its own", `{"service_account":"../../projects/other/serviceAccounts/x@p.iam.gserviceaccount.com"}`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			server, calls := iamServer(t, func(w http.ResponseWriter, _ *http.Request) {
				_ = json.NewEncoder(w).Encode(map[string]string{"accessToken": "impersonated-access-token"})
			})

			// No Source: a value this minter refuses is refused before it looks for the broker's
			// own credentials, which this machine does not have.
			_, err := (secrets.GoogleServiceAccount{BaseURL: server.URL, HTTP: server.Client()}).
				Mint(t.Context(), secrets.MintRequest{Value: tc.value})
			if err == nil || secrets.MintErrorClass(err) != "stored-value" {
				t.Fatalf("Mint error = %v (class %s), want class stored-value", err, secrets.MintErrorClass(err))
			}
			if calls.Load() != 0 {
				t.Fatalf("the IAM endpoint saw %d calls for a value that is no service account, want none", calls.Load())
			}
		})
	}
}
