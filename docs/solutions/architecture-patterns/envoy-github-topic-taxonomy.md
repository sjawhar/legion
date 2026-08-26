# Envoy GitHub Topic Taxonomy

Envoy maps GitHub webhook events to NATS topics so consumers can subscribe to the resource and
event shape they need without consuming unrelated repository traffic.

## Topic shapes

| Event | Topic |
|---|---|
| Push to a branch | `notifications.github.<o>.<r>.push.branch.<branch_sanitized>` |
| Push to a tag | `notifications.github.<o>.<r>.push.tag.<tag_sanitized>` |
| Push to other refs (e.g. `refs/pull/.../merge`) | **dropped** — no envelope emitted |
| `workflow_run` | `notifications.github.<o>.<r>.workflow.<filename_sanitized>.<action>` |
| `workflow_run` with missing `path` field | **dropped** — no envelope emitted |
| PR-associated `check_run` | `notifications.github.<o>.<r>.pr.<number>.check` — immediate raw per-check observation |
| CI summary after the debounce window | `notifications.github.<o>.<r>.pr.<number>.ci` — per-commit aggregate |
| `check_suite`, or `check_run` without an associated PR | **dropped** — no envelope emitted |

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

# Watch immediate state changes for each CI check on a PR
envoy_subscribe(["notifications.github.sjawhar.legion.pr.9880.check"])

# Watch the debounced CI summary for a PR
envoy_subscribe(["notifications.github.sjawhar.legion.pr.9880.ci"])
```

Note NATS wildcard semantics: `*` matches exactly one token, `>` matches one or more remaining tokens. Use `>` for "everything under this prefix" and `*` for "exactly one segment here, then this suffix".

## Routing exclusions

- `workflow_job` events are not routed; use `workflow_run` for workflow-level state.
- `release`, `deployment`, `deployment_status`, and `package` events use no specialized topic in this taxonomy.
- A `.` → `_` subject-segment transform is lossy, so consumers that need an exact branch or workflow identifier inspect the envelope payload.

## Where to look

| Concern | File |
|---|---|
| TS subject helpers (source of truth) | `packages/contracts/src/subject.ts` |
| TS subject tests | `packages/contracts/src/envelope.test.ts` |
| Go subject helpers (generated mirror) | `packages/envoy/internal/contracts/generated.go` (via `packages/contracts/scripts/gen-go.ts`) |
| Push/workflow routing and CI envelopes | `packages/envoy/internal/contracts/normalize.go` (`githubTopic`, `githubPushRefSegments`, `githubWorkflowFilename`, `GithubCIEnvelope`) |
| Drop policy for un-routable events | `packages/envoy/internal/contracts/normalize.go` (`GithubEnvelopes`) |
| CI aggregation | `packages/envoy/internal/cistore/` |
| Subscription docs | `skills/envoy/SKILL.md` |

