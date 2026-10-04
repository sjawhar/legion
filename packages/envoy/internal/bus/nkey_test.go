package bus_test

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/nats-io/nkeys"
	"github.com/sjawhar/envoy/internal/bus"
	"github.com/sjawhar/envoy/internal/contracts"
	"github.com/sjawhar/envoy/internal/testnats"
)

// seedFile writes contents to a 0600 file and returns its path.
func seedFile(t *testing.T, contents string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "nats.seed")
	if err := os.WriteFile(path, []byte(contents), 0o600); err != nil {
		t.Fatalf("write seed file: %v", err)
	}
	return path
}

// unsetNkeyEnvironment clears both seed variables for the test, whatever the shell running it set.
func unsetNkeyEnvironment(t *testing.T) {
	t.Helper()
	for _, name := range []string{"NATS_NKEY_SEED_FILE", "NATS_NKEY_SEED"} {
		t.Setenv(name, "")
		os.Unsetenv(name)
	}
}

func publishOnce(t *testing.T, client *bus.Client, name string) {
	t.Helper()
	err := client.Publish(contracts.Envelope{
		EventID:        "evt-" + name,
		Source:         "github",
		SourceEventID:  "source-" + name,
		Topic:          "notifications.github.acme.widgets.push.branch.main",
		DedupeKey:      name,
		IssuedAt:       contracts.NowMillis(),
		PayloadSummary: name,
		TraceID:        "trace-" + name,
	})
	if err != nil {
		t.Fatalf("publish %s: %v", name, err)
	}
}

// With no seed configured, a client connects to a server that asks for no credential, as every
// client did before servers required one. The shared server asks for a password, so the test
// starts one of its own.
func TestAClientWithNoSeedConnectsToAServerWithoutAuthorization(t *testing.T) {
	unsetNkeyEnvironment(t)
	_, uri := testnats.Start(t)
	client, err := bus.ConnectOwningStream([]string{uri})
	if err != nil {
		t.Fatalf("connect without a seed: %v", err)
	}
	t.Cleanup(client.Close)
	publishOnce(t, client, "no-seed-open-server")
}

// A server that admits only its nkey users accepts the client that names the user's seed, from
// the file or the variable, and every connection the client dials later to recover is that user's
// too.
func TestAClientWithItsSeedConnectsToAServerThatRequiresIt(t *testing.T) {
	seed, public := testnats.User(t)
	uri := testnats.StartNkeyAuthorized(t, public)

	t.Run("NATS_NKEY_SEED_FILE, which wins over NATS_NKEY_SEED", func(t *testing.T) {
		unsetNkeyEnvironment(t)
		t.Setenv("NATS_NKEY_SEED_FILE", seedFile(t, "  "+seed+"\n"))
		t.Setenv("NATS_NKEY_SEED", "not a seed")
		client, err := bus.ConnectOwningStream([]string{uri})
		if err != nil {
			t.Fatalf("connect with the seed file: %v", err)
		}
		t.Cleanup(client.Close)
		publishOnce(t, client, "seed-file")

		// A closed connection is replaced on the next publish, as the same user.
		client.Conn.Close()
		publishOnce(t, client, "seed-file-after-redial")
	})

	t.Run("NATS_NKEY_SEED", func(t *testing.T) {
		unsetNkeyEnvironment(t)
		t.Setenv("NATS_NKEY_SEED", seed)
		// Connect, which owns no stream, publishes into the stream the first client ensured.
		client, err := bus.Connect([]string{uri})
		if err != nil {
			t.Fatalf("connect with the seed variable: %v", err)
		}
		t.Cleanup(client.Close)
		publishOnce(t, client, "seed-variable")
	})
}

// With no seed configured, the server that requires one refuses the client, and the error says so.
func TestAClientWithNoSeedIsRefusedByAServerThatRequiresOne(t *testing.T) {
	unsetNkeyEnvironment(t)
	_, public := testnats.User(t)
	uri := testnats.StartNkeyAuthorized(t, public)
	client, err := bus.Connect([]string{uri})
	if err == nil {
		client.Close()
		t.Fatal("a client with no seed connected to a server that requires an nkey user")
	}
	if !strings.Contains(strings.ToLower(err.Error()), "authorization violation") {
		t.Fatalf("refusal = %v, want the server's authorization violation", err)
	}
}

// A seed configured but unusable is an error before any dial, naming the variable and the path,
// never a fallback to NATS_NKEY_SEED or to connecting without a credential. The URL is one nothing
// listens on, so an attempted dial would fail differently.
func TestAnUnusableSeedIsAnErrorNamingItsVariable(t *testing.T) {
	missing := filepath.Join(t.TempDir(), "missing.seed")
	blank := seedFile(t, " \n")
	notASeed := seedFile(t, "SUNOTASEED")
	accountSeed := func() string {
		account, err := nkeys.CreateAccount()
		if err != nil {
			t.Fatalf("create nkey account: %v", err)
		}
		raw, err := account.Seed()
		if err != nil {
			t.Fatalf("account seed: %v", err)
		}
		return seedFile(t, string(raw))
	}()
	userSeed, _ := testnats.User(t)
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
			unsetNkeyEnvironment(t)
			for name, value := range tc.env {
				t.Setenv(name, value)
			}
			client, err := bus.Connect([]string{"nats://127.0.0.1:1"})
			if err == nil {
				client.Close()
				t.Fatal("connected with an unusable seed")
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("error = %q, want it to contain %q", err, tc.want)
			}
			if strings.Contains(err.Error(), userSeed) || strings.Contains(err.Error(), "hunter2") {
				t.Fatalf("error %q carries the seed", err)
			}
			if _, err := bus.Dial("nkey-test", []string{"nats://127.0.0.1:1"}, os.LookupEnv); err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("Dial error = %v, want it to contain %q", err, tc.want)
			}
		})
	}
}

// A seed against a server with no users is refused, never silently dropped: the server sends no
// nonce, and nats.go refuses the nkey. This pins the deploy order (a process gets a seed only after
// its server has nkey users) and catches a change that falls back to connecting without it. The
// shared server has users, so the test starts one of its own.
func TestASeedAgainstAServerWithoutUsersIsRefused(t *testing.T) {
	_, uri := testnats.Start(t)
	seed, _ := testnats.User(t)
	unsetNkeyEnvironment(t)
	t.Setenv("NATS_NKEY_SEED", seed)
	client, err := bus.Connect([]string{uri})
	if err == nil {
		client.Close()
		t.Fatal("a seed connected to a server with no users")
	}
	if !strings.Contains(err.Error(), "nkeys not supported by the server") {
		t.Fatalf("refusal = %v, want nats.go's nkeys-not-supported error", err)
	}
}
