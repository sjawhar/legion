---
title: "Envoy Auto-Subscription Patterns in the Daemon"
category: daemon
tags:
  - envoy
  - fire-and-forget
  - subscription
  - post-workers
  - optional-fields
date: 2026-04-06
status: active
module: daemon
related_issues:
  - "#199"
  - "#176"
symptoms:
  - "Envoy subscription blocking worker dispatch"
  - "How to add optional fields to POST /workers"
  - "subscribeWorkerToEnvoy pattern"
  - "Worker stale subscriptions after completion"
  - "Controller CI event subscriptions"
  - "Worker resume re-subscription"
---

# Envoy Auto-Subscription Patterns in the Daemon

## Fire-and-Forget Subscription Pattern

When subscribing workers or controllers to Envoy topics, the subscription call must
never block the caller. Envoy is a speed optimization — polling is the authoritative
fallback.

### Pattern

```typescript
function subscribeWorkerToEnvoy(sessionId: string, owner: string, repo: string, issueNumber: number): void {
  const envoyUrl = process.env.ENVOY_URL ?? "http://127.0.0.1:9020";
  const topic = `notifications.github.${owner}.${repo}.issue.${issueNumber}.>`;
  fetch(`${envoyUrl}/v1/interests/subscribe`, { /* ... */ })
    .then((res) => {
      if (!res.ok) console.warn(`... (non-fatal)`);
    })
    .catch((err) => {
      console.warn(`... (non-fatal): ${err}`);
    });
}
```

### Key Rules

1. **Return type is `void`, not `Promise`** — the caller does not `await` this.
2. **Place after `persistState()`** — the worker must be tracked before attempting subscription, so a subscription failure never leaves an untracked worker.
3. **Log failures as `console.warn`** — never throw, never return an error response.
4. **Guard with field checks** — only subscribe when both `repo` and `issueNumber` are present. Skip silently for workspace-only or Linear-backed workers.

### Controller Subscription

The controller uses the same fire-and-forget pattern in `subscribeControllerToEnvoy()` (index.ts). Topics use wildcards for broad coverage:

```typescript
topics: [
  "notifications.legion.controller",
  "notifications.slack.*.*.mention",
  "notifications.github.*.*.mention",
]
```

Both internal and external controller startup paths call the same function — one edit covers both.

## Adding Optional Fields to POST /workers

When extending the dispatch API, follow this pattern:

1. **Interface**: Add to `DispatchOptions` with `?` (optional).
2. **CLI**: Add a string arg, parse to the correct type, validate.
3. **Handler**: Extract with `typeof` guard: `typeof payload.field === "number" ? payload.field : undefined`.
4. **Body construction**: Conditionally include: `if (opts.field !== undefined) body.field = opts.field`.
5. **Downstream**: Gate new behavior on the field's presence.

This keeps the API backward-compatible — callers that don't pass the field get existing behavior.

### Block-Scoped Variable Gotcha

In `server.ts` POST /workers, the `repoRef` from workspace resolution is `const` inside an `if` block. It's not accessible later when you need it for subscription. Re-call `parseIssueRepo(repo)` at the subscription point — it's a trivial string split with no performance concern.

## Topic Construction Convention

Envoy topics follow: `notifications.github.<owner>.<repo>.<resource_type>.<resource_id>.>`

- `>` suffix = "this topic and all subtopics"
- `*` = single-level wildcard
- Resource types: `issue`, `pr`, `mention`, `ci`, `push`

Use `parseIssueRepo()` to decompose `owner/repo` strings — never hand-parse the slash.

## Subscription Lifecycle

### Ownership Model

**Daemon = authoritative.** The daemon owns subscription lifecycle through `POST /workers` (subscribe on dispatch) and `DELETE /workers` (unsubscribe all on deletion). Workers provide best-effort early cleanup using Envoy MCP tools.

**Key distinction:** The daemon uses fire-and-forget `fetch()` calls (void return, never blocking). Workers use Envoy MCP tools (`envoy_subscribe`/`envoy_unsubscribe`). Both are non-blocking from the caller's perspective.

### State Table

| Worker State Transition | Subscription Action | Owner | Authority |
|------------------------|---------------------|-------|-----------|
| Dispatched | Subscribe worker to `issue.{n}.>` | Daemon (`POST /workers`) | Authoritative |
| PR created (implement) | Subscribe worker to `pr.{n}.>` | Worker (`envoy_subscribe`) | Best-effort |
| Worker resumed | Re-subscribe worker to issue topics | Daemon (prompt path) | Authoritative |
| Worker resumed with PR | Worker re-subscribes to PR topics | Worker (`envoy_subscribe`) | Best-effort |
| Worker exits (`worker-done`) | Unsubscribe from explicit issue+PR topics | Worker (`envoy_unsubscribe`) | Best-effort |
| Worker deleted | Unsubscribe from all topics | Daemon (`DELETE /workers`) | Authoritative |
| Worker crashes before exit | No self-unsubscribe | — | Daemon DELETE is safety net |

### Worker Self-Unsubscribe

Workers use explicit topic lists for self-unsubscription — never empty-array unsubscribe. Empty-array would also remove `notifications.agent.{sessionId}`, creating a delivery gap between `worker-done` and daemon cleanup.

```
# CORRECT: explicit topics
envoy_unsubscribe([
  "notifications.github.owner.repo.issue.42.>",
  "notifications.github.owner.repo.pr.123.>"
])

# WRONG: removes all topics including agent routing
envoy_unsubscribe([])
```

### Controller CI Subscriptions [REMOVED]

**Removed in #377.** The daemon previously subscribed the controller to `notifications.github.{owner}.{repo}.ci` on first worker dispatch per repo, tracked via `subscribedCiRepos` Set. This was removed because CI events from unrelated branches (Vercel previews, skipped jobs) flooded the controller with noise. The controller can subscribe manually via `envoy_subscribe` when it specifically needs to watch a CI run.

### Resume Re-Subscription

When a worker is resumed via `POST /workers/:id/prompt`, the daemon re-subscribes the worker to its issue topics using stored `repo` and `issueNumber` from the worker entry. PR topic re-subscription is the worker's responsibility (via `envoy_subscribe` in the skill).

Fields `repo` and `issueNumber` are persisted in the worker entry (added to `WorkerEntry` interface in serve-manager.ts, preserved by schema `.passthrough()`).
