package api

// DaemonAPIVersion is the contract this daemon speaks with the Oh My Pi plugin: the claim,
// credential, workflow, controller and state shapes the plugin's client parses strictly
// (`packages/pi-envoy/src/legion/go-daemon-client.ts`, through
// `packages/contracts/src/legion-go-api.ts`), and the pane environment it reads — every variable
// the tmux runtime sets on a pane (`internal/runtime/tmux/spawn.go`'s `panePairs`) and the Sandbox
// runtime on a pod's worker container (`internal/runtime/sandbox/manifest.go`'s
// `mainEnvironment`). The installed plugin declares the number it was built against as
// `legion.daemonApiVersion` in its `package.json`, and the boot gate (`internal/daemon/bootgate.go`)
// refuses to start unless the two are equal.
//
// Bump rule: a change to either surface bumps this constant and the manifest field in the same
// commit; `packages/contracts/fixtures/daemon-api/version.json`, written by this package's golden
// test and read by the plugin's, holds the two together.
//
// 8: AGENTC-393 -- AGENT_SECRETS_URL and AGENT_SECRETS_KEY_DIR on an enrolled pod's worker
// container, and the shim's hello2.
//
// 9: AGENTC-393 Plan C -- the daemon's own agent-secrets machine login state
// (agentSecretsLogin) on GET /legion/v1/state.
//
// 10: LEGION-208 -- POST /legion/v1/roots/close, the Go legion tool's close_root.
//
// 11: LEGION-208 -- legionAppLogins on POST /legion/v1/gh-token, each Legion role App's login keyed by
// its App role.
//
// 12: LEGION-223 -- one daemon: the manifest field is renamed `legion.daemonApiVersion`, `legion
// probe-image` takes `--daemon-api-version` and its OK line ends `daemon-api-version=<N>`, and no
// pane or pod carries the variable that chose between two daemons' clients.
const DaemonAPIVersion = 12
