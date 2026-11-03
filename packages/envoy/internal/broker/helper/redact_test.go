package helper

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/sjawhar/envoy/internal/redact"
)

// TestABrokerCallErrorNamesNoCredentialTheURLCarries: a session's renew against a broker that
// never answers, or at a URL that does not parse, fails with an error the helper logs and
// records as the session's last error (which `agent-secrets register --wait` prints). For an
// AGENT_SECRETS_URL redact.URL will not print, it names the URL as redact.NotPrinted and the
// failure's class alone; for a clean one, it reads as net/http wrote it.
func TestABrokerCallErrorNamesNoCredentialTheURLCarries(t *testing.T) {
	s, err := newSession(1, 1, "runtime", nil)
	if err != nil {
		t.Fatal(err)
	}
	np := `"` + redact.NotPrinted + `": `
	plain := httptest.NewServer(http.NotFoundHandler())
	defer plain.Close()
	closing := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if conn, _, err := w.(http.Hijacker).Hijack(); err == nil {
			conn.Close()
		}
	}))
	defer closing.Close()
	overHTTPS := "https://" + strings.TrimPrefix(plain.URL, "http://")
	// Each case is named for what its URL is rather than for the URL itself: two of them carry
	// an httptest server's port, which the kernel picks, so a subtest named after the URL is a
	// different test in every run and no comparison of two runs can pair it up.
	for _, row := range []struct{ name, base, want string }{
		{"a port nothing listens on", "http://127.0.0.1:1", `Post "http://127.0.0.1:1/v1/enrollments//renew": dial tcp 127.0.0.1:1: connect: connection refused`},
		{"https against a plain server", overHTTPS, `Post "` + overHTTPS + `/v1/enrollments//renew": http: server gave HTTP response to HTTPS client`},
		{"a server that closes the connection", closing.URL, `Post "` + closing.URL + `/v1/enrollments//renew": EOF`},
		{"a password in the URL", "http://tokuser9f3:hunter2pw@127.0.0.1:1", "Post " + np + "connection refused"},
		{"a token in the query", "http://127.0.0.1:1/?token=qtok77", "Post " + np + "connection refused"},
		{"a password the URL parser refuses", "http://tokuser9f3:hunter2pw%zz@127.0.0.1:1", "parse " + np + "not a valid URL"},
		{"a password with no scheme", "tokuser9f3:hunter2pw@127.0.0.1:1", "Post " + np + "the request failed"},
		{"a password behind one slash", "http:/tokuser9f3:hunter2pw@127.0.0.1:1", "Post " + np},
		{"a password holding an escaped at sign", "http://ada@corp.example:hunter2pw%40127.0.0.1:1", np},
	} {
		t.Run(row.name, func(t *testing.T) {
			b := &Broker{URL: row.base, HTTP: &http.Client{Timeout: 5 * time.Second}}
			_, err := b.Renew(context.Background(), s)
			if err == nil {
				t.Fatal("renew against a broker that never answers succeeded")
			}
			if !strings.Contains(err.Error(), row.want) {
				t.Fatalf("renew error %q; want it to name %s", err, row.want)
			}
			for _, secret := range []string{"tokuser9f3", "hunter2pw", "qtok77", "%zz", "ada@"} {
				if strings.Contains(err.Error(), secret) {
					t.Fatalf("renew error %q names %q", err, secret)
				}
			}
		})
	}
}
