package fixtureenv

import (
	"os"
	"testing"
)

// A live proof of the e2e build takes back, for its own run, what the process had before Guard:
// the operator's credential and git configuration, and no GIT_ALLOW_PROTOCOL. Every test after it
// runs guarded again.
func TestLiveGivesALiveProofTheProcesssOwnEnvironmentForItsRunAlone(t *testing.T) {
	keep, before := guardedKeep, unguarded
	t.Cleanup(func() { guardedKeep, unguarded = keep, before })
	// A process Guard has not run in yet, holding what an operator's shell does.
	guardedKeep, unguarded = nil, nil
	own := map[string]string{
		"GH_TOKEN": "the operator's token", "GIT_CONFIG_GLOBAL": "/home/operator/.gitconfig",
		"LEGION_WORKSPACE": "/home/operator/src/legion",
	}
	for name, value := range own {
		t.Setenv(name, value)
	}
	t.Setenv("GIT_ALLOW_PROTOCOL", "file")
	if err := os.Unsetenv("GIT_ALLOW_PROTOCOL"); err != nil {
		t.Fatal(err)
	}
	if err := Guard(); err != nil {
		t.Fatal(err)
	}

	t.Run("the live proof", func(t *testing.T) {
		Live(t)
		for name, value := range own {
			if got := os.Getenv(name); got != value {
				t.Errorf("%s=%q in the live proof, want the process's own %q", name, got, value)
			}
		}
		if value, set := os.LookupEnv("GIT_ALLOW_PROTOCOL"); set {
			t.Errorf("GIT_ALLOW_PROTOCOL=%q in the live proof, want it unset as the process had it", value)
		}
	})

	RequireGuarded(t)
	for name := range own {
		if _, set := os.LookupEnv(name); set {
			t.Errorf("the process holds %s after the live proof, which Guard took out", name)
		}
	}
}
