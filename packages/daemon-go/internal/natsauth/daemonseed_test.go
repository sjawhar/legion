package natsauth_test

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/sjawhar/legion/daemon/internal/natsauth"
	"github.com/sjawhar/legion/daemon/internal/testnats"
)

// DaemonSeed reads the daemon's own seed by Seed's precedence under its own names: the configuration
// key's file over NATS_DAEMON_NKEY_SEED_FILE over NATS_DAEMON_NKEY_SEED. It never reads the pane
// seed's names, and Seed never reads its.
func TestTheDaemonSeedResolvesByItsOwnNamesAlone(t *testing.T) {
	daemon, _ := testnats.User(t)
	pane, _ := testnats.User(t)
	other, _ := testnats.User(t)
	for _, tc := range []struct {
		name string
		key  string
		env  map[string]string
		want string
	}{
		{"the key's file, over both variables", testnats.SeedFile(t, daemon+"\n"),
			map[string]string{"NATS_DAEMON_NKEY_SEED_FILE": "", "NATS_DAEMON_NKEY_SEED": "hunter2"}, daemon},
		{"NATS_DAEMON_NKEY_SEED_FILE, over NATS_DAEMON_NKEY_SEED", "",
			map[string]string{"NATS_DAEMON_NKEY_SEED_FILE": testnats.SeedFile(t, "\n"+daemon+"  \n"), "NATS_DAEMON_NKEY_SEED": other}, daemon},
		{"NATS_DAEMON_NKEY_SEED", "", map[string]string{"NATS_DAEMON_NKEY_SEED": " " + daemon + " "}, daemon},
		{"none, whatever the pane seed's names hold", "",
			map[string]string{"NATS_NKEY_SEED_FILE": testnats.SeedFile(t, pane), "NATS_NKEY_SEED": pane}, ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, err := natsauth.DaemonSeed(tc.key, env(tc.env))
			if err != nil || got != tc.want {
				t.Fatalf("DaemonSeed answered another seed than the case's (error %v)", err)
			}
		})
	}
	if got, err := natsauth.Seed("", env(map[string]string{"NATS_DAEMON_NKEY_SEED": daemon})); got != "" || err != nil {
		t.Errorf("Seed with only NATS_DAEMON_NKEY_SEED set answered a seed (error %v); want no pane seed", err)
	}
}

// A daemon seed configured but unusable is an error naming its key or variable and the path, under
// the pane seed's file mode rule (config.ReadGroupSecretPointer: a file the daemon's uid owns is
// held to 0600, and only one another uid owns may be group-readable), never a fallback to the next source, to the pane seed, or to no
// credential, and never carrying the seed.
func TestAnUnusableDaemonSeedIsAnErrorNamingItsSource(t *testing.T) {
	userSeed, _ := testnats.User(t)
	missing := filepath.Join(t.TempDir(), "missing.seed")
	blank := testnats.SeedFile(t, " \n")
	notASeed := testnats.SeedFile(t, "SUNOTASEED")
	accountSeed := testnats.SeedFile(t, testnats.Account(t))
	good := testnats.SeedFile(t, userSeed)
	public := testnats.SeedFile(t, userSeed)
	if err := os.Chmod(public, 0o604); err != nil {
		t.Fatal(err)
	}
	writable := testnats.SeedFile(t, userSeed)
	if err := os.Chmod(writable, 0o660); err != nil {
		t.Fatal(err)
	}
	pane := map[string]string{"NATS_NKEY_SEED": userSeed}
	for _, tc := range []struct {
		name string
		key  string
		env  map[string]string
		want string
	}{
		{"a missing key file", missing, map[string]string{"NATS_DAEMON_NKEY_SEED_FILE": good}, "nats_daemon_nkey_seed_file names " + missing + ", which could not be read"},
		{"a blank key file", blank, pane, "nats_daemon_nkey_seed_file names " + blank + ", which is empty"},
		{"a key file holding no seed", notASeed, map[string]string{"NATS_DAEMON_NKEY_SEED": userSeed}, "nats_daemon_nkey_seed_file (" + notASeed + ") does not hold a valid nkey seed"},
		{"a key file holding an account seed", accountSeed, nil, "nats_daemon_nkey_seed_file (" + accountSeed + ") holds an nkey seed that is not a user's"},
		{"a key file others can read", public, nil, "nats_daemon_nkey_seed_file " + public + " is readable by its group or others (mode 0604); chmod 0600 it"},
		{"a key file its group can read and write", writable, nil, "nats_daemon_nkey_seed_file " + writable + " is readable by its group or others (mode 0660); chmod 0600 it"},
		{"a missing file", "", map[string]string{"NATS_DAEMON_NKEY_SEED_FILE": missing, "NATS_DAEMON_NKEY_SEED": userSeed}, "NATS_DAEMON_NKEY_SEED_FILE names " + missing + ", which could not be read"},
		{"a blank file", "", map[string]string{"NATS_DAEMON_NKEY_SEED_FILE": blank}, "NATS_DAEMON_NKEY_SEED_FILE names " + blank + ", which is empty"},
		{"an empty pointer", "", map[string]string{"NATS_DAEMON_NKEY_SEED_FILE": "", "NATS_DAEMON_NKEY_SEED": userSeed, "NATS_NKEY_SEED": userSeed}, "NATS_DAEMON_NKEY_SEED_FILE is set but empty"},
		{"a file holding no seed", "", map[string]string{"NATS_DAEMON_NKEY_SEED_FILE": notASeed}, "NATS_DAEMON_NKEY_SEED_FILE (" + notASeed + ") does not hold a valid nkey seed"},
		{"a file others can read", "", map[string]string{"NATS_DAEMON_NKEY_SEED_FILE": public}, "NATS_DAEMON_NKEY_SEED_FILE " + public + " is readable by its group or others (mode 0604); chmod 0600 it"},
		{"a blank variable", "", map[string]string{"NATS_DAEMON_NKEY_SEED": "  ", "NATS_NKEY_SEED": userSeed}, "NATS_DAEMON_NKEY_SEED is set but empty"},
		{"a variable holding no seed", "", map[string]string{"NATS_DAEMON_NKEY_SEED": "hunter2"}, "NATS_DAEMON_NKEY_SEED does not hold a valid nkey seed"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := natsauth.DaemonSeed(tc.key, env(tc.env)); err == nil {
				t.Fatal("DaemonSeed answered a seed, want a refusal")
			} else if !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("error = %q, want it to contain %q", err, tc.want)
			} else if strings.Contains(err.Error(), userSeed) || strings.Contains(err.Error(), "hunter2") {
				t.Fatal("the error carries the seed")
			}
		})
	}
	// A daemon-owned 0640 seed is refused as the pane seed's is.
	shared := testnats.SeedFile(t, userSeed)
	if err := os.Chmod(shared, 0o640); err != nil {
		t.Fatal(err)
	}
	if _, err := natsauth.DaemonSeed(shared, env(nil)); err == nil || !strings.HasSuffix(err.Error(), " is readable by its group or others (mode 0640); chmod 0600 it") {
		t.Errorf("DaemonSeed(daemon-owned 0640 key file) = %v; want the 0600 refusal", err)
	}
}
