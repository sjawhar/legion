package redact

import (
	"errors"
	"net/http"
	"net/url"
	"strings"
	"syscall"
	"testing"
)

// secretsIn are the credential-like values the rows below put in a URL; no redacted form may
// hold one.
var secretsIn = []string{"tokuser9f3", "hunter2pw", "hunt", "er2pw", "nter2pw", "qtok77", "frag9z", "AbCtok9f3", "dEfgh", "ij=", "1234"}

// TestURLHidesEveryPartThatCanCarryACredential: a user name, a password, a query and a fragment
// each print as "xxxxx" after their separator, in a URL net/url reads and in one it does not;
// the host stays named, and a URL with none of them prints unchanged.
func TestURLHidesEveryPartThatCanCarryACredential(t *testing.T) {
	for _, row := range []struct{ raw, want string }{
		{"https://secrets.internal.example", "https://secrets.internal.example"},
		{"http://127.0.0.1:13380/v1/requests", "http://127.0.0.1:13380/v1/requests"},
		{"https://tokuser9f3:hunter2pw@secrets.internal.example/v1/requests", "https://xxxxx@secrets.internal.example/v1/requests"},
		{"https://tokuser9f3@secrets.internal.example", "https://xxxxx@secrets.internal.example"},
		{"https://secrets.internal.example/v1/requests?token=qtok77", "https://secrets.internal.example/v1/requests?xxxxx"},
		{"https://secrets.internal.example#frag9z", "https://secrets.internal.example#xxxxx"},
		{"https://secrets.internal.example/?token=qtok77#frag9z", "https://secrets.internal.example/?xxxxx#xxxxx"},
		// An '@' inside the password, and a password percent-encoded.
		{"https://tokuser9f3:hunt@er2pw@secrets.internal.example", "https://xxxxx@secrets.internal.example"},
		{"https://tokuser9f3:hu%40nter2pw@secrets.internal.example", "https://xxxxx@secrets.internal.example"},
		// A bad escape, which net/url refuses.
		{"http://tokuser9f3:hunter2pw%zz@127.0.0.1:1", "http://xxxxx@127.0.0.1:1"},
		// A password holding a character a URL must escape: net/url refuses these, and the
		// value must not print whole because of it.
		{"https://tokuser9f3:hunt/er2pw@secrets.internal.example", "https://xxxxx@secrets.internal.example"},
		{"https://tokuser9f3:hunt?er2pw@secrets.internal.example", "https://xxxxx@secrets.internal.example"},
		{"https://tokuser9f3:hunt#er2pw@secrets.internal.example", "https://xxxxx@secrets.internal.example"},
		{"http://tokuser9f3:hunter2pw@127.0.0.1:badport", "http://xxxxx@127.0.0.1:badport"},
		// IPv6 hosts, with and without userinfo.
		{"http://[::1]:13380/v1/requests", "http://[::1]:13380/v1/requests"},
		{"http://tokuser9f3:hunter2pw@[::1]:13380/v1/requests", "http://xxxxx@[::1]:13380/v1/requests"},
		// An '@' in the path or query hides everything before it, host included: a broker's base
		// URL has neither, and a credential holding '/' or '?' reads the same way.
		{"https://secrets.internal.example/v1/users/ada@example.com", "https://xxxxx@example.com"},
		{"https://secrets.internal.example/?next=ada@example.com", "https://xxxxx@example.com"},
		// No scheme at all, and no scheme with "://" later in the value.
		{"tokuser9f3:hunter2pw@secrets.internal.example", "xxxxx@secrets.internal.example"},
		{"tokuser9f3:hunter2pw@secrets.internal.example/?next=https://x", "xxxxx@secrets.internal.example/?xxxxx"},
		{"secrets.internal.example/v1?next=https://tokuser9f3:hunter2pw@x", "xxxxx@x"},
		{"tokuser9f3:hu://nter2pw@secrets.example", "xxxxx@secrets.example"},
		// A mistyped scheme separator: one slash, or no colon.
		{"http:/tokuser9f3:hunter2pw@127.0.0.1:1/v1", "http:/xxxxx@127.0.0.1:1/v1"},
		{"https//tokuser9f3:hunter2pw@secrets.example", "https//xxxxx@secrets.example"},
		{"http:tokuser9f3:hunter2pw@secrets.example", "xxxxx@secrets.example"},
		// A token holding '/', '?' or '#', and a password that begins with digits, which reads
		// as a port.
		{"https://AbCtok9f3/dEfgh+ij=@secrets.example", "https://xxxxx@secrets.example"},
		{"https://AbCtok9f3?dEfgh@secrets.example", "https://xxxxx@secrets.example"},
		{"https://AbCtok9f3#dEfgh@secrets.example", "https://xxxxx@secrets.example"},
		{"https://tokuser9f3:1234/hunter2pw@secrets.example", "https://xxxxx@secrets.example"},
		{"", ""},
	} {
		got := URL(row.raw)
		if got != row.want {
			t.Errorf("URL(%q) = %q, want %q", row.raw, got, row.want)
		}
		for _, secret := range secretsIn {
			if strings.Contains(got, secret) {
				t.Errorf("URL(%q) = %q still holds %q", row.raw, got, secret)
			}
		}
	}
}

// TestErrorHidesTheURLAndACauseThatRepeatsIt: the errors net/http and net/url make name the URL
// they were given, with the user name and query in clear; redact.Error leaves a message naming
// the URL redacted, says only "not a valid URL" for one that did not parse (its cause quotes the
// bad part), withholds any other cause that repeats a hidden part, and keeps every cause
// reachable for errors.Is and errors.As.
func TestErrorHidesTheURLAndACauseThatRepeatsIt(t *testing.T) {
	_, parseErr := url.Parse("http://tokuser9f3:hunter2pw%zz@127.0.0.1:1/v1/requests")
	_, schemeErr := http.Get("tokuser9f3:hunter2pw@127.0.0.1:1/v1/requests")
	for _, row := range []struct {
		name  string
		err   error
		want  string
		cause error // a cause errors.Is must still find, nil for none
	}{
		{"a refused call", &url.Error{Op: "Post", URL: "http://tokuser9f3:***@127.0.0.1:1/?token=qtok77/v1/requests", Err: syscall.ECONNREFUSED},
			`Post "http://xxxxx@127.0.0.1:1/?xxxxx": connection refused`, syscall.ECONNREFUSED},
		{"a bad escape in the password", parseErr, `parse "http://xxxxx@127.0.0.1:1/v1/requests": not a valid URL`, nil},
		{"a user name read as a scheme", schemeErr, `Get "xxxxx@127.0.0.1:1/v1/requests": (withheld: it repeats a part of the URL that can carry a credential)`, nil},
		{"wrapped", errors.Join(errors.New("enroll"), &url.Error{Op: "Get", URL: "http://tokuser9f3@127.0.0.1:1", Err: syscall.ECONNREFUSED}),
			"enroll\nGet \"http://xxxxx@127.0.0.1:1\": connection refused", syscall.ECONNREFUSED},
	} {
		t.Run(row.name, func(t *testing.T) {
			if row.err == nil {
				t.Fatal("the fixture made no error")
			}
			got := Error(row.err)
			if got.Error() != row.want {
				t.Fatalf("Error = %q, want %q", got, row.want)
			}
			for _, secret := range append(secretsIn, "%zz") {
				if strings.Contains(got.Error(), secret) {
					t.Fatalf("Error = %q still holds %q", got, secret)
				}
			}
			if row.cause != nil && !errors.Is(got, row.cause) {
				t.Fatalf("errors.Is(%v, %v) = false; the cause must stay reachable", got, row.cause)
			}
		})
	}
	if err := errors.New("no URL here"); Error(err) != err {
		t.Fatal("an error holding no *url.Error must come back as it is")
	}
}

// leakyTimeout is a net timeout whose text repeats the URL's user name, so Error withholds it.
type leakyTimeout struct{}

func (leakyTimeout) Error() string   { return "dial tokuser9f3@127.0.0.1:1: i/o timeout" }
func (leakyTimeout) Timeout() bool   { return true }
func (leakyTimeout) Temporary() bool { return true }

// TestErrorKeepsATimeoutItWithholds: (*url.Error).Timeout and Temporary ask the error it wraps
// directly, so a cause Error withholds still answers them, and a caller still tells a broker
// that did not answer in time from one it could not reach.
func TestErrorKeepsATimeoutItWithholds(t *testing.T) {
	ue := &url.Error{Op: "Get", URL: "http://tokuser9f3@127.0.0.1:1", Err: leakyTimeout{}}
	got := Error(ue)
	if strings.Contains(got.Error(), "tokuser9f3") {
		t.Fatalf("Error = %q still holds the user name", got)
	}
	if _, ok := ue.Err.(withheld); !ok {
		t.Fatalf("the cause %T was not withheld", ue.Err)
	}
	if !ue.Timeout() || !ue.Temporary() {
		t.Fatalf("Timeout() = %v, Temporary() = %v; want both true through the withheld cause", ue.Timeout(), ue.Temporary())
	}
	plain := &url.Error{Op: "parse", URL: "http://tokuser9f3:hunter2pw%zz@x", Err: errors.New("invalid URL escape")}
	if Error(plain); plain.Timeout() || plain.Temporary() {
		t.Fatal("a withheld cause that is no timeout must not read as one")
	}
}
