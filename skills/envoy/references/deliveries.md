# Envoy deliveries

Read this when a delivery field is unclear, or when a publish did not reach a session you expected.

## A rendered delivery

Envoy renders an annotated delivery before its source summary and complete payload:

```text
envoy:
  to: you (ses_example_recipient)
  from: 01a0bbbb-cccc-7ddd-eeee-0123456789ab (Reviewer)
  at: "2026-09-07T04:41:12Z"
  delivered: "2026-09-07T04:41:13Z"
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
- `at`: when the sender sent it, by the sender's clock. It is not the present time: a delivery read late carries an old `at`.
- `delivered`: when this session got the delivery. An Oh My Pi session shows it in every model request, beside `at`, or beside `published_at` on a notice that names a message the session could not be shown. A session the host moves to shows when it got the delivery there, which can be later than another session got it. A session that got the delivery before its process restarted can show an estimate of that time, taken from its transcript, never later than the delivery.
- `waited`: present when `delivered` is 15 minutes or more after the send time: how long the delivery waited, in words. An old send time on a late delivery says the delivery waited, not that a clock is wrong.
- `id`: the delivery id; pass it as `in_reply_to` when you reply.
- `by`: the expiry deadline, when the sender set one.
- `urgency`: how soon the sender asks you to act: `low`, `med`, `high` or `blocking`. It is for you to read only: it does not change when a delivery reaches you, or its place among others.
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

- A session that holds its own NATS connection (every Oh My Pi session, and a Claude Code one) receives every message published on a subject it subscribes to, whoever sent it: its own publish included, and a subagent's publish to a topic the parent subscribes to. This holds for a topic the stream keeps and for a live team topic alike while it is connected. After an outage, once its connection or subscription is back, the session recovers from the stream what was published meanwhile on its own inbox and on each kept topic it follows, up to 72 hours back, and is shown each message once, marked as published while its connection was down. That includes a team topic it registered with the listener (every topic it follows with `envoy_subscribe`): the listener forwards each team topic message to the inbox of every session registered on it but the sender, and the stream keeps that copy. A team topic it subscribes to without registering it is gone for the outage.
- A session the listener delivers to by push, over a serve port (such as the OpenCode plugin), does not receive its own publish, and receives a team topic only through the listener that owns it when that listener runs with `ENVOY_SUBSCRIBE_ALL_TOPICS=1` (the shared hub's listener does not).
- A role is not a subject its holder subscribes to: the listener finds the live holder and forwards the message to it, and drops the message when that holder is the message's source session.

## Listener API errors

When you call the listener's `/v1` API directly, every error response is JSON, and when a field is at fault, `expected` names it: fix that field and retry.
