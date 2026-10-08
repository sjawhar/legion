# Pi Legion Extension

Tracked Oh My Pi extension package `@sjawhar/pi-legion`: the Legion entry (`extensions/legion.ts`),
the Legion lifecycle modules (`src/`), the task agents Legion's prompts dispatch (`agents/`), and
the eight Legion skills (`legion-architect`, `legion-controller`, `legion-oracle`, `legion-retro`,
`legion-worker`, `ce-simplify-code`, `thermonuclear-code-quality`, `thermonuclear-deep-review`),
staged into `dist/skills` by `scripts/pi-plugin-prepack.sh` at the repository root. It is installed
beside `@sjawhar/pi-envoy` (`packages/pi-envoy`), which every session loads for Envoy messaging and
the Dispatch tools; the Legion daemon installs both into its Oh My Pi profile and a worker image
carries both. `extensions/legion.ts` is inert without `LEGION_TREE`/`LEGION_ROLE`/`LEGION_CONTROLLER`
in the environment: a person's own session gets no tool, no title and no daemon call from it.

## What this plugin needs from pi-envoy

The Legion entry claims roles, matches injected user turns and reads the bootstrapped session
through the interface the Envoy entry publishes: one process-wide object on `globalThis` under
`Symbol.for("legion.pi-shared.envoy-plugin-interface")`, at `ENVOY_PLUGIN_INTERFACE_VERSION`
(currently 1), described in `packages/pi-shared/AGENTS.md`. At factory time `extensions/legion.ts`
sets its own load marker, `Symbol.for("legion.pi-legion.loaded")`, to
`{ from: import.meta.url, envoyInterface: ENVOY_PLUGIN_INTERFACE_VERSION }`, which the daemon's load
probe reads.

`envoyPluginRefusal()` in `extensions/legion.ts` says why this entry cannot run in this process, in
this order, each sentence carrying its remedy (this section names the pre-split package because the
remedy does; `.github/scripts/check-plugin-names.sh` allowlists it for that reason):

- the pre-split package is loaded (`Symbol.for("legion.pi-envoy.legion-loaded")`, the
  `LEGACY_LEGION_LOADED_KEY` of `@legion/pi-shared/interface`, is set): `Legion plugin at <this
  entry's URL> found the pre-split @sjawhar/pi-legion-envoy loaded from <its URL>; uninstall
  @sjawhar/pi-legion-envoy (omp plugin uninstall @sjawhar/pi-legion-envoy) and keep
  @sjawhar/pi-envoy beside @sjawhar/pi-legion`;
- no Envoy entry has published (`readEnvoyPluginInterface()` answers `absent`): `Legion plugin at
  <URL> needs @sjawhar/pi-envoy at interface version <N>, found none; install @sjawhar/pi-envoy
  beside @sjawhar/pi-legion`;
- the publisher speaks another version (`mismatch`): `Legion plugin at <URL> needs
  @sjawhar/pi-envoy at interface version <expected>, found version <found> from <the publisher's
  URL>; install the @sjawhar/pi-envoy released with this @sjawhar/pi-legion`.

`session_start` runs it after the subagent check, the session title and the phase-stall restore, for
every Legion session kind (`root`, `worker`, `controller`), and on a refusal writes the sentence with
`logger.error` and exits the process (`exitProcess(1)`, the path a refused boot registration takes),
which the daemon counts as a launch failure. A `not-legion` session has returned before the check
and is never touched. `/legion-claim-controller`, registered at factory time and usable from a
hand-started session, runs the same check first and on a refusal answers the command with that
sentence (the handler throws it) instead of claiming; it never exits. `recordBootstrappedSession` is
called only after the check passed, so it writes into an interface that exists.

The daemon's boot gate (`packages/daemon/internal/daemon/bootgate.go`) refuses the same three
before any pane starts, in every lane — the daemon's pane gate, `legion probe-image` for a worker
image, and `legion controller start` — each in its own words: its load probe (`probe.mjs`) prints
the Legion marker, the interface version the Legion entry speaks, the version and origin of the
Envoy entry that published, and the pre-split package's marker when one is set, and `probeLoad`
judges not loaded, then the old package loaded, then no Envoy entry, then a version mismatch,
before it resolves the prompts' agents and skills. The pane lane's remedy for the old package is
`<OMP_PROFILE=… >omp plugin uninstall @sjawhar/pi-legion-envoy`; the pod lane's, for each of the three, is to build the
worker image from the daemon's commit.

## Daemon contract

The plugin speaks to the Legion daemon (`packages/daemon`) through
`src/daemon-client.ts`, which reads every response through the strict schemas of
`@legion/contracts/legion-api`, and boots every Legion session through the claim session
(`src/claim-session.ts`) or, for the controller, the controller session
(`src/controller-session.ts`). `package.json` declares the contract it was built against as
`legion.daemonApiVersion`: the claim, credential, workflow, controller, and state
shapes that client parses, and the pane's environment — the identity variables
`LEGION_TREE`/`LEGION_ISSUE`/`LEGION_ROLE`/`LEGION_CONTROLLER`/`LEGION_PROJECT` (read by
`src/classify.ts`, `extensions/legion.ts` and the two session modules), `LEGION_STATE_DIR`
(where the claim session writes its jj attribution overlay), `LEGION_WORKSPACE` (where the handoff
actions run, `src/handoff-actions.ts`), `LEGION_GENERATION` (set by the daemon, read by
nothing here), `LEGION_BOOT_TOKEN_FILE`, `LEGION_GRANT_FILE`, `LEGION_DAEMON_URL`, the Envoy
variables (`ENVOY_URL`, `ENVOY_NATS_URL`, `ENVOY_TOKEN_FILE`, read by `@legion/envoy-client`),
`NATS_NKEY_SEED_FILE` when the daemon has a NATS nkey seed, and `DISPATCH_URL`/`DISPATCH_TOKEN_FILE`
when the daemon has `dispatch_url` configured — and, beside the pane, `LEGION_REMOVABLE_WORKSPACES`
on a pod's `workspace-init provision` container, which the image's own `legion`, built from the same
commit as this plugin, decodes strictly. A change to any of these surfaces bumps the field and the
daemon's `DaemonAPIVersion` (`internal/api/version.go`, whose doc comment is the contract's
history) in the same commit: `packages/contracts/fixtures/daemon-api/version.json`, written by the
daemon's golden test, is what `src/daemon-api-version.test.ts` pins the field to, so neither
side bumps alone, and the number is re-read against `main` at every rebase. Contract 12 renamed the
field from `legion.goDaemonApiVersion` when the plugin dropped its TypeScript-daemon client
(LEGION-223): a release before it declares the TypeScript daemon's 9 under this name and is
refused naming that number. The split of the one plugin into this package and `@sjawhar/pi-envoy`
(LEGION-247) moved no request, response or pane variable, so it bumped nothing of its own: the
number is 15 for contract 15's `dispatch` command instructions in every daemon role prompt
(LEGION-588), after contract 14's daemon-launched controller pod (LEGION-592) and contract 13's
`push` grant and `LEGION_REMOVABLE_WORKSPACES` payload (LEGION-583); the Envoy plugin's manifest
carries no `legion` key, and the gate reads only this package's.

The daemon's boot gate (`internal/daemon/bootgate.go`) refuses to start unless the installed
manifest's field equals its `DaemonAPIVersion` — the manifest at the plugin root Oh My Pi resolves
under the environment a pane will get, and the plugin a pane's Oh My Pi actually loads, which must
be that same package — and `legion probe-image` holds a worker image's plugin to it the same way.
Notes on earlier contracts that still describe behaviour:
Contract 4 adds the operator-launched controller: `POST /legion/v1/controller/secret` (the CLI's
call, never this extension's), a controller registration on `claims/register` answered with the
role `controller` and no tree or issue, the `/grants` controller-session form
(`{sessionId, secret}`), and `controllerLocator` (`{runtime, external: true, sessionId,
registeredAt}`) on `/legion/v1/state`.
Contract 5 adds `LEGION_GRANT_FILE` to the pane's environment, tmux pane and Sandbox pod alike
(LEGION-262): Oh My Pi copies its environment once for every `gh` it runs to serve a `pr://` or
`issue://` read or its `github` tool, so the pointer has to be there from its start, and this
extension does not set it after the claim registers. On a pane launched without it, the plugin
refuses every bash command and every call Oh My Pi serves with `gh`, answering
`LEGION_GRANT_FILE is not set on this pane: …`. Restarting the daemon does not clear it, since
a restarted daemon re-adopts a live pane without relaunching it; relaunching the pane does
(`legion claims suspend` and then `legion claims resume` on its claim).
Contract 6 adds `phase` to a claim's pending delivery on `/legion/v1/state` — the issue phase the
task was queued for, absent for a task of no phase — and `unrecorded` as the `phase` and `status`
the state route reads for an issue the workflow does not record, where an operator's claim exists
and an issue does not (#1345). No request names `unrecorded`: the phase-backward request takes the
workflow's phases alone. The handoff completion request is unchanged: a completion names no run,
and the daemon attributes it to the run of the task the worker took, reading the claim in three
steps — the task whose turn is running; or, once that turn ends and retires it, the run the claim
is left serving, which is what answers for a worker woken by a notice; or, for a claim that has
served no run at all, a task it holds that the agent may have read. A task the agent refused is
not one it read, so it answers for none of them. `POST /legion/v1/handoff/complete` answers 409
`HANDOFF_NO_RUN` to a claim that has taken no task at all, and the workflow refuses
`HANDOFF_STALE_GENERATION` for a run the issue has left. The pane's `LEGION_GENERATION` is the
claim's launch counter and says nothing about the run; nothing reads it for this.
Contract 7 adds `holdReason` to an issue on `/legion/v1/state`: `escalated` while an issue its
architect escalated stays held in a tree that runs, absent otherwise (a tree that lingers or is
closed shows none until it is re-admitted). The controller skill reads it at every start,
since the escalation's wake reaches only a controller running when it is published (#1420).
Contract 8 adds `NATS_NKEY_SEED_FILE` to the pane's environment (LEGION-279): when the daemon
has the `legion-pane` NATS nkey seed (`nats_nkey_seed_file`, else `NATS_NKEY_SEED_FILE`, else
`NATS_NKEY_SEED` in its own environment), every root and worker pane's pointer names a 0600
`<role token>-nats_nkey_seed` file under the daemon's `<state_dir>/secrets`, pruned with the pane's
other secret files, and every Sandbox pod's names the providers Secret's own `NATS_NKEY_SEED` file;
`legion controller start` sets it to the operator file's `nats_nkey_seed_file`. The
extension's Envoy connections read the seed from it (`@legion/envoy-client`'s `nats-auth.ts`), so
they authenticate as that nkey user once production NATS stops admitting credential-less clients.
With no seed there is no pointer, and the connections carry no credential.
Contract 10 adds `POST /legion/v1/roots/close` (`LegionRootCloseRequest`: `grantId`, `issue`,
`reason`), the `legion` tool's `close_root`: a root architect ends its tree while the root is
admitted and no phase has started, and the daemon posts the reason on the issue before it writes
`done`.

The claim session's boot (`createClaimSession`) is the same for a root architect and a phase
worker — the daemon registers both on one route, a root being the claim whose issue is its tree:
the persisted transcript; `claims/register` with the pane's boot token and this build's
`daemonApiVersion` (`pluginContract`), where any 4xx exits the process with one log line naming
the route, status, and daemon sentence (`exitOnRegistrationRefusal`) and a 5xx or transport
failure propagates without exiting; jj session attribution; the Envoy role, which is the claim
token and the topic the daemon sends an architect every notice on (no claim subscribes to an
issue's notice topic, so no phase worker is woken by an architect's notice); and `claims/ready`,
retried three times a second apart on a 5xx or transport failure only, and run again whenever the
Envoy heartbeat regains the role. A session with no Legion environment boots nothing and gets no
tool. The tool-call hook mints a fresh grant into the pane's `LEGION_GRANT_FILE` before every call
that redeems one (see the grant file row below). It also registers the `legion` tool: architects
register gates, release children, request a backward move, choose retry or escalation, sign off,
close an admitted root tree (a root architect only), and read records; phase workers request a
backward move and read records. No `claims/exit` report runs at shutdown, because a
daemon-requested suspend ends the session but keeps its claim for resumption.

A daemon that sets `controller: daemon` launches the controller itself, as a pod with
`LEGION_CONTROLLER=1` and `LEGION_BOOT_TOKEN_FILE` (`controllerSession` in
`src/controller-session.ts`): the session registers on `claims/register` with that boot
token in place of a capability, is answered with the same controller registration, claims the role,
subscribes to the controller topic, and then calls `claims/ready`, which is when the daemon sends
its start message. Any step of that claim that fails exits Oh My Pi, so the daemon relaunches it;
the operator's controller logs and stays up instead. Only a session classified as the controller
(`LEGION_CONTROLLER=1`) registers its boot token that way: `/legion-claim-controller` in a root
architect's or phase worker's pane, whose boot token is its own claim's, is a takeover by hand that
needs the capability and stops before any daemon call without it, never re-registering the
worker's claim or exiting it. Every other daemon launches no controller: the
operator starts one with `legion controller start`, which drops an inherited boot token,
fetches the controller capability with the operator's bearer and runs Oh My Pi with
`LEGION_CONTROLLER=1` and `LEGION_CONTROLLER_SECRET_FILE`. No boot gate checks the operator's
machine, so the plugin's contract is held there three times. Before its one daemon call,
`legion controller start` launches Oh My Pi as the controller will run — its launch prefix and
invocation, the controller's environment, in `<state_dir>/controller` — with the boot gate's load
probe, and refuses when that Oh My Pi loads no pi-legion, loads one whose `daemonApiVersion` is
not its own, loads it without pi-envoy or with a pi-envoy at another interface version, or still
loads the pre-split package beside it (the section above). The manifest it reads is the one Oh My
Pi reports loading, so whatever moves the plugin root (a dotenv file, the launch prefix, a project
plugin root, a symlinked state directory) moves the check with it. That call,
`POST /legion/v1/controller/secret`, names the contract the CLI held the plugin to
(`pluginContract`), and the daemon refuses one that is not its `DaemonAPIVersion` with 409, naming
both, before it mints anything. The mint revokes the incumbent controller, so neither a plugin that
would refuse the new session nor a CLI built for another daemon (a binary replaced before its
daemon restarted) cuts the running controller off. And the daemon refuses a controller registration
whose `pluginContract` is not its `DaemonAPIVersion` with 409, naming both. That session goes
through the controller session (`src/controller-session.ts`), not the claim session, and gets no
`legion` tool: `GET /legion/v1/state` first, since a registration replaces the running controller:
an unset `LEGION_PROJECT`, or one whose controller role is not that of the project the state names
(`legionProjectToken` in `@legion/contracts`, the daemon's own rule), stops the claim there; then
`claims/register` with the capability in place of a boot token, answered with
`api.ControllerRegisterResponse` (`LegionControllerRegisterResponse`), then the Envoy role
`legion-<project>-controller`, then a subscription to the project's controller topic
`notifications.legion.<project>.controller` (`legionControllerNoticeSubject`, the project from
`LEGION_PROJECT`), then a controller grant per credentialed tool call from the `/grants`
controller-session form with the secret the registration was issued. What the daemon publishes
on that topic is listed at `notify.ControllerTopic` (`packages/daemon/internal/notify`). The
subscription lasts while the session holds the controller role (`subscribeLegionNotice`'s
`whileHolding`): once another live session holds it, the heartbeat's refused re-assertion closes
it, so a replaced controller stops taking wakes within one heartbeat, and a dropped connection's
retries, each compared with the role's state when the connection dropped, do not reopen it once
the role has ended. A `/new` or `/resume` keeps it open whichever extension handles the switch
first: Oh My Pi runs the manifest's order but moves on from a handler that outlasts its 30-second
budget, and the switch's drop of the outgoing role (`endOutgoingRole`) leaves a role already
claimed under the new session id alone. It is never registered with
the listener, so a replaced controller resumed later gets it back only by claiming the role, which
the daemon refuses its replaced capability. It is a live wake that changes no
request, response or pane variable, and a daemon publishes to the topic whether anyone listens. A
controller on an earlier plugin release never runs against this daemon: contract 7 ships with the
subscription, `legion controller start` refuses to launch an Oh My Pi whose plugin speaks another
contract, and the daemon refuses its registration with 409. A later
`legion controller start` mints a new capability, so the earlier session's grants stop working.
`legion status <issue> <status>` in that session reads the grant file; from an operator shell it
takes `--operator-token-file`, which buys a controller grant over the operator's bearer and, like
`legion claims`, is refused when its group or others can read it.

## Session titles

Every Legion session names itself as soon as it starts, in a tmux pane or a
pod: `Legion <role> · <ISSUE>` for a root architect or a phase worker (a sub-architect included),
from `LEGION_ROLE` and `LEGION_ISSUE`, and `Legion controller · <PROJECT>` for a controller, from
`LEGION_PROJECT` displayed in uppercase (the daemon carries that token in lowercase for subjects
and paths; without it the title is `Legion controller`, so no daemon contract number moves for it).
`session_start` in `extensions/legion.ts` calls `pi.setSessionName`
(`src/session-title.ts`) after the subagent check and before any daemon
call or Envoy role claim, so the claim's registration already carries the title the Envoy listener
lists, and every Dispatch write stamps it as `origin.session_title` (`getSessionName`, read at
call time). Oh My Pi titles a session itself from the first message typed at its terminal or given
on its command line, so a headless `omp --mode rpc` session the daemon prompts otherwise has none.
The controller also titles the session a `/new`, `/resume`, `/fork`, branch or tree navigation
leaves it on, before it re-claims.

Oh My Pi persists the title in the transcript with its source, and a pane relaunched with
`--resume` keeps it. A session already carrying the Legion title is left as it is; a title Oh My Pi
generated (`titleSource: "auto"`) is replaced; any other title is a person's (a `/rename`, the RPC
`set_session_name`) and is kept. Oh My Pi records an extension's `setSessionName` as `user` too, so
its own title model never replaces the Legion title. A `task` subagent and a session with no Legion
environment get no title from this extension.

## Phase workers' handoff actions and the phase-stall follow-up

A worker's handoff operations are actions of the `legion` tool, never shell text:
`handoff_write`, `handoff_read`, and `handoff_complete` (`src/handoff-actions.ts`), which
`src/tools.ts` carries for every session but the root architect. Each action runs the
daemon's own `legion handoff ...` command, `legion` found on the pane's PATH (the tmux
`<state_dir>/bin/legion` launcher, or the image's binary in a pod), in `LEGION_WORKSPACE`;
`handoff_complete` first mints a grant into `LEGION_GRANT_FILE`, as the tool-call hook does before a
shell command. `legion gh` and `legion credential` stay shell commands: git and gh call them. What a
later phase needs goes in the handoff; a question for another live role goes to its role topic with
`envoy_publish`.

`handoff_write` sends its payload on the command's stdin, which both CLIs read when `--data` is
omitted: one argv string is capped at 128 KiB (Linux's `MAX_ARG_STRLEN`), and a tester's handoff that
accumulates review rounds outgrows it.

The shell's completion is closed: the tool_call hook refuses `legion handoff complete` (by name or by
a path ending `/legion`) in a phase-worker pane, a sub-architect's included, and a root architect's,
ahead of every role gate so that it binds a `task` subagent too — a `bash` command in any position of
a chain (a supervised service's start included), and `eval` code or stdin written to a supervised
service (a `write` to `proc://<id>`, the path in a pasted `read` header, `[proc://<id>#XXXX]`,
included, since Oh My Pi's `write` strips that before it routes) by a plain-text rule, exactly as
it refuses the jj operation-log rewrites (`PANE_RULES` in `extensions/legion.ts`). A completion run
from the shell would never reach the phase stall below. `legion handoff write` and `read` stay open
to the shell: they leave no phase open, a root architect reads committed handoffs with
`legion handoff read`, and a worker can pipe a handoff built from the one on disk to
`legion handoff write` on stdin (`skills/legion-worker/SKILL.md`, the handoff write section).

In a phase-worker session (planner, implementer, tester, reviewer, merger: never an architect, the
controller, a session with no Legion environment, or a `task` subagent), `src/phase-stall.ts`
tracks the phase: the daemon's assignment (a user message) opens it, the tool's successful
`handoff_complete` closes it. A person's direct message the Envoy extension sent in as the user's
own turn is a user message too, and counts as an Envoy delivery rather than an assignment: the
Envoy extension records each body it sends in, process-wide, on the shared interface
(`injectedUserTurns`, `@legion/pi-shared/injected-user-turns`), and legion.ts asks that record at
`message_start` (`matchInjectedUserTurn`). The record is forgotten at the run's `agent_end`, so a
Send or an Aside sent in after the run's last queue or aside poll and before that `agent_end`,
which the host then runs as a turn of its own, matches nothing: it counts as an assignment, which
opens even a closed phase, and the dashboard shows it twice. One sent in after that `agent_end`
starts a fresh record and is matched. When a run is about to settle (`session_stop`) with the phase still
open, the extension returns one follow-up (`{continue: true, additionalContext}`), which the host sends
as the next turn of the same session: run `handoff_complete`, or reply with a WAITING line. A final
message holding a tool call written as text is told so. One follow-up per stall; a WAITING reply or a
sent follow-up stays quiet until the next Envoy delivery or assignment. The state is appended to the
transcript (`legion-phase-stall` entries) and restored at `session_start`, so a worker relaunched with
`--resume` keeps it. `extensions/legion-phase-stall-omp.test.ts` proves it on the pinned Oh My Pi
(`LEGION_TEST_OMP`), loading this entry beside `../pi-envoy/extensions/envoy.ts`.

## Where to look

| Task | Location | Notes |
| --- | --- | --- |
| OMP extension entry | `extensions/legion.ts` | The one entry; ships as `dist/legion.js` in `@sjawhar/pi-legion`. Inert without `LEGION_TREE`/`LEGION_ROLE`/`LEGION_CONTROLLER` in the environment; in a Legion session it refuses to run without the Envoy plugin's interface (the section above). The Envoy entry is `../pi-envoy/extensions/envoy.ts` (`packages/pi-envoy/AGENTS.md`) |
| Legion lifecycle modules | `src/` | Classification (`classify.ts`), the daemon client (`daemon-client.ts`) and the claim session (`claim-session.ts`; see Daemon contract), grant file (`grant-file.ts`: the `tool_call` hook mints one grant per call that redeems one — every `bash` command, the `github` tool, and any tool whose `path`/`paths` names a `pr://` or `issue://` URL, which Oh My Pi serves by running `gh` (`needsGrant` in `extensions/legion.ts`) — writes it atomically to the pane's `LEGION_GRANT_FILE` as 0600, creating its directory 0700 when absent, and returns `undefined` — it never touches the tool's input; the static gh environment and the `LEGION_GRANT_FILE` pointer are the daemon's pane environment), jj attribution (`jj-attribution.ts`: the `JJ_CONFIG` overlay that adds the `Omp-Session` trailer; the commit identity itself is not the extension's — the daemon puts `JJ_USER`/`JJ_EMAIL` and the Git author/committer variables on the pane, and worker boot writes no jj config), the session title (`session-title.ts`; see Session titles), the `legion` tool (`tools.ts`, `handoff-actions.ts`), the phase stall (`phase-stall.ts`) |
| Controller session | `src/controller-session.ts` | Owns the controller's identity, the transcript a session navigation compares to decide whether to claim again, the claim and reclaim hooks, and grant minting: it reads the daemon's project, registers on `claims/register` with the controller capability, claims the controller role, and mints with the registration's secret (see Daemon contract). The event router writes each returned grant through `grant-file.ts` to `LEGION_GRANT_FILE`. |
| Shared modules and the interface | `../pi-shared/` | `@legion/pi-shared`: the interface this entry reads (`interface`), the role-claim bridge, the injected-user-turn record, the subagent check, the host types and `toolSuccess`/`toolFailure`; inlined into `dist/legion.js` by `bun build`. See `packages/pi-shared/AGENTS.md` |
| Extension unit tests | `extensions/legion.test.ts` | Mocked Pi and NATS surface; every test starts from no `LEGION_*`/`ENVOY_*`/`DISPATCH_*` environment and sets only what it declares, each stubs `fetch` itself, and `afterEach` resets the process-wide interface (`resetEnvoyPluginInterfaceForTests`) so one test's bound Envoy instance or bootstrapped session never reaches the next. Pins the three Envoy-plugin refusals and that `/legion-claim-controller` answers the sentence without exiting |
| Both entries in one process | `extensions/legion-role-claim.test.ts`, `extensions/legion-phase-stall-omp.test.ts` | Load `../pi-envoy/extensions/envoy.ts` by relative path (test-only; the shipped sources never import the sibling): the role claim through the interface with Legion initialised first, and the phase stall on the pinned Oh My Pi (`LEGION_TEST_OMP`) |
| Shipped agents | `agents/`, `src/shipped-agents.test.ts` | The task agents Legion's prompts dispatch (`oracle`; the reviewer's pair `thermonuclear-deep-review` and `thermonuclear-code-quality`; `deep-worker`; the planner's `plan-gap-analyst` and `plan-reviewer`); each declares the name of its file and its model only as role aliases |
| Daemon contract pin | `src/daemon-api-version.test.ts` | Pins `legion.daemonApiVersion` to `packages/contracts/fixtures/daemon-api/version.json`, which the daemon's golden test writes |
| Skills partition and its guard | `src/skills-guard.test.ts`, `scripts/pi-plugin-prepack.sh` (repository root) | The partition this package ships, staged as its prepack stages it, held to the size, name and link rules in `@legion/pi-shared/test/skills-guard`, with the daemon's prompts as linking roots and every `legion-worker` reference linked from somewhere |
| No import of the sibling | `src/no-cross-import.test.ts` | Fails on a shipped source under `extensions/` or `src/` whose relative import resolves into `packages/pi-envoy` |
| Rigs | `scripts/grant-rig/`, `scripts/skill-scenarios/` | The grant rig proves on a real phase worker how each shell command gets its grant (`scripts/grant-rig/README.md`); the skill-scenario rig replays skill scenarios on real agents (`../pi-envoy/scripts/README.md`, its last section) |
| Shared HTTP/tool behavior | `../envoy-client/src/` | Do not duplicate it here |

## Critical conventions

- Register every schema through the injected `pi.zod` (`toolSchema` in `src/tools.ts`). Every field counts, not just the outer object: OMP's converter reads internals (`.ir`) only its own Zod produces, and a field from another Zod instance fails the whole extension load.
- Nothing under `extensions/` or `src/` imports `packages/pi-envoy`: what both plugins need lives in `@legion/pi-shared`, and this entry reaches the Envoy plugin through the interface alone (`src/no-cross-import.test.ts`). The tests may load the sibling's entry by path.
- A `task` subagent's session shares its parent's identity (`subagentSessionCheck` in `@legion/pi-shared/subagent-session`): in a Legion process it claims no role, calls no daemon route, installs no tool gate, and never exits (`extensions/legion.ts`). The check asks the host's own roster first (`AgentRegistry.global()` from `@oh-my-pi/pi-coding-agent`) and falls back to the transcript; the process-local signal it reads is the transcript path the claim session's boot and a launched controller's claim record on the shared interface (`bootstrappedSession`), so a later `session_start` in the same process with a different transcript path is a subagent even under `OMP_SESSION_STORAGE=sql`. The full account is the subagent convention in `packages/pi-envoy/AGENTS.md`.
- `claim-session.ts` registers the heartbeat's regain listener on the shared interface's `roleClaim.regained` slot (`@legion/pi-shared/interface`) only once it holds a Legion identity, since a `task` subagent's re-bound instance shares the process and would otherwise replace it; the regain re-runs `claims/ready` with bounded retries, and the controller re-runs nothing.
- A daemon refusal of the boot registration itself — `/legion/v1/claims/register` — ends the process (`exitOnRegistrationRefusal` in `src/claim-session.ts`: one log line naming the route, the status, and the daemon's sentence, then `exitProcess(1)`) for every 4xx: a 400 or 404 (a request or a route the daemon does not have), a 403 (the boot token is stale, consumed, or unknown), and a 409 (the same-agent rule, `Worker respawn must resume the same agent session`: this session is not the one the resumed claim recorded; under a database session store that is Oh My Pi having started a fresh session at a path whose row is gone). None changes on retry, and a process that stayed up unregistered would sit alive under the daemon's registration deadline with nothing ever retiring it; exiting hands the outcome to the daemon, which counts the launch failure. Every other error there — a 5xx, a transport failure — propagates out of `session_start` without exiting (LEGION-81; `extensions/legion.test.ts` pins 400, 403, 404 and 409, and 500/503 as the negative control). The Envoy-plugin refusals above take the same exit.
- Do not alter `~/.omp` from this package. The README documents the development install.

## Checks

`bunx tsc --noEmit`, `bun test`, `bunx biome check extensions/ src/`, each from this directory;
`LEGION_TEST_OMP=<omp> bun test extensions/legion-phase-stall-omp.test.ts` for the real binary.
