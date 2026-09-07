---
name: envoy
description: Use when subscribing sessions to Envoy topics, sending agent-to-agent messages, or reasoning about topic formats for Slack/GitHub/agent routing.
---

# Envoy

Envoy delivers external signals and session messages. Deliveries are at-least-once and can arrive
out of order across topics: use `id` to deduplicate and `at` to judge freshness.

## The one subscription you need for a PR

Subscribe to the whole PR family, not individual event types:

```text
envoy_subscribe([
  "notifications.github.example-org.example-repo.pr.42.>"
])
```

NATS `>` matches **one or more** trailing tokens, so it does not match the lifecycle base
`pr.42` itself. Envoy registers that concrete base automatically when you subscribe to
`<subject>.>`, making `pr.<n>.>` the recommended default: one call receives both the lifecycle
subject and its child events.

For a typical push, this receives `pr.42` with `synchronize`, then any comments or reviews, then
one `pr.42.checks` event when that head's checks settle. The family is `pr.42` (lifecycle),
`pr.42.comment`, `pr.42.review`, `pr.42.mention`, and `pr.42.checks`. A closed lifecycle payload
carries `merged`, `merge_commit_sha`, `merged_by`, and `head_sha`.

`pr.42.check`, `pr.42.ci`, `pr.42.merged`, and `pr.42.closed` do not exist. Lifecycle stays on the
base PR topic and CI arrives as one settled `checks` event, so this default receives the useful
signals without redundant subscriptions.

## How to read a notification

Envoy renders delivery metadata before the source summary and payload:

```text
envoy:
  to: you (01a0…)
  from: 01a0bbbb-cccc-7ddd-eeee-0123456789ab (Reviewer)
  at: "2026-09-07T04:41:12Z"
  id: agent-message-2
  by: "2026-09-07T05:00:00Z"
  urgency: high
  expects_reply: required
  re: agent-message-1
  reply_with: "envoy_send(session_id=\"01a0bbbb-cccc-7ddd-eeee-0123456789ab\", message=\"...\")"
  reply_role: "envoy_publish(topic=\"notifications.role.legion-reviewer\", message=\"...\")"
  summary: "First paragraph.\n\nSecond paragraph.\n\nThird paragraph."
```

- `to: you` means this is in your inbox. `from` and the generated `reply_with` identify the reply
target.
- Use `at` for freshness and `id` as `in_reply_to`.
- Never use a session ID quoted in `message` as the recipient. A reply was once misrouted that
way; delivery metadata is authoritative.

## Talking to another session

Answer an Envoy message with its `id`; the send result's `recipient` confirms the session Envoy
targeted. Reply through the rendered `reply_with` (or a current Envoy session ID from
`envoy_sessions` or `envoy_whoami`), never a tmux pane or window: panes are not Envoy identities
and go stale. Put the artefact URL in the message itself. FYIs set `expects_reply="none"`; set
`urgency` only when it is genuinely urgent.

```text
envoy_send(
  session_id="ses_example_reviewer",
  message="Review complete: artifact://review.md",
  in_reply_to="agent-message-2",
  expects_reply="none"
)
```

## Waiting for CI or a merge

Subscribe to `notifications.github.example-org.example-repo.pr.42.>` and end the turn. The single
`pr.42.checks` event wakes you when the current head settles; a `pr.42` `closed` event with
`merged: true` tells you the PR merged. Do not create `gh` pollers.
Settlement waits for the head to be quiet for a few seconds, every reported check run to finish,
and every recorded GitHub check suite to be `completed`; until then, a silent subscription is
normal, not a failure.

If another check run appears after a head settled, Envoy re-arms that settlement and sends a new
`checks` payload with `superseded_settlement: "true"`. Treat it as the current verdict.

## When a subscription is silent

Check the `warnings` returned by `envoy_subscribe`, then inspect the active topics with
`envoy_list()`:

```text
envoy_subscribe([
  "notifications.github.example-org.example-repo.pr.42.>"
])
// warnings: ["no GitHub event for example-org/example-repo in the stream's retention window; is the App installed there?"]
```

A warning means the GitHub App may not be installed on that repository or no matching event is in
retention. Install the App before relying on a wakeup.

## Roles

Publish to a role; do not subscribe as its holder. A successful `envoy_publish` to a role returns
its live `holder`; an unheld role returns an error. Use `envoy_role_get(role="reviewer")` to find
the live holder first when you need one.

## Legion role claims

Legion agents receive through a daemon-minted role token. Claim the assigned role with
`envoy_role_set(role="<assigned-role>")`; a claimant does not manually subscribe to its role
topic. Claims use last-claim-wins semantics, survive parking and worker re-creation, and remain
through the issue's post-close linger. The daemon owns the authoritative token-to-issue map, so do
not construct a token from a partial issue reference.

## Legion exception lane

`no_holder` and `delivery_failed` for a Legion role are daemon liveness signals, not reasons to
add a second subscriber or manually retry. For example, treat a `delivery_failed` wake as the
daemon's responsibility to revive or recreate the backing worker and re-deliver; otherwise it
resurrects the root process with derived catch-up and the workspace handoffs. Raw delivery failures
stay out of architect context.

## Slack

Use the real team ID, not a workspace slug. Slack delivers a one-line prose `summary` plus a
structured `message` payload. The payload records `subtype` for edits, deletes, and bot messages;
`thread_ts` for replies; and `bot_id` and `bot_name` when a bot supplied the message.

```text
envoy_subscribe([
  "notifications.slack.T01234567.C01234567.thread.1_000.>"
])
```

This follows every message and mention in one thread. Channel-level topics end in `.message` or
`.mention`; thread timestamps replace dots with underscores.

## Ghost Wispr

Ghost Wispr topics are `notifications.ghostwispr.<session>.<kind>`, where `kind` is
`session.started`, `session.ended`, or `summary.ready`. Their summary is concise prose and their
payload is structured; `summary_ready` includes its status, summary, and summary metadata.

```text
envoy_subscribe([
  "notifications.ghostwispr.session-example.summary.ready"
])
```

## WhatsApp

WhatsApp topics are `notifications.whatsapp.<phone>.<jid>.message` or `.status`. A JID contains
dots, which become additional NATS segments, so use `>` rather than `*` for a chat.

```text
envoy_subscribe([
  "notifications.whatsapp.15551234567.5551234567@s.whatsapp.net.>"
])
```

### WhatsApp routing smoke test

This checks Envoy routing, not real WhatsApp ingestion. In Session A, subscribe as above. From a
different Session B, publish a synthetic message to the same `.message` topic:

```text
envoy_publish(
  topic="notifications.whatsapp.15551234567.5551234567@s.whatsapp.net.message",
  message="Synthetic WhatsApp routing test"
)
```

Session A should receive it. Broadcasts do not echo to their publishing session, so one session
cannot perform both steps. Real WhatsApp delivery additionally requires a configured MCP bridge.
