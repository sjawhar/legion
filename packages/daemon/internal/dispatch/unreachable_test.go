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
	} {
		t.Run(testCase.name, func(t *testing.T) {
			if got := Unreachable(testCase.err); got != testCase.want {
				t.Fatalf("Unreachable(%v) = %t, want %t", testCase.err, got, testCase.want)
			}
		})
	}
}

// transportFailure's three shapes, each reproduced against a real server rather than a synthetic
// error: a certificate the client does not trust and an https:// URL pointed at a plain-HTTP port
// both stay loud refusals no wait fixes, exactly like the certificate case; a dial the network
// refuses is wrapped as a TransientError, exactly what a real outage produces, so Unreachable
// rides it out. dispatchBase's scheme check (internal/config) catches a scheme that is not http
// or https at load time, but not a correct https:// whose port serves plain HTTP, which only a
// real connection attempt reveals.
func TestTransportFailureClassifiesEachRealServerShape(t *testing.T) {
	for _, testCase := range []struct {
		name          string
		client        func(t *testing.T) *HTTPClient
		wantTransient bool
	}{
		{
			name: "a certificate the client does not trust",
			client: func(t *testing.T) *HTTPClient {
				server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					w.WriteHeader(http.StatusOK)
					w.Write([]byte("[]"))
				}))
				t.Cleanup(server.Close)
				// The default client trusts no certificate the test server minted for itself.
				return New(server.URL, "test-token")
			},
			wantTransient: false,
		},
		{
			name: "an https:// URL pointed at a plain-HTTP port",
			client: func(t *testing.T) *HTTPClient {
				server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					w.WriteHeader(http.StatusOK)
				}))
				t.Cleanup(server.Close)
				return New("https://"+strings.TrimPrefix(server.URL, "http://"), "test-token")
			},
			wantTransient: false,
		},
		{
			name: "a dial the network refuses",
			client: func(t *testing.T) *HTTPClient {
				reserved, err := net.Listen("tcp", "127.0.0.1:0")
				if err != nil {
					t.Fatalf("reserve a port: %v", err)
				}
				addr := reserved.Addr().String()
				reserved.Close() // nothing answers here now.
				return New("http://"+addr, "test-token")
			},
			wantTransient: true,
		},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			client := testCase.client(t)
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			_, err := client.ListIssues(ctx, "ACME", nil)
			if err == nil {
				t.Fatal("ListIssues must fail")
			}
			var transient *TransientError
			if got := errors.As(err, &transient); got != testCase.wantTransient {
				t.Fatalf("errors.As(err, &TransientError{}) = %t, want %t (err: %v)", got, testCase.wantTransient, err)
			}
			if got := Unreachable(err); got != testCase.wantTransient {
				t.Fatalf("Unreachable(%v) = %t, want %t", err, got, testCase.wantTransient)
			}
		})
	}
}
