---
name: envoy
description: Use when subscribing sessions to Envoy topics, sending agent-to-agent messages, or reasoning about topic formats for Slack/GitHub/agent routing.
---

# Envoy

Envoy is Legion's event-routing subsystem. It delivers Slack, GitHub, and agent-to-agent events to OpenCode sessions.

## What the tools do

- `envoy_subscribe(topics)` — make the current session RECEIVE future events on those topics
- `envoy_unsubscribe(topics?)` — stop receiving some or all topics
- `envoy_list()` — show the session's current subscriptions
- `envoy_send(target_session, message)` — SEND a message directly to another session

## Topic formats

### Agent-to-agent

- Direct session route:
  - `notifications.agent.<session_id>`

Example:

- `notifications.agent.ses_2e6ca3034ffejVikSZ8mDwk0mR`

### GitHub

- PR events (all PRs):
  - `notifications.github.<owner>.<repo>.pr`
- PR events (specific PR):
  - `notifications.github.<owner>.<repo>.pr.<number>`
- Issue events (all issues):
  - `notifications.github.<owner>.<repo>.issue`
- Issue events (specific issue):
  - `notifications.github.<owner>.<repo>.issue.<number>`
- Comment events (all):
  - `notifications.github.<owner>.<repo>.comment`
- Comment events (specific PR/issue):
  - `notifications.github.<owner>.<repo>.comment.<number>`
- Mention events (all):
  - `notifications.github.<owner>.<repo>.mention`
- Mention events (specific PR/issue):
  - `notifications.github.<owner>.<repo>.mention.<number>`
- CI/check events:
  - `notifications.github.<owner>.<repo>.ci`
- Push events:
  - `notifications.github.<owner>.<repo>.push`

Examples:

- `notifications.github.trajectory-labs-pbc.agent-c.pr` (all PRs)
- `notifications.github.trajectory-labs-pbc.agent-c.pr.7706` (PR #7706 only)
- `notifications.github.sjawhar.legion.mention` (all mentions)
- `notifications.github.sjawhar.legion.mention.171` (mentions on PR #171 only)

### Slack

- Channel message events:
  - `notifications.slack.<team_id>.<channel_id>.message`
- App mention events:
  - `notifications.slack.<team_id>.<channel_id>.mention`
- Thread events:
  - `notifications.slack.<team_id>.<channel_id>.thread.<thread_ts>`

Examples:

- `notifications.slack.T09FRELLTS8.C0A0DHVU8HE.message`
- `notifications.slack.T09FRELLTS8.C0A0DHVU8HE.mention`
- `notifications.slack.T09FRELLTS8.C0A0DHVU8HE.thread.1234567890.123456`

## When to use what

### To receive future Slack/GitHub events

1. Decide the exact topic(s)
2. Call `envoy_subscribe([...])`
3. Optionally call `envoy_list()` to confirm

### To talk directly to another agent/session

1. Get the target session ID
2. Call `envoy_send(target_session, message)`

You do NOT need to subscribe in order to send or publish.

### Tools

- `envoy_subscribe(topics)` — receive future events on those topics
- `envoy_unsubscribe(topics?)` — stop receiving some or all topics
- `envoy_list()` — show current subscriptions
- `envoy_send(target_session, message)` — send directly to a specific session (point-to-point)
- `envoy_publish(topic, message)` — broadcast to any topic (all matching subscribers receive it)

## Patterns

### Subscribe controller to a specific Slack channel mentions

```text
envoy_subscribe([
  "notifications.slack.T09FRELLTS8.C0A0DHVU8HE.mention"
])
```

### Subscribe controller to all PR events for agent-c

```text
envoy_subscribe([
  "notifications.github.trajectory-labs-pbc.agent-c.pr"
])
```

### Subscribe controller to GitHub @mentions for agent-c

```text
envoy_subscribe([
  "notifications.github.trajectory-labs-pbc.agent-c.mention"
])
```

### Message another session directly

```text
envoy_send(
  target_session="ses_2e6ca3034ffejVikSZ8mDwk0mR",
  message="Please continue the smoke test"
)
```

## Important notes

- Sessions choose their own Slack/GitHub subscriptions
- Different sessions can subscribe to different channels/repos
- Agent-to-agent delivery uses exact session IDs
- If you are unsure what a session is currently subscribed to, call `envoy_list()` first
- For Slack, use the real `team_id` in topics (for example `T09FRELLTS8`), not a workspace slug like `trajectorylabs`
- GitHub mention routing is body-based because GitHub has no dedicated app mention webhook event
