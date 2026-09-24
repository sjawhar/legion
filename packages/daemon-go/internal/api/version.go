package api

// GoDaemonAPIVersion is the contract this daemon speaks with the Oh My Pi plugin: the
// `/legion/v1/claims/*` and `/legion/v1/state` shapes the plugin's Go client parses strictly
// (`packages/pi-envoy/src/legion/go-daemon-client.ts`, through
// `packages/contracts/src/legion-go-api.ts`), and the pane environment it reads — every variable
// the tmux runtime sets on a pane (`internal/runtime/tmux/spawn.go`'s `panePairs`), with
// `LEGION_DAEMON_API=go` the one that selects that client. The installed plugin declares the
// number it was built against as `legion.goDaemonApiVersion` in its `package.json`, and the boot
// gate (`internal/daemon/bootgate.go`) refuses to start unless the two are equal.
//
// Bump rule: a change to either surface bumps this constant and the manifest field in the same
// commit; `packages/contracts/fixtures/daemon-api/version.json`, written by this package's golden
// test and read by the plugin's, holds the two together. The TypeScript daemon's own contract,
// `legion.daemonApiVersion` against `LEGION_DAEMON_API_VERSION`, is a separate number that moves on
// its own until Stage 7 removes it.
const GoDaemonAPIVersion = 3
