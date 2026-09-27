package runtime

import (
	"slices"
	"testing"
)

// The raw seed variables go; their _FILE pointers, and everything else, stay, in order, and the
// caller's slice is left as it was.
func TestWithoutNATSSeedsDropsOnlyTheRawSeeds(t *testing.T) {
	environ := []string{"PATH=/bin", "NATS_NKEY_SEED=pane", "NATS_NKEY_SEED_FILE=/p", "NATS_DAEMON_NKEY_SEED=daemon", "NATS_DAEMON_NKEY_SEED_FILE=/d", "HOME=/h"}
	before := slices.Clone(environ)
	got := WithoutNATSSeeds(environ)
	if want := []string{"PATH=/bin", "NATS_NKEY_SEED_FILE=/p", "NATS_DAEMON_NKEY_SEED_FILE=/d", "HOME=/h"}; !slices.Equal(got, want) {
		t.Errorf("WithoutNATSSeeds = %q, want %q", got, want)
	}
	if !slices.Equal(environ, before) {
		t.Errorf("WithoutNATSSeeds changed its argument to %q", environ)
	}
}
