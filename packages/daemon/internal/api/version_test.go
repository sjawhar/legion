package api

import (
	"testing"

	"github.com/sjawhar/legion/daemon/internal/supervise"
)

// The contract number the boot gate requires of the installed plugin is written where both sides
// read it: `packages/pi-legion/src/daemon-api-version.test.ts` pins the plugin manifest's
// `legion.daemonApiVersion` to this fixture, so a bump on one side alone fails a test.
func TestDaemonAPIVersionGolden(t *testing.T) {
	golden(t, "version.json", map[string]int{"daemonApiVersion": DaemonAPIVersion})
}

// supervise.PodUpdateContract is the contract that introduced ready_for_update, and supervise
// cannot read DaemonAPIVersion: internal/api imports supervise, not the other way round. This is
// the one place the two meet, so a later bump of DaemonAPIVersion can never leave the constant
// above the daemon's own contract, where every claim would read as unable to answer an update
// round and no pod would ever be replaced.
func TestPodUpdateContractIsNotAboveTheDaemonContract(t *testing.T) {
	if supervise.PodUpdateContract > DaemonAPIVersion {
		t.Fatalf("supervise.PodUpdateContract = %d, above DaemonAPIVersion %d: no claim could answer an update round",
			supervise.PodUpdateContract, DaemonAPIVersion)
	}
}
