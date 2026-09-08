# Envoy GitHub Topic Taxonomy

Envoy maps GitHub webhook events to resource-scoped NATS topics. Subscribers
choose the resource and event shape they need without consuming unrelated
repository traffic.

## Topic shapes

| Event | Topic |
|---|---|
| Push to a branch | `notifications.github.<owner>.<repo>.push.branch.<branch_sanitized>` |
| Push to a tag | `notifications.github.<owner>.<repo>.push.tag.<tag_sanitized>` |
| `workflow_run` | `notifications.github.<owner>.<repo>.workflow.<filename_sanitized>.<action>` |
| Settled PR CI | `notifications.github.<owner>.<repo>.pr.<number>.checks` |
| PR opened/closed/merged/ready | `notifications.github.<owner>.<repo>.pr.<number>` |
| PR comment | `notifications.github.<owner>.<repo>.pr.<number>.comment` |
| Issue comment | `notifications.github.<owner>.<repo>.issue.<number>.comment` |
| Push to another ref | Dropped — no envelope |
| `workflow_run` missing `path` | Dropped — no envelope |
| CI event without an associated PR | Dropped — no state or envelope |

`check_run` and `check_suite` webhook events do not publish raw envelopes.
They update durable CI state; one `checks` envelope publishes after the PR
head's CI is complete.

## Subscription examples

```text
# All signal-worthy events beneath one pull request, including settled CI
envoy_subscribe(["notifications.github.example-org.example-repo.pr.42.>"])

# Settled CI for a single pull request
envoy_subscribe(["notifications.github.example-org.example-repo.pr.42.checks"])

# Workflow events across a repository
envoy_subscribe(["notifications.github.example-org.example-repo.workflow.>"])

# Pushes to the main branch
envoy_subscribe(["notifications.github.example-org.example-repo.push.branch.main"])
```

NATS `*` matches one token and `>` matches one or more remaining tokens.

## Sanitization

Branch, tag, and workflow filename segments replace `.` with `_` so they
remain one NATS token. The transformation is lossy; consumers that require an
exact identifier inspect the envelope payload.

Slashes are preserved because they are not special in a NATS subject. A
workflow segment is the basename of `workflow_run.path`: for example,
`.github/workflows/ci.yml` becomes `ci_yml`.

## Routing exclusions

`workflow_job`, `release`, `deployment`, `deployment_status`, and `package`
have no specialized topic in this taxonomy.

## Implementation

| Concern | File |
|---|---|
| GitHub envelope routing | `packages/envoy/internal/contracts/normalize.go` |
| CI observation extraction | `packages/envoy/internal/contracts/normalize.go` |
| CI state and `checks` publication | `packages/envoy/internal/cistore/` |
| CI webhook ingestion | `packages/envoy/internal/webhook/github.go` |
