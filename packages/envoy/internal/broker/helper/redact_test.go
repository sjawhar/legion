package helper

import (
	"context"
	"net/http"
	"strings"
	"testing"
	"time"
)

// TestABrokerCallErrorNamesNoCredentialTheURLCarries: a session's renew against a broker that
// never answers, or at a URL that does not parse, fails with an error the helper logs and
// records as the session's last error (which `agent-secrets register --wait` prints); it names
// the broker's address with no user name, password, query or bad escape AGENT_SECRETS_URL
// carries.
func TestABrokerCallErrorNamesNoCredentialTheURLCarries(t *testing.T) {
	s, err := newSession(1, 1, "runtime", nil)
	if err != nil {
		t.Fatal(err)
	}
	for _, row := range []struct{ base, want string }{
		{"http://tokuser9f3:hunter2pw@127.0.0.1:1", `"http://xxxxx@127.0.0.1:1/v1/enrollments//renew"`},
		{"http://127.0.0.1:1/?token=qtok77", `"http://127.0.0.1:1/?xxxxx"`},
		{"http://tokuser9f3:hunter2pw%zz@127.0.0.1:1", `"http://xxxxx@127.0.0.1:1/v1/enrollments//renew": not a valid URL`},
		{"tokuser9f3:hunter2pw@127.0.0.1:1", `"xxxxx@127.0.0.1:1/v1/enrollments//renew"`},
	} {
		t.Run(row.base, func(t *testing.T) {
			b := &Broker{URL: row.base, HTTP: &http.Client{Timeout: 5 * time.Second}}
			_, err := b.Renew(context.Background(), s)
			if err == nil {
				t.Fatal("renew against a broker that never answers succeeded")
			}
			if !strings.Contains(err.Error(), row.want) {
				t.Fatalf("renew error %q; want it to name %s", err, row.want)
			}
			for _, secret := range []string{"tokuser9f3", "hunter2pw", "qtok77", "%zz"} {
				if strings.Contains(err.Error(), secret) {
					t.Fatalf("renew error %q names %q", err, secret)
				}
			}
		})
	}
}
