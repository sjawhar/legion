# Slack, Ghost Wispr and WhatsApp topics

Read this when you subscribe to one of these sources or test its routing.

## Slack

- Use the real team id (`T...`), never a workspace slug: the topic carries the id, so a slug receives nothing.
- Channel topics: `notifications.slack.<team>.<channel>.message` or `.mention`. One thread: `notifications.slack.<team>.<channel>.thread.<ts>.>`, with the dot in the thread timestamp written `_`.
- A delivery has a one-line prose `summary` and a structured `message`. The payload records `subtype` for edits, deletes and bot messages, `thread_ts` for replies, and `bot_id` and `bot_name` when a bot sent it.
- Reply in a thread with `slack_post`; its tool description gives the arguments and refusals.

```text
envoy_subscribe(["notifications.slack.T01234567.C01234567.thread.1_000.>"])
```

## Ghost Wispr

Topics are `notifications.ghostwispr.<session>.<kind>`, where `kind` is `session.started`, `session.ended` or `summary.ready`. The summary is prose and the payload structured; A `summary.ready` payload carries `status`, `summary` (capped; `body_truncated` when cut), `summary_preset`, `timestamp` and `version`.

```text
envoy_subscribe(["notifications.ghostwispr.session-example.summary.ready"])
```

## WhatsApp

Topics are `notifications.whatsapp.<phone>.<jid>.message` or `.status`. A JID contains dots, which become extra NATS tokens, so match a chat with `>`, never `*`.

```text
envoy_subscribe(["notifications.whatsapp.15551234567.5551234567@s.whatsapp.net.>"])
```

To test Envoy routing without real WhatsApp ingestion, subscribe as above, then publish a synthetic message to the same `.message` topic:

```text
envoy_publish(
  topic="notifications.whatsapp.15551234567.5551234567@s.whatsapp.net.message",
  message="Synthetic WhatsApp routing test"
)
```

The subscribed session should receive it. An Oh My Pi or Claude Code session receives its own publish, so it can run both steps; a session delivered over a serve port cannot, and needs a second session to publish. Real WhatsApp delivery also needs the listener's MCP bridge configured with a `whatsapp` server (`packages/envoy/internal/mcpbridge`).
