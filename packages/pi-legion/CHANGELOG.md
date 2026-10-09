# Changelog

`@sjawhar/pi-legion` was split from `@sjawhar/pi-legion-envoy` 7.x (LEGION-247). Every release
before the split, and the Envoy plugin's releases after it, are in `packages/pi-envoy/CHANGELOG.md`.

## [Unreleased]

### Added

- A controller the Go daemon launches itself (`controller: daemon` in `legion.yaml`, LEGION-592)
  runs as a controller session: a session with `LEGION_CONTROLLER=1` and `LEGION_BOOT_TOKEN_FILE`
  registers on `/legion/v1/claims/register` with that boot token in place of a controller
  capability, claims `legion-<project>-controller`, subscribes to the controller topic, and then
  calls `/legion/v1/claims/ready`, when the daemon sends its start message. A claim step that fails
  exits Oh My Pi, so the daemon relaunches it. The operator-launched controller is unchanged.

### Changed

- `legion.daemonApiVersion` is 17 (LEGION-631). Each Legion role's GitHub App token is now a file
  the pane's plain `gh` and `git` read: the daemon sets `GH_CONFIG_DIR` (gh's `hosts.yml` and
  `config.yml`, rendered and refreshed by the daemon) and empties `GH_TOKEN`, `GITHUB_TOKEN` and
  `GH_HOST`; the daemon's credential routes, the `gh` shim, the `legion gh` and `legion credential`
  commands and the absolute-path pins of a pane's gh, git and jj are gone. No agent runs `legion`
  from bash any more, so the extension registers no `tool_call` hook: it mints no grant before any
  shell command, writes no grant file (`LEGION_GRANT_FILE` is gone from the pane's environment) and
  refuses nothing — the jj operation-log refusal (`jj undo`, `jj abandon`, `jj op restore` and the
  rest, LEGION-45) is deleted with it. The `legion` tool's operations mint their grants in-process
  and post them with the request: `handoff_complete` is a direct `POST /legion/v1/handoff/complete`
  (`summary`, `verdict`, `ready`; no `commit`) whose daemon reads the issue branch's head on GitHub
  and checks the committed handoff file and, for READY, the head's checks there, answering `note`
  when READY was published without reading them, so a worker pushes before it completes; the
  `handoff_write` and `handoff_read` operations are gone with the `legion handoff` and
  `legion push` commands they shelled out beside (a handoff is written with `write`, committed and
  pushed with plain `jj`, read with `read`); the tool resolves no review thread (`legion threads
  resolve` is gone, and the daemon serves no thread route: the reviewer names the bot threads it
  accepted to the implementer, who resolves them with plain `gh` as the pull request's author);
  and the controller session gets the tool with `read_state` (the whole `GET /legion/v1/state`)
  and `set_status` (`POST /legion/v1/issues/status` under a controller grant minted with its
  registration secret), in place of running `legion state` and `legion status` from bash. The
  client's strict parse needs this release beside a daemon at 17; the daemon's boot gate and
  `legion probe-image` refuse any earlier contract (the 8.4.1 release declares 16 and still mints
  a grant before every command).
- No Legion handoff reaches the default branch (LEGION-605). The `legion-retro` skill ends retro
  with one final commit that removes the issue's `.legion/<issue>/`, pushed with its
  `docs/solutions/` commit in one push; the `legion-worker` and `legion-architect` skills and the
  merge-gate reference say the merger accepts that commit above the approved head and that READY
  refuses a head still carrying it, in place of the rule that every head kept `.legion/` and the
  daemon stripped main's from the next branch. Install this release with a Go `legion` built from
  the same commit, whose READY makes that refusal. `legion.daemonApiVersion` is unchanged.
- `legion.daemonApiVersion` is 16 (LEGION-578). Contract 16 adds `capabilities` to
  `GET /legion/v1/state`: the deployment's capability report, one row per capability with its
  `status` (`present`, `installed`, `unchecked`, `live`, `withheld`, `decided` or `open`), its
  `detail`, and on an open row the `configLine` to write into `legion.yaml`. Install this release
  with a Go `legion` built from the same commit; the daemon's image probe refuses a worker image
  whose plugin declares 15 (the 8.3.0 release, whose strict state reader has no `capabilities`).
- `legion.daemonApiVersion` is 15. Contract 15 changes a Sandbox locator on the daemon's
  `GET /legion/v1/state`: every role of an issue now runs in one shared Agent Sandbox pod, so the
  `sandbox` member names the issue's Sandbox, the pod's uid, the role container and the process
  generation, and the incarnation is `<pod uid>/<generation>` (LEGION-462). The client's strict
  state parse needs a daemon at this release's contract (the `legion.daemonApiVersion` entry
  above); the daemon's boot gate refuses any earlier contract.
- Every phase worker stays live from its role's first assignment until its issue closes
  (LEGION-462): no move between phases suspends it, so a role that finished its phase still
  answers questions through Envoy, and its next assignment arrives in the same session. The worker
  and architect skills and the headless worker prompt say so, in place of the phase-end suspension
  LEGION-223's entry in `packages/pi-envoy/CHANGELOG.md` describes.
- `legion.daemonApiVersion` is 14 (LEGION-592). Contract 14 adds the daemon-launched controller's
  pod, whose agent's environment carries `LEGION_CONTROLLER=1` beside `LEGION_BOOT_TOKEN_FILE`: this
  release registers it with the launch's boot token, where an earlier one reads it as the
  operator's controller and never registers. Install this release with a Go `legion` built from
  the same commit; the daemon's image probe refuses a worker image whose plugin declares 13.
- **Breaking:** the Legion plugin is its own package, `@sjawhar/pi-legion`, split from
  `@sjawhar/pi-legion-envoy` 7.x (LEGION-247). It carries `extensions/legion.ts` (published as
  `dist/legion.js`), the Legion lifecycle modules (`src/`, the former `src/legion/`), the task agents
  in `agents/`, the eight Legion skills (`legion-architect`, `legion-controller`, `legion-oracle`,
  `legion-retro`, `legion-worker`, `ce-simplify-code`, `thermonuclear-code-quality`,
  `thermonuclear-deep-review`) at `dist/skills`, and `legion.daemonApiVersion`, which the split
  itself did not move (no request, response or pane variable changed): the pre-split package's last
  release left it at 13 (LEGION-583: the `push` grant for `legion push` and the worker image's
  `LEGION_REMOVABLE_WORKSPACES` payload, described in `packages/pi-envoy/CHANGELOG.md`), and this
  release declares 16 (LEGION-578, above; 15 since LEGION-462, 14 since LEGION-592). The Envoy messaging and Dispatch tools every session loads are
  `@sjawhar/pi-envoy`'s, installed beside this package; a Legion pane needs both.
  The Legion entry claims roles, matches injected user turns and reads the bootstrapped session
  through the in-process interface the Envoy entry publishes (`@legion/pi-shared/interface`, version
  1), and in a Legion session refuses to run, naming the remedy, when no `@sjawhar/pi-envoy` is
  loaded, when the loaded one speaks another interface version, or when `@sjawhar/pi-legion-envoy`
  is still installed beside it (`omp plugin uninstall @sjawhar/pi-legion-envoy`). The daemon's boot
  gate, `legion probe-image` and `legion controller start` refuse the same three by name. Links from
  a Legion skill into the `dispatch` skill are `skill://dispatch/...`, which resolve once both plugins
  are installed. Install both into the daemon's Oh My Pi profile:
  `omp plugin install @sjawhar/pi-envoy && omp plugin install @sjawhar/pi-legion`.

### Removed

- The extension's role gate — the architect's bash restriction to a single `legion` command, and
  the architect's, reviewer's and merger's refusal of `edit`, `write` and `apply_patch` — and the
  shell refusal of `legion handoff complete` are removed (LEGION-630). No role is refused a tool:
  the role prompts alone say who edits what, and the operation-log rule (`jj undo`, `jj abandon`,
  `jj op restore|revert|abandon|undo` refused in every tree pane and its `task` subagents) stays.
  `legion.daemonApiVersion` is unchanged.
