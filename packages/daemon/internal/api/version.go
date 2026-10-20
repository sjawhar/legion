package api

// DaemonAPIVersion is the contract this daemon speaks with the Legion plugin (@sjawhar/pi-legion):
// the claim, credential, workflow, controller and state shapes the plugin's client parses strictly
// (`packages/pi-shared/src/daemon-client.ts`, through
// `packages/contracts/src/legion-api.ts`), the pane environment it reads — every variable
// the tmux runtime sets on a pane (`internal/runtime/tmux/spawn.go`'s `panePairs`) and the Sandbox
// runtime on a pod's worker container (`internal/runtime/sandbox/manifest.go`'s
// `mainEnvironment`) — and the command line and environment of a pod's `workspace-init provision`
// container (`initContainers` in `internal/runtime/sandbox/podkind.go`, and `initEnvironment`),
// which the image's own `legion` reads. The installed plugin declares the
// number it was built against as `legion.daemonApiVersion` in its `package.json`, and the boot
// gate (`internal/daemon/bootgate.go`) refuses to start unless the two are equal.
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
// 17: LEGION-588 -- the role prompts the daemon embeds name the `dispatch` command an agent runs in
// its shell, which the Envoy plugin puts on the pane's PATH, rather than the `dispatch_*` tools it
// no longer registers: a daemon and a plugin from either side of that change would hand agents
// instructions for a surface they lack. (This branch first took 16; LEGION-578 landed at 16
// first, and pi-legion 8.4.0 declares it beside a pi-envoy 8.4.0 that still registers the native
// tools and puts no `dispatch` on the PATH, so a daemon at 16 would pass the gate against panes
// that lack the command its prompts name. Renumbered, as the same note says.)
//
// 18: LEGION-632 -- each issue's pod is independent, on a volume of its own, so the pod's
// `workspace-init provision` container no longer carries `LEGION_REMOVABLE_WORKSPACES` (the
// removable-workspaces pass is gone with the shared tree volume), nor
// `LEGION_WORKSPACE_INIT_LOCK_WAIT_SECONDS` (no two provisions share a repository, so the flock
// and its wait are gone), nor the `LEGION_ROLE` and `LEGION_GENERATION` that seeded that pass's
// candidate rotation; and `LEGION_EXPECT_TREE_VOLUME` is now `LEGION_EXPECT_ISSUE_VOLUME`. An image
// built before 18 acts on a contract this daemon no longer speaks: it looks for the old name and so
// never learns the volume must already hold the clone, provisioning a lost volume afresh instead of
// exiting 3, and runs a lock and a removal pass against variables nothing sets; the image probe
// pairs the daemon with an image built from this contract instead. Each role's agent is told a
// state home of its own (`XDG_STATE_HOME=/home/legion/.local/state/<role>`), which the image's
// shim makes Oh My Pi's profile directory under, so the browser broker's lock is one name per
// container. (This branch first took 16, then 17; LEGION-578 landed at 16 and LEGION-588 at 17
// first, and pi-legion 8.4.1 and 8.5.0 declare those with the init container's old environment,
// so a daemon at either would pass the gate against an image whose `workspace-init` speaks the
// other contract. Renumbered twice, as the same learning says.)
//
// 19: LEGION-631 -- each role's GitHub App token is a file its plain `gh` and `git` read, never a
// grant the plugin redeems: the three credential routes, `POST /legion/v1/gh-token`,
// `POST /legion/v1/git-credential` and `POST /legion/v1/provisioning-credential`, and
// `GrantRequest.push` on `POST /legion/v1/grants` are deleted. The pane and pod environment gains
// `GH_CONFIG_DIR`, the role's directory of gh files (`hosts.yml` and `config.yml` rendered from
// its App token and rewritten as the lease turns over), with `GH_TOKEN`, `GITHUB_TOKEN` and
// `GH_HOST` set to the empty string so nothing in the environment outranks the file — all four
// runtime-set — and loses `LEGION_GH_PATH`, `LEGION_GIT_PATH`, `LEGION_JJ_PATH`,
// `LEGION_CREDENTIAL_HELPER` and `LEGION_GRANT_FILE`: a pane's gh, git and jj are its PATH's,
// every shared clone's helper is `gh auth git-credential`, and nothing in a pane runs `legion`
// from bash any more, since the `legion` tool mints its grants in-process for its own daemon calls
// (`handoff_complete`, `read_record`, the controller's `read_state` and `set_status`). No
// `worker-bin` directory leads PATH: nothing shims gh, and `PI_SHELL_PREFIX` puts the `legion`
// launcher directory first alone. `HandoffCompleteResponse` gains `note` (READY's three refusals
// run in the daemon on the pull request's head: `.legion/<issue>/` still carried, a conflict with
// the base, a required check not green or a required workflow without a passing run); the
// request's `commit` is the pushed commit carrying the phase's handoff, which the `legion` tool's
// `handoff_complete` finds in the pane as `legion handoff complete` did.
// `POST /legion/v1/threads/resolve` is deleted: the reviewer names the bot threads
// it accepted to the implementer, who resolves them with plain `gh` as the pull request's author. A
// plugin or image built before 19 would shim gh over a token file it never reads and mint a grant
// before every command, against a daemon on 19 that serves none of that, so the boot gate and
// `legion probe-image` refuse the mixed pair. (This branch first took 16, then 17, then 18;
// LEGION-578 landed at 16, LEGION-588 at 17 and LEGION-632 at 18 first, pi-legion 8.4.1, 8.6.0
// and 8.9.0 declaring them without the token file, so a daemon at any of them would pass the gate
// against them. Renumbered three times, as the collision note says.)
//
// 20: LEGION-668 -- the controller holds the review App's GitHub token like every role: `POST
// /legion/v1/controller/github-credential` is the route `legion controller start` fetches the
// controller's gh files from and refreshes them over, with the controller capability. The
// controller pod's agent environment gains `GH_CONFIG_DIR`, `GH_TOKEN`, `GITHUB_TOKEN` and
// `GH_HOST` exactly as an issue pod's role container has them, backed by a `gh-controller` volume
// of its role Secret. A `legion` built before 20 never fetches the credential, so its controller's
// gh acts as nobody against a daemon that expects it to, and a daemon before 20 serves no such
// route, so `legion controller start` from 20 is refused after the mint and cuts the incumbent
// controller off for nothing: the boot gate and `legion probe-image` refuse the mixed pair. (This
// branch first took 19; LEGION-631 landed at 19 first, pi-legion 8.10.1 declaring it without this
// route, so a daemon at 19 would pass the gate against it. Renumbered, as the collision note says.)
//
// 21: LEGION-663 -- every Legion session reports the six live capability rows with its ready, and
// the daemon renders them from those reports. `claim.RegisterResponse` gains `promptAgents`, the
// sorted names of the task agents the role's prompts dispatch, which the session's subagents check
// measures against; `claim.ReadyRequest` gains an optional `capabilities` report (`measuredAt`,
// `elapsedMs`, `rows[{name, ok, detail}]`), which the daemon normalises (capabilities.Normalize)
// and never refuses: a session runs and reports, whatever it reports. `api.ClaimView` gains
// `capabilities` (`measuredAt`, `incarnation`, `ok`, `open`), the claim's latest report as the
// state shows it, and `api.CapabilityState.status` loses `live`: a live row is `present`, `open` or
// `unchecked` from the sessions' reports, never a promise of a check to come. A 20 plugin's strict
// reader refuses a state whose claim views carry `capabilities`, and a 21 plugin's `capabilities`
// on `claims/ready` is an unknown field a 20 daemon's `readBody` refuses with 400, which exits the
// plugin, so the boot gate and `legion probe-image` refuse the mixed pair. (This branch first took
// 19, based on LEGION-631's branch while that branch declared 18; LEGION-631 landed at 19 first,
// pi-legion 8.11.0 declaring it without the report. Renumbered to 20 at the retarget to main; then
// LEGION-668 landed at 20 first, pi-legion 8.14.0 declaring it without the report, so a daemon at
// 20 would pass the gate against it. Renumbered twice, as
// docs/solutions/legion/daemon-api-contract-collision-renumber-when-the-release-declaring-the-number-lacks-your-shapes.md
// says.)
//
// 22: LEGION-618 -- the daemon logs in to the secrets broker with its own Kubernetes ServiceAccount
// token, which no person approves, so GET /legion/v1/state's `agentSecretsLogin` loses `code` (no
// confirmation code exists any more) and its `state` is `none`, `issued` or `failed`. A 21 plugin's
// strict reader requires `code` and refuses a 22 daemon's state, so the boot gate and `legion
// probe-image` refuse the mixed pair.
//
// 23: LEGION-670 -- api.State gains `strandedClaims`, every claim nothing will wake
// (workflow.Stranded): a suspended or failed worker of its issue's phase, or tree architect, in a
// tree that runs, each named by its issue, role and state. Never null, so a plugin built before it
// refuses the state, and the bump is why the two never meet. (This branch first took 19, then 20,
// then 21, then 22; LEGION-631 landed at 19, LEGION-668 at 20, LEGION-663 at 21 and LEGION-618 at
// 22 first, so a daemon at any of them would pass the gate against a plugin whose strict reader
// lacks the field. Renumbered four times, as the collision note says.)
//
// 24: LEGION-164 -- pluginContract on the claim views (api.ClaimView, api.OperatorClaim) and the
// controller locator (api.ControllerLocator), always emitted, 0 meaning no contract recorded; the
// stale-caller refusals (409 naming both contracts). A 23 plugin's strict reader refuses a state
// whose views carry the field, so the boot gate and legion probe-image refuse the mixed pair.
//
// 25: LEGION-636 -- `source` on a deployment row of `api.State.capabilities`, "configuration"
// (what legion.yaml alone decides) or "boot" (what boot measured), always emitted there; absent on
// every other row. `installed` leaves the status set: no row has awaited a pod launch since
// LEGION-629 turned extension discovery on in the pod's launch, so no daemon has emitted it since,
// and the field that rendered it (capabilities.Capability.Awaits) is gone. The probe's OK line
// gains a third `model-fallback` mark, `unread`, when the probe could not read `retry.modelFallback`.
// A 24 plugin's strict reader refuses a state whose deployment rows carry `source`, so the boot
// gate and `legion probe-image` refuse the mixed pair.
//
// 26: reserved for sjawhar/legion#1999 (LEGION-694) by 27 below while that branch was open; no
// release declares it, since that branch landed at 28 once releases had declared 27 without its
// field. A gap, left so the history reads as it happened.
//
// 27: sjawhar/legion#1986 -- a pod's `workspace-init provision` command names the repository's
// default branch, `--default-branch <name>`, which it requires: an image whose `legion` was built
// before 27 exits 2 on the unknown flag in every new issue pod's init container, so the daemon's
// image probe, which holds the image's pi-legion to this number, refuses such an image at boot
// instead. (26 was sjawhar/legion#1999's, open when this was taken; a release declaring 26 would
// carry no `--default-branch`, so this branch took the number above it, as the collision note says.)
//
// 28: LEGION-694 (sjawhar/legion#1999) -- api.State gains `heldOutboxRows`, every due lifecycle
// outbox row held behind its issue's running lifecycle row (record.Store.HeldRows), each with its
// id, kind, issue, how long it has waited and the row it waits on. Never null, so a plugin built
// before it refuses the state, and the bump is why the two never meet. (This branch took 25, then
// 26 when LEGION-636 landed at 25, and 27 above reserved 26 for it; but pi-legion v10.3.5 and
// v10.3.6 were released from main declaring 27 without `heldOutboxRows`, so a daemon of this branch
// at 27 would pass the boot gate against a plugin whose strict reader then refuses the state. The
// release that first declares a number must carry your shapes: it does not, so this branch takes
// the number above it, as the collision note says.)
//
// 29: sjawhar/legion#1998 -- under runtime.kubernetes.agent_secrets the daemon-launched
// controller's pod carries AGENT_SECRETS_URL and AGENT_SECRETS_KEY_DIR and its shim the three
// agent-secrets flags, as every workflow role's does, so its `slack_post` is granted the Legion
// daemon service's LEGION_SLACK_BOT_TOKEN; and a project's `slack_channels` (which only
// `controller: daemon` with the broker may set) reach that controller as LEGION_SLACK_CHANNELS,
// always set (empty for none), which the plugin requires there and subscribes to while the session
// holds the controller role and which the controller's generation records with its addresses, so a
// daemon restarted with another list relaunches it, and as the addressing's `Slack channels:` line,
// which cites the controller skill's Slack reference. A plugin from before 29 subscribes to no
// channel and ships neither `slack_post` nor the reference, so the boot gate and
// `legion probe-image` refuse the mixed pair. The controller secret's answer is unchanged.
//
// 30: sjawhar/legion#1986 (per-owner credentials) -- every pane and pod holds a gh bundle, one
// directory per GitHub owner of the project under LEGION_GH_OWNERS_DIR, GH_CONFIG_DIR its issue's
// owner's (write) and every other owner's read-only, with LEGION_GH_PATH the real gh the `gh`
// wrapper first on PATH (`legion gh`; `/opt/legion/bin/gh` in the image) runs; every shared
// clone's credential helper is `legion git-credential` with `credential.useHttpPath` on. A role
// Secret's gh keys are per owner (`github-hosts-<owner>`, `github-config-<owner>`,
// `github-default`), and `POST /legion/v1/controller/github-credential` takes an optional
// `default` and answers `owners` (each owner's `hosts` and `config`), `default`, `app` and
// `expiresAt` in place of one `hosts` and `config`. An image built before 30 has neither the
// wrapper nor the two commands, so the image probe refuses it. (This branch took 28, then 29 when
// sjawhar/legion#1999 landed at 28; main then landed sjawhar/legion#2003 at 29 and released
// pi-legion v10.5.0 declaring it without these shapes, so this branch takes the number above it,
// as the collision note says.)
//
// 31: sjawhar/legion#1986 (the issue's repository) -- an issue's record carries the repository it
// targets and that repository's default branch, taken from Dispatch's resolved_repo and GitHub
// when the daemon first records it: `GET /legion/v1/state` and `read_record` answer `repo` and
// `defaultBranch` on every issue and `repo` on its pull request, and the state answers
// `repositories`, the project's mapping as the daemon serves it (`owners`, `mapped` with each
// repository's `default`, and `refused`, each mapped repository left out with its `reason`). A
// plugin at 30 reads the state with a strict schema that knows none of these fields, so it
// refuses the state.
//
// 32: sjawhar/legion#1986 (the repository cache) -- the daemon runs one repository cache pod per
// served repository, whose one container is the image's `legion workspace-init cache`, and every
// issue pod's `workspace-init fetch` is told its repository's cache (`LEGION_REPOSITORY_CACHE_URL`)
// and that cache's bearer token (`LEGION_REPOSITORY_CACHE_TOKEN_FILE`), and fills a fresh feed
// from that cache alone, verified, fetching only the delta from GitHub; its `workspace-init
// provision` makes the issue's clone shared with the feed, which now lives on the issue's own
// volume. An image whose `legion` was built before 32 exits 2 on the unknown subcommand in every
// cache pod, and its fetch clones from GitHub, ignoring the cache, so the daemon's image probe,
// which holds the image's pi-legion to this number, refuses such an image at boot instead. The
// state's `repositories` gains `cache` on each mapped repository under the Kubernetes runtime, with
// its `state`, and `caches`, the caches of repositories the mapping no longer names, each with
// `retainedUntil`, which a plugin at 31 reads with a strict schema that knows neither.
const DaemonAPIVersion = 32
