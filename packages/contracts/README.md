# @legion/contracts

Shared event contracts for the Legion monorepo, including the Envoy subsystem.

This package is the language-neutral contract layer for cross-runtime event payloads.

Current scope:

- envelope schema
- subject helpers
- JSON Schema source for future code generation

## GitHub Topic Hierarchy

The GitHub receiver publishes per-resource topics. Consumers subscribe at the
granularity they need using wildcard patterns.

### Base subjects

| Helper | Example output |
|--------|---------------|
| `githubSubject(owner, repo, kind)` | `notifications.github.acme.widgets.pr` |
| `githubResourceSubject(owner, repo, type, number)` | `notifications.github.acme.widgets.pr.42` |

### Published topic patterns

| Event type | Published topic |
|-----------|----------------|
| PR opened/closed/merged | `notifications.github.{owner}.{repo}.pr.{number}` |
| PR comment | `notifications.github.{owner}.{repo}.pr.{number}.comment` |
| PR review | `notifications.github.{owner}.{repo}.pr.{number}.review` |
| Issue event | `notifications.github.{owner}.{repo}.issue.{number}` |
| Issue comment | `notifications.github.{owner}.{repo}.issue.{number}.comment` |
| Mention (repo-level) | `notifications.github.{owner}.{repo}.mention` |
| Mention (resource-level) | `notifications.github.{owner}.{repo}.{type}.{number}.mention` |

### Subscription granularity

| Want | Subscribe to |
|------|-------------|
| All events for PR #42 | `notifications.github.acme.widgets.pr.42.>` |
| All PR events in repo | `notifications.github.acme.widgets.pr.>` |
| All events in repo | `notifications.github.acme.widgets.>` |
| Exact resource topic | `notifications.github.acme.widgets.pr.42` |

The `>` wildcard matches the current level and all deeper levels.
The `*` wildcard matches exactly one level.

### Not yet published

- CI events (`check_run`, `check_suite`) — tracked in #175
