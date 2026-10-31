# Envoy deliveries

Read this when a delivery field is unclear, or when a publish did not reach a session you expected.

## A rendered delivery

Envoy renders an annotated delivery before its source summary and complete payload:

```text
envoy:
  to: you (ses_example_recipient)
  from: 01a0bbbb-cccc-7ddd-eeee-0123456789ab (Reviewer)
  at: "2026-09-07T04:41:12Z"
  id: agent-message-2
  by: "2026-09-07T05:00:00Z"
  urgency: high
  expects_reply: required
  re: agent-message-1
  supersedes: agent-message-0
  reply_with:
    tool: envoy_send
    args:
      session_id: 01a0bbbb-cccc-7ddd-eeee-0123456789ab
      in_reply_to: agent-message-2
      message: ...
  reply_role:
    tool: envoy_publish
    args:
      topic: notifications.role.legion-reviewer
      message: ...
  summary: Deployment needs confirmation.
  message: "Confirm the listener health check passed.\n\nThen publish the release."
  note: body names session 01a0cccc-dddd-7eee-ffff-0123456789ab; the sender is 01a0bbbb-cccc-7ddd-eeee-0123456789ab
```

- `to`: the local inbox that receives this delivery.
- `from`: the sending session's self-asserted id, with its session title in parentheses when the envelope carries one. Attribution and a direct-reply target, not an authenticated identity.
- `at`: the envelope timestamp; judge freshness by it.
- `id`: the delivery id; pass it as `in_reply_to` when you reply.
- `by`: the expiry deadline, when the sender set one.
- `urgency`: the sender's priority: `low`, `med`, `high` or `blocking`.
- `expects_reply`: `none`, `optional` or `required`.
- `re`: the delivery this one replies to. A Dispatch frame names it as a `dispatch://` ref: the ask (`dispatch://KEY/ask/<id>`, or `dispatch://PROJECT/artifact/<slug>/ask/<id>` for a document ask) or the message (`dispatch://KEY/message/<id>`), never the quoted text. On an ask, `dispatch.question` holds the start of the question's text; `dispatch read` gives the whole.
- `supersedes`: an earlier delivery this one replaces; act on this one in its place.
- `reply_with`: the direct-reply call to the sender. On a Dispatch frame it is `command:`, the `dispatch` command line that answers it, with `...` where your text goes.
- `reply_role`: the role-publish reply call, when the sender holds a role.
- `summary`: the one-line source summary.
- `message`: the complete payload; it can hold several paragraphs.
- `note`: the payload names another session; never use that quoted id as the recipient.
- `unrecognised`: present when the payload failed validation or came from an unknown source. It names what failed, not every unknown key.

## Who receives a publish

It depends on where a session takes its deliveries, not on whether the topic is built in or your own.

- An Oh My Pi or Claude Code session takes its messages from a durable consumer the listener keeps for it on Envoy's stream and relays to it in order, acknowledging each one only once the session wrote it. So every message the stream keeps — your inbox, and each topic you follow with `envoy_subscribe` — reaches you once, whoever sent it: your own publish included, and a subagent's publish to a topic its parent follows. One published while you were restarting, disconnected or not running reaches you when you are back, up to 72 hours later. A session does not take its messages from a NATS subscription of its own; `packages/envoy/AGENTS.md`, "Session consumers", is the rule.
- A team or other ad-hoc topic is the exception: the stream keeps none, so such a topic reaches only the sessions connected and subscribed when it is published, and nothing recovers it. The listener also forwards the message to the inbox of every other session registered on the topic with `envoy_subscribe`, and the stream keeps that copy, so a session that registered it gets it once after a restart too. What a team topic you subscribed to without registering carried meanwhile is gone.
- A session the listener delivers to by push, over a serve port (such as the OpenCode plugin), does not receive its own publish, and receives a team topic only through the listener that owns it when that listener runs with `ENVOY_SUBSCRIBE_ALL_TOPICS=1` (the shared hub's listener does not). It receives the stored messages for it by the listener's push, which the listener repeats until the port accepts each one.
- A role is not a subject its holder subscribes to: the listener finds the live holder and forwards the message to it, and drops the message when that holder is the message's source session. The stream keeps the forward on the holder's inbox, so the holder's session gets it once, also after a restart. A role message no live holder can take is kept for the session that claims the role next, which is handed it once, within one hour of the keep.
- A session whose plugin, or whose listener, is of an earlier release falls back to the core path while the rollout lasts (dispatch://INFRA-771 removes it): there it receives live on its own NATS subscriptions, and recovers what the stream stored for it itself, marked as published while its connection was down. It does not recover a role message that way.

## Listener API errors

When you call the listener's `/v1` API directly, every error response is JSON, and when a field is at fault, `expected` names it: fix that field and retry.
