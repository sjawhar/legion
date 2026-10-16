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
`envoy_unsubscribe` manages only normal topic subscriptions; it does not relinquish a held role.
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
Whole-issue or whole-document subscription is the model's own `envoy_subscribe` of the topic every
write result names (`notifications.dispatch.issue.<KEY>.>` or
`notifications.dispatch.document.<PROJECT>.<SLUG>.>`).

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
