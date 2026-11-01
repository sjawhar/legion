package natsauth_test

import (
	"testing"

	"github.com/sjawhar/legion/daemon/internal/natsauth"
	"github.com/sjawhar/legion/daemon/internal/runtime"
)

// The variables every daemon child loses are exactly the two a seed's value is read from.
func TestChildrenLoseBothSeedVariables(t *testing.T) {
	got := runtime.WithoutNATSSeeds([]string{natsauth.SeedVariable + "=a", natsauth.DaemonSeedVariable + "=b", "KEEP=c"})
	if len(got) != 1 || got[0] != "KEEP=c" {
		t.Fatalf("WithoutNATSSeeds kept %q, want only KEEP=c", got)
	}
}
