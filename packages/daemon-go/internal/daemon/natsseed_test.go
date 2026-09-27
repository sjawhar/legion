package daemon

import (
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/nats-io/nats.go"
	"github.com/nats-io/nats.go/jetstream"
	"github.com/nats-io/nkeys"

	"github.com/sjawhar/legion/daemon/internal/testnats"
)

// userSeed is a fresh nkey user's seed.
func userSeed(t *testing.T) string {
	t.Helper()
	seed, _ := nkeyUser(t)
	return seed
}

// nkeyUser is a fresh nkey user's seed and public key.
func nkeyUser(t *testing.T) (seed, public string) {
	t.Helper()
	user, err := nkeys.CreateUser()
	if err != nil {
		t.Fatalf("create nkey user: %v", err)
	}
	raw, err := user.Seed()
	if err != nil {
		t.Fatalf("user seed: %v", err)
	}
	if public, err = user.PublicKey(); err != nil {
		t.Fatalf("user public key: %v", err)
	}
	return string(raw), public
}

// accountSeed is a fresh nkey account's seed: a valid seed, but not a user's.
func accountSeed(t *testing.T) string {
	t.Helper()
	account, err := nkeys.CreateAccount()
	if err != nil {
		t.Fatalf("create nkey account: %v", err)
	}
	raw, err := account.Seed()
	if err != nil {
		t.Fatalf("account seed: %v", err)
	}
	return string(raw)
}

// writeSeed writes contents to a 0600 file of its own and answers its path.
func writeSeed(t *testing.T, contents string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "legion-pane.nk")
	if err := os.WriteFile(path, []byte(contents), 0o600); err != nil {
		t.Fatalf("write the seed file: %v", err)
	}
	return path
}

// lookup is os.LookupEnv over exactly values, whatever the shell running the test set.
func lookup(values map[string]string) func(string) (string, bool) {
	return func(name string) (string, bool) {
		value, ok := values[name]
		return value, ok
	}
}

// The workflow's own NATS connection authenticates as the seed the daemon resolved from
// nats_nkey_seed_file — the seed every pane receives — so a daemon boots against a server whose one
// user is that seed's nkey, and without it the same server refuses the daemon's boot.
func TestTheWorkflowConnectsAsTheNatsSeedTheDaemonResolved(t *testing.T) {
	quickAppMints(t)
	seed, public := nkeyUser(t)
	url := testnats.StartNkeyAuthorized(t, public)
	user, err := nkeys.FromSeed([]byte(seed))
	if err != nil {
		t.Fatal(err)
	}
	conn, err := nats.Connect(url, nats.Nkey(public, user.Sign), nats.Timeout(5*time.Second))
	if err != nil {
		t.Fatalf("connect as the seed's user: %v", err)
	}
	t.Cleanup(conn.Close)
	js, err := jetstream.New(conn)
	if err != nil {
		t.Fatalf("open JetStream: %v", err)
	}
	if _, err := js.CreateStream(t.Context(), jetstream.StreamConfig{Name: "ENVOY_NOTIFICATIONS", Subjects: []string{"notifications.>"}}); err != nil {
		t.Fatalf("create notification stream: %v", err)
	}

	t.Run("with the seed", func(t *testing.T) {
		cfg := workflowConfig(t, url)
		cfg.NatsNkeySeedFile = writeSeed(t, seed+"\n")
		tokens, _ := appTokens(t, func(w http.ResponseWriter, _ *http.Request, _ string, _ int) { installed(w) })
		if err := bootsOrExits(t, cfg, tokens, 60*time.Second); err != nil {
			t.Fatalf("boot with the seed = %v, want the daemon serving", err)
		}
	})
	t.Run("without one", func(t *testing.T) {
		cfg := workflowConfig(t, url)
		tokens, _ := appTokens(t, func(w http.ResponseWriter, _ *http.Request, _ string, _ int) { installed(w) })
		err := bootsOrExits(t, cfg, tokens, 60*time.Second)
		if err == nil || !strings.Contains(strings.ToLower(err.Error()), "authorization violation") {
			t.Fatalf("boot without a seed = %v, want the server's authorization violation", err)
		}
	})
}
