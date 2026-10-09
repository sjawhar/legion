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

- `legion.daemonApiVersion` is 18 (LEGION-631). Each Legion role's GitHub App token is now a file
  the pane's plain `gh` and `git` read: the daemon sets `GH_CONFIG_DIR` (gh's `hosts.yml` and
  `config.yml`, rendered and refreshed by the daemon) and empties `GH_TOKEN`, `GITHUB_TOKEN` and
  `GH_HOST`; the daemon's credential routes, the `gh` shim, the `legion gh` and `legion credential`
  commands and the absolute-path pins of a pane's gh, git and jj are gone. No agent runs `legion`
  from bash any more, so the extension registers no `tool_call` hook: it mints no grant before any
  shell command, writes no grant file (`LEGION_GRANT_FILE` is gone from the pane's environment) and
  refuses nothing — the jj operation-log refusal (`jj undo`, `jj abandon`, `jj op restore` and the
  rest, LEGION-45) is deleted with it, and the `dispatch` here-document scan of the LEGION-588
  entry below now has one reader, `@sjawhar/pi-envoy`'s run-end check. The `legion` tool's
  operations mint their grants in-process and post them with the request: `handoff_complete` finds
  the commit it reports in the pane with its jj (`src/handoff-commit.ts`, what `legion handoff
  complete` did — for a file-backed phase the pushed commit carrying
  `.legion/<issue>/<phase>.json`, refused while the file is uncommitted, missing, not on this
  branch, another pane's or not yet on `legion/<issue>@origin`; otherwise `@-`) and posts a direct
  `POST /legion/v1/handoff/complete` (`summary`, `verdict`, `ready`, `commit`), whose daemon reads
  no handoff file and no branch head, refuses `HANDOFF_NOT_NEW` and, for READY, runs the head's
  checks (`READY_HEAD_CARRIES_HANDOFFS`, `READY_CHECKS_NOT_GREEN`), answering `note` when READY
  was published without reading them, so a worker pushes before it completes; the `handoff_write`
  and `handoff_read` operations are gone with the `legion handoff` and `legion push` commands they
  shelled out beside (a handoff is written with `write`, committed and pushed with plain `jj`,
  read with `read`), so the shell
  `legion handoff complete` the LEGION-634 entry below describes no longer exists; the tool
  resolves no review thread (`legion threads resolve` is gone, and the daemon serves no thread
  route: the reviewer names the bot threads it accepted to the implementer, who resolves them with
  plain `gh` as the pull request's author); and the controller session gets the tool with
  `read_state` (the whole `GET /legion/v1/state`) and `set_status` (`POST /legion/v1/issues/status`
  under a controller grant minted with its registration secret), in place of running
  `legion state` and `legion status` from bash. The client's strict parse needs this release beside
  a daemon at 18; the daemon's boot gate and `legion probe-image` refuse any earlier contract (the
  8.6.0 release declares 17 and still mints a grant before every command; 8.4.1 declares 16).
- The `legion-worker` skill says what a shell `legion handoff complete` and a `task` subagent's bash
  can do since LEGION-630 removed the role gate, and how the phase-stall reminder then behaves
  (LEGION-634): a shell completion, should one run, completes the phase at the daemon and is not
  refused, but only the `legion` tool's `handoff_complete` closes the extension's phase stall, so
  the reminder recurs at each settle after an Envoy delivery until a tool `handoff_complete`
  succeeds, and a completion made on it is refused (`HANDOFF_NOT_CURRENT_PHASE` or
  `HANDOFF_ALREADY_RECORDED`); a subagent has every host tool but the `legion` tool and mints no
  grant, so a `legion` command from its bash is the parent's — today on the parent's last per-call
  grant, within its 60 seconds, which LEGION-631 replaces with the role's mounted token file.
  `legion.daemonApiVersion` is unchanged.
- A `dispatch` command's quoted here-document is data on its stdin: the operation-log pane rule
  reads only the command's head line, so a message body that names `jj abandon` is not refused,
  and the command mints no grant, since the CLI authenticates with the pane's Dispatch token. The
  head is found by one scan held to bash: a here-document ends only where bash ends it, at a line
  that is exactly its delimiter, with leading tabs stripped under `<<-` alone and no line's
  trailing blanks dropped; the scan splits words on space and tab only, never a no-break space,
  byte-order mark or ideographic space, which bash reads as part of a word; and a head carrying
  `$()`, backticks, a redirect, a comment or a second command is no `dispatch` head, so the whole
  command is held to the pane rule. A test holds the scan to a real bash over 2,822 generated
  commands.
- `legion.daemonApiVersion` is 17 (LEGION-588). Contract 17 moves the daemon's role prompts from
  native Dispatch tools to the `dispatch` command bundled by `@sjawhar/pi-envoy`; pair this
  release with a Go `legion` built from the same commit, so an agent is never instructed to use a
  surface its installed plugin does not provide. The daemon's image probe refuses a worker image
  whose plugin declares 16 (the 8.4.0 release, whose `@sjawhar/pi-envoy` registers the native
  tools and bundles no `dispatch`).
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
  release declares 18 (LEGION-631, above; 17 since LEGION-588, 16 since LEGION-578, 15 since
  LEGION-462, 14 since LEGION-592). The Envoy messaging and Dispatch command every session uses are
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
