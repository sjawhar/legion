# Changelog

## [Unreleased]

### Added

- The `dispatch` command (`dist/dispatch.js`) takes `dispatch issues --all`, which reads the whole
  filtered issue listing in one request rather than a page, for a caller that must not miss or
  repeat a row while the list changes under it. `--all` with `--limit` or `--offset` is a usage
  error. The bundled tool descriptions carry the new argument.
- The `dispatch` command (`dist/dispatch.js`) moves an issue on its project's board:
  `dispatch issue-update --issue <KEY> --before <OTHER>` puts it above that issue,
  `--after <OTHER>` below, and both together land it between them, printed as `rank -> before KEY`
  or `rank -> after KEY`. The bundled tool descriptions carry the two arguments, and the plugin's
  own copy of the `legion-controller` skill carries the corrected backlog rule, since `dist/` and
  `legion/` were regenerated (INFRA-188).
- The `dispatch` command (`dist/dispatch.js`) imports every architecture source of a project, one
  per repository: `dispatch architecture-sync --project <KEY>` prints a line for each source, and
  `--repo <owner/name>` imports that one alone. Against a Dispatch older than this release,
  `dispatch architecture-sync --project <KEY>` falls back to that server's single-source sync and
  prints its one line, and `--repo` needs a Dispatch of this release. The `dispatch` skill says an
  issue's component ids may come from any of its project's source repositories, and the plugin's
  own copy of the `legion-architect` skill says a component file goes in the branch of the source
  repository that holds it, with an id no other source of the project uses, since `dist/` and
  `legion/` were regenerated (INFRA-391).

### Changed

- The `dispatch` command (`dist/dispatch.js`) refuses `dispatch issue-update --status done
  --reason` on an issue already closed, `ISSUE_CLOSED`, before it posts the reason, since Dispatch
  now takes messages on a closed issue; the bundled `dispatch_issue_update` description no longer
  says a closed issue takes no messages or comments (INFRA-448).
- The plugin declares daemon API contract 38 in `package.json`'s `legion.daemonApiVersion`
  (INFRA-225): `GET /legion/v1/state` carries `admission.blocked`, every root and child Legion is
  waiting to start behind an unfinished Dispatch issue, each `{issue, blockedBy}`, and
  `release_children` answers `started` and `held` beside `released`, all required, so the MCP
  server's strict readers admit the fields and a plugin built before it refuses them; a session of
  it registers with a daemon at 38. Install it with a `legion` built from the same commit. Its
  `legion/` copies of the `legion-architect` and `legion-controller` skills carry the blocked-by
  rule, since `legion/` was regenerated: children linked with `--blocked-by`, a held child
  released all the same, a never-started child that still waits handed over carrying no run, the
  controller's free slots counted without the waiting roots in `admission.blocked`, and a root both
  waiting and blocked read in its admission reference.
- The plugin declares daemon API contract 37 in `package.json`'s `legion.daemonApiVersion`
  (INFRA-80): under the Kubernetes runtime an issue's disk is a generic ephemeral volume its pod
  owns, created with the pod and deleted with it, so the `workspace-init provision` container is
  no longer told `LEGION_EXPECT_ISSUE_VOLUME` and the lost-disk machinery it drove — the
  provisioning exit code 3, the recovery marker and the probe for an issue's retained sessions,
  with the `legion sessions mark-lost` command — is gone, and `runtime.kubernetes.session_store`
  must name a store that keeps sessions in a database, `postgres` or `cloudsql-iam`. Nothing this
  plugin reads or writes moved; a session of it registers with a daemon at 37.
- The plugin's regenerated `legion/` copy of the `legion-worker` skill's tools reference no longer
  tells a role to add a new image package to the licence file by hand: the worker image lists every
  Debian package it carries in `/usr/share/doc/legion/THIRD_PARTY_NOTICES` from its own dpkg
  database (INFRA-255). The Dockerfile's comments and its toolchain smoke step stay hand-kept.

- The plugin declares daemon API contract 36 in `package.json`'s `legion.daemonApiVersion`
  (INFRA-229): `GET /legion/v1/state` carries `state` on an issue's pull request, its `open`,
  `merged` or `closed`, always emitted, so the MCP server's strict state reader admits the field
  and nothing this plugin reads or writes moved; a session of it registers with a daemon at 36.
- The bundled `envoy_publish` description (`dist/` regenerated) and the `envoy` skill say a team
  topic, any `notifications.` topic the stream does not store, reaches a session on a serve port
  through its machine's listener when that listener subscribes to every topic
  (`ENVOY_SUBSCRIBE_ALL_TOPICS=1`; the shared hub's listener does not), where they said such a
  session received none. A session of this plugin holds its own NATS connection and receives such
  a topic as before (INFRA-122).

### Fixed

- `dispatch-first` now says to open a `dispatch ask` the moment a person blocks your work, and to
  record every request you accept in Dispatch and read that record back before a turn ends.
- The plugin's `legion/` copy of the `legion-architect` skill is regenerated with the same
  reference: its Wake routing table moves unchanged to
  `skill://legion-architect/references/wake-routing.md` (INFRA-376).

- The session title's tmux query runs `tmux -u`, so its tab-separated answer no longer depends on
  the calling process's environment: tmux printed the tabs as `_` to a process with neither `TMUX`
  nor a UTF-8 locale, and the answer read as one field. A Claude Code session in a tmux pane has
  `TMUX`, so its title was not affected; the real-tmux test, which ran tmux without `-u`, failed in
  such an environment and now passes. The daemon's real-tmux test rig reads tmux with `-u` too.

- The `dispatch` command (`dist/dispatch.js`) waits at most 1 s for tmux to name the session's
  pane, then kills the tmux client and sends the call without the pane. A tmux client waiting to
  connect to a tmux server too busy to accept caught the SIGTERM of the lookup's 5 s timeout and
  kept waiting, and the command waited for it, so a `dispatch` call from a pane of a loaded shared
  tmux server took up to 85 s.

- The README and Dispatch's Agents page guide say to restart Claude Code once after its first
  launch on a new profile and to check that its banner says the channel is loaded: a first launch
  can come up with `--channels ignored` (all four new profiles tried whose key came only from an
  `apiKeyHelper`, measured on 2.1.309), dropping every channel message while the session
  advertises Aside, so Dispatch records the message as sent. They also say that the first launch's
  "waiting for your organization's policy … Messages are dropped" banner stays up after the policy
  loads, while messages already reach the model. The README's "Enable the channel" section says
  whoever administers the machine writes the managed settings.

- A session whose `--channels` entry is not allowlisted says where the allowlist goes: the
  machine's `managed-settings.json` (`/etc/claude-code/managed-settings.json` on Linux), or the
  server-managed settings when the account receives them, where it said an organization adds it.

- The `dispatch` command (`dist/dispatch.js`) ends a write's cadence advice (three or more
  messages on an issue with no human response) with that line, opening `Posted.`, so a tail of
  the output shows the write went through; `dispatch issue --help` says `--priority` takes a
  number from 0 to 3, 0 highest, where it named P0 to P3.

- The `dispatch` command (`dist/dispatch.js`) reads an issue's document as a document summary,
  including the links `dispatch search` returns, where it printed the issue's summary; refuses
  owner arguments that disagree or that a command would drop (a document `comment` posts past
  without `--quote`, `ask` without `--anchor`, `--issue` beside another issue's ref, and the
  rest), naming the spelling that does each; and refuses
  `dispatch://KEY/artifact/<slug>/comment/<id>` with one problem naming
  `dispatch://KEY/comment/<id>`. An ask or comment a reference names is held to the issue or
  document it names by `read`, `resolve-comment`, `resolve-ask`, `edit-ask` and `follow`, and
  `dispatch ask` beside `--issue` or `--project` may cite any other issue or document.
- The `dispatch` skill: `dispatch read` of a document prints its summary, and a document hit
  names its issue on `Issue:`.
- The `dispatch-first` skill: scout, the spec checker it names, has no shell, so the Dispatch, gh
  and git output a spec's claims rest on is saved to files and handed to it.
- The `dispatch` command (`dist/dispatch.js`) ends a write with `(not subscribed to KEY; …)` only
  when none of the session's Envoy subscriptions, as the listener records them, receives every
  event on the issue or document; it used to end every write that way. An ask or an ask reply
  names its `envoy_subscribe` line under the same rule.
- The `dispatch` command (`dist/dispatch.js`) finds an ask in a spec by its `:::ask{#<id>}` block
  id in `dispatch read --ref dispatch://KEY/ask/<id>`, whole or as a unique 8+ hex prefix, and
  prints the ask's own id (`Ask id:`).
- The `dispatch` skill: a typed answer that disagrees with the selected option is the answer; a
  changed decision on an answered ask is a new ask, since an answered ask cannot be retracted;
  repeated measurements go in one artifact edited in place; and `dispatch read` takes an ask
  block's id, while the writes to an ask take the ask's own.
- A session whose deliveries are channel notifications Claude Code did not load registers with no
  delivery mode instead of `aside` (a Legion pane, whose deliveries go to the bridge's inbox, is
  unaffected). Claude Code drops a notification from a channel it did not load without telling the
  server, so Dispatch used to record an Aside to such a session (one started without `--channels`,
  or whose `--channels` entry is not allowlisted) as sent while the model never saw it; the listener
  now refuses it and Dispatch records the attempt as failed. The server reads the `claude`
  process's command line and the managed settings' `allowedChannelPlugins` (`src/claude-channel.ts`),
  says why on stderr and in its MCP instructions, and takes `ENVOY_CHANNEL_LOADED=1` or `0` as an
  override.

- The `dispatch` command (`dist/dispatch.js`) waits for its stdout and stderr writes before it
  exits. Bun's `process.exit` dropped whatever a pipe had not yet taken, so a Bash tool lost the
  end of any output over the pipe's buffer (8 KB to 16 KB) with exit code 0.
- The `dispatch` command (`dist/dispatch.js`) takes a bare issue key as the first argument of a
  command with `--issue` as that `--issue` (`dispatch read ACME-12`), only when `--issue` is not
  also given; a key-shaped word later in the line is refused.
- The `dispatch` command (`dist/dispatch.js`) refuses an ask, comment or message link as `--ref`
  to `comment`, `suggest`, `doc-edit` and `doc-read`, which used to act on the link's owner and
  drop the item; the refusal names `reply_to_ask` (`--reply-to-ask`), `reply_to` (`--reply-to`) or
  `dispatch read`.

### Changed

- The `legion/` plugin's skills fix a clear defect outside a role's issue in its own tree's pull
  request when the tree touches or depends on that code, and send any other to the controller,
  which hands it to the tree that owns the code and files it for Legion only when none does. A
  `dispatch ask` stays for a finding that needs a decision.

- The `legion/` plugin's `plan-reviewer` reviews a spec as well as a plan, checking each factual
  claim, and it and `plan-gap-analyst` get `Bash` for read-only commands. Its
  `legion-architect` runs both on a root spec before every approval request, and ends the
  request's `--summary` with their results. The bundled `dispatch request-approval` help names
  both reviewers.

- The plugin ships `claude-envoy:dispatch-writing-specs` in place of
  `claude-envoy:dispatch-brainstorming`: `skills/dispatch-writing-specs` links the
  repository's new skill, and the old link is gone. The three Dispatch skills are under 2,000 words
  together, and `dispatch` no longer has a `references/` directory; `pi-envoy`'s changelog lists
  what each skill holds and which rules are gone. The bundled `dispatch` command (`dist/dispatch.js`,
  `dist/envoy-channel.js`, `dist/session-hook.js`) prints the new notice after a primary spec with
  no ask block: a person does not watch the session's chat, and sees its need only through an open
  ask or a spec that waits for approval. `dispatch issue --spec` and `dispatch doc-edit` point at
  `skill://dispatch-writing-specs`. After approval, `dispatch-writing-specs` makes a new version only
  for a change to scope, acceptance, cost, risk or a person's decision, all in one version.

- The bundled Dispatch tools (`dist/dispatch.js`, `dist/envoy-channel.js`, `dist/session-hook.js`)
  carry the 32,000-character body limit, up from 2,000 (INFRA-45): the `comment`, `suggest` and
  `message` bodies and `issue-update`'s closing `reason` accept that many characters, and their
  descriptions say so. Nothing gates the skew: a bundle of this release against a Dispatch server
  still on 2,000 lets an agent compose a longer body and take a `400 CAP_EXCEEDED` from the
  server, so install it after that server is deployed (`packages/envoy/CHANGELOG.md` has the
  rollout order).
- The generated `legion/` plugin carries the new `thermonuclear-deep-review` and
  `thermonuclear-code-quality` rubrics (see `packages/pi-legion/CHANGELOG.md`), and the README no
  longer says the dotfiles plugin ships a copy.

### Added

- The plugin ships a fifth skill, `claude-envoy:asd-ste100`, as a symlink into the root
  `skills/`: the ASD-STE100 rules (Simplified Technical English) for text a reader must not
  misread. It is Legion's copy of the MIT-licensed `danyuchn/asd-ste100-skill` at 32511c69, with
  its licence and without its linter.
- The `dispatch` command (`dist/dispatch.js`) takes `dispatch read --events N` (an issue's last N
  events) and `--all-events` (every one), on an issue read and its `dispatch://KEY/log`, which list
  the last 10 without either; the bundled tool specs in `dist/envoy-channel.js` and
  `dist/session-hook.js` carry the two arguments, and the shipped `dispatch` skill's reading
  reference names the two flags.
- A Claude Code session registers under the name a person gave it (`claude -n`, `--name`,
  `/rename`), alone, so Dispatch's Agents page shows that name where it showed
  `<tmux pane or folder> <short id>`; `ENVOY_SESSION_TITLE` still wins, and a session nobody named
  keeps that `<tmux pane or folder> <short id>` title. Claude Code hands the chosen name to a hook
  alone, as `session_title`, so a new `UserPromptSubmit` hook, and the `SessionStart` one, records
  it under `${CLAUDE_PLUGIN_DATA}/titles/` and the channel server reads it on every registration:
  a rename shows after the session's next prompt and the heartbeat that follows, and taking the
  name away brings the fallback title back. That hook is a bundle of its own, since Claude Code
  holds the prompt until it returns: it costs about what starting `bun` at all costs. Title files
  are pruned at thirty days, and so are the role files beside them, which a session that held its
  role to the end used to leave behind for ever.
- A Claude Code session registers with a title, so Dispatch's Agents page names it instead of
  showing its session id: `ENVOY_SESSION_TITLE`; else a name and the session id's first eight
  characters. Inside tmux the name is `session:window` for a window whose `automatic-rename` is
  off, else the session (for grouped sessions, the group), asked of the tmux server `TMUX` names
  and used when that server's pid matches and the pane's process is one of the session's own
  ancestors. Otherwise (outside tmux, a refused or missing answer, or a session named only by
  digits in a window nobody named) it is the project directory's name. The `dispatch` command
  stamps the same title on the session's Dispatch writes (with inbound messages or Envoy off, for
  the id the session starts with; not when it starts with Dispatch off). It registered an empty title.
- The MCP server lists `slack_post` beside the ten `envoy_*` tools (sjawhar/legion#1998), a reply
  in a Slack thread with the bot token the secrets broker grants (`LEGION_SLACK_BOT_TOKEN`).
- A Legion phase worker can run on Claude Code (sjawhar/legion#2032). When `legion claude-bridge`
  launches Claude Code, the MCP server sees `LEGION_BOOT_TOKEN_FILE`, `LEGION_DAEMON_URL` and
  `CLAUDE_CONFIG_DIR`: it waits for the session's transcript, registers with the daemon (daemon API
  contract 39, pinned in `package.json`'s `legion.daemonApiVersion`), claims the issue's Envoy role
  soft and reports ready, and readies again whenever it regains the role. A registration the daemon
  refuses stops Claude Code so the daemon relaunches it.
- The MCP tool `legion` (`read_record`, `handoff_complete`, `request_backward_move`) serves the
  phase ops `@sjawhar/pi-legion`'s tool serves, on one shared schema, description template and
  executor (`@legion/pi-shared/legion-phase-ops`), the same daemon routes and session grants; its
  description also says what `read_record` and `request_backward_move` do, and a call is refused
  whole before any request, each problem named. The claim boots through the claim boot both share
  (`@legion/pi-shared/legion-claim-core`).
- When `LEGION_CLAUDE_INBOX` is set, Envoy deliveries go to the bridge's inbox socket and arrive as
  user turns, since Claude Code's print mode delivers no channel notification, and the session
  advertises `["aside", "steer"]`: the bridge starts a turn with a delivery when Claude Code is
  idle and sends it as the next user message of the running turn otherwise, which serves a
  Dispatch Send (`steer`, Dispatch's default) as well as an Aside. A session whose deliveries are
  channel notifications advertises `aside` alone.
- `legion/`, a plugin generated by `scripts/build.ts`: Legion's worker skills and its six task
  agents, with their tools mapped to Claude Code's names. `scripts/build.ts --check` covers it.

### Changed

- The plugin declares daemon API contract 39 in `package.json`'s `legion.daemonApiVersion`
  (INFRA-208): under the Kubernetes runtime an issue pod runs one container, `roles`, whose
  launcher runs every role, and `runtime.kubernetes.resources` in `GET /legion/v1/state`'s
  `settings` is `{issue, controller}`; nothing this plugin reads or writes moved, and
  `dist/envoy-channel.js`, which embeds the number, is rebuilt; a session of it registers with a
  daemon at 39.
- The plugin declares daemon API contract 35 in `package.json`'s `legion.daemonApiVersion`
  (LEGION-791): `GET /legion/v1/state` carries `settings`, the daemon's resolved `legion.yaml` with
  every credential, credential source and credential file path left out and a URL's userinfo masked
  as `xxxxx@`, required, so the MCP server's strict state reader admits the field and nothing this
  plugin reads or writes moved; a session of it registers with a daemon at 35.
- The plugin declares daemon API contract 34 in `package.json`'s `legion.daemonApiVersion`
  (INFRA-31): under the Kubernetes runtime the roles of an issue now share one scratch on the
  issue's volume, and the daemon tells every role container of a pod the same temporary directory,
  cache home, data home and bun install cache under it (`/legion/scratch/tmp`,
  `/legion/scratch/cache`, `/legion/scratch/data` and `/legion/scratch/bun/install/cache`), and
  the same Go build and module caches, one pair for the issue on that volume beside the
  workspaces (`GOCACHE=/legion/go/build` and `GOMODCACHE=/legion/go/mod`), names the runtime did
  not set before. The container's own gigabyte of ephemeral storage no longer bounds any of them.
  The state home is the one directory a role keeps to itself, `/legion/scratch/state/<role>`,
  since Oh My Pi's browser broker lock is an abstract unix socket named from the state root and
  such a name is one across every container of a pod. Nothing this plugin reads or writes moved;
  a session of it registers with a daemon at 34.
- The `dispatch`, `dispatch-first` and `dispatch-brainstorming` skills are rewritten shorter
  (the dispatch skill and its references from about 20,300 words to 9,400), with detail moved to
  references and `dispatch <command> --help`; a blind comparison of old and new text on each
  skill's main situations found the same decisions. Two kinds of rule are new: `dispatch` says to
  ask once (an open ask is the request; do not raise it again in each message), and
  `dispatch-brainstorming` says how to hand a spec or plan to the human: the reply leads with each
  open decision, then each unsettled point with a line reference, then the link; it asks for no
  review when nothing is new; it says "ready for review", never "complete"; "plan only" stops after
  the plan; and the spec stays a record of the current design, not of how it was reached.
- The `envoy_unsubscribe` and `envoy_role_set` descriptions name the role-bound subscriptions
  an Oh My Pi session keeps; Claude Code holds none, so the tools behave as before.
- The `dispatch` command's `issue` and `issue-update` take `--repo <owner/name>` (`''` clears), and
  `dispatch read` prints the issue's `Repository:` line. Against a Dispatch server older than the
  repository fields the read prints `Repository: unknown (…)`, with a warning on stderr.
- `dispatch issue` names the new issue's assignee, and every `dispatch` link points at
  `DISPATCH_PUBLIC_URL` when it is set; `slack_post` refuses a reply linking a cluster-internal
  address.
- `dispatch issue` prints the new issue's dashboard link on its `Created` line.
- `envoy_sessions`' `machine` filter now matches a session's real `host` (falling back to
  `machine_id` for a session whose plugin predates the field) rather than the listener that
  registered it, so filtering by machine finds a session on its own machine again even when every
  session shares one listener.
- The `dispatch` command's approval texts, and the `dispatch` skill this plugin ships, state the
  carried approval (LEGION-573): a human's own answer to a decision block carries their approval
  to the version it writes when that answer is the whole change since the version they approved,
  so nothing is requested for it; a change outside the decision blocks makes the approval stale,
  and the next request's `--summary` names what changed since the earlier approval.
- The plugin reads one envoy.json with a `dispatch` and an `envoy` section (LEGION-707):
  `$CLAUDE_CONFIG_DIR/envoy.json`, else the file `ENVOY_CONFIG_FILE` names, else
  `~/.config/legion/envoy.json`. `~/.config/opencode/envoy.json` is not read: while it is there and
  the session's file is missing, the channel server's notice, and the `open-asks` hook's summary
  when that missing file is what turns Dispatch off, say so and name the move
  (`mkdir -p ~/.config/legion && mv ~/.config/opencode/envoy.json ~/.config/legion/`), or, for
  `$CLAUDE_CONFIG_DIR/envoy.json`, the file to copy or link it to; under a relocated
  `XDG_CONFIG_HOME` they say only that the old file is not read there. A top-level `natsUrls` is
  refused, naming `envoy.natsUrls`.
- No notice gives a `mv` across a mount: where the old file's directory is on another filesystem than the new file's nearest existing directory (an agent box's bind-mounted `~/.config/opencode`), it names the file to create and says to move the old one on the machine that owns it.
- The `open-asks` hook writes `export ENVOY_CONFIG_FILE='<path>'` to `CLAUDE_ENV_FILE` beside `DISPATCH_HOST`, and the `dispatch` command reads the file `ENVOY_CONFIG_FILE` names whenever `DISPATCH_HOST` is set, so it reads the session's own file. A Bash that lacks what that hook writes, as a subagent's can, runs `dispatch` as host `claude` (`CLAUDE_CODE_CHILD_SESSION=1` with `DISPATCH_HOST` unset) on `CLAUDE_CONFIG_DIR`'s file, where it was refused for want of `DISPATCH_HOST`.
- With Envoy off the channel server lists no tool, refuses each call with `Envoy is off for this session: <why>`, and exits 0 when stdin ends, instead of failing on `ENVOY_NATS_URL is required`.
- A `token` or `natsSeed` starting with `!` is a command whose output is the value. A checkout's
  `.envoy/envoy.json` configures Dispatch alone; its `.opencode/envoy.json` is not read, and while
  `.envoy/envoy.json` is absent the `open-asks` hook's context, the channel server's notice and the
  `dispatch` command name the rename. A checkout's file that is not valid refuses Dispatch only
  while `DISPATCH_URL` is unset, and a checkout's `dispatch.token` needs its own
  `dispatch.serverUrl` beside it.
- `DISPATCH_URL`, `ENVOY_URL` and `ENVOY_NATS_URL` take a file's token or seed only for that file's own server.
- One rule for `enabled` in both sections (envoy-client's README): `enabled: false` turns a section off; otherwise it is on when its address resolves, from the file or a variable, so an `envoy` section with `url` and `token` and no `enabled` is on with its bearer. A checkout's file is not read while `DISPATCH_URL` is set, so its token never goes to the server `DISPATCH_URL` names, and its `dispatch.enabled: true` alone turns nothing on.
- With Envoy on and no NATS server, or with its NATS seed refused, the channel server serves the Envoy tools with inbound messages off, as pi-envoy does, and says so once on stderr (`Envoy's inbound messages are off: <why>; the Envoy tools still work`, where `<why>` is `Envoy has no NATS server (envoy.natsUrls, ENVOY_NATS_URL)` or the seed's refusal); `envoy_subscribe` is refused naming why. It exited 1.
- The channel server writes one notice to stderr when a section is off: the missing file, the section's error, or what a checkout's files hold that is not read.
- `bin/envoy-send.ts` sends to the listener the envoy.json a command reads names (`envoy.url`, or `ENVOY_URL`), with its bearer (`envoy.token`, or `ENVOY_TOKEN_FILE`/`ENVOY_TOKEN`), and refuses, saying why, when Envoy is off; it sent to `ENVOY_URL` or `http://127.0.0.1:9020` with no bearer.
- `envoy-send`, a command, counts `CLAUDE_CONFIG_DIR` only under Claude Code (`CLAUDE_CODE_CHILD_SESSION=1`): run from a plain terminal whose shell exports it, it does not read `$CLAUDE_CONFIG_DIR/envoy.json`. The hook and the channel server read `CLAUDE_CONFIG_DIR` whenever it is set.

### Fixed

- `claude plugin marketplace update legion-plugins`, then `claude plugin update claude-envoy@legion-plugins`, installs main's current tree: `.claude-plugin/plugin.json` declares no `version`, so Claude Code versions the plugin by its commit. Its fixed `1.0.0` held every install at the tree it was first installed from (`claude-envoy is already at the latest version (1.0.0)`).

## [1.0.0]

### Changed

- Agents reach Dispatch through the `dispatch` command (LEGION-588): `bin/dispatch` puts it on the
  Bash tool's `PATH`, running the committed `dist/dispatch.js`, and the `open-asks` hook writes
  `export DISPATCH_HOST=claude` to `CLAUDE_ENV_FILE` once per session. The command acts as
  the session `CLAUDE_CODE_SESSION_ID` names and prints the follow notice itself, once per ask.

### Removed

- The MCP server's twenty-one `dispatch_*` tools: it lists the ten `envoy_*` tools only, and a
  `dispatch_*` call is an unknown tool. The follow announcement on the channel and the MCP image
  blocks went with them, since no MCP tool returns a Dispatch result.

## [0.6.7]

### Added

- A Dispatch tool result's `images` (`dispatch_doc_read` of a picture, the pictures `dispatch_read`
  shows) arrive as MCP image blocks after the JSON text, which leaves them out (LEGION-541). Channel
  deliveries stay text: the notification channel carries text only. Through `executeDispatchTool`
  the MCP `dispatch_read` shows a session each picture once: a later read names one it was already
  shown, and `dispatch_doc_read` shows it again, because every request carries the session's
  history and Anthropic refuses one over 32 MB. The id `/clear` replaces is forgotten, and the new
  one starts with none shown, as its conversation does. A resumed session (`--resume`) is shown
  each picture once more: the server starts with no record of it and cannot read its transcript.

## [0.6.6]

### Fixed

- `scripts/build.ts` refuses to build under a Bun other than the one `.bun-version` pins, instead
  of silently bundling with whatever `bun` resolves from PATH (LEGION-568). `bun run build` and
  `bun run check-dist` spawn a new "bun" process to run the script — package.json's script text is
  a shell command, and Bun's own `run` resolves the "bun" in it from PATH rather than reusing
  whichever binary the caller invoked `bun run` with — so a devbox whose default Bun (a version
  manager's active version) differs from the pin silently bundled with that *other Bun version*
  instead. This is a separate, compounding cause from `0.6.5`'s `minify: false` fix, not the same
  bug restated: that one was a non-determinism *within* one Bun build's own bundler, on the
  identical pinned version; this one is an entirely different Bun version running in the first
  place. Either alone could make `check-dist` fail unpredictably, and the prior investigation
  (LEGION-548, `dist/`'s round-5 note) found the first and mistakenly attributed a failure the
  second actually caused to the build machine itself — confirmed directly: the identical pinned
  Bun, same source, same lockfile, disagreed between a devbox and the CI runner, because the
  devbox's `bun run` reached a different Bun than the pin, not because the machines differ.
  `buildBundles()` now checks `Bun.version` against `.bun-version` before building, and the CLI
  entry point checks before touching the filesystem at all, so a refusal never deletes the
  committed `dist/` it was about to replace.

## [0.6.5]

### Fixed

- `scripts/build.ts` disables minification entirely (`minify: false`) rather than disabling
  `whitespace`, `identifiers` and `syntax` individually: the partial-disable form still routed
  through Bun 1.3.14's minifying code-generation path, which picked a non-deterministic CJS/ESM
  interop check on repeated builds of this package's module graph and made CI's "Check the
  committed bundle is fresh" step fail unpredictably on any change to `@legion/contracts`, this
  package's own source untouched (LEGION-548). Confirmed directly: ten rebuilds of one unmodified
  checkout, same pinned Bun, same lockfile, disagreed with each other eight times out of ten
  before the fix, and agreed every time after it.

## [0.6.4]

### Added

- The plugin ships a fourth skill, `claude-envoy:dispatch-brainstorming` (LEGION-475): in a session
  with Dispatch, a design conversation runs in its issue's spec, each question a decision block
  and nothing asked in chat, approval is requested once with nothing new in it, and the plan is the
  issue's `plan.md` document, never a repository file. It replaces superpowers' `brainstorming` and
  `writing-plans` in such a session. The `dispatch-first` skill the hook injects sends every design
  conversation and plan to it, and the `dispatch` skill's brainstorming reference is gone, its
  process now in the new skill.

## [0.6.3]

### Changed

- An approval request carries nothing new (LEGION-475). `dispatch_request_approval`'s `summary`
  says only what the human is approving, with no commentary and no question; its description, its
  refusal while a decision block is open, its two result texts and a stale approval's line say to
  request again only once the human has agreed to every point in the new version. The `dispatch`
  and `dispatch-first` skills this plugin ships say to brainstorm a design change in its issue's
  spec rather than in chat.

## [0.6.2]

### Fixed

- A malformed delivery can no longer make Claude Code accept a repeat just because one identity
  field is empty or otherwise invalid (LEGION-468, #1668). The channel read a frame's `event_id`,
  `dedupe_key`, `source` and `source_event_id` as one record, so a single invalid field discarded
  all of them and the frame passed as new. It now ignores only the invalid field and keeps a
  valid `event_id` or `dedupe_key`, so either still recognises the second copy; when neither is
  valid, the delivery remains at-least-once.

## [0.6.1]

### Fixed

- A Dispatch Retry no longer reaches a Claude Code session twice. The channel recognised a repeat
  by its `event_id`, and the listener mints a new one for every send, so a same-mode Retry of a
  message that had already reached the session arrived as a new event and was notified again, while the
  dashboard promised "Retry won't deliver it twice". The channel now recognises a repeat by its
  `dedupe_key`, which a Retry shares with the send before it, through the same
  `createDeliveryDedupe` the Oh My Pi extension uses, for the 72-hour duplicate window and only for
  a key that names its event (`dedupeKeyNamesItsEvent` in `@legion/contracts`): every Dispatch key,
  a webhook key of its delivery id, and a key the listener or the shared transport minted once for
  its message. Any other key is never dropped, so two distinct events that share one (the MCP
  bridge's, the Go daemon's outbox row id) both arrive. A second copy of one publish, which a
  session that follows overlapping topics (a pull request's whole thread and its checks) receives
  once per subscription, is still recognised by the `event_id` the copies share, whatever its key,
  so a CI settlement reaches Claude once. A send whose notification failed, or a Dispatch frame
  the channel answered with an error instead of showing it, is released, so its re-send still
  arrives; the release undoes only what that frame's own claim recorded, so a frame that merely
  carries a recorded Dispatch key cannot unclaim it. Keys live in memory, at most 250,000 of them
  with the oldest forgotten first; a restarted channel server (`claude --resume` included)
  forgets them.

## [0.6.0]

### Changed

- `dispatch_request_approval` requires `summary`: the proposals in the document's latest version
  the human hasn't already agreed to, in one to three sentences (LEGION-387). The Inbox shows it
  after "Approve spec.md (version N)?", and the result text quotes the question the human sees.
  It needs a Dispatch server that accepts `summary`; an older one refuses the call.
- `dispatch_request_approval` is refused, with nothing sent, while the document holds an open
  decision block, and the refusal names each block.
- `dispatch_doc_edit` is refused, with nothing sent, when a `delete` or `retype` would take a
  decision block out of the document while its ask is open, even in a batch that inserts it
  again; the refusal names `replace`, `move` and, for the session that asked, `dispatch_edit_ask` instead. A whole-document
  replace through `dispatch_artifact` is not refused, so it can still remove an open block.
- The `dispatch_issue` and `dispatch_doc_edit` descriptions no longer list spec headings; they
  point at the dispatch skill's "Writing a spec", which describes a spec as the design
  conversation: the problem and its evidence, each open question a decision block at the end of
  the section that discusses it, and approval requested only once those are settled.
- `dispatch_search` refuses a `query` over 1,000 characters (LEGION-386) and a `project` that is
  not a project key such as CORE before any request, naming the rule. Both ride in the search URL,
  which the load balancer in front of production Dispatch answers with a bare HTML `414` when it
  is too long, so this refusal is what stops a pasted passage from becoming that error; a
  lowercased key, which used to come back as no results, is now refused by name.

## [0.5.0]

### Added

- With Dispatch configured, every Claude Code session and subagent carries the `dispatch-first`
  skill (LEGION-386): the hook command puts it into the model's context as `additionalContext` on
  every `SessionStart` (startup, resume, clear, compact, fork) and on every `SubagentStart`, each a
  hook of its own so the open-asks summary cannot push it past Claude Code's 10,000-character hook
  limit. On a resume or fork Claude Code adds it only when the transcript does not already hold the
  same text, so a session opened on 0.4.0 gets it when it is resumed on 0.5.0, and one that started
  with it keeps one copy. The plugin ships the skill as a third symlink, `skills/dispatch-first`,
  and the hook reads it from `${CLAUDE_PLUGIN_ROOT}` at run time; an install without it fails the
  hook naming the file.

### Changed

- The two hook commands are one bundle, `dist/session-hook.js` (`hooks/session-hook.ts`), whose
  argument picks the mode: `open-asks` or `dispatch-first`. `dist/open-asks-hook.js` is gone.

## [0.4.0]

### Added

- Added the nine native Dispatch tools and automatic subscriptions to each mutation result's issue topic.

### Changed

- The bundled `envoy_subscribe` description and `envoy` skill say a `pr.<n>.checks` settlement is
  published for every commit of the pull request whose checks settle and names its `sha`
  (LEGION-208).
- Committed bundles preserve whitespace and identifier names so independent source edits merge at line level; syntax minification remains disabled because Bun 1.3.14's constant folding can truncate concatenated string literals in CI builds.

- The plugin is `claude-envoy` (package `@sjawhar/claude-envoy`, directory `packages/claude-envoy`), matching `pi-envoy`. Its channel entry is `plugin:claude-envoy@legion-plugins`, its tools are `mcp__plugin_claude-envoy_envoy__*`, its skills are `claude-envoy:<skill>`, and the managed-settings allowlist names `{ "marketplace": "legion-plugins", "plugin": "claude-envoy" }`. An install of `claude-envoy-bridge@legion-plugins` is not migrated: install `claude-envoy@legion-plugins` and uninstall the old one. Claude Code keys `CLAUDE_PLUGIN_DATA` on the plugin and marketplace names (`plugins/data/claude-envoy-legion-plugins`), so a `claude --resume` of a session started before the rename does not get its held role back.
- The plugin ships only the `envoy` and `dispatch` skills (`claude-envoy:envoy`, `claude-envoy:dispatch`). The other root skills (`legion-*`, `ce-simplify-code`, `thermonuclear-*`) are Legion's Oh My Pi lane skills and no longer load from this plugin.

### Fixed

- The bundled Dispatch tools refuse a bare document reference that is one document's slug and
  another's filename, on an issue or a project, naming both ids, where they took the slug's
  document; on a project an id also outranks another document's slug. A `dispatch://` document
  reference, or a dashboard document URL, still resolves by its slug.
- Oh My Pi no longer launches the channel server. Every omp session that had this plugin installed opened with `Failed: claude-envoy-bridge:envoy [...]: MCP subprocess closed stdout before responding.`, because the server needs Claude Code's session identity and exits without it. A new `.omp-plugin/plugin.json` declares an empty `mcpServers`, which omp reads before `.claude-plugin/plugin.json` and applies instead of `.mcp.json`; Claude Code reads only `.claude-plugin/plugin.json` and still launches the server. omp gets Envoy and Dispatch from `@sjawhar/pi-legion-envoy`. Skills are unaffected in both harnesses.
- After `/clear`, the session's registry entry no longer lists the pre-`/clear` id's direct subject. The server dropped that subject only after its first registration under the new id, and the listener merges registered topics without removing any, so the entry kept it for the life of the entry and `envoy_unsubscribe` could not remove it. Every registry write now runs one at a time and a registration reads the id and topics when it runs, so a registration that was in flight when `envoy_unsubscribe` or a human in Dispatch removed a topic, or when a handoff deregistered the old id, no longer writes it back, and a handoff that fails before the id switch no longer leaves the new id's direct subject in registrations under the old id.
- `/clear` is adopted as soon as the SessionStart hook writes the handoff file instead of at the next heartbeat (up to two minutes), during which the session answered to the old id and a send to the new id was refused.
- A handoff whose first registration under the new id fails takes the held role back on a later heartbeat instead of leaving it on the old id.

## [0.3.0]

### Added

- The plugin runs from its marketplace install: `.mcp.json` launches the committed bundle `dist/envoy-channel.js` (`bun run build` / `bun run check-dist`), and the `env` block passes through `CLAUDE_PROJECT_DIR` only.
- `SessionStart` hook (`hooks/hooks.json`, `dist/open-asks-hook.js`) that injects the session's open Dispatch asks and records the current session id per Claude process.
- The channel server follows the session id across `/clear` (per-process handoff file), keeps role state per session id so `claude --resume <id>` restores the role, and rebuilds the interests a resumed id already registered.
- Rejected targeted Dispatch frames answer Dispatch (`Invalid Dispatch targeted delivery frame`); malformed ones are dropped; `envoy_list` reports the live/registry union; a newly followed issue topic is announced once.
- The smoke answers every startup dialog unattended, defaults `CLAUDE_CHANNEL_FLAG` from the entry's marketplace, and captures the pane.

### Fixed

- A plain direct send no longer gets an empty receipt (it is a JetStream publish; the receipt failed the listener's publish with `invalid jetstream publish response` after delivery).
- SIGHUP from the Claude process group deregisters the session instead of killing the server silently.
- Channel meta key `source` renamed to `producer`; it duplicated Claude Code's own `source` attribute on the `<channel>` tag.
