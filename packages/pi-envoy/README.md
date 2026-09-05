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
Legion lifecycle: root bootstrap, worker spawning, budgets, and daemon capabilities).
Loading the package directory with OMP's `--extension` flag — as the Legion daemon does
when it launches trees and workers — loads both.

For local development of the messaging extension alone, link the entry into OMP:

```sh
ln -sfn "$PWD/packages/pi-envoy/extensions/envoy.ts" \
  ~/.omp/agent/extensions/envoy.ts
```

The repository root `package.json` likewise loads only `extensions/envoy.ts` for dev
sessions inside this repo: the Legion extension is meant to be loaded by the daemon with
its environment prepared, not by ambient dev sessions.

## Published package

Released installs come from npm as `@sjawhar/pi-legion-envoy`. The tarball is
self-contained: it ships only `dist/envoy.js` — bundling every dependency except the
OMP host package — and the repo `skills/` tree staged beside it at `dist/skills` so
`resources_discover` serves the Legion skills from the installed package. The published
manifest exposes only `dist/envoy.js` — matching the repository root — while the
committed manifest keeps the TypeScript entries for repo checkouts; `extensions/legion.ts`
is daemon infrastructure and is not packed at all.

`.github/workflows/release-pi-envoy.yaml` performs that manifest rewrite around
`bun pm pack` and restores the committed file before tagging. Packing with the committed
source manifest is refused by `scripts/prepack.sh`, because such a tarball would point
OMP at extension files it does not contain.

## Dispatch

Every OMP session — Legion sessions included — gets a native `dispatch` tool from this
extension when `dispatch.enabled` is true in the shared envoy.json
(`~/.config/opencode/envoy.json`, shallow-merged with `<cwd>/.opencode/envoy.json`) or
`DISPATCH_MCP_URL` names a service endpoint explicitly. The service URL comes from
`dispatch.serverUrl` (default `http://localhost:8766`). Dispatch is how any agent — an
interactive session or a headless Legion role — raises a durable question to the human
and keeps the conversation on one GitHub issue: `subject` opens a thread, `thread: N`
continues one. Each call reads the session's id and title from OMP, resolves the cwd's
GitHub repo, mints a GitHub token with `gh auth token` in the session cwd, and makes one
stateless request to the dispatch service, which writes the issue or comment. The
`dispatch` skill (shipped in `skills/`) says when and how to ask. Replies route back to the
asking session, which is auto-subscribed to the thread's GitHub topic on every successful
call; a Legion role's session survives kill/resume because Legion resurrection resumes the
same OMP session file. Lifecycle and scope decisions still go through `hub` to the owning
architect — Dispatch is for durable questions to the human, not for coordination between roles.

The tool's model-facing schema is the shared contract's zod shape
(`@legion/envoy-client/dispatch-contract`) serialised to JSON Schema, so OMP shows the model
the same arguments and descriptions as every other host.

An invalid envoy.json disables the tool and the session is told why on start; a machine
without dispatch configured has no `dispatch` tool at all.
