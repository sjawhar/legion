// packages/envoy/internal/googleidentity/googleidentity_test.go
//
// The metadata reader against a stub metadata server: what it asks for, what it answers, how it
// refuses, and that no proxy variable can take the token.
package googleidentity

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
)

// stubMetadata stands up a metadata server at a host GCE_METADATA_HOST names, answering handler.
// It returns the requests it was asked, so a test reads back the path and the query the reader
// sent.
func stubMetadata(t *testing.T, handler http.HandlerFunc) *[]*http.Request {
	t.Helper()
	var asked []*http.Request
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		asked = append(asked, r)
		handler(w, r)
	}))
	t.Cleanup(server.Close)
	t.Setenv(MetadataHostVar, strings.TrimPrefix(server.URL, "http://"))
	return &asked
}

// TestTokenAsksTheMetadataServerForAFullFormatIdentityToken: the reader asks the default service
// account's identity path for the audience it was given, with format=full and the header Google
// requires, and answers the token with no whitespace around it.
func TestTokenAsksTheMetadataServerForAFullFormatIdentityToken(t *testing.T) {
	asked := stubMetadata(t, func(w http.ResponseWriter, _ *http.Request) {
		w.Write([]byte("header.payload.signature\n"))
	})

	token, err := Token(context.Background(), "example-audience")
	if err != nil {
		t.Fatalf("Token: %v", err)
	}
	if token != "header.payload.signature" {
		t.Fatalf("Token = %q, want the metadata server's answer with its trailing newline trimmed", token)
	}
	if len(*asked) != 1 {
		t.Fatalf("the metadata server was asked %d times, want once", len(*asked))
	}
	request := (*asked)[0]
	if request.URL.Path != identityPath {
		t.Errorf("the reader asked %q, want %q: the identity token of the instance's default service account", request.URL.Path, identityPath)
	}
	if got := request.URL.Query().Get("audience"); got != "example-audience" {
		t.Errorf("audience = %q, want the one the caller named", got)
	}
	if got := request.URL.Query().Get("format"); got != "full" {
		t.Errorf("format = %q, want full: without it Google mints a token carrying no google.compute_engine claims", got)
	}
	if got := request.Header.Get("Metadata-Flavor"); got != "Google" {
		t.Errorf("Metadata-Flavor = %q, want Google: the metadata server refuses a request without it", got)
	}
}

// TestTokenIgnoresEveryProxyVariable: the reader's transport has no proxy at all, so a machine
// whose environment names one hands that proxy nothing. The proxy here answers every request
// with a token of its own, so a reader that honoured it would pass the first check below and
// fail the count.
func TestTokenIgnoresEveryProxyVariable(t *testing.T) {
	var proxied atomic.Int64
	proxy := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		proxied.Add(1)
		w.Write([]byte("the.proxys.token"))
	}))
	t.Cleanup(proxy.Close)
	for _, name := range []string{"HTTP_PROXY", "http_proxy", "HTTPS_PROXY", "ALL_PROXY"} {
		t.Setenv(name, proxy.URL)
	}
	stubMetadata(t, func(w http.ResponseWriter, _ *http.Request) {
		w.Write([]byte("the.machines.token"))
	})

	token, err := Token(context.Background(), "example-audience")
	if err != nil {
		t.Fatalf("Token: %v", err)
	}
	if token != "the.machines.token" {
		t.Fatalf("Token = %q, want the metadata server's own answer: a proxy may never see a machine's identity token", token)
	}
	if n := proxied.Load(); n != 0 {
		t.Fatalf("the proxy was asked %d times, want none: the metadata server's address is link-local and its token acts as the machine", n)
	}
}

// TestTokenRefusesWhatItCannotRead names each refusal, and none of them repeats a token: the
// server that answers a status, and the one that answers nothing at all.
func TestTokenRefusesWhatItCannotRead(t *testing.T) {
	for _, refusal := range []struct {
		name    string
		handler http.HandlerFunc
		says    string
	}{
		{
			name:    "a status that is not a success",
			handler: func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusForbidden) },
			says:    "answered HTTP 403",
		},
		{
			name:    "an empty body",
			handler: func(w http.ResponseWriter, _ *http.Request) { w.Write(nil) },
			says:    "answered an empty identity token",
		},
	} {
		t.Run(refusal.name, func(t *testing.T) {
			stubMetadata(t, refusal.handler)
			token, err := Token(context.Background(), "example-audience")
			if err == nil || !strings.Contains(err.Error(), refusal.says) {
				t.Fatalf("Token = %q, %v; want a refusal carrying %q", token, err, refusal.says)
			}
		})
	}
}

// TestTokenNamesTheHostItCouldNotReach: a metadata server that answers nothing is a fault of the
// machine's own link, and the refusal names the variable an operator sets to point the reader
// somewhere else.
func TestTokenNamesTheHostItCouldNotReach(t *testing.T) {
	t.Setenv(MetadataHostVar, "127.0.0.1:1")

	_, err := Token(context.Background(), "example-audience")
	if err == nil || !strings.Contains(err.Error(), MetadataHostVar) {
		t.Fatalf("Token's error = %v, want one naming %s", err, MetadataHostVar)
	}
}
