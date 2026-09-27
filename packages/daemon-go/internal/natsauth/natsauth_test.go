package natsauth_test

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/nats-io/nats.go"
	"github.com/nats-io/nats.go/jetstream"
	"github.com/nats-io/nkeys"

	"github.com/sjawhar/legion/daemon/internal/natsauth"
	"github.com/sjawhar/legion/daemon/internal/testnats"
)

func TestMain(m *testing.M) { os.Exit(testnats.Main(m)) }

// env is a lookup over exactly the variables it names, whatever the shell running the test set.
func env(values map[string]string) func(string) (string, bool) {
	return func(name string) (string, bool) {
		value, ok := values[name]
		return value, ok
	}
}

func testUser(t *testing.T) (seed, public string) {
	t.Helper()
	user, err := nkeys.CreateUser()
	if err != nil {
		t.Fatalf("create nkey user: %v", err)
	}
	raw, err := user.Seed()
	if err != nil {
		t.Fatalf("user seed: %v", err)
	}
	public, err = user.PublicKey()
	if err != nil {
		t.Fatalf("user public key: %v", err)
	}
	return string(raw), public
}

func seedFile(t *testing.T, contents string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "nats.seed")
	if err := os.WriteFile(path, []byte(contents), 0o600); err != nil {
		t.Fatalf("write seed file: %v", err)
	}
	return path
}

// usable proves a connection does what the daemon's boot does with it: open JetStream and read the
// account.
func usable(t *testing.T, conn *nats.Conn) {
	t.Helper()
	js, err := jetstream.New(conn)
	if err != nil {
		t.Fatalf("open JetStream: %v", err)
	}
	if _, err := js.AccountInfo(t.Context()); err != nil {
		t.Fatalf("JetStream account info: %v", err)
	}
}

// With no seed configured, the daemon connects to a server that asks for no credential, as it did
// before servers required one.
func TestNoSeedConnectsToAServerWithoutAuthorization(t *testing.T) {
	conn, err := natsauth.Connect([]string{testnats.URL(t)}, env(nil), nats.Timeout(5*time.Second))
	if err != nil {
		t.Fatalf("connect without a seed: %v", err)
	}
	t.Cleanup(conn.Close)
	usable(t, conn)
}

// A server that admits only its nkey users accepts the daemon naming the user's seed, from the file
// (which wins over the variable) or the variable.
func TestItsSeedConnectsToAServerThatRequiresIt(t *testing.T) {
	seed, public := testUser(t)
	url := testnats.StartNkeyAuthorized(t, public)
	for _, tc := range []struct {
		name string
		env  map[string]string
	}{
		{"NATS_NKEY_SEED_FILE, over NATS_NKEY_SEED", map[string]string{"NATS_NKEY_SEED_FILE": seedFile(t, "\n"+seed+"  \n"), "NATS_NKEY_SEED": "not a seed"}},
		{"NATS_NKEY_SEED", map[string]string{"NATS_NKEY_SEED": seed}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			conn, err := natsauth.Connect([]string{url}, env(tc.env), nats.Timeout(5*time.Second))
			if err != nil {
				t.Fatalf("connect with the seed: %v", err)
			}
			t.Cleanup(conn.Close)
			usable(t, conn)
		})
	}
}

// With no seed configured, the server that requires one refuses the daemon, and the error says so.
func TestNoSeedIsRefusedByAServerThatRequiresOne(t *testing.T) {
	_, public := testUser(t)
	url := testnats.StartNkeyAuthorized(t, public)
	conn, err := natsauth.Connect([]string{url}, env(nil), nats.Timeout(5*time.Second))
	if err == nil {
		conn.Close()
		t.Fatal("connected with no seed to a server that requires an nkey user")
	}
	if !strings.Contains(strings.ToLower(err.Error()), "authorization violation") {
		t.Fatalf("refusal = %v, want the server's authorization violation", err)
	}
}

// A seed configured but unusable is an error before any dial, naming the variable and the path,
// never a fallback to NATS_NKEY_SEED or to connecting without a credential. The URL is one nothing
// listens on, so an attempted dial would fail differently.
func TestAnUnusableSeedIsAnErrorNamingItsVariable(t *testing.T) {
	userSeed, _ := testUser(t)
	missing := filepath.Join(t.TempDir(), "missing.seed")
	blank := seedFile(t, " \n")
	notASeed := seedFile(t, "SUNOTASEED")
	account, err := nkeys.CreateAccount()
	if err != nil {
		t.Fatalf("create nkey account: %v", err)
	}
	rawAccountSeed, err := account.Seed()
	if err != nil {
		t.Fatalf("account seed: %v", err)
	}
	accountSeed := seedFile(t, string(rawAccountSeed))
	for _, tc := range []struct {
		name string
		env  map[string]string
		want string
	}{
		{"a missing file", map[string]string{"NATS_NKEY_SEED_FILE": missing, "NATS_NKEY_SEED": userSeed}, "NATS_NKEY_SEED_FILE names " + missing + ", which could not be read"},
		{"a blank file", map[string]string{"NATS_NKEY_SEED_FILE": blank}, "NATS_NKEY_SEED_FILE names " + blank + ", which is empty"},
		{"an empty pointer", map[string]string{"NATS_NKEY_SEED_FILE": ""}, "NATS_NKEY_SEED_FILE is set but empty"},
		{"a file holding no seed", map[string]string{"NATS_NKEY_SEED_FILE": notASeed}, "NATS_NKEY_SEED_FILE (" + notASeed + ") does not hold a valid nkey seed"},
		{"a file holding an account seed", map[string]string{"NATS_NKEY_SEED_FILE": accountSeed}, "NATS_NKEY_SEED_FILE (" + accountSeed + ") holds an nkey seed that is not a user's"},
		{"a blank variable", map[string]string{"NATS_NKEY_SEED": "  "}, "NATS_NKEY_SEED is set but empty"},
		{"a variable holding no seed", map[string]string{"NATS_NKEY_SEED": "hunter2"}, "NATS_NKEY_SEED does not hold a valid nkey seed"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			conn, err := natsauth.Connect([]string{"nats://127.0.0.1:1"}, env(tc.env))
			if err == nil {
				conn.Close()
				t.Fatal("connected with an unusable seed")
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("error = %q, want it to contain %q", err, tc.want)
			}
			if strings.Contains(err.Error(), userSeed) || strings.Contains(err.Error(), "hunter2") {
				t.Fatalf("error %q carries the seed", err)
			}
		})
	}
}
