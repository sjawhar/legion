---
title: "Post-Collection Processing Pattern for State Delta Detection"
category: daemon
tags:
  - state-collection
  - delta-detection
  - envoy-publish
  - dependency-injection
  - health-tick
date: 2026-04-11
status: active
module: daemon
related_issues:
  - "406"
symptoms:
  - "need to add behavior after state collection"
  - "how to detect changes between state collections"
  - "how to expose server functions to health tick"
  - "how to widen return types without breaking test mocks"
---

# Post-Collection Processing Pattern for State Delta Detection

## Context

The daemon collects issue state via two HTTP endpoints (`/state/collect` and
`/state/fetch-and-collect`) and from the health tick. All three paths need
consistent post-collection behavior: cache population, delta detection, and
Envoy notifications.

## Pattern: Shared Post-Collection Processing Hook

Extract a `runPostCollectionProcessing(state, titles?)` function inside
`startServer()` that handles all side effects after state collection:

1. Populate dispatch validation cache (`issueStateCache`)
2. Populate title cache (`issueTitleCache`)
3. Convert to `IssueStateDict` via `IssueState.toDict()`
4. Compute delta against `previousIssueState` baseline
5. Publish delta to controller via Envoy (fire-and-forget)
6. Update baseline

Both HTTP handlers and the health tick call this single function, ensuring
consistent behavior. Future post-collection behaviors (metrics, logging) go
here — no handler duplication needed.

## Pattern: DI Type Widening for Evolving Return Types

When adding new methods to `startServer()`'s return type, existing test mocks
break because they don't return the new method. Fix with `Partial<Pick<>>`:

```typescript
type StartServerDependency = (
  ...args: Parameters<typeof startServer>
) => Pick<ServerHandle, "server" | "stop">
   & Partial<Pick<ServerHandle, "fetchAndProcessState">>;
```

In the daemon, default the missing method:

```typescript
fetchAndProcessState = serverHandle.fetchAndProcessState ?? (async () => {});
```

This avoids updating all 29 test mock returns while maintaining full type
safety for production code. New test mocks can include the method; old ones
keep working.

## Pattern: Baseline Establishment (First-Cycle Skip)

Store `previousIssueState` as `null` initially. On first collection, set the
baseline without publishing. Only publish on subsequent collections when
`previousIssueState !== null`. This prevents a spurious "everything is new"
delta notification on daemon startup.

The baseline always advances after successful collection — even if no delta
was published (e.g., no controller active). Failed fetches do NOT advance the
baseline, preventing false "everything changed" deltas after transient errors.

## Pattern: Shared Helper Extraction for HTTP Handler Dedup

When two HTTP handlers share identical logic (parse → enrich → build), extract
a shared inner function that closes over server state. Key gotcha: the helper
needs `server.port`, but `server` is assigned by `Bun.serve()` which is called
after the function definition.

Solution: forward-declare the server variable:

```typescript
let server: Server | null = null;

const fetchAndCollectState = async (backend, rawIssues) => {
  if (!server) throw new Error("server_not_started");
  const daemonUrl = `http://127.0.0.1:${server.port}`;
  // ... parse, enrich, build
};

server = Bun.serve({ ... });
```

The null guard ensures safety. All closures execute after initialization.

## Pattern: Fire-and-Forget Envoy Publish

Follow the established daemon pattern for Envoy interactions:

```typescript
function publishStateDelta(delta: StateDelta): void {
  const envoyUrl = process.env.ENVOY_URL ?? "http://127.0.0.1:9020";
  fetch(`${envoyUrl}/v1/messages/publish`, { ... })
    .then((res) => {
      if (!res.ok) console.warn(`[state-delta] publish failed: ${res.status} (non-fatal)`);
    })
    .catch((err) => {
      console.warn(`[state-delta] publish error (non-fatal): ${err}`);
    });
}
```

Key traits: `void` return, no `await`, `.then().catch()` chains,
`console.warn` with `(non-fatal)` suffix. Matches `subscribeWorkerToEnvoy`
and `detachWorkerFromEnvoy`.

## Files

- `packages/daemon/src/daemon/state-delta.ts` — pure delta computation
- `packages/daemon/src/daemon/server.ts` — post-collection processing, shared helper
- `packages/daemon/src/daemon/index.ts` — health tick integration
