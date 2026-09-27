package daemon

import (
	"bytes"
	"fmt"
	"log/slog"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/nats-io/nats.go"
	"github.com/nats-io/nats.go/jetstream"
	"github.com/nats-io/nkeys"

	"github.com/sjawhar/legion/daemon/internal/api"
	"github.com/sjawhar/legion/daemon/internal/claim"
	"github.com/sjawhar/legion/daemon/internal/runtime"
	"github.com/sjawhar/legion/daemon/internal/runtime/fake"
	"github.com/sjawhar/legion/daemon/internal/testnats"
)

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
	seed, public := testnats.User(t)
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
		cfg.NatsNkeySeedFile = testnats.SeedFile(t, seed+"\n")
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

// The workflow's connection authenticates as the daemon's own seed when it has one, over the pane
// seed every pane receives, and logs that user once, by public key and whether it is the pane user:
// a server whose one user is the daemon seed's admits a daemon configured with both seeds, and
// refuses one configured with the pane seed alone, which it then connects as.
func TestTheWorkflowConnectsAsTheDaemonSeedOverThePaneSeed(t *testing.T) {
	quickAppMints(t)
	daemonSeed, daemonUser := testnats.User(t)
	paneSeed, paneUser := testnats.User(t)
	url := testnats.StartNkeyAuthorized(t, daemonUser)
	user, err := nkeys.FromSeed([]byte(daemonSeed))
	if err != nil {
		t.Fatal(err)
	}
	conn, err := nats.Connect(url, nats.Nkey(daemonUser, user.Sign), nats.Timeout(5*time.Second))
	if err != nil {
		t.Fatalf("connect as the daemon seed's user: %v", err)
	}
	t.Cleanup(conn.Close)
	js, err := jetstream.New(conn)
	if err != nil {
		t.Fatalf("open JetStream: %v", err)
	}
	if _, err := js.CreateStream(t.Context(), jetstream.StreamConfig{Name: "ENVOY_NOTIFICATIONS", Subjects: []string{"notifications.>"}}); err != nil {
		t.Fatalf("create notification stream: %v", err)
	}

	t.Run("with both seeds", func(t *testing.T) {
		cfg := workflowConfig(t, url)
		cfg.NatsNkeySeedFile = testnats.SeedFile(t, paneSeed+"\n")
		cfg.NatsDaemonNkeySeedFile = testnats.SeedFile(t, daemonSeed+"\n")
		var out syncBuffer
		tokens, _ := appTokens(t, func(w http.ResponseWriter, _ *http.Request, _ string, _ int) { installed(w) })
		if err := bootsOrExitsLogging(t, cfg, tokens, 60*time.Second, slog.New(slog.NewTextHandler(&out, nil))); err != nil {
			t.Fatalf("boot with both seeds = %v, want the daemon serving", err)
		}
		logged := out.String()
		want := `level=INFO msg="legion daemon connects to NATS" user=` + daemonUser + ` paneUser=false seed=daemon`
		if strings.Count(logged, "legion daemon connects to NATS") != 1 || !strings.Contains(logged, want) {
			t.Errorf("log =\n%s\nwant exactly one line containing %s", logged, want)
		}
		for _, seed := range []string{daemonSeed, paneSeed} {
			if strings.Contains(logged, seed) {
				t.Errorf("log carries a seed:\n%s", logged)
			}
		}
	})
	t.Run("with the pane seed alone", func(t *testing.T) {
		cfg := workflowConfig(t, url)
		cfg.NatsNkeySeedFile = testnats.SeedFile(t, paneSeed+"\n")
		var out syncBuffer
		tokens, _ := appTokens(t, func(w http.ResponseWriter, _ *http.Request, _ string, _ int) { installed(w) })
		err := bootsOrExitsLogging(t, cfg, tokens, 60*time.Second, slog.New(slog.NewTextHandler(&out, nil)))
		if err == nil || !strings.Contains(strings.ToLower(err.Error()), "authorization violation") {
			t.Fatalf("boot with the pane seed alone = %v, want the server's authorization violation", err)
		}
		want := `level=INFO msg="legion daemon connects to NATS" user=` + paneUser + ` paneUser=true seed=pane`
		if !strings.Contains(out.String(), want) {
			t.Errorf("log =\n%s\nwant a line containing %s", out.String(), want)
		}
	})
}

// chooseNATSConnection picks the daemon seed, else the pane seed, else none, and its one line says
// which user that is and whether it is the pane user, the daemon seed naming the pane user included.
func TestChooseNATSConnectionNamesTheUserTheDaemonConnectsAs(t *testing.T) {
	daemonSeed, daemonUser := testnats.User(t)
	paneSeed, paneUser := testnats.User(t)
	for _, tc := range []struct {
		name, daemon, pane, paneUser, seed, line string
	}{
		{"both", daemonSeed, paneSeed, paneUser, daemonSeed, "user=" + daemonUser + " paneUser=false seed=daemon"},
		{"the daemon seed alone", daemonSeed, "", "", daemonSeed, "user=" + daemonUser + " paneUser=false seed=daemon"},
		{"a daemon seed that is the pane seed", paneSeed, paneSeed, paneUser, paneSeed, "user=" + paneUser + " paneUser=true seed=daemon"},
		{"the pane seed alone", "", paneSeed, paneUser, paneSeed, "user=" + paneUser + " paneUser=true seed=pane"},
		{"neither", "", "", "", "", `user="" paneUser=false seed=none`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			nc, err := chooseNATSConnection(tc.daemon, tc.pane, tc.paneUser)
			// Named, never printed: a failure message is no place for a seed.
			name := map[string]string{daemonSeed: "the daemon seed", paneSeed: "the pane seed", "": "none"}
			if err != nil || nc.seed != tc.seed {
				t.Fatalf("chooseNATSConnection = %s, %v; want %s", name[nc.seed], err, name[tc.seed])
			}
			var out bytes.Buffer
			nc.log(slog.New(slog.NewTextHandler(&out, nil)))
			want := `level=INFO msg="legion daemon connects to NATS" ` + tc.line + "\n"
			if !strings.HasSuffix(out.String(), want) || strings.Count(out.String(), "\n") != 1 {
				t.Errorf("log = %q, want one line ending %q", out.String(), want)
			}
		})
	}
}

// The daemon's own seed reaches no launch: with both seeds configured, a root's and a worker's
// launch carry the pane seed as NATS_NKEY_SEED and nothing of the daemon seed, in no secret, no
// variable, and no other field of the spec.
func TestNoLaunchCarriesTheDaemonSeed(t *testing.T) {
	daemonSeed, paneSeed := testnats.UserSeed(t), testnats.UserSeed(t)
	for _, tc := range []struct {
		name    string
		key     string
		environ []string
	}{
		{"nats_daemon_nkey_seed_file", testnats.SeedFile(t, daemonSeed), nil},
		{"NATS_DAEMON_NKEY_SEED_FILE", "", []string{"NATS_DAEMON_NKEY_SEED_FILE=" + testnats.SeedFile(t, daemonSeed)}},
		{"NATS_DAEMON_NKEY_SEED", "", []string{"NATS_DAEMON_NKEY_SEED=" + daemonSeed}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cfg := testConfig(t)
			cfg.NatsNkeySeedFile = testnats.SeedFile(t, paneSeed)
			cfg.NatsDaemonNkeySeedFile = tc.key
			rt := fake.NewRuntime()
			o := fakeRuntime(rt, &built{})
			o.environ = tc.environ
			d := startDaemon(t, cfg, o)

			root := lastLaunch(t, rt, d.spawn(architect()))
			worker := lastLaunch(t, rt, d.spawn(api.SpawnRequest{Tree: "LEGION-1", Issue: "LEGION-2", Role: claim.RoleImplementer, Prompt: "Wait."}))
			for kind, spec := range map[string]runtime.SpawnSpec{"root": root, "worker": worker} {
				if got := spec.Secrets["NATS_NKEY_SEED"]; got != paneSeed {
					t.Errorf("the %s launch's NATS_NKEY_SEED secret is %q, want the pane seed", kind, got)
				}
				if dump := fmt.Sprintf("%#v", spec); strings.Contains(dump, daemonSeed) || strings.Contains(dump, "NATS_DAEMON") {
					t.Errorf("the %s launch carries the daemon seed or its name: %s", kind, dump)
				}
			}
		})
	}
}
