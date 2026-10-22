package core_test

import (
	"os/exec"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/sjawhar/envoy/infra/core"
	"github.com/sjawhar/envoy/internal/config"
)

// refusedAllowedHosts and acceptedAllowedHosts are the entries the listener's own table holds
// (packages/envoy/internal/config/config_test.go): each refused one carries a port, a scheme, a
// path or a stray bracket, or is no host name or address at all, and each accepted one is a form
// an operator writes. They are this package's allowed-host entries too, so the refusal table of
// TestListenerConfigCheck and the rendering of TestListenerConfigEnv are the same entries the
// agreement below runs through both rules.
var (
	refusedAllowedHosts = []string{
		"hub.internal.example:9020", "http://hub.internal.example", "hub.internal.example/v1",
		"[2001:db8::1]:9020", ".", "hub.internal.example]", "::1]", "-hub.internal.example",
		"hub_name", "*", "192.0.2.10:9020", "192.0.2.10:", "cafe:80", "dead.beef:443", "1:2",
		"[192.0.2.10]", "[cafe]", "[abc.def]", "cafe:9020", "a:b",
	}
	acceptedAllowedHosts = []string{"Hub.Internal.Example.", "192.0.2.7", "[2001:db8::1]"}
)

// core carries its own copy of the listener's ENVOY_ALLOWED_HOSTS rule, since importing the
// listener's config package would widen the plugin's release-cut path list. A copy can drift from
// its original, and a listener deployed with an entry the listener itself refuses does not start
// at all, so this holds the two to one rule: every entry either side refuses, the other refuses.
func TestCoreAndTheListenerAgreeOnAnAllowedHost(t *testing.T) {
	type agreement struct {
		entry   string
		refused bool
	}
	var rows []agreement
	for _, entry := range refusedAllowedHosts {
		rows = append(rows, agreement{entry: entry, refused: true})
	}
	for _, entry := range acceptedAllowedHosts {
		rows = append(rows, agreement{entry: entry})
	}
	for _, row := range rows {
		t.Run(row.entry, func(t *testing.T) {
			t.Setenv("ENVOY_MACHINE_ID", "test-machine")
			t.Setenv("NATS_URLS", "nats://127.0.0.1:4222")
			t.Setenv("ENVOY_ALLOWED_HOSTS", row.entry)
			_, loadErr := config.Load(core.DefaultListenerPort)

			checkErr := core.ListenerConfig{
				MachineID:    "test-machine",
				Port:         core.DefaultListenerPort,
				AllowedHosts: []string{row.entry},
			}.Check()

			require.Equal(t, row.refused, loadErr != nil, "the listener's config.Load answered %v", loadErr)
			require.Equal(t, loadErr != nil, checkErr != nil,
				"config.Load answered %v and core's Check answered %v", loadErr, checkErr)
			if row.refused {
				requireInputError(t, checkErr, "allowedHosts", row.entry)
			}
		})
	}
}

// The test above imports the listener's config package, which go list does not count: a non-test
// import would put it in `go list -deps ./cmd/...`, the path list a plugin release is cut from
// (AGENTS.md, "Release contract"), so an edit to Envoy's config would cut a plugin release.
func TestCoreCompilesNoListenerConfigPackage(t *testing.T) {
	out, err := exec.Command("go", "list", "-deps", "-f", "{{.ImportPath}}", ".").CombinedOutput()
	require.NoError(t, err, string(out))
	for _, dep := range strings.Fields(string(out)) {
		require.NotEqual(t, "github.com/sjawhar/envoy/internal/config", dep,
			"the core package imports the listener's config package")
	}
}
