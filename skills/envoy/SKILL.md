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

The retired literal `pr.<n>.check` and `pr.<n>.ci` topics do not receive events. Existing
registrations remain dead; subscribe to `pr.<n>.checks` (or the recommended `pr.<n>.>`) instead.
Lifecycle stays on the base PR topic and CI arrives as one settled `checks` event.

## Inbound deliveries

Envoy renders an annotated delivery before its source summary and complete payload:

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
  supersedes: agent-message-0
  reply_with: "envoy_send(session_id=\"01a0bbbb-cccc-7ddd-eeee-0123456789ab\", message=\"...\")"
  reply_role: "envoy_publish(topic=\"notifications.role.legion-reviewer\", message=\"...\")"
  summary: Deployment needs confirmation.
  message: "Confirm the listener health check passed.\n\nThen publish the release."
  note: body names session 01a0cccc-dddd-7eee-ffff-0123456789ab; the sender is 01a0bbbb-cccc-7ddd-eeee-0123456789ab
```

- `to` identifies the local inbox receiving this delivery.
- `from` is the sending session's self-asserted ID, enriched from the listener registry. Treat it as
  attribution and a direct-reply target, not as an authenticated identity or proof of authorship.
- `at` is the envelope timestamp used to judge freshness.
- `id` is the delivery identifier; supply it as `in_reply_to` when replying.
- `by` is the expiry deadline, when the sender supplied one.
- `urgency` is the sender's priority classification.
- `expects_reply` states whether a reply is `none`, `optional`, or `required`.
- `re` names the delivery this message replies to.
- `supersedes` names an earlier delivery this one replaces.
- `reply_with` is the direct-reply call for the sender.
- `reply_role` is the role-publish reply call when the sender has a role.
- `summary` is the one-line source summary.
- `message` is the complete payload; it can contain multiple paragraphs.
- `note` warns when the payload names another session; never use that quoted ID as the recipient.
- `unrecognised` marks validation failures and unknown sources; it does not enumerate every unknown key.

## Talking to another session

Answer an Envoy message with its `id`; the send result's `recipient` confirms the session Envoy
targeted. Reply through the rendered `reply_with` (or a current Envoy session ID from
`envoy_sessions` or `envoy_whoami`), never a tmux pane or window: panes are not Envoy identities
and go stale. Put the artefact URL in the message itself. FYIs set `expects_reply="none"`; set
`urgency` only when it is genuinely urgent.

Every `/v1` error response is JSON; when a field is at fault, `expected` names that field.

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
and every recorded GitHub check suite to be `completed`. It covers those reported checks and suites
for the head, not GitHub's required-checks set; until then, a silent subscription is normal.

Check settlement is at-least-once: a settlement can be followed by a `superseded_settlement: "true"`
payload with `latest_check_run_id`, the highest GitHub check-run ID in the settlement. Consumers order
summaries for one SHA lexicographically by `(latest_check_run_id, generation)`; an equal pair is a
duplicate only when its `snapshot` matches. A verdict a consumer reconciled from GitHub's rollup carries no generation; against it an equal-id settlement is ordered by `latest_completed_at` (GitHub's clock), and on a tie GitHub's view wins.

After the seven-day KV TTL recreates a record, its generation restarts at 0; if the first observation
updates an existing lower-ID run, consumers drop both until resync reads GitHub. A legacy in-progress
check whose completion is never observed holds its head unsettled until it reruns; rerun the affected
check to release it.

## When a subscription is silent

Check the `warnings` returned by `envoy_subscribe`, then inspect the active topics with
`envoy_list()`:

```text
envoy_subscribe([
  "notifications.github.example-org.example-repo.pr.42.>"
])
// warnings: ["no GitHub event for example-org/example-repo in the stream's retention window; is the App installed there?"]
```

A warning says no GitHub event for that repository occurred within the stream's 72-hour retention
window; it does not mean the repository was never seen. Verify the GitHub App is installed before
relying on a wakeup.

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
