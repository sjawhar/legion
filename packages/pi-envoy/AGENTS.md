# Pi Envoy Extension

Tracked Oh My Pi (`pi-*`) extension package for Envoy messaging.

## Overview

This package owns Pi-specific tool registration, direct NATS subscriptions, steering delivery,
and self-subscription registration for every session. HTTP transport, tool metadata, and subject
construction come from the Envoy core packages. `@legion/envoy-client/delivery` is the sole
inbound renderer: it produces a tolerant TOON block and never exposes raw envelope bytes. Role
claims are routed by the listener:
this extension receives a receipt-backed request on its direct agent subject instead of subscribing
to a role subject itself. The agent pump replies after it accepts the envelope, so the listener can
turn a claimed-but-deaf holder into a `delivery_failed` exception after two seconds.

## Native Dispatch tools

The thirteen native Dispatch tools — `dispatch_issue`, `dispatch_ask`, `dispatch_edit_ask`, `dispatch_resolve_ask`,
`dispatch_comment`, `dispatch_suggest`, `dispatch_message`, `dispatch_doc_edit`,
`dispatch_doc_read`, `dispatch_request_approval`, `dispatch_artifact`, `dispatch_read`, and `dispatch_search` — register only when the shared
configuration resolves a URL and bearer token. Set
`dispatch.enabled: true`, `dispatch.serverUrl`, and `dispatch.token` in
`~/.config/opencode/envoy.json` or `<cwd>/.opencode/envoy.json`; `DISPATCH_URL`
and `DISPATCH_TOKEN` override those settings; `DISPATCH_TOKEN_FILE` (a path whose
trimmed contents are the token — how the Legion daemon delivers it to a pane) wins
over both and never falls back when unreadable. A human mints a personal token in
Dispatch Settings → Agent tokens, then supplies it through `dispatch.token` or
`DISPATCH_TOKEN`; the server attributes that session's writes to the minting human.
`DISPATCH_AGENT_TOKEN` is the shared devbox fallback, not a token to configure for
an individual agent. A successful mutation returns `details.topic`, and the `tool_result` hook subscribes to that exact retained
Dispatch topic — a new subscription also tells the model (`pi.sendMessage`
with `deliverAs: "steer"`, the same channel `deliver` uses for inbound
envelopes, since the host does not let a `tool_result` handler amend what the
model already saw); an already-subscribed write stays quiet. Reads
(`dispatch_read`, `dispatch_doc_read`) return only `details.issue`: surveying
the board never subscribes the session. A `subscription.removed` notice (a
human unsubscribed a session from the dashboard) reaches both the issue's own
topic and the removed session's agent topic directly; only the session the
payload names renders it and drops the matching local NATS subscription
(so the dead-connection recovery path does not resurrect it) — every other
subscriber ignores it.
`dispatch_issue` accepts optional initial labels; project-document arguments resolve the document's artifact id, slug, or filename.

## Where to look

| Task | Location | Notes |
| --- | --- | --- |
| OMP extension entries | `extensions/envoy.ts`, `extensions/legion.ts` | Both ship in the published npm package and load in every installed OMP session; `legion.ts` is inert without `LEGION_TREE`/`LEGION_ROLE`/`LEGION_CONTROLLER` in the environment |
| Legion lifecycle modules | `src/legion/` | Classification, daemon client, gh shim, jj attribution, control directives, tools |
| Extension unit tests | `extensions/envoy.test.ts`, `extensions/legion.test.ts` | Mocked Pi and NATS surface |
| Shared HTTP/tool behavior | `../envoy-client/src/` | Do not duplicate it here |
| Event subjects | `../contracts/src/subject.ts` | Canonical subject construction |
| Dispatch tools | `extensions/envoy.ts` (the `registerTool` block), `@legion/contracts` (`dispatchToolSpecs`, `dispatchToolSchema`, `zodSchemaApi`), `@legion/envoy-client/dispatch-execute` (`executeDispatchTool`) | Registers the thirteen native tools only when `resolveDispatchConfig` resolves URL and token. Build each tool schema with `dispatchToolSchema(spec, zodSchemaApi(pi.zod))`, pass the live session id/title to `executeDispatchTool`, and subscribe from a successful result's `details.topic`. |
| Role session prompts | `roles/*.md` | One file per launched Legion process: `architect-root`, `controller-root`, and one per `LegionRole`; the daemon appends each as `--append-system-prompt` |
| Real end-to-end delivery smoke | `scripts/smoke-delivery.sh`, `scripts/README.md` | Claims a role and receives a message from a real, installed-plugin `omp` TUI session against a live Envoy; see `scripts/README.md` for the runbook |

## Critical conventions

- Register every schema through the injected `pi.zod`. Every field counts, not just the outer object: OMP's converter reads internals (`.ir`) only its own Zod produces, and a field from another Zod instance fails the whole extension load (`undefined is not an object (evaluating 'e.ir.desc')`). The shared Dispatch contract exposes field shapes and cross-field validation through `dispatchToolSchema`; pass it `zodSchemaApi(pi.zod)`. `envoy.test.ts` proves every registered field came from the injected instance. |
- Keep direct NATS subscription lifecycle and Pi steering delivery adapter-local. Inbound messages deliver as `steer` so they interrupt an in-flight turn; `triggerTurn` still wakes idle sessions.
- Render every inbound envelope through `renderInbound`. Keep its bounded 50-item `envoy_inbox` metadata-only; use the shared `envoy_role_get` transport operation for current role holders.
- `envoy_list` must report the union of locally live and registry-persisted topics, with each topic marked `live`, `registry`, or `both`.
- Do not alter `~/.omp` from this package. The README documents the local developer symlink.
- `scripts/smoke-delivery.sh` is a manual, real end-to-end smoke against the installed plugin; never wire it into CI without live Envoy/NATS and a configured model provider.
