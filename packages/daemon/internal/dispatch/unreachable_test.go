package dispatch

import (
	"context"
	"errors"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// Unreachable is the predicate daemon.go's boot gate judges every reconcile failure with: a boot
// reading Dispatch for admission must wait out an outage and refuse a misconfiguration loud.
func TestUnreachableClassifiesEachErrorShape(t *testing.T) {
	for _, testCase := range []struct {
		name string
		err  error
		want bool
	}{
		{"a 503 with no JSON body, the shape a raw HTML error page from in front of Dispatch takes",
			&Error{Status: 503, Message: "<html>503 Service Temporarily Unavailable</html>"}, true},
		{"a codeless 4xx from whatever sits in front of Dispatch, such as a wrong host's own 404 page",
			&Error{Status: 404, Message: "not found"}, true},
		{"408 Request Timeout", &Error{Status: 408, Code: "TIMEOUT", Message: "come back later"}, true},
		{"429 Too Many Requests", &Error{Status: 429, Code: "RATE_LIMITED", Message: "slow down"}, true},
		{"401, a credential answer PermanentRefusal rides out for a queued write and Unreachable rides out here too",
			&Error{Status: 401, Code: "UNAUTHORIZED", Message: "bad token"}, true},
		{"403, the same as 401", &Error{Status: 403, Code: "FORBIDDEN", Message: "denied"}, true},
		{"a genuine coded 404: the project or route named is not there, no wait fixes that",
			&Error{Status: 404, Code: "NOT_FOUND", Message: "no such project"}, false},
		{"a genuine coded 400", &Error{Status: 400, Code: "BAD_REQUEST", Message: "malformed"}, false},
		{"a TransientError wrapping a dial refusal", &TransientError{Err: &net.OpError{Op: "dial", Err: errors.New("connection refused")}}, true},
		{"a decode error on a 2xx response: not Dispatch answering, not a transport failure — a schema mismatch",
			errors.New("decode Dispatch GET /api/v1/issues response: unexpected end of JSON input"), false},
		{"an https:// dispatch_url pointed at a plain-HTTP port (http.ErrSchemeMismatch), left unwrapped by transportFailure",
			http.ErrSchemeMismatch, false},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			if got := Unreachable(testCase.err); got != testCase.want {
				t.Fatalf("Unreachable(%v) = %t, want %t", testCase.err, got, testCase.want)
			}
		})
	}
}

// A certificate the client does not trust must stay a loud refusal, never ride out forever as
// "the server merely isn't up yet." Reproduced against a real TLS server whose certificate the
// client's pool does not trust.
func TestTransportFailureExcludesACertificateTheClientDoesNotTrust(t *testing.T) {
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		w.Write([]byte("[]"))
	}))
	defer server.Close()

	// The default client trusts no certificate the test server minted for itself.
	client := New(server.URL, "test-token")
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	_, err := client.ListIssues(ctx, "ACME", nil)
	if err == nil {
		t.Fatal("ListIssues against an untrusted certificate must fail")
	}
	var transient *TransientError
	if errors.As(err, &transient) {
		t.Fatalf("an untrusted certificate was wrapped as a TransientError: %v; it must stay a loud refusal, not an outage a boot gate rides out forever", err)
	}
	if Unreachable(err) {
		t.Fatalf("Unreachable(%v) = true for an untrusted certificate; it must classify false so the boot exits loud", err)
	}
}

// An https:// dispatch_url pointed at a plain-HTTP port must stay a loud refusal the same way:
// dispatchBase's scheme check (internal/config) catches a scheme that is not http or https at
// load time, but not a correct https:// whose port serves plain HTTP, which only a real
// connection attempt reveals. Reproduced against a real plain-HTTP server dialed with an
// https:// URL.
func TestTransportFailureExcludesAnHTTPSURLPointedAtAPlainHTTPPort(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	defer server.Close()

	client := New("https://"+strings.TrimPrefix(server.URL, "http://"), "test-token")
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	_, err := client.ListIssues(ctx, "ACME", nil)
	if err == nil {
		t.Fatal("ListIssues against an https:// URL pointed at a plain HTTP port must fail")
	}
	var transient *TransientError
	if errors.As(err, &transient) {
		t.Fatalf("a scheme mismatch was wrapped as a TransientError: %v; it must stay a loud refusal, not an outage a boot gate rides out forever", err)
	}
	if Unreachable(err) {
		t.Fatalf("Unreachable(%v) = true for a scheme mismatch; it must classify false so the boot exits loud", err)
	}
}

// A dial the network refuses is wrapped as a TransientError, so Unreachable rides it out —
// exactly the connection a real outage produces, as against the certificate case above.
func TestTransportFailureWrapsAConnectionRefusalAsTransient(t *testing.T) {
	reserved, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("reserve a port: %v", err)
	}
	addr := reserved.Addr().String()
	reserved.Close() // nothing answers here now.

	client := New("http://"+addr, "test-token")
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	_, err = client.ListIssues(ctx, "ACME", nil)
	if err == nil {
		t.Fatal("ListIssues against a closed port must fail")
	}
	var transient *TransientError
	if !errors.As(err, &transient) {
		t.Fatalf("a connection refusal was not wrapped as a TransientError: %v", err)
	}
	if !Unreachable(err) {
		t.Fatalf("Unreachable(%v) = false for a connection refusal; it must classify true so the boot waits", err)
	}
}
