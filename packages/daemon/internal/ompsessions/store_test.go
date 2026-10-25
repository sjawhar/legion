package ompsessions

import (
	"net/url"
	"strings"
	"testing"
)

// The endpoint a role container's launcher serves: a socket in the generation's own private
// directory, and an IAM login, which holds an `@`.
var testEndpoint = Endpoint{
	SocketDir: "/var/run/legion/private/generation-1",
	Database:  "legion_sessions",
	User:      "legion-sessions@example-project.iam",
}

// Only the two database kinds put a session in a row; the volume store, which is the zero value's,
// leaves every session a file.
func TestInDatabaseIsTrueOfBothDatabaseKinds(t *testing.T) {
	for _, tc := range []struct {
		store Store
		want  bool
	}{
		{Store{}, false},
		{Store{Kind: StoreVolume}, false},
		{Store{Kind: StorePostgres, DSNSecret: "SESSION_DSN"}, true},
		{Store{Kind: StoreCloudSQLIAM, Database: Database{Instance: "example-project:us-central1:example-instance", Name: "legion_sessions", User: testEndpoint.User}}, true},
	} {
		t.Run(string(tc.store.Kind), func(t *testing.T) {
			if got := tc.store.InDatabase(); got != tc.want {
				t.Errorf("InDatabase() = %t, want %t", got, tc.want)
			}
		})
	}
}

// Oh My Pi's client takes the socket whole as the URL's `path` parameter and ignores the host; the
// login, which holds an `@`, is percent-encoded in the userinfo and survives net/url's own parser.
func TestAgentURLNamesTheSocketAndSurvivesARoundTrip(t *testing.T) {
	raw := testEndpoint.AgentURL()
	if strings.Count(raw, "@") != 1 {
		t.Errorf("AgentURL() = %q, want the login's @ percent-encoded, leaving only the userinfo separator", raw)
	}
	u, err := url.Parse(raw)
	if err != nil {
		t.Fatalf("parse %q: %v", raw, err)
	}
	if got := u.User.Username(); got != testEndpoint.User {
		t.Errorf("user = %q, want %q", got, testEndpoint.User)
	}
	if u.Host != "localhost" || u.Path != "/"+testEndpoint.Database {
		t.Errorf("host and path = %q and %q, want localhost and /%s", u.Host, u.Path, testEndpoint.Database)
	}
	if got := u.Query().Get("path"); got != testEndpoint.Socket() {
		t.Errorf("path = %q, want the socket %q", got, testEndpoint.Socket())
	}
	if got := u.Query().Get("sslmode"); got != "disable" {
		t.Errorf("sslmode = %q, want disable", got)
	}
}

// The launcher's own pgx client takes libpq's `host` as the socket's directory and appends
// SocketName itself, and reads the login from the `user` parameter, where the `@` is again
// percent-encoded.
func TestClientURLNamesTheSocketsDirectoryAndSurvivesARoundTrip(t *testing.T) {
	raw := testEndpoint.ClientURL()
	if strings.Contains(raw, "@") {
		t.Errorf("ClientURL() = %q, want the login's @ percent-encoded", raw)
	}
	u, err := url.Parse(raw)
	if err != nil {
		t.Fatalf("parse %q: %v", raw, err)
	}
	if u.Host != "" || u.Path != "/"+testEndpoint.Database {
		t.Errorf("host and path = %q and %q, want no host and /%s", u.Host, u.Path, testEndpoint.Database)
	}
	if got := u.Query().Get("host"); got != testEndpoint.SocketDir {
		t.Errorf("host parameter = %q, want the socket's directory %q", got, testEndpoint.SocketDir)
	}
	if got := u.Query().Get("user"); got != testEndpoint.User {
		t.Errorf("user parameter = %q, want %q", got, testEndpoint.User)
	}
	if got := u.Query().Get("sslmode"); got != "disable" {
		t.Errorf("sslmode = %q, want disable", got)
	}
}

// The socket is the directory's file under libpq's own name, which is what AgentURL names and the
// launcher binds.
func TestSocketIsLibpqsNameInTheDirectory(t *testing.T) {
	if got, want := testEndpoint.Socket(), testEndpoint.SocketDir+"/.s.PGSQL.5432"; got != want {
		t.Errorf("Socket() = %q, want %q", got, want)
	}
}
