# Pi Envoy Extension

Tracked Oh My Pi extension for Envoy messaging. It shares the Envoy HTTP client, tool
contract, envelope parsing, and subject helpers with the other Legion adapters while keeping
OMP's direct NATS subscriptions and Pi steering delivery local (inbound messages steer an
in-flight turn instead of queueing behind it).

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

This package declares two OMP extension entries in `package.json`: `extensions/envoy.ts`
(Envoy messaging, subscriptions, and steering delivery) and `extensions/legion.ts` (the
Legion lifecycle: root and phase-worker bootstrap from the daemon's environment, and
daemon capabilities). The Legion daemon spawns every root and phase-worker process with
the installed plugin already active, so `extensions/legion.ts` loads for them automatically;
it is inert without `LEGION_TREE`/`LEGION_ROLE`/`LEGION_CONTROLLER` in the environment.

For local development of the messaging extension alone, link the entry into OMP:

```sh
ln -sfn "$PWD/packages/pi-envoy/extensions/envoy.ts" \
  ~/.omp/agent/extensions/envoy.ts
```

The repository root `package.json` likewise loads only `extensions/envoy.ts` for dev
sessions inside this repo, since `legion.ts` needs nothing from a repo checkout beyond
what the installed package already ships.

## Published package

Released installs come from npm as `@sjawhar/pi-legion-envoy`. The tarball is
self-contained: it ships `dist/envoy.js` and `dist/legion.js` — bundling every
dependency except the OMP host package — and the repo `skills/` tree staged beside it
at `dist/skills`. The manifest declares `omp.skills: ["dist/skills"]` alongside
`omp.extensions`: OMP's own plugin discovery only reads `skill://`-resolvable skills from
`<plugin root>/skills` or from directories a manifest's `omp.skills` array names, and this
package ships skills at `dist/skills` (not the plugin root itself), so the field is required
for every session — including a headless Legion controller — to see the Legion skills at all.
The tarball also ships `agents/`, the task agents Legion's prompts dispatch (`oracle`; the
reviewer's pair `thermonuclear-deep-review` and `thermonuclear-code-quality`; `deep-worker`,
which writes the implementer's code; and the planner's checks, `plan-gap-analyst` before it drafts,
which also reads a spec before a human does, and `plan-reviewer` after), which Oh My Pi discovers
under any extension package root: an installed plugin in a pane, the explicit `--extension` root in
a Sandbox pod. Each declares its model as a role the operator maps (`docs/kubernetes.md`,
"Model roles"). The skills those agents and the
role prompts load (the pair's rubrics, the implementer's `ce-simplify-code`) ship in
`dist/skills` with the rest. The Go daemon's boot gate and `legion probe-image` resolve every
`task(agent="…")` and every `skill://<name>` those prompts name through the same launch, and
refuse by name an agent or a skill Oh My Pi cannot find.
`resources_discover` additionally serves the same directory as a secondary, extension-owned
discovery path. The published manifest exposes `dist/envoy.js` and `dist/legion.js` for
`omp.extensions` (only `omp.skills` ships as committed, since `dist/skills` is its path in both
a repo checkout post-`prepack.sh` and the published tarball), while the committed manifest keeps
the TypeScript entries for repo checkouts. The Legion daemon spawns every session against this
one installed package instead of also loading a repo checkout with OMP's `--extension` flag, so
a daemon session ends up with exactly one instance of each extension.

`.github/workflows/release.yaml` performs that manifest rewrite around `bun pm pack`
and restores the committed file before tagging. Packing with the committed source
manifest is refused by `scripts/prepack.sh`, because such a tarball would point OMP at
extension files it does not contain.

## The `dispatch` command

The extension registers no Dispatch tool. Agents reach Dispatch through the `dispatch` command in
their shell: `dispatch --help` lists its commands, `dispatch <command> --help` each one's flags and
an example, and the `dispatch` skill teaches when to use each. The package ships `bin/dispatch`, a
shim that runs the bundled `dist/dispatch.js` (built from `@legion/envoy-client`'s
`bin/dispatch.ts`) with `bun`. At load the extension puts that `bin/` first on `PATH` and sets
`DISPATCH_HOST=omp`; each session start, switch and branch sets `DISPATCH_SESSION_ID` to the
session's id, so a command acts as the session whose shell ran it.

Configure the shared `envoy.json` with:

```json
{
  "dispatch": {
    "enabled": true,
    "serverUrl": "https://dispatch.example",
    "token": "<agent-bearer-token>"
  }
}
```

The user file is `~/.config/opencode/envoy.json`; a
`<cwd>/.opencode/envoy.json` file shallow-merges over it. A repository
`dispatch.serverUrl` requires `dispatch.token` in that same repository file:
it never combines with a user-file or process-environment credential. To
override a repository endpoint for one process, set `DISPATCH_URL` together
with `DISPATCH_TOKEN` or `DISPATCH_TOKEN_FILE`. `DISPATCH_TOKEN_FILE` (a path
whose trimmed contents are the token — how the Legion daemon delivers it to a
pane) wins over every other token source and never falls back when unreadable.
Omitting `dispatch.serverUrl` while `dispatch.enabled` is true targets
`http://localhost:8766`, the Go server's listen address. Invalid configuration,
an invalid URL, or an empty token makes every `dispatch` command exit 2 naming the source of the
error, and the extension reports it when the session starts.

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
