# Pi Envoy Extension

`@sjawhar/pi-envoy` is the Oh My Pi extension for Envoy messaging and the `dispatch` command,
the plugin every session loads. It shares the Envoy HTTP client, tool contract, envelope parsing,
and subject helpers with the other Legion adapters while keeping OMP's direct NATS subscriptions
and Pi delivery local (an inbound message reaches an in-flight turn at its next step instead of
waiting for it to end). The Legion lifecycle — the entry a Legion pane boots a root architect, a
phase worker or the controller with — is the separate plugin `@sjawhar/pi-legion` (`packages/pi-legion`), installed
beside this one in a Legion deployment; it reaches this plugin through the in-process interface
described in `packages/pi-shared/AGENTS.md`.

Normal topic subscriptions are direct NATS subscriptions owned by this extension. A role claim is
different: the listener arbitrates the core-NATS role lane for the current live holder, then sends
a receipt-backed request with the original role topic to the holder's direct agent subject.
`envoy_unsubscribe` manages only normal topic subscriptions; it does not relinquish a held role,
and it keeps open, listing them as kept, the subscriptions another plugin holds for that role.
The agent pump replies after accepting the envelope. A message sent through the listener's
`POST /v1/messages/publish` waits for that reply: the listener forwards it again after two seconds
without one, and answers the sender with an error once four seconds pass without a reply. A role
topic published straight onto NATS is forwarded once, and a miss is a `receipt_timeout` exception.
The same route forwards a team or other ad-hoc topic's message to each session registered on the
topic that holds its own NATS connection, as this one does, other than its sender, and the sender's
error names each session that did not reply. The stream stores each forward on the recipient's
agent subject, and when the listener supports consumer delivery that session takes the stored copy
once, also after a restart or a dropped connection (below). A role message is not kept for a later
claimant, and on the core path a session does not recover one it missed.

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

The session's messages come from a durable consumer the listener keeps for it on Envoy's
notification stream and relays on `notifications.envoy.deliver.<session id>`, which
the session subscribes before it registers: its own agent subject, the stored copies of role
messages to a role it holds, and every followed topic the stream stores, in stream order. The
session answers each frame `received` as it decodes and `done` once Oh My Pi wrote the card or
turn to the session file, so a frame not written when the session stops, or published while it
was down or disconnected, comes again and is shown once, for up to 72 hours. Team and other
ad-hoc topics, which the stream does not store, stay live only on the session's own
subscription. "Consumer delivery" in `AGENTS.md` has the rules, and
`packages/envoy/AGENTS.md`, "Session consumers", states them once for every host.

The core path is the rollout fallback dispatch://INFRA-771 removes, for a listener that answers
`core` and for a plugin of an earlier release. There a message
published while the session's NATS connection or one of its subscriptions was down is
recovered from Envoy's notification stream, which keeps messages for 72 hours: the session asks
its listener (`POST /v1/replay`) for what the stream stored on its own agent subject and on each
topic it follows, after the place it last read to, and shows each missed message once, as a card
saying it was published while the connection was down. The session pings NATS every 10 seconds
and drops a connection that leaves two pings unanswered, so a connection that stops answering
without closing is left within 30 seconds and redialed. It asks after it reconnects, after a
subscription is reopened, when it starts or resumes, and on every heartbeat. A targeted Dispatch
delivery recovered this way (BTW, Aside, Steer, a person's Send) is shown as that card only and
runs nothing. Role messages are live only and are not recovered; a team or other ad-hoc topic is
recovered only from the copy the listener forwarded to the session's agent subject while it was
registered on the topic. Nor is anything older than 72 hours, published before the session
followed the topic, or published before the session first ran this plugin. A message the session
fails to show three times is replaced by one card naming it and saying how to read it. The
session file keeps what it was shown and where it read to, so a restart skips nothing the stream
still holds and shows nothing again (up to 20,000 messages shown in 72 hours); a crash between
showing a message and recording it can show it twice, never lose it. Against a listener without
the route, the session warns once that missed messages are not recovered, and live delivery goes
on as before. "Missed messages" in `AGENTS.md` has the full rules.

## Session identity

Run `/whoami` to copy the active session ID to the clipboard. OMP copies through its host
clipboard API, which sends OSC 52
first for tmux and SSH sessions. The notification shows the session ID even if the copy fails.

For tmux to accept OSC 52 clipboard writes, enable clipboard support in the tmux server:

```tmux
set -g set-clipboard on
```

## What every model request carries

A session with Dispatch configured — a top-level session, a Legion pane or a `task` subagent —
sends two extra user messages with every model request of its conversation. At the head, after
any compaction summary, it sends the `dispatch-first` skill, whose text never changes. At the
tail, after the request's last message, it sends what people wrote to the session in the last 72
hours that Dispatch delivered to it: answers to its asks, replies on them and in its comment
threads, comments on its documents and routed issues, mentions, and messages, newest first and
word for word. A subagent reads its parent's list. The list stays under 8,000 characters (at most 7,999): the oldest
items that do not fit are left out, and its last line says how many and that `dispatch answers`
prints them all. It is read on every request and waits at most 800 ms for Dispatch. Past that,
or when the read fails, the request carries the last list read, with the time it was read and why
this read failed, or, when no list read has an item from the last 72 hours, one line saying the
list could not be read and why. A Dispatch build older than the answers route answers it with its
own `404 NOT_FOUND` "no route for …". Against that Dispatch a request carries no such line, since
`dispatch answers` cannot read it either; a list read before from it is still carried, its
left-out line naming no command; the log says once per session that the Dispatch has no answers
route; and that Dispatch is not asked again for 5 minutes. Any other answer, a proxy's 404 page or
a 200 that is no answers list included, is a failed read. The list sits at the tail so that a
change in it costs only its own tokens: the cached conversation before it is sent byte for byte as
before.

Oh My Pi also builds requests that are not turns of the conversation from it; those carry the
skill but no list, and wait on no read: a side turn (a Dispatch BTW, the stop-time self-check,
`/btw`), whose last message stays its question; the handoff, whose last message stays the handoff
prompt; and a remote compaction's request, which ends where the compaction cuts the conversation.

## Development install

This package declares one OMP extension entry in `package.json`, `extensions/envoy.ts`: Envoy
messaging, subscriptions, inbound delivery, and the Dispatch tools. For local development, link
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
until 7.x) uninstalls it and installs this one once; the `CHANGELOG.md` entry that renames the
package `@sjawhar/pi-envoy` gives the two commands. A Legion deployment installs
`@sjawhar/pi-legion` as well (`packages/pi-legion/README.md`); the daemon's boot gate refuses a
pane that still loads the pre-split package beside the new ones.

The tarball is self-contained: it ships `dist/envoy.js` — bundling every dependency except the OMP
host packages (`@legion/pi-shared`, `@legion/contracts` and `@legion/envoy-client` are inlined) —
`dist/THIRD_PARTY_NOTICES`, and the five skills every session with Dispatch reads, `asd-ste100`,
`dispatch`, `dispatch-first`, `dispatch-writing-specs` and `envoy`, staged from the repository's `skills/` at
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
nothing is restored); the `publish` job publishes that tarball and commits the version bump to the
committed manifest, touching `.version` alone.
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
`envoy: no envoy.json at <path>: Dispatch and Envoy are off`, the lost `DISPATCH_URL` when
`DISPATCH_TOKEN_FILE` is set and no server is named (`DISPATCH_TOKEN_FILE is set and DISPATCH_URL
is not, so Dispatch is off: …`, which every `dispatch` command also prints), the section's error,
what a checkout's file holds that is not read, or that inbound messages are off and why. Invalid configuration, an
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

AskGate reviews each Dispatch write of a session's main agent before the write posts. The charter
check, one model call, judges the write against the rules in `extensions/askgate/rules.md` and
answers `allow` or `revise`. A `revise` answer carries the reason and the fix. A call that opens a
new ask for a person also gets the rules check (below), a second model call beside it. The gate
reviews these calls:

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
its content names a file the gate does not read before Dispatch opens it. An upload whose name or
path holds an expansion (`"$NAME"`) is outside the form the gate reads. The gate reads each file
whose text the write posts, a `--<field>-file` operand and an upload's `--path`, in place, shows
those bytes to the model, and runs the call as written. The marker it mints binds the sha256 of
each file. Before the write posts, the CLI reads each file again and refuses one whose bytes
changed after the review, and an upload sends the bytes the CLI read for that check. An `--image`
file is not one of them: the model reads the path, not the picture. The file must be a regular
file. The gate refuses a missing file, a directory, a device and a FIFO before it reads them, and
a path that reaches a file descriptor of whichever process opens it (`/dev/stdin`, `/dev/fd/N`,
`/proc/self/fd/N`, `/proc/thread-self/fd/N`, `/proc/<n>/fd/N`, and any link to one): AskGate
would open it in Oh My Pi's process and the command in its shell. For text from standard input,
use `--<field>-file -` with a quoted here-document. For an upload, that is `--content-file -`:
the CLI reads `--path -` as a file named `-`. The gate cuts all arguments at 64 KiB as one JSON
block, so it reviews only part of a long document. It judges an upload against content failure
10 alone: a plan or a report is long by nature. An upload of any other file is not reviewed. A
`task` subagent is not reviewed.

**A file a call of the same message writes.** Oh My Pi runs the gate for every call of a message
before any call of that message runs. So the gate reads a file before an earlier call of the same
message writes or edits it. A file that is not there yet is refused in `block` mode with the
advice to send the call again in a later message; in `warn` mode the call runs unreviewed, with
that advice as its note. A file that an earlier call changed is refused by the CLI in `block` mode
(`… AskGate reviews the calls of a message before any of them runs, so it did not review that
change. Send the call again in a later message.`); in `warn` mode it posts with a
`dispatch: warning:` line. Write or edit a file in one message, and send the write that posts it
in the next.

**The form the gate reads.** One `dispatch` call, alone in the bash call:

```
[cd <literal path> && ] dispatch <subcommand> <literal words…> [2>&1] [<<'DELIM' … DELIM] [| head|tail [-n N]] [# --advisor-rebuttal '<line>']
```

- The program is `dispatch` by name, or a literal path that ends in it.
- Every word is literal. Outside quotes a word holds only letters, digits and `._/+@%:,=-`, so no
  `$`, backtick, `~`, `*`, `?`, `[`, `{`, `}` or backslash. Single quotes make every character
  literal. Inside double quotes there is no `$`, backtick or backslash. A quoted word may run over
  several lines.
- A backslash at the end of a line continues the command, as bash reads it.
- One `cd <literal path> &&` may stand before it, and one `2>&1` after it.
- One here-document may feed it, with its delimiter in quotes (`<<'EOF'` or `<<"EOF"`), before or
  after the `2>&1`. Its body is on the lines after the line that opens it.
- One trailing pipe stage may read its output: `head` or `tail`, alone or with `-n N`, `-nN` or
  `-N`. The dry run leaves it out.
- One shell comment may stand where the command's line ends — after its last word and after every
  `2>&1`, here-document opener and `head` or `tail` stage, which is where bash reads one — and
  only when its words are override flags alone (`# --advisor-rebuttal '<line>'`, the form the gate
  hands a rebutted command back in). It is read as those overrides, and no part of what runs:
  `commandArgv` never holds it, so the marker binds the words bash hands the CLI.
- Nothing else is in the call: no second command, no other pipe, no `;`, `&&`, `||` or `&`, no
  assignment, no other redirect and no other comment.

**Everything else runs, and the `dispatch` command enforces the review.** The gate reads no other
form and refuses none: a call outside the literal grammar runs as written. What holds it back is
the CLI. For each literal call it lets run, the gate mints a single-use marker; the CLI posts a
gated write only when it consumes one, so a write sent any other way is refused in `block` mode:

- **The record.** While the gate is bound, pi-envoy keeps one owner-only file for its process in
  the `askgate/` directory of the CLI's state directory of each session id it binds
  (`<state>/sessions/<id>/askgate/<pid>-<start>.json`, where `<state>` is `DISPATCH_STATE_DIR`,
  else `$XDG_STATE_HOME/dispatch`, else `~/.local/state/dispatch`). It names the Oh My Pi process,
  its Linux start time, and the process's owner-only marker store. Each process writes and removes
  only its own file, so two processes that bind one session never drop each other. pi-envoy sets
  `DISPATCH_STATE_DIR` in the session's environment when it is not set, so a command that changes
  `HOME` or `XDG_STATE_HOME` still finds the record. A record of a session the process switched
  away from stays until the process exits, since a shell or an eval kernel started before the
  switch still acts as that session. A record whose process is gone, or whose pid another process
  now holds, is inert, and the CLI's session pruning keeps a directory whose record is live. A
  record pi-envoy cannot write is a notice in the session, at start or at a switch.
- **The marker.** The gate mints one file in the store for each literal call it lets run, created
  owner-only and exclusively. It binds the exact argument vector the CLI receives and, behind a
  review or a deliberate pass (a rebuttal, a breaker trip, a halt, another provider), the digest
  of the arguments the dry run printed and the sha256 of each file the argv names, as the gate
  read it. A call no verdict stands behind, and a `task` subagent's literal call, get a marker of
  the argument vector alone. Nothing of it is in the command the agent sees. A marker goes when
  its call ends, is refused or the session shuts down; a backgrounded call keeps its marker until
  shutdown, and a marker older than two hours, longer than any tool's time limit, is refused and
  removed. The process removes its store when it exits; the CLI's daily session pruning removes
  the store of a process that is gone and that nothing changed for two hours.
- **The check.** After `--help`, `--dry-run` and the session and Dispatch settings, and before a
  gated write posts, the CLI reads the session's record. With a live one it reads each file the
  argv names once and consumes one matching marker by unlinking it: exactly one process wins, so a
  marker posts one write and a replay finds none. In `block` mode a write with no matching marker
  is refused (`dispatch <command> was not called: AskGate is on for this session, …`, which names
  the form to send, or, for a file that changed after the review, says to send the call again in a
  later message). In `warn` mode the CLI first records the write in the session's ledger, refuses
  it when the ledger cannot take that line, and then posts it with a `dispatch: warning:` line.
  Either way pi-envoy records an `advisor-gate` entry with outcome `unmarked`. A record that cannot
  be read, or that names a store that is gone, is a refusal. A session no live record names needs
  no marker: a terminal, CI, Claude Code, OpenCode, a pod without pi-envoy, a session with no
  Dispatch and one with `askgate.enabled: false`.
- **The cause note.** When a `bash` call outside the literal form holds a `dispatch` word and a
  gated subcommand, and its result holds the CLI's refusal (or warn-mode warning) of a write with
  no marker, the gate appends an `AskGate note:` to that result. It names the first cause it found
  in the call, with the call's own text (an expansion in a value, a double-quoted value over lines
  with an expansion in it, a program that runs the write such as `xargs` or `timeout`, a pipe, a
  second command, an assignment, a redirect, a comment that is no override), and the change that
  puts the write in the literal form. For a comment it says that AskGate reads a `#` only where
  the command's line ends and only when its words are override flags alone. The note is
  help only: the gate refuses nothing for it.
- **The unmarked write that carries an ask.** The `unmarked` entry reaches only this machine's
  disk. Where such a write carries at least one ask for a person, the CLI counts those asks from
  the write's own text (`newAsks` in `@legion/envoy-client/askgate-new-asks`) and writes the count
  into the session's ledger as `asksCarried`, and pi-envoy publishes one decision record for it on
  Dispatch (below), with the tool, the mode and that count and no ask text. The count is of the
  asks the write carries, so a re-upload of ask blocks the stored version already holds counts
  them as carried: in `warn` mode the write has already posted when the ledger is read, so a read
  of the stored version would see the new text and count nothing.

The gated writes are the same list for the gate and the CLI (`gatedWrite` in
`@legion/envoy-client/askgate-marker`): the six commands above, an `issue` with a spec and an
`artifact` that uploads a markdown document. `--help`, `--dry-run` and every other command need no
marker. So a pipe to a program other than one trailing `head` or `tail`, a loop, a variable,
`xargs`, an alias, a script, `python3 -c`, `node -e`, or a process an eval cell starts runs, and
its gated write is refused by the CLI in `block` mode with the form to send; a call that holds no
Dispatch write, such as a commit message or a search pattern that names one, runs and posts
nothing. A literal call whose last command gives no argument vector is refused in `block` mode. A
gate that did not load refuses each literal call at the hook, so those never reach the CLI; it
writes the record and mints nothing, so every gated write that does reach the CLI — one outside the
literal form, and a subagent's literal one — is refused where the session set `OMP_ASKGATE=block`,
and at the warn default posts with a warning and an `unmarked` entry.

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

**The modes.** A model's revise verdict and the CLI's marker checks have separate defaults. A revise
verdict blocks by default: the write does not post. `OMP_ASKGATE=warn`, or a session whose latest
`experiments` entry turns its `askgate_block` arm off, lets the write post with the reason. The
CLI's checks warn by default: a gated write with no matching marker, a file changed after the
review, and a file missing when the gate ran each post with a warning and an `unmarked` or
`unreadable` entry that the report counts. They refuse only when the session chose block
explicitly: `OMP_ASKGATE=block`, or, with no such variable, an `experiments` entry whose arm is on.
AskGate ignores any other value of `OMP_ASKGATE`, `off` included, and the first review shows one
notice that names `askgate.enabled` as the switch. The gate writes the checks' mode into the
session's own record (not a process-wide file, so a process that binds two sessions keeps each
one's mode, and a switch never changes another session's) at each main-agent `bash` or `eval`
call, where the CLI reads it, and a record with no mode is warn. pi-envoy also writes
`OMP_ASKGATE=block` into the record at the bind, so a session that chose block that way stays at
block even when its gate does not load. A later bind of a session id that recorded block (a switch
back to it, or a branch or host move that keeps the id) keeps block, so its mode never goes back to
warn before the gate's next call. An `experiments` arm alone does not reach the record at the
bind: only the loaded gate reads the arm, so a session that chose block through the arm alone has
no mode on disk until the gate's first main-agent `bash` or `eval` call, and none at all when the
gate does not load — a write outside the literal form there posts with a warning. **An operator
who selects block through the arm should also set `OMP_ASKGATE=block` for a hard guarantee**,
which holds from the session's start and through a failed load. Three refusals follow neither
mode: a match of the rules check (below), which refuses the write in both modes; no model role;
and a gate that did not load, the last two refusing each literal call they would review in both
modes, since neither can review anything.

**The rules check.** A call that opens a new ask for the person gets a second model call, beside
the charter call and on the same chain, deadline and transcript. That call knows two rules
(`extensions/askgate/rule-check.md`) and judges only the new asks:

- No change: the ask's own recommendation is to change nothing, such as to keep things as they
  are, to take no action or to keep the current design.
- Untried access: the ask asks the person to do a step with their own hands (run a command,
  change a setting, grant access, sign in), or says that only the person can do the step. The
  rule matches only when neither the ask nor the transcript shows that the agent tried the step
  with the access it has or can request. A step that needs the person's security key, a sign-in
  to their own account in a browser, a payment or their own decision does not match. An ask that
  also asks the person to choose between options with different outcomes does not match.

A new ask is a `dispatch ask`, an ask block that a `dispatch doc-edit` inserts, a block that it
retypes into an ask, an ask block in a `dispatch issue` spec, or an ask block in a markdown
upload whose `#id` the document does not already hold. When that document cannot be read, an
upload's ask blocks that carry an `#id` are left out — a stored version may hold any of them —
and only the blocks with no id are judged, so a re-upload of an unchanged document is never
refused for an ask block it already held.

The rules check runs only on a call the gate reads, which is the literal form above. A call in
any other form reaches no gate at all: the CLI refuses it in `block` mode and posts it with a
warning in `warn` mode, and the rules check never sees it. Where such a write carries an ask, the
Dispatch record below counts it.

A match refuses the call in `warn` mode and in `block` mode, so no part of the call posts and no
marker is minted for it. The refusal names the matching asks, each with its rule and the words
that match. It says what to do instead and how to send the call anyway. The agent sends the call
anyway in one of three ways:

- `--advisor-rebuttal '<line>'` on a `dispatch` command.
- `"advisor_rebuttal": "<line>"` in a device's JSON.
- The third attempt at the same thing on the same target: after the gate refuses one thing twice
  on one target, it sends the third call that holds it without review. A thing is each new ask a
  call opens, or the call as a whole when it opens none. A charter refusal counts on every thing
  of the call, and a rules refusal counts on the asks it refused. Only a refusal counts: in `warn`
  mode a charter `revise` sends the call, so it never moves the breaker, while a match of the
  rules check refuses the call in either mode and always does. A call that opens an ask refused
  once, or never, on that target is reviewed like any other, however often the gate refused other
  asks there, and a call that opens no ask counts its own refusals, whatever asks were refused
  there before it. An allow, a rebuttal or the third attempt starts every count on the target
  again.

A rebuttal sends the call with no review by either check, and the entry records the line. The
CLI takes no such flag, so the gate moves it into a shell comment where the command's line ends,
after everything that runs, where the agent's record of its call still shows it and dispatch never
sees it; that command sent again is read as the same rebuttal and comes back unchanged. The
comment stands at the end of the command's line because that is the one place bash reads it as a
comment at all: every word of it is quoted, which keeps the comment's own quotes paired, but
nowhere else would that quoting hold — written where a single-quoted value is still open, the
quotes would pair around the agent's line and leave it outside them. The gate reads the commented
command back as the same call before it hands it over: an override no
word of the grammar can carry — a line holding both a `'` and a `$`, or a newline — is removed
instead, and the acknowledgement says so. A device takes no such field, so the gate removes it
before the call runs and says so. Either way the acknowledgement
quotes the override exactly as the agent sent it. A rules call that ends in an error, a timeout or
an answer that does not parse refuses nothing: the charter check decides. The third such rules
call in a row writes one warning, saying that the new asks of those three calls went unchecked and
that the check still runs on each new ask, to the host's plain log, shows it as a notice in a
session with a UI, and adds it to that call's own result. It is not one of the notices a session
shows at its start.

**The model.** Both checks ask the session's `askgate` model role (`modelRoles.askgate`). When that
role is not set, they ask the `default` role. A model that answers with an error passes the request
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
| `rebuttal` | the agent sent `--advisor-rebuttal <line>`, which is recorded and kept only as a shell comment, or `"advisor_rebuttal"`, which is recorded and removed |
| `breaker` | the third attempt at one thing on one target, after the gate refused it twice there |
| `unreadable` (warn mode) | a file the literal call names was missing or not a regular file when the gate ran |
| `abandoned` (warn mode) | the call or the run ended before the review answered |
| `shutdown` (warn mode) | the session shut down before the review answered |
| `unmarked` (warn mode) | the CLI found no marker for a gated write sent in another form |

Where the mode that applies is `block` (the verdict's for `abandoned` and `shutdown`, the checks'
for `unreadable` and `unmarked`), those outcomes refuse the call instead. Only
`askgate.enabled: false`, and a session with no Dispatch, are silent. Each outcome after a failure
runs the agent's own command as written: a gate that fails breaks no valid write.

**The time limit.** `OMP_ASKGATE_TIMEOUT_MS` sets the review's time limit. The default is 90 s. The
gate holds the limit 5,000 ms under `extensionHandlers.toolCallTimeoutMs`, the host's limit for
one `tool_call` handler: the room its handler still needs once the limit passes, for the aborted
call's usage report and for the entry. The Dispatch record below is no part of it, since it runs
after the handler has returned. That key is 30 s by
default, so the limit is then 25,000 ms. A review of a long session can take longer than 25 s,
and a review that times out lets the write post. Three timeouts in a row stop the gate for the
rest of the session. Set the key to 120000 or more in the session's settings, for example in
`~/.omp/agent/config.yml` or an overlay in `PI_CONFIG_FILES`:

```yaml
extensionHandlers:
  toolCallTimeoutMs: 120000
```

When the limit is under 30 s, the gate logs one warning at start that names the key, the host's
limit and the review's limit. It still reviews each write. The key must be at least 15,000 ms,
since the gate does not bind on a limit that leaves under 10 s for a verdict. A session set under
15,000 ms gets a gate that does not load, and a gate that did not load refuses every Dispatch
write it would have reviewed.

`OMP_ASKGATE_DUMP` names a file that gets the two prompts of each model call.

**The record.** Every gated call writes one `advisor-gate` entry into the session's transcript.
Every entry whose arguments the gate read carries `target`, the call's target in readable form:
the tool name, then each of `issue`, `ask`, `artifact`, `project` and `in_reply_to` that the input
carries, as `dispatch_ask issue=ACME-1`. An `unreadable` entry, and the `unmarked` entry pi-envoy
writes from the CLI's own ledger line, carry none: neither has the arguments to name one. The
third attempt (`outcome: "breaker"`) counts the refusals of
one thing on one such target, and `revisesForKey` counts every `revise` on it, the `warn` ones
included, since its counts last started again. A call that opens a new ask adds the fields of the
rules check:

- `rule`: `no_change` or `untried_access`, the rule that the first matching ask broke. The gate
  sets it exactly when the rules check refused the call.
- `ruleAsk`: the number of that ask in the rules request, from 1.
- `ruleQuote`: the words of that ask that the rules call quoted.
- `ruleOutcome`: how the rules check ended: `match`, `none`, `error`, `timeout`, `no-verdict`,
  `abandoned`, `shutdown` or `unjudged`. The gate sets it on each gated call that opens a new ask
  and reached the model call. `unjudged` is the one with no model call: an upload whose stored
  version the gate could not read and whose every ask block carries an `#id`, so it judged none
  of the call's asks. The call is not refused for it; the charter check decides alone.
- `ruleModel`: the `provider/id` that answered the rules call.
- `ruleLatencyMs`: the time of the rules check, its document read included. `charterLatencyMs` is
  the charter check's own time on the same clock; the two calls run side by side, so each says
  what it cost of the entry's whole `latencyMs`.
- `ruleTextMissing`: `true` when the gate could not read the text of an ask and judged the ask
  without it. A failed read of an upload's stored version never sets it: no ask of an upload
  loses its own text.
- `ruleError`: the error text for `error` (the provider's, or the check's own when it fails before
  a model answers), `timeout` for a timeout, and `no-verdict` for an answer that does not parse.
- `ruleAsksCut`: how many asks the request cut. The request lists every new ask, and aims to keep
  the list within 32 KiB: each ask's text gets an equal share of it, at most 8 KiB, and an ask cut
  to its share keeps its start and its end, where the rules read. The gate never leaves an ask out,
  or cuts one below its start and its end, to stay within that bound: a write of very many ask
  blocks, over a hundred, makes a longer request. An ask's `#id` is cut to 128 bytes.
- `ruleAsksPartial`: `true` when the call was judged on part of its asks, or on none of them
  (`ruleOutcome: "unjudged"`): the gate could not read the document an upload replaces, so the ask
  blocks carrying an `#id` were left out of the check.
- `mintSkipped`: `call-ended`, on a call whose decision let the write out and whose own tool ended
  while the gate still held it. No marker was minted, since no command of that call can run.

`decision` is `revise` on a rules refusal in `warn` mode too. The gate refused a call exactly when
`decision` is `revise` and one of these is true: `verdictMode` is `block`, `outcome` is
`no-model-role` or `load-failed`, or `rule` is set. An `unreadable` or `unmarked` entry is a
refusal in `block` mode only, where its `decision` is `revise`; in `warn` mode its `decision` is
`allow`, and the write went out with the note that it was unreviewed. To find the refusals that
the agent overrode, join each rules refusal to the next entry with the same `target`. The agent
overrode the refusal when that entry's `outcome` is `rebuttal` or `breaker`.

`scripts/askgate-report.ts` reports over those entries: what the gate sent back and what the
reviews cost, a count for each outcome, every session that had a write the gate did not review,
with its counts, the markers it skipped, the outcomes and errors of the rules check, and the rules
refusals that the agent overrode. The report counts a write as **sent back** when the gate did not
pass it as sent: every entry whose decision is `revise` — a revise verdict, a rules refusal,
`no-model-role`, and in block mode `abandoned`, `shutdown` and `unmarked` — and every
`unreadable`, `unmarked` or `load-failed` entry, whatever its mode, counted over every gated call,
since some of them never reach a review; as a **refusal** when the write did not post (one sent
back in block mode, and `no-model-role`, `load-failed` and a rules refusal, which refuse in warn
mode too); and as **unreviewed** when it posted with no review behind it, which is every `allow`
whose outcome is no verdict (the table above, a rebuttal, a breaker trip and warn mode's
`unreadable`, `unmarked`, `abandoned` and `shutdown` included).

**The Dispatch record.** The entry above reaches only the machine the session ran on. A session
that counts the rule decisions over a fortnight — a standing session holding a Dispatch token and
nothing else — reads them from Dispatch instead: every gated call whose rules check ran (every
entry with a `ruleOutcome`), every call the agent sent anyway (a rebuttal or the breaker, whose
record has `ruleOutcome: null` and the refusal's own `target`), and every write that reached the
CLI outside the form the gate reads and carried an ask for a person (`outcome: "unmarked"`, with
`asksCarried` above zero) are published to a Dispatch project document of the session's own,
`askgate-decisions-<session id>.md`.

- **What it carries.** The time, the session, `id` (the gated call's tool call id, and for an
  unmarked write the CLI ledger line's place in its tool call, so a reader counts one decision
  once), `source` (`pod` for a Legion role's session, which `LEGION_ISSUE` names, and `machine`
  for every other), the project, `tool`, `target`, `rule`, `ruleOutcome`, `ruleError`,
  `ruleTextMissing`, `ruleAsksCut`, `ruleAsksPartial`, `decision`, `outcome`, `verdictMode`,
  `asksCarried`, `ruleLatencyMs`, `charterLatencyMs` and the plugin's version. **No ask text and
  no quote leaves the session**, so nothing a person wrote travels with the counts.
- **Which project.** `OMP_ASKGATE_RECORD_PROJECT`, else `LEGION_PROJECT` (a Legion pod has no
  envoy.json, so its environment is the only setting it can carry), else no record at all and no
  Dispatch call.
- **When.** Once the call has been answered: the gate appends the call's `advisor-gate` entry,
  returns, and then hands the record to the session's recorder, so no review is ever cut short to
  pay for it. It never changes the decision. The write runs until Dispatch has answered it or the
  session's shutdown cuts it — the client asks again under the call's own `Idempotency-Key`, and
  nothing retries it from outside — so a Dispatch that is slow under load is waited
  out rather than given up. A record that fails or that the shutdown cuts writes one line to the
  host's log and appends one `advisor-gate-record` entry, carrying `toolCallId`, `tool`,
  `recordError` and, for an unmarked write, the record's own `recordId`; the decision's own entry
  stands as written. Once the session is shutting down, the one `session_shutdown` handler the
  gate binds gives the records 1.25 s from that moment and then cuts them, whether or not the
  gate loaded, and the gate's own handler waits for what is still pending inside the 1.5 s it
  holds shutdown for, so a record still lands, and the entry of one that could not, before the
  session is sealed; a record cut that way says the shutdown cut it. An unmarked write's record is
  started from the `tool_result` the CLI's line reached, after that write's `advisor-gate` entry
  is appended and awaited by nothing of the tool result, which returns while the record is still
  in flight: the records of a session are written one at a time and each has the client's own
  deadline, so a Dispatch that does not answer would otherwise stall every such write. It goes
  through the one recorder the gate's own records use.
- **One document per session, written once and appended to.** The document's create carries a
  header paragraph and no record; every record, the session's first included, is a fenced JSON
  line appended through `docEdit`. That rule is what keeps a record safe, because an upload of a
  name Dispatch already holds stores a new version of that document and the reader reads the
  latest version alone, so a second upload would erase every record appended since the first. A
  process holding no id for the session — a fresh process serving it, or one whose create was cut
  — reads the document by its name, one request of a few bytes, and appends to that; it creates
  one only where that read answers 404. The create carries an `Idempotency-Key` of the document's
  own, `askgate-doc.` and the sha256 of the project and the name, and a body fixed for the
  session — the header alone, with no record, no time and no plugin version in it — so every
  attempt of it, in this process or another, is one intent Dispatch runs once: it replays a key
  that finished, answers `409 KEY_IN_FLIGHT` while the first run lives, `409 KEY_COMMITTED` where
  that run landed and lost its answer, and `422 KEY_REUSED` for a different body, and the last two
  send the recorder back to the read and then to the append. Each append carries a key of its own,
  the sha256 of the session and the record's id, so an append Dispatch ran late is replayed to its
  next attempt rather than written twice; the same record sent again with another time in its body
  gets `422 KEY_REUSED`, is not written, and is counted as not confirmed. Keys
  belong to the login that sent them, and every Legion pod writes under the shared agent token,
  which is one login for all of them (`requestKeyLogin`,
  `packages/envoy/internal/dispatch/api/request_keys.go`); Dispatch keeps a key 24 h
  (`IdempotencyKeyTTL`), well past the 10 minutes a create's run can hold it
  (`keyedHandlerLimit`). The records of one session are written one at a time, each sent once the
  one before it has settled, and each carries the time the gate made it, which is what the reader
  counts and sorts by: a write given up on this side may still be running on Dispatch's while the
  next is sent, so the order they land in is not the order they were made.
- **What is lost, and what is counted.** A session killed between a decision and its write loses
  that one record, and a session that never reached a gated call writes no document. A record
  that fails, or that the shutdown cuts, is unconfirmed too — and one given up that way may have
  landed all the same, so a record the session could not confirm can still be in the document,
  where the reader counts it once by its `id` and counts the second copy as a duplicate. What no
  longer happens: a create given up here and committed on Dispatch after later records were
  written cannot replace them, since every create of that document is the one keyed create, and
  two processes writing one session's first record at the same moment make that same one create
  between them. What the
  machine could not get Dispatch to confirm is counted on the machine's own side: `--sessions`
  counts the entries carrying a `recordError` as **decision records not confirmed**, an upper
  bound on what the day's `--dispatch-project` counts cannot hold.

`scripts/askgate-report.ts --dispatch-project <KEY> --since <RFC 3339> [--json]` reads those
documents and prints the day's refusals by rule, the refusals the agent overrode, the calls
judged on part of their asks and those that judged none, the unmarked writes by mode with the asks
they carried, and the rules-call error rate, each split by `source`, with the documents and lines
it could not read, and the records it read twice, counted beside them. An unmarked write is in no
rules-call count and in no override count: no check judged it. Its `asksCarried` counts every ask
and ask block the write carries, so a re-upload of blocks a stored version already holds counts
them as carried. The same script's `--sessions <dir>` mode still reports the entries on disk, the
decision records not confirmed among them. Each flag is given once: the
script refuses a flag given twice by name, rather than reading only its last value.

**What AskGate is not.** AskGate reviews the writes of a cooperating agent, so that a write that
breaks its rules comes back with the reason before it posts. It is no sandbox. A process that
runs as the same user as the session defeats it: it can set `askgate.enabled` to false in its own
envoy.json, or point the session at another envoy.json with `OMP_PROFILE`, `PI_CODING_AGENT_DIR`,
`ENVOY_CONFIG_FILE`, `XDG_CONFIG_HOME` or `HOME` in the launching shell.

The CLI's check holds only for the CLI. It does not cover a direct HTTP request to Dispatch with
the session's token, an older or a different `dispatch` binary, or a same-user process that
removes the record or a marker file, sets `DISPATCH_STATE_DIR` or `DISPATCH_SESSION_ID` itself to
act as another state directory or session id, or otherwise goes around the CLI. claude-envoy has
no Dispatch write tool over MCP: Claude Code writes through the same CLI
under host `claude`, where AskGate keeps no record, so nothing there asks for a marker. Use the
gate for the writes of an agent that wants the review, and not as a boundary against a program
that wants around it.
