# Changelog

`@sjawhar/pi-legion` was split from `@sjawhar/pi-legion-envoy` 7.x (LEGION-247). Every release
before the split, and the Envoy plugin's releases after it, are in `packages/pi-envoy/CHANGELOG.md`.

## [Unreleased]

### Added

- Every root architect's and phase worker's session measures six live capabilities as it boots
  and reports them with `POST /legion/v1/claims/ready` (`capabilities`, contract 21, LEGION-663),
  which the daemon renders in `legion state`: `subagents` (`pi.agents` exposed, `task` registered,
  and `discoverAgents` resolving every agent the registration's `promptAgents` names, none of them
  in `task.disabledAgents`), `web-search` (`web_search` registered and one real `runSearchQuery`
  for `jujutsu version control` answered by a provider), `mcp` (every configured MCP server
  connected — the check connects a second, short-lived client per configured server and
  disconnects it, since the session's own are the host's), `repository-extensions` (every
  `.omp/extensions/` module, `.omp/skills/*/SKILL.md` and `.claude/skills/*/SKILL.md` the
  workspace carries is among what Oh My Pi discovers), `dispatch-envoy-tools` (the ten Envoy tools
  registered and `dispatch read --issue <issue>` answering) and `github` (the role's App login
  through `gh api graphql {viewer{login}}`, since an App installation token may not call REST
  `GET /user`). The six checks run concurrently, each within 8 s and the whole report within 10 s;
  a check that fails, throws or times out is a failing row with the reason, and never stops the
  session (the measurer itself throwing costs the report, not the boot). The ready after a regained
  Envoy role re-sends the same report; the controller's ready carries none.
  `src/capability-report.ts` reaches Oh My Pi's own modules (`@oh-my-pi/pi-coding-agent` and its
  subpaths) through string-literal `import()` at measurement time: they exist only inside Oh My
  Pi's bundle, whose loader resolves them as literals alone, so they load lazily and never under
  `bun test` or in a session that is no Legion session.
- A resumed Legion session whose role launcher sets `LEGION_WORKSPACE_RECREATED=true` (its
  workspace was provisioned after the session was last written, LEGION-654) is told so at session
  start: one `legion-workspace-recreated` message, sent as a steer that starts no turn, is saved
  ahead of the next turn, whatever starts it, naming `legion/<LEGION_ISSUE>` and saying anything not
  pushed is gone. When the next prompt (the daemon's task, or a person's message delivered as a
  user turn) first recovers a failed last turn off the branch, and this process's message with it,
  the prompt's run carries the message again and saves it; the model sees it once, and a notice an
  earlier recreation left in the history does not count as this one. A session that is no Legion
  session, and a `task` subagent, is never told.
- A controller the Go daemon launches itself (`controller: daemon` in `legion.yaml`, LEGION-592)
  runs as a controller session: a session with `LEGION_CONTROLLER=1` and `LEGION_BOOT_TOKEN_FILE`
  registers on `/legion/v1/claims/register` with that boot token in place of a controller
  capability, claims `legion-<project>-controller`, subscribes to the controller topic, and then
  calls `/legion/v1/claims/ready`, when the daemon sends its start message. A claim step that fails
  exits Oh My Pi, so the daemon relaunches it. The operator-launched controller is unchanged.

### Changed

- Daemon API contract 25 (LEGION-636): a deployment row of `legion state`'s `capabilities` carries
  `source`, `configuration` (what legion.yaml alone decides) or `boot` (what boot measured);
  `installed` leaves the status set (nothing has emitted it since LEGION-629); the probe's OK line
  may read `model-fallback=unread` (boot could not read `retry.modelFallback`). Install this
  release with a `legion` built from the same commit.
- The `legion-architect` skill's section 1 and its `design-approved` row say what follows a
  human's answer to a decision block in the approved root spec (LEGION-573): when the answer is the
  whole change since the version they approved, Dispatch carries their approval to the new version,
  the daemon reopens the gate and sends `design-approved`, and that notice opens nothing new — the
  architect requests nothing and releases no wave because of it; when the answer changes the
  Summary, the Acceptance or the tree's scope, the architect rewrites that text, repeats the
  fresh-reader check, the approval request and `register_gate` on the new version with a
  one-sentence `--summary` naming what changed since the earlier approval, and releases no wave on
  the `design-approved` the answer brought. An approved spec gets nothing but the decision block
  and its evidence; an in-block edit before the answer closes the gate like any version without
  stopping the answer carrying the approval.
- A production check whose deploy the implementer cannot perform goes to the agent or role the
  deployment instructions name for that deploy, with the merge commit, and waits on the production
  signal they name (LEGION-447). It opens a `dispatch ask` to a person only when the instructions
  name nobody, and never asks for approval to deploy. `legion-architect`'s sign-off step,
  `legion-worker`'s merge-gate reference, `legion-retro`'s `Production check:` line and the
  daemon's implementer prompt say so in place of a deploy ask to a human.

- `legion.daemonApiVersion` is 24 (LEGION-164): the daemon records each session's `pluginContract`
  and shows it on the claim views (`GET /legion/v1/state`, `legion state --json`) and the
  controller locator; a session whose plugin speaks another contract is relaunched onto the
  current plugin and its calls are refused with `pi-legion speaks daemon API contract <got>; this
  daemon requires <want>: the daemon is relaunching this process onto the current plugin` (or
  `pi-legion registered no daemon API contract; this daemon requires <want>: …` when none was
  recorded), and the operator's controller with `…: run legion controller start from the release
  built with this daemon`. Install this release with a Go `legion` built from the same commit: a
  23 plugin's strict reader refuses a state whose views carry `pluginContract`, so the boot gate
  and `legion probe-image` refuse the mixed pair.

- `register_gate`'s document lookup (LEGION-707) resolves Dispatch as the session's Envoy plugin
  does, from the pane's own envoy.json: a named Oh My Pi profile's
  `$PI_CODING_AGENT_DIR/envoy.json`, else the file `ENVOY_CONFIG_FILE` names, else
  `${XDG_CONFIG_HOME:-~/.config}/legion/envoy.json`, with the `dispatch` section of the workspace's
  `.envoy/envoy.json` over it. `DISPATCH_URL` and `DISPATCH_TOKEN_FILE`, which the daemon sets on
  every pane when Dispatch is configured (`dispatch_url`), still override both.
  `~/.config/opencode/envoy.json` and a workspace's `.opencode/envoy.json` are not read. A tmux
  pane's `XDG_CONFIG_HOME` is relocated under the daemon's state directory, so while the old user
  file is there a default-profile pane's Envoy plugin says it is not read under that
  `XDG_CONFIG_HOME`, and a named-profile pane's names the profile's file to copy or link it to; a
  pod carries no old file.
- `legion.daemonApiVersion` is 23. Contract 23 adds `strandedClaims` to `GET /legion/v1/state`
  (LEGION-670): every claim nothing will wake — a suspended or failed worker of its issue's phase
  that no queued start will wake, or a suspended or failed tree architect, in a tree that holds an
  admission slot — each named by its issue, role and state. The list is never absent, so a plugin
  built before 23 refuses the state; install this release with a Go `legion` and a worker image
  built from the same commit.
- `legion.daemonApiVersion` is 22 (LEGION-618). The daemon logs in to the secrets broker with its
  own Kubernetes ServiceAccount token, which no person approves, so `agentSecretsLogin` on
  `GET /legion/v1/state` has no `code` any more and its `state` is `none`, `issued` or `failed`.
  Install this release with a Go `legion` built from the same commit: this release's strict state
  reader refuses an older daemon's `code`, and an older release's refuses a 22 daemon's state
  without it, so the boot gate and `legion probe-image` refuse the mixed pair.
- The implementer no longer disarms a withdrawn READY (LEGION-683): the Go daemon disables the pull
  request's auto-merge and takes it out of a merge queue itself, so the
  `gh pr merge <n> -R <owner>/<repo> --disable-auto` step is gone from the implementer's prompt and
  from `legion-worker`'s merge-gate reference, which now says no role runs a disarm.
- The `legion` tool's description no longer lists READY's refusals, which had fallen behind the
  daemon's: it points at the merger's role, which names each one, and says an accepted READY whose
  `note` begins `MANUAL_MERGE_REQUIRED:` is one a person merges, so nothing arms it (LEGION-683).
- `legion.daemonApiVersion` is 21 (LEGION-663). Contract 21 adds an optional `capabilities`
  report to `POST /legion/v1/claims/ready` — what a session measured of the six live capability
  rows at boot — and `promptAgents`, the sorted task agents the role's prompts dispatch, to the
  `claims/register` answer; on `GET /legion/v1/state` each claim view carries its session's latest
  report under `capabilities`, and a capability row's `status` is never `live` any more (a live
  row is `present`, `open` or `unchecked` from the sessions' reports). Install this release with a
  Go `legion` built from the same commit: a daemon at 20 or earlier refuses the report as an
  unknown field with 400, which exits the session, and this release's strict state reader is the
  first to accept a claim view carrying one. The daemon's boot gate and `legion probe-image` refuse
  any earlier contract (the 8.11.0 release declares 19, the LEGION-631 entry below; the 8.14.0
  release declares 20, the LEGION-668 entry below).
- `legion.daemonApiVersion` is 19 (LEGION-631). Each Legion role's GitHub App token is now a file
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
  read with `read`; a handoff-only push ends its head's message with two empty lines and
  `skip-checks: true` as the last line, the one shape GitHub honours — `legion push` wrote it so,
  and the prompt rule that replaces it does too), so the shell
  `legion handoff complete` the LEGION-634 entry below describes no longer exists; the tool
  resolves no review thread (`legion threads resolve` is gone, and the daemon serves no thread
  route: the reviewer names the bot threads it accepted to the implementer, who resolves them with
  plain `gh` as the pull request's author); and the controller session gets the tool with
  `read_state` (the whole `GET /legion/v1/state`) and `set_status` (`POST /legion/v1/issues/status`
  under a controller grant minted with its registration secret), in place of running
  `legion state` and `legion status` from bash. The client's strict parse needs this release beside
  a daemon at 19; the daemon's boot gate and `legion probe-image` refuse any earlier contract (the
  8.9.0 release declares 18 and the 8.6.0 release 17, each still minting a grant before every command; 8.4.1 declares 16). The
  merger submits the merge itself the moment READY is accepted, with
  `gh pr merge <n> -R <owner>/<repo> --auto --squash --match-head-commit <head>` (a merge queue
  enqueues the pull request, a repository without one arms auto-merge, and the repository's
  required reviews and checks decide when it lands); the implementer disarms it with
  `gh pr merge <n> -R <owner>/<repo> --disable-auto` at the start of a round that follows a
  withdrawn READY; and no skill or prompt says Legion never merges. The tester's prompt and the
  `legion-worker` skill now say the tester runs the changed surface for real, tries to break it,
  judges whether the implementer's tests would catch what it broke and reads CI for everything
  static; the sentences that had it rerun the lint, type or unit lanes before a push are gone.
- `legion.daemonApiVersion` is 18. Contract 18 makes each issue's Agent Sandbox pod independent,
  on a volume of its own (LEGION-632): the pod's `workspace-init provision` container no longer
  carries `LEGION_REMOVABLE_WORKSPACES` or `LEGION_WORKSPACE_INIT_LOCK_WAIT_SECONDS` (the
  removable-workspaces pass and the repository flock went with the shared tree volume), nor the
  `LEGION_ROLE` and `LEGION_GENERATION` that seeded that pass, and `LEGION_EXPECT_TREE_VOLUME` is now
  `LEGION_EXPECT_ISSUE_VOLUME`; each role's agent is told a state home of its own
  (`XDG_STATE_HOME=/home/legion/.local/state/<role>`), under which the image's shim makes Oh My
  Pi's profile directory, so the browser broker's lock is one name per container. A worker image
  built before 18 would act on a contract the daemon no longer speaks, so the daemon's image probe
  refuses it (the 8.4.x and 8.5.x releases declare 16 and 17, whose `workspace-init` still reads
  the shared disk's variables); install this release with a Go `legion` and a worker image built
  from the same commit.
- The plugin interface it reads from `@sjawhar/pi-envoy` is at version 2 (LEGION-394). In a phase
  worker, a person's `/skill:<name>` sent from Dispatch's Agents page counts as an inbound event,
  as their plain Send does. On a host with `pi.sendUserInput`, so does a Send the host runs as a
  turn of its own after a run's `agent_end`, where it counted as the daemon's assignment; on a host
  without that method this remains unchanged. Install this release together with the
  `@sjawhar/pi-envoy` built from the same commit: this entry refuses a pi-envoy at another interface
  version, naming both, and so does the daemon's boot gate. `legion.daemonApiVersion` is unchanged.
- The `legion-controller` skill no longer tells a controller the daemon launched
  (`controller: daemon`) that nobody types into its session or reads its replies, or that text
  left in its session reaches no one (LEGION-306). A plain user turn in that session other than
  the start message or a task an operator sent with `legion claims deliver` is a person writing
  from Dispatch's Agents page, and the controller answers it first, in the conversation. The
  daemon's `How this controller runs` prompt for that launch says the same.
  `legion.daemonApiVersion` is unchanged.
- `legion.daemonApiVersion` is 20 (LEGION-668): the controller holds the review App's GitHub token
  like every role — a daemon-launched controller pod gets it as a mounted gh directory
  (`GH_CONFIG_DIR`, with `GH_TOKEN`, `GITHUB_TOKEN` and `GH_HOST` emptied, backed by a
  `gh-controller` volume of the role Secret) and `legion controller start` fetches it over `POST
  /legion/v1/controller/github-credential` and refreshes it every minute. The plugin's code does
  not change; the number only gates the pair.
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
  release declares 19 (LEGION-631, above; 18 since LEGION-632, 17 since LEGION-588, 16 since LEGION-578, 15 since
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

### Fixed

- Session attribution's `jj config path --user` gives up after 10 s and kills its process group.
  Root bootstrap awaits it, so a `jj` that never answered (a PATH wrapper that loops) held the
  `session_start` handler past Oh My Pi's 30 s and the claim never reached `ready`; attribution now
  degrades to no trailer, as it does for any other failure to resolve jj.
- A `handoff_complete` whose jj fails on a git lock file still on disk — the `index.lock` a git or
  jj interrupted mid-write leaves, which fails every later jj that snapshots a change into the
  working copy (LEGION-731) — names each such lock in its error, with how long ago it was last
  written. It says not to remove the lock while the issue's pod runs, since another role's git,
  which the role's container cannot see, may hold it, and that replacing the issue's pod removes
  it; and that under the tmux runtime nothing removes it, so it is removed by hand once no git or
  jj on the host is using the repository (`src/git-lock.ts`). The lock is left as it is.
