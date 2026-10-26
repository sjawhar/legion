package redact

import (
	"strings"
	"testing"
)

// TestURLHidesEveryPartThatCanCarryACredential: a user name, a password, a query and a fragment
// each print as "xxxxx", in a URL net/url reads and in one it does not, and a URL with none of
// them prints unchanged.
func TestURLHidesEveryPartThatCanCarryACredential(t *testing.T) {
	for _, row := range []struct{ raw, want string }{
		{"https://secrets.internal.example", "https://secrets.internal.example"},
		{"http://127.0.0.1:13380/v1/requests", "http://127.0.0.1:13380/v1/requests"},
		{"https://tokuser9f3:hunter2pw@secrets.internal.example/v1/requests", "https://xxxxx@secrets.internal.example/v1/requests"},
		{"https://tokuser9f3@secrets.internal.example", "https://xxxxx@secrets.internal.example"},
		{"https://secrets.internal.example/v1/requests?token=hunter2pw", "https://secrets.internal.example/v1/requests?xxxxx"},
		{"https://secrets.internal.example#hunter2pw", "https://secrets.internal.example?xxxxx"},
		// A password holding a character a URL must escape: net/url refuses these, and the
		// value must not print whole because of it.
		{"https://tokuser9f3:hunt/er2pw@secrets.internal.example", "https://xxxxx@secrets.internal.example"},
		{"https://tokuser9f3:hunt?er2pw@secrets.internal.example", "https://xxxxx@secrets.internal.example"},
		{"https://tokuser9f3:hunt#er2pw@secrets.internal.example", "https://xxxxx@secrets.internal.example"},
		{"http://tokuser9f3:hunter2pw@127.0.0.1:badport", "http://xxxxx@127.0.0.1:badport"},
		// No scheme at all.
		{"tokuser9f3:hunter2pw@secrets.internal.example", "xxxxx@secrets.internal.example"},
		{"", ""},
	} {
		got := URL(row.raw)
		if got != row.want {
			t.Errorf("URL(%q) = %q, want %q", row.raw, got, row.want)
		}
		for _, secret := range []string{"tokuser9f3", "hunter2pw", "hunt", "er2pw"} {
			if strings.Contains(got, secret) {
				t.Errorf("URL(%q) = %q still holds %q", row.raw, got, secret)
			}
		}
	}
}
