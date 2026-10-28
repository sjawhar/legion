package redact

import (
	"context"
	"crypto/tls"
	"errors"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"syscall"
	"testing"

	"golang.org/x/oauth2"
)

// secretsIn are the credential-like values the probes below put in a URL; no output may hold
// one.
var secretsIn = []string{"tokuser9f3", "hunter2pw", "hunt", "er2pw", "qtok", "frag9z", "AbCtok9f3", "dEfgh", "ij=", "1234", "12/34",
	"2pwSECRET", "77SECRET", "9zSECRET", "klSECRET", "p@ss", "f@g", "t@k", "cd==", "GRANT_ME", "ada@"}

// cleanURLs are plain http(s) URLs URL prints unchanged.
var cleanURLs = []string{
	"https://secrets.internal.example",
	"https://secrets.internal.example/",
	"http://127.0.0.1:13380/v1/requests",
	"http://127.0.0.1:1/v1/enrollments//renew",
	"http://[::1]:13380/v1/requests",
	"https://[fe80::1]",
	"HTTPS://Secrets.Example:443/v1/a-b_c.d~e",
	"http://localhost:13380",
}

// probes are every input the three gate rounds on this redactor printed a credential, a part of
// one, or a forged line from, and the shapes around them: none is provably a plain http(s) URL,
// so each prints as NotPrinted.
var probes = []string{
	// User info, a query, a fragment, and both.
	"https://tokuser9f3:hunter2pw@secrets.internal.example/v1/requests",
	"https://tokuser9f3@secrets.internal.example",
	"https://secrets.internal.example/v1/requests?token=qtok77",
	"https://secrets.internal.example#frag9z",
	"https://secrets.internal.example/?token=qtok77#frag9z",
	"http://127.0.0.1:1/?token=qtok77",
	// An '@' in the password, escaped or not, and a bad escape.
	"https://tokuser9f3:hunt@er2pw@secrets.internal.example",
	"https://tokuser9f3:hu%40nter2pw@secrets.internal.example",
	"http://tokuser9f3:hunter2pw%zz@127.0.0.1:1",
	"http://tokuser9f3:hunter2pw@127.0.0.1:badport",
	// IPv6 with user info, a zone, a bad port, an unclosed bracket.
	"http://tokuser9f3:hunter2pw@[::1]:13380/v1/requests",
	"http://tokuser9f3:hunter2pw@[fe80::1%25eth0]:1",
	"http://[fe80::1%25eth0]:1",
	"http://tokuser9f3:hunter2pw@[::1]:x",
	"http://tokuser9f3:hunter2pw@[::1:1",
	// Round one: a mistyped separator, a token with '/', '?' or '#', a password that begins with
	// digits, "://" inside a password, no scheme.
	"http:/tokuser9f3:hunter2pw@127.0.0.1:1/v1",
	"https//tokuser9f3:hunter2pw@secrets.example",
	"http:tokuser9f3:hunter2pw@secrets.example",
	"//tokuser9f3:hunter2pw@secrets.example",
	"://tokuser9f3:hunter2pw@secrets.example",
	"HTTPS://tokuser9f3:hunter2pw@secrets.example",
	"https://AbCtok9f3/dEfgh+ij=@secrets.example",
	"https://AbCtok9f3?dEfgh@secrets.example",
	"https://AbCtok9f3#dEfgh@secrets.example",
	"https://tokuser9f3:1234/hunter2pw@secrets.example",
	"https://tokuser9f3:12/34@secrets.example",
	"https://ab/cd==@secrets.example",
	"https://tokuser9f3?hunter2pw@secrets.example",
	"tokuser9f3:hu://nter2pw@secrets.example",
	"tokuser9f3:hunter2pw@secrets.internal.example",
	"tokuser9f3:hunter2pw@127.0.0.1:1",
	"tokuser9f3:hunter2pw@secrets.internal.example/?next=https://x",
	"secrets.internal.example/v1?next=https://tokuser9f3:hunter2pw@x",
	"https://secrets.internal.example/v1/users/ada@example.com",
	// Round two: an '@' in a query value or fragment, an escaped '@', a whole URL escaped.
	"https://secrets.internal.example/?next=ada@example.com",
	"https://secrets.example/?password=hunter@2pwSECRET",
	"https://secrets.example/?password=p@ss",
	"https://secrets.example/#f@g",
	"https://secrets.example/#frag@9zSECRET",
	"https://secrets.example/?a=1&token=t@k&b=2",
	"https://secrets.example/?a=1&token=qtok@77SECRET&b=2",
	"https://tokuser9f3:hunter2pw@secrets.example/?next=ada@example.com",
	"http://tokuser9f3:hunter2pw%40127.0.0.1:1",
	"https%3A%2F%2Ftokuser9f3%3Ahunter2pw%40secrets.example",
	"https://secrets.example/?token=qtok77%40x",
	// Round three: a literal '@' with an escaped one, double escaping, a decoded control byte
	// that forges a line, a short token head, and bad escapes beside an '@'.
	"http://ada@corp.example:hunter2pw%40127.0.0.1:1",
	"http://tokuser9f3:hun@ter2pw%40127.0.0.1:1",
	"http://tokuser9f3:hunter2pw%2540127.0.0.1:1",
	"https%253A%252F%252Ftokuser9f3%253Ahunter2pw%2540secrets.example",
	"http://tokuser9f3:hunter2pw%40127.0.0.1:1%0Aagent-secrets: GRANT_ME from the broker",
	"http://127.0.0.1:1\nagent-secrets: GRANT_ME from the broker",
	"http://127.0.0.1:1/\x1b[2Kagent-secrets: GRANT_ME",
	"http://Ab?cdefghijklSECRET@127.0.0.1:1",
	"http://tokuser9f3:hunter2pw%zz%40127.0.0.1:1",
	"http://tokuser9f3:hunter2pw%%40127.0.0.1:1",
	"http://tokuser9f3:hunter2pw%4@127.0.0.1:1",
	"http://tokuser9f3:hunter2pw@127.0.0.1:1%40",
	// A backslash, whitespace, a host net/url takes that is no DNS name, a bad port, a path
	// character outside the allow-list.
	`http://tokuser9f3:hunter2pw\@secrets.example`,
	"http://secrets.example/ tokuser9f3",
	"http://secrets.example.com;token=qtok77",
	"https://tokuser9f3_hunter2pw.example",
	"https://secrets.example:",
	"https://secrets.example:123456",
	"https://-tokuser9f3.example",
	"https://secrets.example/v1/tokuser9f3=hunter2pw",
	"ftp://secrets.example",
	"secrets.example:13380",
}

// TestURLPrintsOnlyAPlainHTTPURL: a provably plain http(s) URL prints unchanged, the empty
// value prints empty, and every probe prints as NotPrinted, holding none of its secrets.
func TestURLPrintsOnlyAPlainHTTPURL(t *testing.T) {
	for _, raw := range cleanURLs {
		if got := URL(raw); got != raw {
			t.Errorf("URL(%q) = %q, want it unchanged", raw, got)
		}
	}
	if got := URL(""); got != "" {
		t.Errorf("URL(\"\") = %q, want empty", got)
	}
	for _, raw := range probes {
		if got := URL(raw); got != NotPrinted {
			t.Errorf("URL(%q) = %q, want %q", raw, got, NotPrinted)
		}
	}
	for _, secret := range secretsIn {
		if strings.Contains(NotPrinted, secret) {
			t.Fatalf("NotPrinted holds %q", secret)
		}
	}
}

// leakyTimeout is a net timeout whose text repeats a credential.
type leakyTimeout struct{}

func (leakyTimeout) Error() string   { return "dial tokuser9f3:hunter2pw@127.0.0.1:1: i/o timeout" }
func (leakyTimeout) Timeout() bool   { return true }
func (leakyTimeout) Temporary() bool { return true }

// TestErrorPrintsOnlyTheClassForAURLItWillNotPrint: for a configured URL URL will not print, the
// error names its operation, the URL as NotPrinted, and the class of failure, and no text of the
// error itself; errors.Is and errors.As still find the cause, and a timeout still answers
// Timeout through the *url.Error.
func TestErrorPrintsOnlyTheClassForAURLItWillNotPrint(t *testing.T) {
	const raw = "http://tokuser9f3:hunter2pw@127.0.0.1:1"
	_, parseErr := url.Parse(raw + "%zz/v1")
	urlErr := func(cause error) error { return &url.Error{Op: "Get", URL: raw + "/v1/enrollments/self", Err: cause} }
	dns := &net.DNSError{Err: "no such host", Name: "tokuser9f3", IsNotFound: true}
	refused := &net.OpError{Op: "dial", Net: "tcp", Err: &net.AddrError{Err: "x", Addr: "tokuser9f3"}}
	refusedErr := &net.OpError{Op: "dial", Net: "tcp", Err: syscall.ECONNREFUSED}
	retrieve := &oauth2.RetrieveError{Response: &http.Response{StatusCode: http.StatusTooManyRequests}, Body: []byte("tokuser9f3")}
	want := func(class string) string { return `Get "` + NotPrinted + `": ` + class }
	for _, row := range []struct {
		name, raw string
		err       error
		want      string
		is        error // a cause errors.Is must still find, nil for none
	}{
		{"name resolution", raw, urlErr(dns), want("name resolution failed"), nil},
		{"a refused connection", raw, urlErr(refusedErr), want("connection refused"), syscall.ECONNREFUSED},
		{"a timeout", raw, urlErr(leakyTimeout{}), want("timed out"), nil},
		{"a deadline", raw, urlErr(context.DeadlineExceeded), want("timed out"), context.DeadlineExceeded},
		{"TLS", raw, urlErr(tls.RecordHeaderError{Msg: "tokuser9f3"}), want("TLS failed"), nil},
		{"an HTTP status", raw, urlErr(retrieve), want("the broker answered HTTP 429"), nil},
		{"anything else", raw, urlErr(refused), want("the request failed"), nil},
		{"a URL that did not parse", raw + "%zz", parseErr, `parse "` + NotPrinted + `": not a valid URL`, nil},
		{"no *url.Error", raw, errors.New("dial tokuser9f3: refused"), "the request failed", nil},
		{"a one-letter user name", "http://u:p@127.0.0.1:1", &url.Error{Op: "Get", URL: "http://u:p@127.0.0.1:1/v1", Err: refusedErr},
			want("connection refused"), syscall.ECONNREFUSED},
		{"a forged line", "http://127.0.0.1:1%0Aagent-secrets: GRANT_ME from", &url.Error{Op: "Get", URL: "http://127.0.0.1:1\nagent-secrets: GRANT_ME from/v1", Err: refusedErr},
			want("connection refused"), syscall.ECONNREFUSED},
	} {
		t.Run(row.name, func(t *testing.T) {
			if row.err == nil {
				t.Fatal("the fixture made no error")
			}
			got := Error(row.err, row.raw)
			if got.Error() != row.want {
				t.Fatalf("Error = %q, want %q", got, row.want)
			}
			for _, secret := range append(secretsIn, "%zz", "\n") {
				if strings.Contains(got.Error(), secret) {
					t.Fatalf("Error = %q holds %q", got, secret)
				}
			}
			if !errors.Is(got, row.err) || (row.is != nil && !errors.Is(got, row.is)) {
				t.Fatalf("errors.Is(%v, cause) = false; the cause must stay reachable", got)
			}
		})
	}
	var ue *url.Error
	if got := Error(urlErr(leakyTimeout{}), raw); !errors.As(got, &ue) || !ue.Timeout() {
		t.Fatal("a timeout must still answer Timeout through the *url.Error it came in")
	}
	var found *net.DNSError
	if got := Error(urlErr(dns), raw); !errors.As(got, &found) || !found.IsNotFound {
		t.Fatal("errors.As must still find the *net.DNSError")
	}
}

// TestErrorLeavesACleanURLsErrorAsItIs: for a configured URL URL prints unchanged, a DNS, a
// refused, a timeout, an http-to-https, an EOF, a reset and a broken-pipe error come back as
// they are, with their own text.
func TestErrorLeavesACleanURLsErrorAsItIs(t *testing.T) {
	const raw = "http://127.0.0.1:1"
	call := func(cause error) error { return &url.Error{Op: "Get", URL: raw + "/v1/enrollments/self", Err: cause} }
	for _, err := range []error{
		call(&net.DNSError{Err: "no such host", Name: "broker.invalid", IsNotFound: true}),
		call(&net.OpError{Op: "dial", Net: "tcp", Err: syscall.ECONNREFUSED}),
		call(context.DeadlineExceeded),
		call(http.ErrSchemeMismatch),
		call(io.EOF),
		call(io.ErrUnexpectedEOF),
		call(syscall.ECONNRESET),
		call(syscall.EPIPE),
	} {
		if got := Error(err, raw); got != err {
			t.Errorf("Error(%v) = %v, want the error as it is", err, got)
		}
	}
	if Error(nil, "http://tokuser9f3:hunter2pw@127.0.0.1:1") != nil {
		t.Fatal("a nil error must stay nil")
	}
}

// closingServer is a server that takes each connection and closes it without an answer.
func closingServer(t *testing.T) string {
	t.Helper()
	s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		conn, _, err := w.(http.Hijacker).Hijack()
		if err == nil {
			conn.Close()
		}
	}))
	t.Cleanup(s.Close)
	return s.URL
}

// TestErrorKeepsARealPeersCause: against real servers - an http server asked over https, and
// one that closes the connection - net/http's error for a clean URL comes back as it is, through
// Error, and its cause through Cause.
func TestErrorKeepsARealPeersCause(t *testing.T) {
	plain := httptest.NewServer(http.NotFoundHandler())
	defer plain.Close()
	for _, row := range []struct{ base, want string }{
		{"https://" + strings.TrimPrefix(plain.URL, "http://"), "http: server gave HTTP response to HTTPS client"},
		{closingServer(t), "EOF"},
	} {
		_, err := http.Get(row.base + "/v1/enrollments/self")
		if err == nil || !strings.HasSuffix(err.Error(), row.want) {
			t.Fatalf("the fixture's error %v does not end with %q", err, row.want)
		}
		if got := Error(err, row.base); got != err {
			t.Errorf("Error(%v) = %v, want the error as it is", err, got)
		}
		if got := Cause(err, row.base); got == nil || got.Error() != row.want {
			t.Errorf("Cause(%v) = %v, want %q", err, got, row.want)
		}
	}
}

// TestCallPrintsAPlainPathOnACleanBase: a call URL a program built on a clean base prints
// unchanged when it is that base, its scheme in any case, followed by a plain path, and as
// NotPrinted otherwise: an escape, a query, another host or path, a base that is not clean.
func TestCallPrintsAPlainPathOnACleanBase(t *testing.T) {
	const base = "http://127.0.0.1:1"
	for _, row := range []struct{ base, call, want string }{
		{base, base + "/v1/enrollments//renew", base + "/v1/enrollments//renew"},
		{base, base, base},
		{base + "/", base + "/v1/x", base + "/v1/x"},
		{"HTTP://Secrets.Example", "http://Secrets.Example/v1/x", "http://Secrets.Example/v1/x"},
		{"HTTP://Secrets.Example", "http://secrets.example/v1/x", NotPrinted},
		{base, base + "/v1/requests/req%25zz", NotPrinted},
		{base, base + "/v1/requests/req%zz", NotPrinted},
		{base, base + "/v1/requests?state=qtok77", NotPrinted},
		{base, base + "0/v1/x", NotPrinted},
		{base, "http://other.example/v1/x", NotPrinted},
		{base, base + "/magic/AbCtok9f3SECRET", NotPrinted},
		{base, base + "/v1x/AbCtok9f3SECRET", NotPrinted},
		{"http://tokuser9f3:hunter2pw@127.0.0.1:1", "http://tokuser9f3:hunter2pw@127.0.0.1:1/v1/x", NotPrinted},
		{"http://127.0.0.1:1%0A", "http://127.0.0.1:1%0A/v1/x", NotPrinted},
	} {
		if got := Call(row.base, row.call); got != row.want {
			t.Errorf("Call(%q, %q) = %q, want %q", row.base, row.call, got, row.want)
		}
	}
}

// redirectingTo is a server that answers every request with a 302 whose Location is location,
// set as written.
func redirectingTo(t *testing.T, location string) string {
	t.Helper()
	s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header()["Location"] = []string{location}
		w.WriteHeader(http.StatusFound)
	}))
	t.Cleanup(s.Close)
	return s.URL
}

// TestErrorPrintsOnlyTheClassAfterARedirect: with a clean base, net/http's error for a request
// that followed a redirect names the redirect's target, on another host or another path, with a
// query or none; a malformed Location puts the whole target in the wrapped error's text while the
// *url.Error names the clean call. Each prints the class alone through Error and Cause, and the
// same call that never redirected keeps its text.
func TestErrorPrintsOnlyTheClassAfterARedirect(t *testing.T) {
	for _, row := range []struct {
		name, location, wantError, wantCause string
		is                                   error
	}{
		{"a query on another port", "http://127.0.0.1:1/signin?state=qtok77SECRET", `Get "` + NotPrinted + `": connection refused`, "connection refused", syscall.ECONNREFUSED},
		{"a path on another port", "http://127.0.0.1:1/magic/AbCtok9f3SECRET", `Get "` + NotPrinted + `": connection refused`, "connection refused", syscall.ECONNREFUSED},
		{"another path on the same broker", "/magic/AbCtok9f3SECRET", `Get "` + NotPrinted + `": the request failed`, "the request failed", nil},
		{"a malformed Location", "http://127.0.0.1:1/signin%zz?state=qtok77SECRET", "", "the request failed", nil},
	} {
		t.Run(row.name, func(t *testing.T) {
			base := redirectingTo(t, row.location)
			_, err := http.Get(base + "/v1/enrollments/self")
			if err == nil || !strings.Contains(err.Error(), "SECRET") {
				t.Fatalf("the fixture's error %v does not name the redirect's target", err)
			}
			wantError := row.wantError
			if wantError == "" {
				wantError = `Get "` + base + `/v1/enrollments/self": the request failed`
			}
			if got := Error(err, base); got.Error() != wantError || !errors.Is(got, err) || (row.is != nil && !errors.Is(got, row.is)) {
				t.Fatalf("Error = %q, want %q, still unwrapping to the cause", got, wantError)
			}
			if got := Cause(err, base); got.Error() != row.wantCause {
				t.Fatalf("Cause = %q, want %q", got, row.wantCause)
			}
		})
	}
	_, err := http.Get("http://127.0.0.1:1/v1/enrollments/self")
	if got := Error(err, "http://127.0.0.1:1"); got != err {
		t.Fatalf("Error = %v, want the clean call's error as it is", got)
	}
	var ue *url.Error
	if got := Cause(err, "http://127.0.0.1:1"); !errors.As(err, &ue) || got != ue.Err {
		t.Fatalf("Cause = %v, want the error the *url.Error wraps", got)
	}
	if got := Error(err, "HTTP://127.0.0.1:1"); got != err {
		t.Fatalf("Error = %v, want the clean call's error as it is with an upper-case scheme", got)
	}
}
