# Envoy GitHub Topic Taxonomy

How Envoy maps GitHub webhook events to NATS topics, and how to subscribe narrowly without consuming a repo-wide firehose.

## Problem

The original GitHub topic shapes had two firehoses:

- `notifications.github.<owner>.<repo>.push` — every push to every branch and tag, undifferentiated. Subscribers had to client-side filter on the `ref` payload field, which means receiving (and paying for the routing of) noise they would immediately discard.
- `notifications.github.<owner>.<repo>.ci` — every `check_run`/`check_suite` event not attached to a PR. Vercel previews, skipped jobs, scheduled workflow runs, and other non-PR CI activity all landed here. The daemon auto-subscribed controllers to this on first worker dispatch per repo, and the noise was bad enough that auto-subscription was removed in #377 (see [envoy-auto-subscription-patterns](../daemon/envoy-auto-subscription-patterns.md)). After that PR, the topic had no active subscribers.

There was also no topic family at all for `workflow_run` events — agents that wanted to react to "CI is starting on main" had no clean subscription target.

## Topic shapes

| Event | Topic |
|---|---|
| Push to a branch | `notifications.github.<o>.<r>.push.branch.<branch_sanitized>` |
| Push to a tag | `notifications.github.<o>.<r>.push.tag.<tag_sanitized>` |
| Push to other refs (e.g. `refs/pull/.../merge`) | **dropped** — no envelope emitted |
| `workflow_run` | `notifications.github.<o>.<r>.workflow.<filename_sanitized>.<action>` |
| `workflow_run` with missing `path` field | **dropped** — no envelope emitted |
| `check_run`/`check_suite` attached to a PR | `notifications.github.<o>.<r>.pr.<number>.ci` (unchanged) |
| `check_run`/`check_suite` not attached to a PR | **dropped** — no envelope emitted |

Where:
- `branch_sanitized` / `tag_sanitized` — branch or tag name with dots replaced by underscores. Slashes are preserved (NATS doesn't treat `/` as special), so `feat/foo` stays as `feat/foo`.
- `filename_sanitized` — `basename(workflow_run.path)` with dots replaced by underscores. `.github/workflows/ci.yml` → `ci_yml`.
- `action` — one of `requested`, `in_progress`, `completed`.

## Sanitization rule

Replace `.` with `_` so the segment stays a single NATS token. Extracted as a shared helper:
- TS: `sanitizeSubjectSegment(value)` in `packages/contracts/src/subject.ts`, used by `slackThreadSubject`, `githubPushSubject`, and `githubWorkflowSubject`.
- Go: `SanitizeSubjectSegment(value)` in `packages/envoy/internal/contracts/generated.go` (generated from TS via `gen-go.ts`), used by the corresponding Go subject builders.

The transform is lossy — `release_yml` could come from `release.yml` or `release_yml`. Subscribers needing the exact identifier must inspect the envelope payload (which always carries the unsanitized `ref` or `workflow_run.path`).

## Subscription examples

```text
# Watch pushes to main only
envoy_subscribe(["notifications.github.sjawhar.legion.push.branch.main"])

# Watch all branch pushes in a repo
envoy_subscribe(["notifications.github.sjawhar.legion.push.branch.>"])

# Watch all tag pushes (release watching)
envoy_subscribe(["notifications.github.sjawhar.legion.push.tag.>"])

# React to a specific workflow starting
envoy_subscribe(["notifications.github.sjawhar.legion.workflow.ci_yml.in_progress"])

# React to any workflow completing across the repo
envoy_subscribe(["notifications.github.sjawhar.legion.workflow.*.completed"])

# Watch CI for a specific PR (unchanged from before)
envoy_subscribe(["notifications.github.sjawhar.legion.pr.9880.ci"])
```

Note NATS wildcard semantics: `*` matches exactly one token, `>` matches one or more remaining tokens. Use `>` for "everything under this prefix" and `*` for "exactly one segment here, then this suffix".

## What was deferred

- **`workflow_job` events** — fired per individual job within a workflow run (one event per matrix cell × 3 lifecycle states). Roughly 10× the volume of `workflow_run`. Skipped to keep the initial change focused; revisit if a use case appears for job-level granularity (e.g. "is the lint job specifically stuck?").
- **`release`, `deployment`, `deployment_status`, `package` events** — currently fall through to the default kind. Add per-event topic shapes when there's a concrete consumer.
- **Sanitization collision disambiguation** — the `.` → `_` transform is one-way. If two real filenames collide (`release.yml` and `release_yml`), subscribers must inspect the envelope payload to disambiguate. No structural escape (e.g. `__` for literal underscore) was added; not worth the complexity for the unlikely collision.

## Breaking changes

- Flat `notifications.github.<o>.<r>.push` is no longer emitted. Subscribers must move to `notifications.github.<o>.<r>.push.>` for the equivalent firehose, or to a specific branch/tag for narrow filtering.
- Bare `notifications.github.<o>.<r>.ci` is no longer emitted. There were no active subscribers in Legion at the time of the change (confirmed via grep + the auto-subscription removal in #377). External consumers subscribing to that exact topic would need to switch to `notifications.github.<o>.<r>.workflow.<filename>.<action>` if they want non-PR CI visibility.

## Where to look

| Concern | File |
|---|---|
| TS subject helpers (source of truth) | `packages/contracts/src/subject.ts` |
| TS subject tests | `packages/contracts/src/envelope.test.ts` |
| Go subject helpers (generated mirror) | `packages/envoy/internal/contracts/generated.go` (via `packages/contracts/scripts/gen-go.ts`) |
| Push/workflow_run routing | `packages/envoy/internal/contracts/normalize.go` (`githubTopic`, `githubPushRefSegments`, `githubWorkflowFilename`) |
| Drop policy for un-routable events | `packages/envoy/internal/contracts/normalize.go` (`GithubEnvelopes`) |
| Subscription docs | `.opencode/skills/envoy/SKILL.md` |
| Controller behavior | `.opencode/skills/legion-controller/SKILL.md` (CI Event Handling section) |
