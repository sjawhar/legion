# Pi Envoy Extension

`@sjawhar/pi-envoy` is the Oh My Pi extension for Envoy messaging and the `dispatch` command,
the plugin every session loads. It shares the Envoy HTTP client, tool contract, envelope parsing,
and subject helpers with the other Legion adapters while keeping OMP's direct NATS subscriptions
and Pi steering delivery local (inbound messages steer an in-flight turn instead of queueing behind
it). The Legion lifecycle — the entry a Legion pane boots a root architect, a phase worker or the
controller with — is the separate plugin `@sjawhar/pi-legion` (`packages/pi-legion`), installed
beside this one in a Legion deployment; it reaches this plugin through the in-process interface
described in `packages/pi-shared/AGENTS.md`.

Normal topic subscriptions are direct NATS subscriptions owned by this extension. A role claim is
different: the listener arbitrates the core-NATS role lane for the current live holder, then sends
a receipt-backed request with the original role topic to the holder's direct agent subject.
`envoy_unsubscribe` manages only normal topic subscriptions; it does not relinquish a held role,
and it keeps open, listing them as kept, the subscriptions another plugin holds for that role.
The agent pump replies after accepting the envelope; without a receipt within two seconds the
listener emits a `delivery_failed` exception. Role messages are live only; they are not retained
for a later claimant.

`envoy_list()` shows the union of the local subscriptions and the listener's persisted interest
registry. Each reported interest identifies whether it is `live`, `registry`, or `both`, so
temporary registration drift does not hide the extension's actual delivery state.

## Inbound delivery

Pi renders every Envoy envelope through the shared `@legion/envoy-client/delivery` renderer. The
result is one TOON block with recognized routing and delivery fields such as `to`, `from`, `at`,
`id`, and `summary`; a structured `payload` appears once as `message`. Invalid JSON is represented
as a safe `unrecognised` value rather than exposed as raw bytes.

`envoy_inbox` returns the 50 most recent delivered envelopes as metadata (`event_id`, `at`, `from`,
and `summary`) for recovery after an interrupt. `envoy_role_get` returns the current holder and
last-seen timestamp for a role. `envoy_subscribe` reports listener warnings in both its text and
details result, including subscriptions whose stream currently has no matching event.

## Session identity

Run `/whoami` to copy the active session ID to the clipboard. OMP copies through its host
clipboard API, which sends OSC 52
first for tmux and SSH sessions. The notification shows the session ID even if the copy fails.

For tmux to accept OSC 52 clipboard writes, enable clipboard support in the tmux server:

```tmux
set -g set-clipboard on
```

## Development install

This package declares one OMP extension entry in `package.json`, `extensions/envoy.ts`: Envoy
messaging, subscriptions, steering delivery, and the Dispatch tools. For local development, link
the entry into OMP:

```sh
ln -sfn "$PWD/packages/pi-envoy/extensions/envoy.ts" \
  ~/.omp/agent/extensions/envoy.ts
```

The repository root `package.json` likewise loads `extensions/envoy.ts` for dev sessions inside
this repo. The Legion entry (`packages/pi-legion/extensions/legion.ts`) is inert without
`LEGION_TREE`/`LEGION_ROLE`/`LEGION_CONTROLLER` in the environment and is not loaded there; the
Legion daemon spawns every root and phase-worker process, and `legion controller start` the
controller, with both installed packages already active (`packages/pi-legion/README.md`).

## Published package

Released installs come from npm as `@sjawhar/pi-envoy`:

```sh
omp plugin install @sjawhar/pi-envoy
```

A session that had the pre-split package installed (the one package that carried both entries
until 7.x) uninstalls it and installs this one once; the Unreleased entry of `CHANGELOG.md` gives
the two commands. A Legion deployment installs `@sjawhar/pi-legion` as well
(`packages/pi-legion/README.md`); the daemon's boot gate refuses a pane that still loads the
pre-split package beside the new ones.

The tarball is self-contained: it ships `dist/envoy.js` — bundling every dependency except the OMP
host packages (`@legion/pi-shared`, `@legion/contracts` and `@legion/envoy-client` are inlined) —
`dist/THIRD_PARTY_NOTICES`, and the five skills every session with Dispatch reads, `asd-ste100`,
`dispatch`, `dispatch-first`, `dispatch-brainstorming` and `envoy`, staged from the repository's `skills/` at
`dist/skills`. The manifest declares `omp.skills: ["dist/skills"]` alongside `omp.extensions`:
OMP's own plugin discovery only reads `skill://`-resolvable skills from `<plugin root>/skills` or
from directories a manifest's `omp.skills` array names, and this package ships skills at
`dist/skills` (not the plugin root itself), so the field is required for every session to see them
at all. `extensions/envoy.ts` reads `dist/skills/dispatch-first/SKILL.md` at load (in a repository
checkout, the same file under `skills/`) for the `dispatch-first` context it injects, so a package
without it fails to load naming the file. The Legion skills and the task agents Legion's prompts
dispatch ship in `@sjawhar/pi-legion`; a link from one of its skills into `dispatch` is written
`skill://dispatch/...`, which resolves by name once both are installed.

`scripts/pi-plugin-prepack.sh` at the repository root is both plugins' `prepack` and the one place
that says which skills each ships: it builds this package's one entry, writes the notices and
stages this package's partition into `dist/skills`. The published manifest exposes `dist/envoy.js`
for `omp.extensions` (only `omp.skills` ships as committed, since `dist/skills` is its path in both
a repo checkout after the prepack and the published tarball), while the committed manifest keeps
the TypeScript entry for repo checkouts. In `.github/workflows/release.yaml`, the `pi_envoy_build`
job rewrites the manifest before `bun pm pack` and uploads the tarball (its tree is discarded,
nothing is restored); the `pi_envoy_publish` job publishes that tarball and commits the version bump
to the committed manifest, touching `.version` alone.
Packing with the committed source manifest is refused by the prepack, because such a tarball would
point OMP at an extension file it does not contain.

## The `dispatch` command

The extension registers no Dispatch tool. Agents reach Dispatch through the `dispatch` command in
their shell: `dispatch --help` lists its commands, `dispatch <command> --help` each one's flags and
an example, and the `dispatch` skill teaches when to use each. The package ships `bin/dispatch`, a
shim that runs the bundled `dist/dispatch.js` (built from `@legion/envoy-client`'s
`bin/dispatch.ts`) with `bun`. At load the extension puts that `bin/` first on `PATH`, sets
`DISPATCH_HOST=omp` and hands the session's envoy.json on as `ENVOY_CONFIG_FILE`; each session
start, switch and branch sets `DISPATCH_SESSION_ID` to the session's id, so a command acts as the
session whose shell ran it and reads the file that session reads.

A session reads one envoy.json, with a section for Dispatch and one for Envoy:

```json
{
  "dispatch": {
    "enabled": true,
    "serverUrl": "https://dispatch.example",
    "token": "!secrets get DISPATCH_TOKEN"
  },
  "envoy": {
    "enabled": true,
    "url": "http://127.0.0.1:9020",
    "natsUrls": ["nats://nats.internal.example:4222"],
    "natsSeed": "!secrets get NATS_NKEY_SEED"
  }
}
```

An Oh My Pi profile (`OMP_PROFILE` other than `default`) reads `$PI_CODING_AGENT_DIR/envoy.json`;
any other session reads the file `ENVOY_CONFIG_FILE` names, else
`${XDG_CONFIG_HOME:-~/.config}/legion/envoy.json`. The session's `dispatch` command reads the file
the session chose, which the extension hands it in `ENVOY_CONFIG_FILE`. The `dispatch` section of a
`<cwd>/.envoy/envoy.json` merges over it: its server is used only with the token written beside it,
and its token only beside its own server. An `envoy` section there is not read, which the start
notice says, and a checkout's file that is not valid refuses Dispatch alone, and only while
`DISPATCH_URL` is unset. Each section is resolved on its own: `enabled: false` turns it off, and a
missing file contributes nothing, so each section is then on only when its variables name it. A
`token` or `natsSeed` starting with `!` is a command whose trimmed output is the value (10 s, its
exit status deciding; never run from a checkout's file). `DISPATCH_URL`, `DISPATCH_TOKEN_FILE`,
`DISPATCH_TOKEN`, `ENVOY_URL`, `ENVOY_NATS_URL`, `ENVOY_TOKEN_FILE`, `ENVOY_TOKEN`,
`NATS_NKEY_SEED_FILE` and `NATS_NKEY_SEED` each override their own field; a file's token goes with
`DISPATCH_URL` or `ENVOY_URL` only when it names the file's own server, and a file's seed with
`ENVOY_NATS_URL` only when it names the file's NATS servers, so another server needs its own token
or seed variable. `DISPATCH_TOKEN_FILE` (a path whose trimmed contents are the token — how the
Legion daemon delivers it to a pane) never falls back when unreadable. Omitting
`dispatch.serverUrl` while `dispatch.enabled` is true targets `http://localhost:8766`, the Go
server's listen address. With Envoy off the session has no Envoy tools and takes no inbound
messages; with Envoy on and no NATS server, or with its NATS seed refused (a seed that pairs with
other servers, a failing `natsSeed` command, a seed variable that cannot be read), it keeps the
tools and `/whoami`, takes no inbound messages, connects to no NATS server, and refuses
`envoy_subscribe` and the Legion role claim with `inbound messages are off for this session: <why>`,
the claim before any listener call, so a Legion process fails its boot rather than hold a role
whose tasks reach nobody. When a
section is off for a reason worth saying, the session says so once at its start:
`envoy: no envoy.json at <path>: Dispatch and Envoy are off`, the section's error, what a
checkout's file holds that is not read, or that inbound messages are off and why. Invalid configuration, an
invalid URL, or an empty token makes every `dispatch` command exit 2 naming the source of the error.

`~/.config/opencode/envoy.json` and a checkout's `.opencode/envoy.json` are not read. While the old
user file is there and the session's file is missing, Dispatch and Envoy stay off unless a variable
names one, and the start notice says the old file is not read and what to do: for
`~/.config/legion/envoy.json`,
`mkdir -p ~/.config/legion && mv ~/.config/opencode/envoy.json ~/.config/legion/`; for a profile's
own file, copy or link the old file to it, or write it; under a relocated `XDG_CONFIG_HOME`, only
that it is not read there. While a checkout has no `.envoy/envoy.json`, the start notice and the
`dispatch` command name the rename of its `.opencode/envoy.json`. A top-level `natsUrls` is
refused, naming `envoy.natsUrls`.

A command that writes to an owner names either an issue (`--issue`, a native `KEY` or an external
`owner/repo#n` reference) or an unlinked project document (`--project` plus `--artifact`). A Legion
session may omit `--issue`: `LEGION_ISSUE` names its issue, and a bare issue number there is read
against the GitHub repository of the working directory. `dispatch doc-read` and `dispatch read`
also accept `--ref` with a `dispatch://` reference, including `dispatch://PROJECT/artifact/<slug>`.
`dispatch search` needs only `--query`. `dispatch edit-ask`, `dispatch resolve-ask`, and
`dispatch follow` instead identify an existing ask with `--ask`, and `dispatch resolve-comment` a
comment with `--comment`. No result carries a subscription topic: opening an ask, or replying to
one with `dispatch comment --reply-to-ask`, makes the session a follower of that ask (its answer
and replies reach the session directly, server-side), and the command prints
`Following ask <id> on <owner>: …` after its result the first time the session follows that ask.
Whole-issue or whole-document subscription is the model's own `envoy_subscribe` of the topic a
write result names (`notifications.dispatch.issue.<KEY>.>` or
`notifications.dispatch.document.<PROJECT>.<SLUG>.>`), which it names only when none of the
session's subscriptions, as the Envoy listener records them, covers that owner.

The shared specs in `@legion/contracts` supply each command's description and flags:
`@legion/envoy-client`'s `dispatch-command.ts` derives one command per spec and one flag per field.

`dispatch artifact` takes exactly one upload source: a local `--path`, or inline `--content`. For
example, an architect can post a specification directly, its text on stdin:

```sh
dispatch artifact --issue LEGION-1 --name spec.md --content-file - <<'EOF'
# Design
EOF
```

Lifecycle and scope decisions between Legion roles go through `envoy_publish` to the owning
architect's role topic; Dispatch is for durable questions to the human and the shared
document, not for coordination between roles.

## AskGate

AskGate reviews each Dispatch write of a session's main agent before the write posts. One model
call judges the write against the rules in `extensions/askgate/rules.md` and answers `allow` or
`revise`. A `revise` answer carries the reason and the fix. The gate reviews these calls:

- `dispatch ask`, `dispatch message`, `dispatch edit-ask`, `dispatch comment`, `dispatch doc-edit`
  and `dispatch request-approval`
- `dispatch issue` with `--spec` or `--spec-file`
- `dispatch artifact` when it uploads a markdown document: inline text (`--content`,
  `--content-file`), a `--path` file whose name ends in `.md` or `.markdown`, or a `--name` that
  ends in `.md`

Every other Dispatch command is outside the gate, and its text posts unreviewed: among them
`dispatch issue-update --reason`, `dispatch resolve-ask --reason` and `dispatch suggest`.

The gate reads the arguments from the command's own `--dry-run`. An upload that reads its name
from a file (`--name-file`) is decided from the name the dry run prints. `--path-file` is refused:
its content names a file the gate cannot capture before Dispatch opens it. An upload whose name or
path holds an expansion (`"$NAME"`) is outside the form the gate reads. The gate
copies each file whose text the write posts, a `--<field>-file` operand and an upload's `--path`,
into a private snapshot. It reviews the snapshot and changes
the command to post that snapshot. An `--image` file is not one of them: the model reads the
path, not the picture, and the gate takes no snapshot of it, because the CLI names the upload
after the file it opens at send time. The file must be a regular file. The gate refuses a missing
file, a directory, a device and a FIFO before it reads them, and a path that reaches a file
descriptor of whichever process opens it (`/dev/stdin`, `/dev/fd/N`, `/proc/self/fd/N`,
`/proc/thread-self/fd/N`, `/proc/<n>/fd/N`, and any link to one): AskGate would open it in Oh My
Pi's process and the command in its shell. For
text from standard input, use `--<field>-file -` with a quoted here-document. For an upload, that
is `--content-file -`: the CLI reads `--path -` as a file named `-`. The gate cuts all arguments
at 64 KiB as one JSON block, so it reviews only part of a long document. It judges an upload
against content failure 10 alone: a plan or a report is long by nature. An upload of any other
file is not reviewed. A `task` subagent is not reviewed.

**The form the gate reads.** One `dispatch` call, alone in the bash call:

```
[cd <literal path> && ] dispatch <subcommand> <literal words…> [2>&1] [<<'DELIM' … DELIM]
```

- The program is `dispatch` by name, or a literal path that ends in it.
- Every word is literal. Outside quotes a word holds only letters, digits and `._/+@%:,=-`, so no
  `$`, backtick, `~`, `*`, `?`, `[`, `{`, `}` or backslash; inside double quotes, no `$`, backtick
  or backslash. Single quotes make every character literal.
- One `cd <literal path> &&` may stand before it, and one `2>&1` after it.
- One here-document may feed it, with its delimiter in quotes (`<<'EOF'` or `<<"EOF"`), opened on
  the command's first line, before or after the `2>&1`.
- Nothing else is in the call: no second command, no pipe, no `;`, `&&`, `||` or `&`, no
  assignment, no other redirect and no line continuation.

**Everything else runs, and the `dispatch` command enforces the review.** The gate reads no other
form and refuses none: a call outside the literal grammar runs as written. What holds it back is
the CLI. For each literal call it lets run, the gate mints a single-use marker; the CLI posts a
gated write only when it consumes one, so a write sent any other way is refused when it runs:

- **The record.** While the gate is bound, pi-envoy keeps an owner-only `askgate.json` in the CLI's
  state directory of each session id it binds (`<state>/sessions/<id>/`, where `<state>` is
  `DISPATCH_STATE_DIR`, else `$XDG_STATE_HOME/dispatch`, else `~/.local/state/dispatch`). It names
  the Oh My Pi process, its Linux start time, and the process's owner-only marker store. A record
  of a session the process switched away from stays until the process exits, since a shell or an
  eval kernel started before the switch still acts as that session. A record whose process is
  gone, or whose pid another process now holds, is inert, and the CLI's session pruning keeps a
  directory whose record is live.
- **The marker.** The gate mints one file in the store for each literal call it lets run, created
  owner-only and exclusively. It binds the exact argument vector the CLI receives and, behind a
  review or a deliberate pass (a rebuttal, a breaker trip, a halt, another provider), the digest
  of the arguments the dry run printed, which holds the text of every `--<field>-file`. A call no
  verdict stands behind, and a `task` subagent's literal call, get a marker of the argument vector
  alone. Nothing of it is in the command the agent sees. A marker goes when its call ends, is
  refused or the session shuts down; a backgrounded call keeps its marker until shutdown, and a
  marker older than ten minutes is refused and removed.
- **The check.** After `--help`, `--dry-run` and the session and Dispatch settings, and before a
  gated write posts, the CLI reads the session's record. With a live one it consumes one matching
  marker by unlinking it: exactly one process wins, so a marker posts one write and a replay
  finds none. In `block` mode a write with no matching marker is refused
  (`dispatch <command> was not called: AskGate is on for this session, …`, which names the form
  to send). In `warn` mode it posts with a `dispatch: warning:` line. Either way the CLI logs it,
  and pi-envoy records an `advisor-gate` entry with outcome `unmarked`. A record that cannot be
  read, or that names a store that is gone, is a refusal. A session no live record names needs no
  marker: a terminal, CI, Claude Code, OpenCode, a pod without pi-envoy, a session with no
  Dispatch and one with `askgate.enabled: false`.

The gated writes are the same list for the gate and the CLI (`gatedWrite` in
`@legion/envoy-client/askgate-marker`): the six commands above, an `issue` with a spec and an
`artifact` that uploads a markdown document. `--help`, `--dry-run` and every other command need no
marker. So a pipe, a loop, a variable, `xargs`, an alias, a script, `python3 -c`, `node -e`, or a
process an eval cell starts runs, and its gated write is refused by the CLI with the form to
send; a call that holds no Dispatch write, such as a commit message or a search pattern that names
one, runs and posts nothing. A gate that did not load still writes the record and mints nothing,
so the CLI refuses every gated write it would have reviewed.

**Where the gate binds.** AskGate binds only in a session that has Dispatch, judged at load from
the session's own envoy.json and the variables over it (`DISPATCH_URL` and the other Dispatch
variables), with a checkout's `.envoy/envoy.json` able to turn it on and never off: a checkout that
names its own server and token gives the session a Dispatch, and one that turns off the Dispatch of
its own directory leaves the gate bound, since a later command can still pass that with
`DISPATCH_URL`. A session or a pod with no Dispatch has no write to review, so it gets no gate, no
record and no refusal. The test runs once, at load: a session that turns Dispatch on afterwards has
no gate until it is started again.

**The switch.** In a session with Dispatch, AskGate runs unless the session's own envoy.json turns
it off. Write this top-level section beside `dispatch`:

```json
{ "askgate": { "enabled": false } }
```

No section, or `enabled: true`, keeps it on. A checkout's `.envoy/envoy.json` cannot set it, and a
checkout's `dispatch.enabled: false` does not turn it off. A section that does not parse keeps
AskGate on, and the start notice names the file.

**The modes.** In `block` mode a write that the gate sends back does not post, and the CLI refuses
a gated write that carries no marker. In `warn` mode the write posts, and the reason or the CLI's
warning comes back to the agent. `OMP_ASKGATE=warn` or `OMP_ASKGATE=block` sets the mode. AskGate
ignores any other value, `off` included, and the first review shows one notice that names
`askgate.enabled` as the switch. With no such variable, the session's latest `experiments` entry
decides: its `askgate_block` arm off is `warn`. Every other session uses `block`. The gate writes
the mode into its store at each main-agent `bash` or `eval` call, where the CLI reads it. Two
refusals do not follow the mode: no model role and a gate that did not load refuse in both modes,
since neither can review anything, and a gate that cannot review is no off switch.

**The model.** The gate asks the session's `askgate` model role (`modelRoles.askgate`). When that
role is not set, it asks the `default` role. A model that answers with an error passes the request
to the next model of that role's `retry.fallbackChains` chain, on the same provider. When neither
role resolves, the gate refuses the write in both modes and names the two roles. A session whose
main model is on another provider than the gate's model is not reviewed: its transcript stays on
that provider.

**When a write posts unreviewed.** Each of these lets the write out, records an `advisor-gate`
entry with its outcome, and hands the agent a note that says the write went out unreviewed and
why. The note reaches a headless session too:

| Outcome | What happened |
| --- | --- |
| `timeout` | the review did not answer within its deadline |
| `error` | the review failed: a model error, a handler exception, or a dry run that failed |
| `no-verdict` | the model answered with no verdict the gate could read |
| `halted` | three calls in a row reached no verdict, and the gate is paused for the session |
| `skipped` | the session's main model is on another provider than the gate's |
| `rebuttal` | the agent sent `--advisor-rebuttal <line>`, which is recorded and removed |
| `breaker` | a third attempt on one target after two revise verdicts |
| `unreadable` (warn mode) | the gate could not capture a file the literal call names |
| `abandoned` (warn mode) | the call or the run ended before the review answered |
| `shutdown` (warn mode) | the session shut down before the review answered |
| `unmarked` (warn mode) | the CLI found no marker for a gated write sent in another form |

In `block` mode `unreadable`, `abandoned`, `shutdown` and `unmarked` refuse the call instead. Only
`askgate.enabled: false`, and a session with no Dispatch, are silent. Each outcome after a failure
returns the agent's own command, never the one naming a snapshot: a gate that fails breaks no
valid write.

**The time limit.** `OMP_ASKGATE_TIMEOUT_MS` sets the review's time limit. The default is 90 s. The
gate holds the limit 5 s under `extensionHandlers.toolCallTimeoutMs`, the host's limit for one
`tool_call` handler. That key is 30 s by default, so the limit is then 25 s. A review of a long
session often takes longer than 25 s, and a review that times out lets the write post. Three
timeouts in a row stop the gate for the rest of the session. Set the key to 120000 or more in the
session's settings, for example in `~/.omp/agent/config.yml` or an overlay in `PI_CONFIG_FILES`:

```yaml
extensionHandlers:
  toolCallTimeoutMs: 120000
```

When the limit is under 30 s, the gate logs one warning at start that names the key, the host's
limit and the review's limit. It still reviews each write.

`OMP_ASKGATE_DUMP` names a file that gets each review's two prompts. Every gated call writes one
`advisor-gate` entry into the session's transcript, and `scripts/askgate-report.ts` reports over
those entries: what the gate sent back and what the reviews cost, a count for each outcome, and
every session that had a write the gate did not review, with its counts. The report counts a
write as **sent back** when the gate did not pass it as sent: every entry whose decision is
`revise` — a revise verdict, `no-model-role`, and in block mode `abandoned`, `shutdown` and
`unmarked` — and every `unreadable`, `unmarked` or `load-failed` entry, whatever its mode, counted
over every gated call, since some of them never reach a review; as a **refusal** when the write did
not post (one sent back in block mode, and `no-model-role` and `load-failed`, which refuse in warn
mode too); and as **unreviewed** when it posted with no review behind it, which is every `allow`
whose outcome is no verdict (the table above, a rebuttal, a breaker trip and warn mode's
`abandoned` and `shutdown` included).

**What AskGate is not.** AskGate reviews the writes of a cooperating agent, so that a write that
breaks its rules comes back with the reason before it posts. It is no sandbox. A process that
runs as the same user as the session defeats it: it can replace a snapshot's bytes between the
review and the open (a snapshot is `0600` in a `0700` directory, which keeps out other users, not
this one), set `askgate.enabled` to false in its own envoy.json, or point the session at another
envoy.json with `OMP_PROFILE`, `PI_CODING_AGENT_DIR`, `ENVOY_CONFIG_FILE`, `XDG_CONFIG_HOME` or
`HOME` in the launching shell.

The CLI's check holds only for the CLI. It does not cover a direct HTTP request to Dispatch with
the session's token, an older or a different `dispatch` binary, or a same-user process that
removes the record or a marker file, acts as another session id or another state directory
(`DISPATCH_SESSION_ID`, `DISPATCH_STATE_DIR`, `XDG_STATE_HOME`, `HOME`), or otherwise goes around
the CLI. claude-envoy has no Dispatch write tool over MCP: Claude Code writes through the same CLI
under host `claude`, where AskGate keeps no record, so nothing there asks for a marker. Use the
gate for the writes of an agent that wants the review, and not as a boundary against a program
that wants around it.
