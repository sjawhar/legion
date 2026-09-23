package api

import "testing"

// The contract number the boot gate requires of the installed plugin is written where both sides
// read it: `packages/pi-envoy/src/legion/daemon-api-version.test.ts` pins the plugin manifest's
// `legion.goDaemonApiVersion` to this fixture, so a bump on one side alone fails a test.
func TestGoDaemonAPIVersionGolden(t *testing.T) {
	golden(t, "version.json", map[string]int{"goDaemonApiVersion": GoDaemonAPIVersion})
}
