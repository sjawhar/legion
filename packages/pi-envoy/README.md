# Pi Envoy Extension

Tracked Oh My Pi extension for Envoy messaging. It shares the Envoy HTTP client, tool
contract, envelope parsing, and subject helpers with the other Legion adapters while keeping
OMP's direct NATS subscriptions and Pi steering delivery local (inbound messages steer an
in-flight turn instead of queueing behind it).

Normal topic subscriptions are direct NATS subscriptions owned by this extension. A role claim is
different: the listener arbitrates the core-NATS role lane for the current live holder, then sends
a receipt-backed request with the original role topic to the holder's direct agent subject. The
agent pump replies after accepting the envelope; without a receipt within two seconds the listener
emits a `delivery_failed` exception. Role messages are live only; they are not retained for a later
claimant.

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
at `dist/skills` so `resources_discover` serves the Legion skills from the installed
package. The published manifest exposes both `dist/envoy.js` and `dist/legion.js`, while
the committed manifest keeps the TypeScript entries for repo checkouts. The Legion
daemon spawns every session against this one installed package instead of also loading
a repo checkout with OMP's `--extension` flag, so a daemon session ends up with exactly
one instance of each extension.

`.github/workflows/release.yaml` performs that manifest rewrite around `bun pm pack`
and restores the committed file before tagging. Packing with the committed source
manifest is refused by `scripts/prepack.sh`, because such a tarball would point OMP at
extension files it does not contain.

## Native Dispatch tools

The extension registers `dispatch_issue`, `dispatch_ask`, `dispatch_resolve_ask`,
`dispatch_comment`, `dispatch_suggest`, `dispatch_message`, `dispatch_doc_edit`,
`dispatch_doc_read`, `dispatch_artifact`, and `dispatch_read` when Dispatch
configuration resolves both a base URL and bearer token.

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
`<cwd>/.opencode/envoy.json` file shallow-merges over it. `DISPATCH_URL` and
`DISPATCH_TOKEN` override the file values for one process. Omitting
`dispatch.serverUrl` while `dispatch.enabled` is true targets
`http://localhost:8766`, the Go server's listen address. Invalid configuration,
an invalid URL, or an empty token leaves the nine tools unavailable and reports
the source of the error.

Native tools operate on a Dispatch issue: a native `KEY` or an external
`owner/repo#n` reference. A Legion session may omit `issue` when
`LEGION_ISSUE` identifies its root issue and its working directory resolves to a
repository. `dispatch_doc_read` and `dispatch_read` also accept
`dispatch://` references.

Every mutation result carries `details.topic` as
`notifications.dispatch.issue.<KEY>.>`. The extension's `tool_result` hook
subscribes to that exact topic, then registers the session so retained issue
events arrive as Pi steering. `dispatch_doc_read` and `dispatch_read` return
issue details without a subscription topic.

The shared contract supplies the model-facing schemas and descriptions. The
`dispatch` skill describes when to use each operation for issues, asks, review
feedback, documents, artifacts, and status reads.

`dispatch_artifact` accepts exactly one upload source: a local `path`, or inline `content`.
For example, an architect can post a specification directly with
`{ issue, name: "spec.md", content: "# Design" }`.

Lifecycle and scope decisions between Legion roles go through `envoy_publish` to the owning
architect's role topic; Dispatch is for durable questions to the human and the shared
document, not for coordination between roles.
