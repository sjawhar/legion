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
`dist/THIRD_PARTY_NOTICES`, and the four skills every session with Dispatch reads, `dispatch`,
`dispatch-first`, `dispatch-brainstorming` and `envoy`, staged from the repository's `skills/` at
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

AskGate reviews each Dispatch write of a session's main agent before the write posts. The charter
check, one model call, judges the write against the rules in `extensions/askgate/rules.md` and
answers `allow` or `revise`. A `revise` answer carries the reason and the fix. The gate reviews
these calls:

- `dispatch ask`, `dispatch message`, `dispatch edit-ask`, `dispatch comment`, `dispatch doc-edit`
  and `dispatch request-approval`
- `dispatch issue` with `--spec` or `--spec-file`
- `dispatch artifact` when it uploads a markdown document: inline text (`--content`,
  `--content-file`), a `--path` file whose name ends in `.md` or `.markdown`, or a `--name` that
  ends in `.md`

The gate reads the arguments from the command's own `--dry-run`. An upload that reads its name or
path from a file (`--name-file`, `--path-file`) is decided from the name and path the dry run
prints. An upload whose name or path holds an expansion (`"$NAME"`) is refused, as any write the
gate cannot read is. For an upload given with `--path`, the gate reads that file from the directory
the `dispatch` command resolves it in and reviews its text, as for `--content-file`. The gate
judges an upload against content failure 10 alone: a plan or a report is long by nature. An upload
of any other file is not reviewed. A `task` subagent is not reviewed.

**The command forms.** The gate reads a write that stands alone, and a write with these things
around it:

- `\`-newline continuations, and `2>&1`, `>/dev/null` or `2>` to a file on the write
- a later stage of its pipeline that holds no `dispatch` word (`| head -3`, `| tail -1`)
- literal `NAME=value` words and one `timeout <duration>` before its program, and a file as its
  stdin (`< q.md`)
- before it, joined by `&&`, `;` or a newline: `cd <path>`, `export NAME=value`, read-only
  commands (`wc`, `cat`, `ls`, `grep`, `head`, `[` …) and `dispatch` calls that do not write
  (`claim`, `read`, `issue-update` …)
- after it: any command that holds no second write
- `$NAME` in the write when an earlier command of the call sets NAME to a literal, and a `~/`
  path to `dispatch`

The dry run keeps the `cd`, the assignments, the prefixes and the stdin, and leaves out the rest.
The gate refuses, before anything in the call runs, a write after a command that can change a file
or the text (`python3`, `sed -i`, `cat > f`, `make`), with a command or process substitution, sent
to the background with `&`, beside a second write, or with a variable the call does not set. The
refusal tells the agent to send the write again on its own.

**The switch.** AskGate runs in each session that has Dispatch on, unless the session's envoy.json
turns it off. Write this top-level section beside `dispatch`:

```json
{ "askgate": { "enabled": false } }
```

No section, or `enabled: true`, keeps it on. A checkout's `.envoy/envoy.json` cannot set it. A
section that does not parse keeps AskGate on, and the start notice names the file.

**The modes.** In `block` mode a write that the gate sends back does not post. In `warn` mode the
write posts, and the reason comes back to the agent as context. A match of the rules check (below)
refuses the write in both modes. `OMP_ASKGATE=warn` or `OMP_ASKGATE=block` sets the mode. AskGate
ignores any other value, `off` included, and the first review shows one notice that names
`askgate.enabled` as the switch. With no such variable, the session's latest `experiments` entry
decides: its `askgate_block` arm off is `warn`. Every other session uses `block`.

**The rules check.** A call that opens a new ask for the person gets a second model call. That
call knows two rules (`extensions/askgate/rule-check.md`) and judges only the new asks:

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

A match refuses the call in `warn` mode and in `block` mode, so no part of the call posts. The
refusal names the matching asks, each with its rule and the words that match. It says what to do
instead and how to send the call anyway. The agent sends the call anyway in one of three ways:

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

A rebuttal sends the call with no review by either check, and the entry records the line. A rules
call that ends in an error, a timeout or an answer that does not parse refuses nothing: the
charter check decides. After three such rules calls in a row, AskGate logs one warning, and a
session with a UI also shows a notice.

**The model.** Both checks ask the session's `askgate` model role (`modelRoles.askgate`). When that
role is not set, they ask the `default` role. A model that answers with an error passes the request
to the next model of that role's `retry.fallbackChains` chain, on the same provider. When neither
role resolves, the gate refuses the write in both modes and names the two roles. A session whose
main model is on another provider than the gate's model is not reviewed: its transcript stays on
that provider.

`OMP_ASKGATE_TIMEOUT_MS` sets the review's time limit (90 s, less than the host's limit for one
`tool_call` handler). `OMP_ASKGATE_DUMP` names a file that gets the two prompts of each model call.

The session's `extensionHandlers.toolCallTimeoutMs` must be at least 17,000 ms. The gate keeps
7 s of that for the two stops its handler still pays once the review's time limit passes — the
aborted call's usage report and the decision record — and it does not bind on less than 10 s for
a verdict. A session set under 17,000 ms gets a gate that does not load, and a gate that did not
load refuses every Dispatch write it would have reviewed.

**The record.** Every gated call writes one `advisor-gate` entry into the session's transcript.
Every entry but an `unreadable` one carries `target`, the call's target in readable form: the tool
name, then each of `issue`, `ask`, `artifact`, `project` and `in_reply_to` that the input carries,
as `dispatch_ask issue=ACME-1`. The third attempt (`outcome: "breaker"`) counts the refusals of
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

`decision` is `revise` on a rules refusal in `warn` mode too. The gate refused a call exactly when
`decision` is `revise` and one of these is true: `verdictMode` is `block`, `outcome` is
`unreadable`, `no-model-role` or `load-failed`, or `rule` is set. To find the refusals that the agent overrode,
join each rules refusal to the next entry with the same `target`. The agent overrode the refusal
when that entry's `outcome` is `rebuttal` or `breaker`. `scripts/askgate-report.ts` reports what
the entries sent back and what they cost, the outcomes and errors of the rules check, and the
rules refusals that the agent overrode.

**The Dispatch record.** The entry above reaches only the machine the session ran on. A session
that counts the rule decisions over a fortnight — a standing session holding a Dispatch token and
nothing else — reads them from Dispatch instead: every gated call whose rules check ran (every
entry with a `ruleOutcome`) and every call the agent sent anyway (a rebuttal or the breaker, whose
record has `ruleOutcome: null` and the refusal's own `target`) are published to a Dispatch project
document of the session's own, `askgate-decisions-<session id>.md`.

- **What it carries.** The time, the session, `source` (`pod` for a Legion role's session, which
  `LEGION_ISSUE` names, and `machine` for every other), the project, `tool`, `target`, `rule`,
  `ruleOutcome`, `ruleError`, `ruleTextMissing`, `ruleAsksCut`, `ruleAsksPartial`, `decision`,
  `outcome`, `verdictMode`, `ruleLatencyMs`, `charterLatencyMs` and the plugin's version. **No ask
  text and no quote leaves the session**, so nothing a person wrote travels with the counts.
- **Which project.** `OMP_ASKGATE_RECORD_PROJECT`, else `LEGION_PROJECT` (a Legion pod has no
  envoy.json, so its environment is the only setting it can carry), else no record at all and no
  Dispatch call.
- **When.** After the decision is computed and before the call returns, with a 5 second stop. It
  never changes the decision and never throws: a write that fails leaves `recordError` on the
  entry and the call goes on. The session's first decision uploads the document; every later one
  appends a fenced JSON line to it.
- **What is lost.** A session killed between a decision and its upload loses that one record, and
  a session that never reached a gated call writes no document. Both show in the reader's counts
  rather than silently.

`scripts/askgate-report.ts --dispatch-project <KEY> --since <RFC 3339> [--json]` reads those
documents and prints the day's refusals by rule, the refusals the agent overrode, the calls
judged on part of their asks and those that judged none, and the rules-call error rate, each split
by `source`, with the documents and lines it could not read counted beside them. The same script's
`--sessions <dir>` mode still reports the entries on disk.
