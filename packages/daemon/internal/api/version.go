package api

// DaemonAPIVersion is the contract this daemon speaks with the Legion plugin (@sjawhar/pi-legion):
// the claim, credential, workflow, controller and state shapes the plugin's client parses strictly
// (`packages/pi-legion/src/daemon-client.ts`, through
// `packages/contracts/src/legion-api.ts`), the pane environment it reads — every variable
// the tmux runtime sets on a pane (`internal/runtime/tmux/spawn.go`'s `panePairs`) and the Sandbox
// runtime on a pod's worker container (`internal/runtime/sandbox/manifest.go`'s
// `mainEnvironment`) — and `LEGION_REMOVABLE_WORKSPACES` on a pod's `workspace-init provision`
// container (`initEnvironment`), which the image's own `legion` decodes strictly. The installed
// plugin declares the number it was built against as `legion.daemonApiVersion` in its
// `package.json`, and the boot gate (`internal/daemon/bootgate.go`) refuses to start unless the
// two are equal.
//
// Bump rule: a change to any of these surfaces bumps this constant and the manifest field in the
// same commit; `packages/contracts/fixtures/daemon-api/version.json`, written by this package's
// golden test and read by the plugin's, holds the two together.
//
// 8: secrets broker enrollment -- AGENT_SECRETS_URL and AGENT_SECRETS_KEY_DIR on an enrolled pod's worker
// container, and the shim's hello2.
//
// 9: secrets broker machine login -- the daemon's own agent-secrets machine login state
// (agentSecretsLogin) on GET /legion/v1/state.
//
// 10: LEGION-208 -- POST /legion/v1/roots/close, the Go legion tool's close_root.
//
// 11: LEGION-208 -- legionAppLogins on POST /legion/v1/gh-token, each Legion role App's login keyed by
// its App role.
//
// 12: LEGION-223 -- one daemon: the manifest field is renamed `legion.daemonApiVersion`, `legion
// probe-image` takes `--daemon-api-version` and its OK line ends `daemon-api-version=<N>`, and no
// pane or pod carries the variable that chose between two daemons' clients. `POST
// /legion/v1/controller/secret` takes the contract `legion controller start` held the controller's
// plugin to (`pluginContract`) and refuses another before it mints.
//
// 13: LEGION-583 -- the claim form of `POST /legion/v1/grants` takes an optional `push` bool,
// true only for a bash command the plugin judges to run `legion push`: such a grant lives
// `credential.pushTTL` (5 minutes) rather than the ordinary `ttl` (60 seconds), since jj's own
// working-copy snapshot before the network push can outrun the ordinary grant on a near-full tree
// volume. `readBody` refuses an unknown field, so a plugin built from this commit sending `push`
// against a daemon older than 13 fails every grant mint, blocking every bash command in every
// pane; the bump is why that never ships paired with an older daemon. 13 also brings
// `initEnvironment`'s `LEGION_REMOVABLE_WORKSPACES` (`runtime.RemovableWorkspacesPayload`) under
// this contract: `workspace-init` refuses a field it does not know and removes nothing, so a
// payload change bumps this number and the image probe pairs the daemon with an image that reads
// it, rather than removal stopping on every pod still running an older image.
//
// 14: LEGION-592 -- the daemon-launched controller (`controller: daemon`): a Sandbox pod's worker
// container may carry `LEGION_CONTROLLER=1` beside `LEGION_BOOT_TOKEN_FILE`, which the plugin must
// answer by registering on `POST /legion/v1/claims/register` with the launch's boot token and
// reporting ready on `claims/ready`. A plugin built before it reads that pod as the operator's
// controller, throws for want of `LEGION_CONTROLLER_SECRET`, and never registers, so the image probe
// must refuse such an image rather than leave the controller's keeper relaunching it forever.
//
// 15: LEGION-462 -- a Sandbox locator on GET /legion/v1/state addresses one role process in its
// issue's shared pod: the issue Sandbox's name, the pod's uid, the role container and the process
// generation, with the incarnation `<pod uid>/<generation>`.
//
// 16: LEGION-578 -- api.State gains `capabilities`, the deployment's capability report
// (capabilities.Deployment.Report): one row per capability of the table, each `present`,
// `installed`, `unchecked`, `live`, `withheld`, `decided` or `open`, an open row carrying the
// legion.yaml line that records a decision. Never null, so a plugin built before it refuses the
// state, and the bump is why the two never meet. (This branch first took 15; LEGION-462 landed at
// 15 first, and pi-legion 8.3.0 declares it without `capabilities`, so a daemon at 15 would pass
// the gate against a plugin whose strict reader refuses its state. Renumbered, as
// docs/solutions/legion/daemon-api-contract-collision-renumber-when-the-release-declaring-the-number-lacks-your-shapes.md says.)
//
// 17: LEGION-631 -- each role's GitHub App token is a file its plain `gh` and `git` read, never a
// grant the plugin redeems: the three credential routes, `POST /legion/v1/gh-token`,
// `POST /legion/v1/git-credential` and `POST /legion/v1/provisioning-credential`, and
// `GrantRequest.push` on `POST /legion/v1/grants` are deleted. The pane and pod environment gains
// `GH_CONFIG_DIR`, the role's directory of gh files (`hosts.yml` and `config.yml` rendered from
// its App token and rewritten as the lease turns over), with `GH_TOKEN`, `GITHUB_TOKEN` and
// `GH_HOST` set to the empty string so nothing in the environment outranks the file — all four
// runtime-set; and it loses `LEGION_GH_PATH`, `LEGION_GIT_PATH`, `LEGION_JJ_PATH` and
// `LEGION_CREDENTIAL_HELPER`, since a pane's gh, git and jj are its PATH's and every shared
// clone's helper is `gh auth git-credential`. No `worker-bin` directory leads PATH: nothing shims
// gh, and `PI_SHELL_PREFIX` puts the `legion` launcher directory first alone. The plugin mints a
// grant only before a bash command that invokes `legion` (`legion push`, say), never before every
// command. `POST /legion/v1/threads/resolve` takes `threads`, the node ids of the review threads
// to resolve (at least one), reads no comment, refuses an id that is no thread of the issue's pull
// request (`THREAD_NOT_ON_PULL_REQUEST`) before any write, and answers `{thread, resolved,
// reason}` per id, with no `withheld` count. A plugin or image built before 17
// would still shim gh over a token file it never reads and mint a grant before every command,
// against routes a daemon on 17 no longer serves, so the boot gate and `legion probe-image`
// refuse the mixed pair. (This branch first took 16; LEGION-578 landed at 16 first, and pi-legion
// 8.4.1 declares it without the token file, still minting a grant before every command for routes
// a daemon on 17 no longer serves, so a daemon at 16 would pass the gate against it. Renumbered,
// as the same note says.)
const DaemonAPIVersion = 17
