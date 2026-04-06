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

## Slack Topic Hierarchy

The Slack receiver publishes channel-level and thread-level topics. Consumers
subscribe at the granularity they need using wildcard patterns.

### Base subjects

| Helper | Example output |
|--------|---------------|
| `slackSubject(team, channel, kind)` | `notifications.slack.T09FRELLTS8.C0A0DHVU8HE.message` |
| `slackThreadSubject(team, channel, threadTs, kind)` | `notifications.slack.T09FRELLTS8.C0A0DHVU8HE.thread.1234567890_123456.message` |

### Published topic patterns

| Event type | Published topic(s) |
|-----------|-------------------|
| Channel message | `notifications.slack.{team}.{channel}.message` |
| Channel mention (`app_mention`) | `notifications.slack.{team}.{channel}.mention` |
| Thread reply (message) | Channel topic + `notifications.slack.{team}.{channel}.thread.{normalized_ts}.message` |
| Thread mention (`app_mention` in thread) | Channel topic + `notifications.slack.{team}.{channel}.thread.{normalized_ts}.mention` |
| Standalone message (no `thread_ts`) | Channel topic only (no thread envelope) |

**Thread timestamp normalization:** Slack `thread_ts` values contain dots
(e.g., `1234567890.123456`) which conflict with NATS segment separators. The
`slackThreadSubject` helper normalizes by replacing `.` with `_`
(→ `1234567890_123456`), making the thread identifier a single NATS segment.

### Subscription granularity

| Want | Subscribe to |
|------|-------------|
| All events in channel | `notifications.slack.T.C.>` |
| All messages in channel | `notifications.slack.T.C.message` |
| All mentions in channel | `notifications.slack.T.C.mention` |
| All events in specific thread | `notifications.slack.T.C.thread.1234567890_123456.>` |
| Only messages in thread | `notifications.slack.T.C.thread.1234567890_123456.message` |
| Only mentions in thread | `notifications.slack.T.C.thread.1234567890_123456.mention` |
| All threads in channel | `notifications.slack.T.C.thread.>` |

### Deduplication

A session subscribed to both `notifications.slack.T.C.>` and a specific thread
topic receives each event **once** (not duplicated), because both envelopes
share the same `dedupe_key` and the listener deduplicates by
`(dedupe_key, session_id)`.
